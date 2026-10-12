package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
	"sort"
	"time"
)

// Installed once by NewRuntime; never accepted from an execution request.
type TurnSkillOwner interface {
	InspectTurnSkillsTx(context.Context, Tx, domain.Scope, string) ([]domain.TurnSkill, error)
}

type skillInvocationProof struct {
	ID, Attempt, Binding, Capability, Hash, Output, Receipt, CommandHash string
	Started, Deadline                                                    time.Time
	Fence                                                                uint64
}

func (s *Store) evaluateProgramSkills(ctx context.Context, u *unit, r BarrierRequest, meta domain.MockResultMetadata, body []byte) ([]domain.CandidateResource, error) {
	if s.TurnSkills == nil {
		return nil, ErrDenied
	}
	var actor string
	if e := u.tx.QueryRowContext(ctx, "SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&actor); e != nil {
		return nil, e
	}
	skills, e := s.TurnSkills.InspectTurnSkillsTx(ctx, u, *r.WorldScope, actor)
	if e != nil {
		return nil, e
	}
	programs := []domain.TurnSkill{}
	for _, skill := range skills {
		if skill.Capability == "SIGN_CUSTODY" {
			programs = append(programs, skill)
		}
	}
	if len(programs) == 0 {
		return s.CandidateOwner.EvaluateOriginalCandidateTx(ctx, u, *r.WorldScope, r.TaskID, meta.Identity.AttemptID, body)
	}
	resources, output, e := s.CandidateOwner.EvaluateSkillActionsTx(ctx, u, *r.WorldScope, r.TaskID, meta.Identity.AttemptID, body)
	if e != nil {
		return nil, e
	}
	for _, skill := range programs {
		if e = s.completeSkill(ctx, u, meta.Identity.AttemptID, meta.Identity.AuthorizationID, r.CommitKey, r.PayloadHash, skill, output[:]); e != nil {
			return nil, e
		}
	}
	if e = s.appendSkillReceipt(ctx, u, r, meta.Identity.AttemptID, "SIGN_CUSTODY"); e != nil {
		return nil, e
	}
	return resources, nil
}

func skillReceiptKey(task, attempt, capability string) string {
	return turnIdentity(task, attempt, "skill-adapter", capability)
}
func skillProofHash(proofs []skillInvocationProof, attempt, capability string) ([32]byte, bool, error) {
	var selected []skillInvocationProof
	seen := map[string]bool{}
	for _, p := range proofs {
		if p.Attempt == attempt && p.Capability == capability && !seen[p.ID] {
			selected = append(selected, p)
			seen[p.ID] = true
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	_, h, e := harnessPayload(selected)
	return h, len(selected) > 0, e
}
func (s *Store) appendSkillReceipt(ctx context.Context, u *unit, r BarrierRequest, attempt, capability string) error {
	h, exists, e := skillProofHash(u.skillProofs, attempt, capability)
	if e != nil || !exists {
		return e
	}
	key := skillReceiptKey(u.taskID, attempt, capability)
	if e = u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}}); e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,'WORLD',?,?,?, ?,?,'COMMIT')", key, u.scope.TenantID, u.scope.ProductID, r.OwnerSubjectID, r.WorldScope.WorldInstanceID, u.taskID, "SKILL_ADAPTER_"+capability, key, h[:])
	return e
}

// Repeated owning entry/finish checks preserve the first invocation's finite
// deadline, including when another attempt or worker is selected.
func checkSkillDeadlines(ctx context.Context, u *unit) error {
	var expired bool
	if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_skill_invocations WHERE tenant_id=? AND product_id=? AND task_id=? AND deadline<=UTC_TIMESTAMP(6))", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&expired); e != nil {
		return e
	}
	if expired {
		return ErrDenied
	}
	return nil
}

// Existing invocation rows are execution evidence, never an authorization by
// themselves. Reconstruct the fixed adapter identity from its original attempt
// and current qualified definition before using it at another owning boundary.
func (s *Store) checkPersistedSkills(ctx context.Context, u *unit, r BarrierRequest) error {
	rows, e := u.tx.QueryContext(ctx, "SELECT invocation_id,attempt_id,binding_id,capability,definition,definition_hash,parent_invocation_id,depth,started_at,deadline,fence,state,output_hash,receipt_key,command_hash FROM harness_skill_invocations WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY invocation_id LIMIT 17", u.scope.TenantID, u.scope.ProductID, u.taskID)
	if e != nil {
		return e
	}
	type record struct {
		p      skillInvocationProof
		skill  domain.TurnSkill
		parent sql.NullString
		depth  int
		state  string
	}
	var records []record
	for rows.Next() {
		var v record
		var raw, hash, out, command []byte
		if e = rows.Scan(&v.p.ID, &v.p.Attempt, &v.p.Binding, &v.p.Capability, &raw, &hash, &v.parent, &v.depth, &v.p.Started, &v.p.Deadline, &v.p.Fence, &v.state, &out, &v.p.Receipt, &command); e != nil {
			rows.Close()
			return e
		}
		v.p.CommandHash = hex.EncodeToString(command)
		v.p.Hash, v.p.Output = hex.EncodeToString(hash), hex.EncodeToString(out)
		if json.Unmarshal(raw, &v.skill) != nil {
			rows.Close()
			return ErrDenied
		}
		records = append(records, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(records) > 16 {
		return ErrDenied
	}
	if s.TurnSkills == nil || r.WorldScope == nil {
		return ErrDenied
	}
	var actor string
	if e = u.tx.QueryRowContext(ctx, "SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&actor); e != nil {
		return e
	}
	skills, e := s.TurnSkills.InspectTurnSkillsTx(ctx, u, *r.WorldScope, actor)
	if e != nil {
		return e
	}
	current := map[string]domain.TurnSkill{}
	for _, v := range skills {
		current[v.BindingID] = v
	}
	observed := map[string]bool{}
	for _, v := range records {
		observed[v.p.ID] = true
	}
	rows, e = u.tx.QueryContext(ctx, "SELECT a.attempt_id,EXISTS(SELECT 1 FROM harness_candidate_approvals p WHERE p.tenant_id=a.tenant_id AND p.product_id=a.product_id AND p.task_id=a.task_id AND p.attempt_id=a.attempt_id) FROM attempts a WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? LIMIT 3", u.scope.TenantID, u.scope.ProductID, u.taskID)
	if e != nil {
		return e
	}
	count := 0
	for rows.Next() {
		count++
		var attempt string
		var approved bool
		if e = rows.Scan(&attempt, &approved); e != nil {
			rows.Close()
			return e
		}
		for _, skill := range skills {
			if (skill.Capability == "TEXT" || approved) && !observed[turnIdentity(u.taskID, attempt, skill.BindingID, "skill")] {
				rows.Close()
				return ErrDenied
			}
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if count > 2 {
		return ErrDenied
	}
	for _, v := range records {
		p := v.p
		_, hash, e := harnessPayload(v.skill)
		if e != nil {
			return e
		}
		if v.parent.Valid || v.depth != 1 || v.state != "COMPLETED" || hex.EncodeToString(hash[:]) != p.Hash || current[p.Binding] != v.skill || p.Capability != v.skill.Capability || p.ID != turnIdentity(u.taskID, p.Attempt, p.Binding, "skill") || !p.Started.Before(p.Deadline) {
			return ErrDenied
		}
		var authorization, route, request string

		if e = u.tx.QueryRowContext(ctx, "SELECT a.authorization_id,t.route,t.request_key FROM dispatch_authorizations a JOIN attempts t USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID, p.Attempt).Scan(&authorization, &route, &request); e != nil {
			return e
		}
		original, e := queryReceipt(ctx, u.tx, p.Receipt)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrDenied
		}
		if e != nil {
			return e
		}
		if original.tenant != u.scope.TenantID || original.product != u.scope.ProductID || original.owner != r.OwnerSubjectID || original.task != u.taskID || original.kind != "WORLD" || original.world.String != r.WorldScope.WorldInstanceID || original.command != p.Receipt || hex.EncodeToString(original.hash) != p.CommandHash {
			return ErrDenied
		}
		if p.Capability == "TEXT" {
			_, out, e := harnessPayload(struct{ Attempt, Authorization, Route, Request string }{p.Attempt, authorization, route, request})
			if e != nil {
				return e
			}
			if original.operation != "TURN_DISPATCH" || original.phase != "AUTHORIZE" || hex.EncodeToString(out[:]) != p.Output {
				return ErrDenied
			}
		} else if p.Capability == "SIGN_CUSTODY" {
			if original.operation != "HARNESS_CANDIDATE_APPROVE" || original.phase != "COMMIT" || p.Receipt != turnIdentity(p.Attempt, "approval") || len(p.Output) != 64 {
				return ErrDenied
			}
		} else {
			return ErrDenied
		}
		u.skillProofs = append(u.skillProofs, p)
	}
	for _, v := range records {
		p := v.p
		key := skillReceiptKey(u.taskID, p.Attempt, p.Capability)
		if e = u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}}); e != nil {
			return e
		}
		h, _, e := skillProofHash(u.skillProofs, p.Attempt, p.Capability)
		if e != nil {
			return e
		}
		receipt, e := queryReceipt(ctx, u.tx, key)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrDenied
		}
		if e != nil {
			return e
		}
		if receipt.tenant != u.scope.TenantID || receipt.product != u.scope.ProductID || receipt.owner != r.OwnerSubjectID || receipt.task != u.taskID || receipt.kind != "WORLD" || receipt.world.String != r.WorldScope.WorldInstanceID || receipt.operation != "SKILL_ADAPTER_"+p.Capability || receipt.phase != "COMMIT" || receipt.command != key || !bytes.Equal(receipt.hash, h[:]) {
			return ErrDenied
		}
	}
	return checkSkillDeadlines(ctx, u)
}

func skillStep(ctx context.Context, u *unit, name string) error {
	if e := u.RequireLock(harnessTaskKey(u.scope, u.taskID)); e != nil {
		return e
	}
	ids := []string{u.taskID}
	if p := u.rehearsalExecution; p != nil {
		ids = append(ids, p.root.TaskID)
	}
	for _, id := range ids {
		if e := u.RequireLock(harnessTaskKey(u.scope, id)); e != nil {
			return e
		}
		res, e := u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET steps_used=steps_used+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE' AND steps_used<max_steps AND execute_by>UTC_TIMESTAMP(6)", u.scope.TenantID, u.scope.ProductID, id)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrDenied
		}
	}
	_, e := u.tx.ExecContext(ctx, "INSERT INTO harness_turn_steps(tenant_id,product_id,task_id,step_number,from_state,to_state,fence,occurred_at) SELECT e.tenant_id,e.product_id,e.task_id,e.steps_used,t.fsm_state,?,?,UTC_TIMESTAMP(6) FROM harness_envelopes e JOIN harness_turns t USING(tenant_id,product_id,task_id) WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", name, u.fence, u.scope.TenantID, u.scope.ProductID, u.taskID)
	return e
}

func (s *Store) executeTextSkills(ctx context.Context, u *unit, d DispatchRequest) error {
	if d.WorldScope == nil || d.HarnessQuoteID == "" {
		return nil
	}
	if s.TurnSkills == nil {
		return ErrDenied
	}
	var actor string
	if e := u.tx.QueryRowContext(ctx, "SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&actor); e != nil {
		return e
	}
	skills, e := s.TurnSkills.InspectTurnSkillsTx(ctx, u, *d.WorldScope, actor)
	if e != nil {
		return e
	}
	for _, skill := range skills {
		if skill.Capability != "TEXT" {
			continue
		}
		_, hash, e := harnessPayload(struct{ Attempt, Authorization, Route, Request string }{d.AttemptID, d.AuthorizationID, d.Route, d.RequestKey})
		if e != nil {
			return e
		}
		if e = s.completeSkill(ctx, u, d.AttemptID, d.AuthorizationID, d.CommitKey, d.PayloadHash, skill, hash[:]); e != nil {
			return e
		}
	}
	return s.appendSkillReceipt(ctx, u, d.BarrierRequest, d.AttemptID, "TEXT")
}

// Completion is the real typed adapter output, not a model or business result:
// TEXT delegated this exact dispatch; PROGRAM evaluated a real action copy.
func (s *Store) completeSkill(ctx context.Context, u *unit, attempt, authorization, receipt string, commandHash [32]byte, skill domain.TurnSkill, output []byte) error {
	if e := u.RequireLock(LockKey{Rank: 60, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.taskID, attempt, authorization}}); e != nil {
		return e
	}
	if skill.MaxDepth < 1 || skill.MaxDepth > 4 || skill.DurationMS == 0 {
		return ErrDenied
	}
	id := turnIdentity(u.taskID, attempt, skill.BindingID, "skill")
	raw, hash, e := harnessPayload(skill)
	if e != nil {
		return e
	}
	var old string
	e = u.tx.QueryRowContext(ctx, "SELECT invocation_id FROM harness_skill_invocations WHERE tenant_id=? AND product_id=? AND invocation_id=?", u.scope.TenantID, u.scope.ProductID, id).Scan(&old)
	if e == nil {
		return ErrConflict
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	var started, deadline, now time.Time
	if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6),LEAST(execute_by,TIMESTAMPADD(MICROSECOND,?,UTC_TIMESTAMP(6))) FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", skill.DurationMS*1000, u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&started, &deadline); e != nil {
		return e
	}
	now = started
	var firstStart, firstDeadline time.Time
	e = u.tx.QueryRowContext(ctx, "SELECT started_at,deadline FROM harness_skill_invocations WHERE tenant_id=? AND product_id=? AND task_id=? AND binding_id=? ORDER BY started_at,invocation_id LIMIT 1", u.scope.TenantID, u.scope.ProductID, u.taskID, skill.BindingID).Scan(&firstStart, &firstDeadline)
	if e == nil {
		started, deadline = firstStart, firstDeadline
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if !now.Before(deadline) {
		return ErrDenied
	}
	if e = skillStep(ctx, u, "SKILL_START"); e != nil {
		return e
	}
	if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_skill_invocations(tenant_id,product_id,task_id,attempt_id,invocation_id,binding_id,capability,definition,definition_hash,parent_invocation_id,depth,started_at,deadline,fence,state,output_hash,receipt_key,command_hash) VALUES(?,?,?,?,?,?,?,?,? ,NULL,1,?,?,?,'COMPLETED',?,?,?)", u.scope.TenantID, u.scope.ProductID, u.taskID, attempt, id, skill.BindingID, skill.Capability, raw, hash[:], started, deadline, u.fence, output, receipt, commandHash[:]); e != nil {
		return e
	}
	if e = skillStep(ctx, u, "SKILL_FINISH"); e != nil {
		return e
	}
	u.skillProofs = append(u.skillProofs, skillInvocationProof{ID: id, Attempt: attempt, Binding: skill.BindingID, Capability: skill.Capability, Hash: hex.EncodeToString(hash[:]), Output: hex.EncodeToString(output), Receipt: receipt, CommandHash: hex.EncodeToString(commandHash[:]), Started: started, Deadline: deadline, Fence: u.fence})
	return nil
}

func (u *unit) checkSkillProofs(ctx context.Context) error {
	for _, p := range u.skillProofs {
		var got skillInvocationProof
		var raw, hash, out, command []byte
		var parent sql.NullString
		var depth int
		var state string
		e := u.tx.QueryRowContext(ctx, "SELECT invocation_id,attempt_id,binding_id,capability,definition,definition_hash,parent_invocation_id,depth,started_at,deadline,fence,state,output_hash,receipt_key,command_hash FROM harness_skill_invocations WHERE tenant_id=? AND product_id=? AND task_id=? AND invocation_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID, p.ID).Scan(&got.ID, &got.Attempt, &got.Binding, &got.Capability, &raw, &hash, &parent, &depth, &got.Started, &got.Deadline, &got.Fence, &state, &out, &got.Receipt, &command)
		if e != nil {
			return e
		}
		got.CommandHash = hex.EncodeToString(command)
		got.Hash, got.Output = hex.EncodeToString(hash), hex.EncodeToString(out)
		var skill domain.TurnSkill
		if json.Unmarshal(raw, &skill) != nil {
			return ErrDenied
		}
		_, h, e := harnessPayload(skill)
		if e != nil {
			return e
		}
		if parent.Valid || depth != 1 || state != "COMPLETED" || hex.EncodeToString(h[:]) != got.Hash || !reflect.DeepEqual(p, got) {
			return ErrConflict
		}
		key := skillReceiptKey(u.taskID, p.Attempt, p.Capability)
		receipt, e := queryReceipt(ctx, u.tx, key)
		if e != nil {
			return e
		}
		expected, _, e := skillProofHash(u.skillProofs, p.Attempt, p.Capability)
		if e != nil {
			return e
		}
		if receipt.task != u.taskID || receipt.owner != u.scope.SubjectID || receipt.tenant != u.scope.TenantID || receipt.product != u.scope.ProductID || receipt.kind != "WORLD" || !receipt.world.Valid || receipt.world.String != u.world || receipt.operation != "SKILL_ADAPTER_"+p.Capability || receipt.command != key || receipt.phase != "COMMIT" || !bytes.Equal(receipt.hash, expected[:]) {
			return ErrConflict
		}
	}
	if len(u.skillProofs) > 0 {
		return checkSkillDeadlines(ctx, u)
	}
	return nil
}
