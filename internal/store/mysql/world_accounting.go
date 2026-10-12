package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"strings"
)

// Only the installation's registered owning adapter supplies this admission.
// It carries no owner permission; the original migration barrier checks that.
type WorldAccountingIdentity struct {
	Installation string
	Generation   uint64
}
type WorldAccountingAuthority interface {
	EnterWorldAccounting(context.Context) (context.Context, WorldAccountingIdentity, func(), error)
}
type accountingTask struct {
	ID, Kind, Owner, Operation, Accepted, Retirement string
	Attempts                                         []cutoffAttempt
	Holds                                            []Reservation
	Rounds                                           []cutoffRound
	Appends                                          []accountingAppend
}
type accountingAppend struct{ Kind, ID string }
type worldAccountingPlan struct {
	Scope                   domain.Scope
	Tasks                   []accountingTask
	Keys                    []LockKey
	Identity                WorldAccountingIdentity
	Operation, Receipt, Own string
}
type accountingQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// The fixed bounded inventory is recomputed under world40 before it is used.
// No public request supplies the task set, account list or cross-task keys.
func loadWorldAccounting(ctx context.Context, q accountingQuery, s domain.Scope, own string) (*worldAccountingPlan, error) {
	p := &worldAccountingPlan{Scope: s, Own: own}
	t, prod, w := s.TenantID, s.ProductID, s.WorldInstanceID
	rows, e := q.QueryContext(ctx, "SELECT t.task_id,t.task_kind,t.owner_subject_id,COALESCE(j.operation,''),COALESCE(h.accepted_commit_key,''),COALESCE(z.receipt_key,'') FROM tasks t LEFT JOIN registered_jobs j USING(tenant_id,product_id,task_id) LEFT JOIN harness_turns h USING(tenant_id,product_id,task_id) LEFT JOIN harness_task_retirements z USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.world_instance_id=? AND t.task_id<>? ORDER BY t.task_id LIMIT 201", t, prod, w, own)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var a accountingTask
		if e = rows.Scan(&a.ID, &a.Kind, &a.Owner, &a.Operation, &a.Accepted, &a.Retirement); e != nil {
			rows.Close()
			return nil, e
		}
		p.Tasks = append(p.Tasks, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(p.Tasks) > 200 {
		return nil, ErrDenied
	}
	for n := range p.Tasks {
		a := &p.Tasks[n]
		if a.Owner != s.SubjectID {
			return nil, ErrDenied
		}
		p.Keys = append(p.Keys, LockKey{Rank: 50, IDs: []string{t, prod, a.ID}})
		rows, e = q.QueryContext(ctx, "SELECT a.attempt_id,COALESCE(d.authorization_id,''),COALESCE(a.marker_commit_key,''),a.request_key,a.route,HEX(a.request_hash),a.slot,a.send_started_at IS NOT NULL,COALESCE(x.supplier_atoms,-1) FROM attempts a LEFT JOIN dispatch_authorizations d USING(tenant_id,product_id,task_id,attempt_id) LEFT JOIN harness_attempt_allocations x USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", t, prod, a.ID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var v cutoffAttempt
			if e = rows.Scan(&v.Attempt, &v.Authorization, &v.Marker, &v.Request, &v.Route, &v.RequestHash, &v.Slot, &v.Started, &v.Amount); e != nil {
				rows.Close()
				return nil, e
			}
			a.Attempts = append(a.Attempts, v)
			p.Keys = append(p.Keys, LockKey{Rank: 60, IDs: []string{t, prod, a.ID, v.Attempt, v.Authorization}})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if len(a.Attempts) > 2 {
			return nil, ErrDenied
		}
		rows, e = q.QueryContext(ctx, "SELECT kind,scope_key,account_id,reservation_id,amount FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY reservation_id LIMIT 9", t, prod, a.ID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var v Reservation
			if e = rows.Scan(&v.Kind, &v.ScopeKey, &v.AccountID, &v.ReservationID, &v.Amount); e != nil {
				rows.Close()
				return nil, e
			}
			a.Holds = append(a.Holds, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if len(a.Holds) > 8 {
			return nil, ErrDenied
		}
		p.Keys = append(p.Keys, holdLocks(a.Holds)...)
		rows, e = q.QueryContext(ctx, "SELECT attempt_id,phase,round_number,original_receipt_key FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY attempt_id,phase,round_number LIMIT 81", t, prod, a.ID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var v cutoffRound
			if e = rows.Scan(&v.Attempt, &v.Phase, &v.Number, &v.Receipt); e != nil {
				rows.Close()
				return nil, e
			}
			v.Prepare = turnIdentity(v.Attempt, v.Phase, "prepare", fmt.Sprint(v.Number))
			a.Rounds = append(a.Rounds, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if len(a.Rounds) > 80 {
			return nil, ErrDenied
		}
		for _, table := range []string{"approved_results", "outbox", "events"} {
			rows, e = q.QueryContext(ctx, "SELECT record_id FROM "+table+" WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY record_id LIMIT 101", t, prod, a.ID)
			if e != nil {
				return nil, e
			}
			count := 0
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					return nil, e
				}
				count++
				a.Appends = append(a.Appends, accountingAppend{table, id})
				p.Keys = append(p.Keys, LockKey{Rank: 110, Kind: table, IDs: []string{t, prod, id}})
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			if count > 100 {
				return nil, ErrDenied
			}
		}
		receiptKeys := []string{turnIdentity(a.ID, "world-commit"), turnIdentity(a.ID, "cutoff"), turnIdentity(a.ID, "authorize")}
		for _, v := range a.Attempts {
			receiptKeys = append(receiptKeys, turnIdentity(v.Attempt, "authorize"))
			if v.Marker != "" {
				receiptKeys = append(receiptKeys, v.Marker)
			}
		}
		for _, v := range a.Rounds {
			receiptKeys = append(receiptKeys, v.Receipt, v.Prepare)
		}
		rows, e = q.QueryContext(ctx, "SELECT commit_key FROM commit_receipts WHERE tenant_id=? AND product_id=? AND task_id=? AND operation IN ('INSTANCE_CONTROL','INSTANCE_MIGRATION','MEMORY_GOVERNANCE','HARNESS_TURN_COMMIT') ORDER BY commit_key LIMIT 101", t, prod, a.ID)
		if e != nil {
			return nil, e
		}
		count := 0
		for rows.Next() {
			var k string
			if e = rows.Scan(&k); e != nil {
				rows.Close()
				return nil, e
			}
			receiptKeys = append(receiptKeys, k)
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if count > 100 {
			return nil, ErrDenied
		}
		if a.Accepted != "" {
			receiptKeys = append(receiptKeys, a.Accepted)
		}
		if a.Retirement != "" {
			receiptKeys = append(receiptKeys, a.Retirement)
		}
		for _, k := range receiptKeys {
			p.Keys = append(p.Keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{k}})
		}
	}
	p.Keys, e = PlanLocks(p.Keys)
	return p, e
}
func accountingSetHash(p *worldAccountingPlan) string {
	members := append([]accountingTask(nil), p.Tasks...)
	for n := range members {
		members[n].Retirement = ""
	}
	_, h, _ := harnessPayload(members)
	return hex.EncodeToString(h[:])
}
func (s *Store) prepareWorldAccounting(ctx context.Context, r *BarrierRequest) (context.Context, func(), error) {
	noop := func() {}
	if r.Runtime == nil || r.Purpose != MigrationPurpose || r.Runtime.Action != "MIGRATE" {
		return ctx, noop, nil
	}
	switch r.Operation {
	case "previewInstanceMigration", "executeInstanceMigration", "INSTANCE_MIGRATION":
	default:
		return ctx, noop, nil
	}
	own := ""
	if r.Operation == "INSTANCE_MIGRATION" {
		own = r.TaskID
	}
	p, e := loadWorldAccounting(ctx, s.DB, *r.WorldScope, own)
	if e != nil {
		return ctx, noop, e
	}
	managed := false
	for _, a := range p.Tasks {
		if a.Kind == "TURN" || len(a.Attempts) > 0 {
			managed = true
		}
	}
	if !managed {
		return ctx, noop, nil
	}
	if s.WorldAccountingAuthority == nil || s.GenerationContext == nil || s.CandidateOwner == nil || s.TurnOwner == nil || s.OriginalResultReader == nil || s.ApprovedTurnReader == nil || s.ProviderEvidence == nil || s.QueryAuthority == nil || s.SettlementAuthority == nil {
		return ctx, noop, ErrDenied
	}
	ctx, p.Identity, noop, e = s.WorldAccountingAuthority.EnterWorldAccounting(ctx)
	if e != nil {
		return ctx, func() {}, e
	}
	p.Operation, p.Receipt = r.Operation, r.CommitKey
	r.worldAccounting = p
	r.Locks = append(r.Locks, p.Keys...)
	return ctx, noop, nil
}
func (p *worldAccountingPlan) contains(k LockKey) bool {
	for _, x := range p.Keys {
		if string(lockBytes(x)) == string(lockBytes(k)) {
			return true
		}
	}
	return false
}

// The exception is private, fixed-plan and only for the current world's actual
// original rows; ordinary resource validation is unchanged.
func acquireAccountingKey(ctx context.Context, tx *sql.Tx, p *worldAccountingPlan, k LockKey) error {
	if !p.contains(k) {
		return ErrDenied
	}
	if e := marker(ctx, tx, k); e != nil {
		return e
	}
	var got string
	var e error
	switch k.Rank {
	case 60:
		e = tx.QueryRowContext(ctx, "SELECT a.attempt_id FROM attempts a JOIN dispatch_authorizations d USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=? AND d.authorization_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3], k.IDs[4]).Scan(&got)
	case 70:
		e = tx.QueryRowContext(ctx, "SELECT account_id FROM budget_accounts WHERE kind=? AND scope_key=? AND account_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1]).Scan(&got)
	case 80:
		e = tx.QueryRowContext(ctx, "SELECT reservation_id FROM reservations WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&got)
	default:
		return ErrDenied
	}
	return e
}

// AssertWorldAccounting never mutates. Preview/execute examine exact eligible
// closures; the migration worker must also consume the atomically stored seal.
func (u *unit) AssertWorldAccounting(ctx context.Context, s domain.Scope) (string, error) {
	p := u.worldAccounting
	if p == nil || p.Scope != s || u.purpose != MigrationPurpose || u.runtime == nil || u.runtime.Action != "MIGRATE" {
		return "", ErrDenied
	}
	for _, k := range p.Keys {
		if e := u.RequireLock(k); e != nil {
			return "", e
		}
	}
	fresh, e := loadWorldAccounting(ctx, u.tx, s, p.Own)
	if e != nil {
		return "", e
	}
	hash := accountingSetHash(fresh)
	if hash != accountingSetHash(p) {
		return "", ErrReplan
	}
	var valid bool
	if e = u.tx.QueryRowContext(ctx, "SELECT generation=? FROM memory_installation WHERE installation_id=?", p.Identity.Generation, p.Identity.Installation).Scan(&valid); e != nil || !valid {
		if e != nil {
			return "", e
		}
		return "", ErrDenied
	}
	for _, a := range fresh.Tasks {
		if e = u.checkAccountingTask(ctx, s, a); e != nil {
			return "", e
		}
	}
	if p.Operation == "INSTANCE_MIGRATION" {
		var receipt, stored string
		e = u.tx.QueryRowContext(ctx, "SELECT set_hash,receipt_key FROM harness_world_seals WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND installation_id=? AND installation_generation=?", s.TenantID, s.ProductID, s.WorldInstanceID, p.Identity.Installation, p.Identity.Generation).Scan(&stored, &receipt)
		if e != nil {
			return "", e
		}
		if stored != hash {
			return "", ErrReplan
		}
		r, e := queryReceipt(ctx, u.tx, receipt)
		if e != nil || r.operation != "executeInstanceMigration" || r.world.String != s.WorldInstanceID || r.owner != s.SubjectID {
			return "", ErrDenied
		}
	}
	return hash, nil
}
func (u *unit) checkAccountingTask(ctx context.Context, s domain.Scope, a accountingTask) error {
	t, p := s.TenantID, s.ProductID
	var state string
	var cancelled bool
	if e := u.tx.QueryRowContext(ctx, "SELECT state,cancelled FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, a.ID).Scan(&state, &cancelled); e != nil {
		return e
	}
	if a.Kind != "TURN" {
		if len(a.Attempts) != 0 || len(a.Holds) != 0 {
			return ErrDenied
		}
		if a.Kind != "CONTROL" && a.Kind != "MIGRATION" && a.Kind != "GOVERNANCE" {
			return ErrDenied
		}
		if a.Operation != map[string]string{"CONTROL": "INSTANCE_CONTROL", "MIGRATION": "INSTANCE_MIGRATION", "GOVERNANCE": "MEMORY_GOVERNANCE"}[a.Kind] {
			return ErrDenied
		}
		var known bool
		e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE tenant_id=? AND product_id=? AND task_id=? AND world_instance_id=? AND owner_subject_id=? AND operation=? AND phase='COMMIT')", t, p, a.ID, s.WorldInstanceID, s.SubjectID, a.Operation).Scan(&known)
		if e != nil {
			return e
		}
		if state != "COMMITTED" || !known {
			return ErrDenied
		}
		return nil
	}
	var leased bool
	if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM outbox WHERE tenant_id=? AND product_id=? AND task_id=? AND (lease_owner IS NOT NULL OR status NOT IN ('PENDING','RETIRED')))", t, p, a.ID).Scan(&leased); e != nil {
		return e
	}
	if leased {
		return ErrDenied
	}
	if a.Operation != "TURN" || len(a.Holds) != 4+2*len(a.Attempts) {
		return ErrDenied
	}
	if e := u.checkAccountingOriginals(ctx, s, a); e != nil {
		return e
	}
	var env string
	var calls int
	var generation uint64
	if e := u.tx.QueryRowContext(ctx, "SELECT state,calls_allocated,narrative_generation FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, a.ID).Scan(&env, &calls, &generation); e != nil {
		return e
	}
	if calls != len(a.Attempts) || generation == 0 {
		return ErrDenied
	}
	if env != "COMMITTED" && env != "CUTOFF" && env != "ACCOUNTING_ONLY" {
		return ErrDenied
	}
	world, e := queryReceipt(ctx, u.tx, turnIdentity(a.ID, "world-commit"))
	if e == nil {
		if !settlementReceiptScope(world, s, a.ID) || world.operation != "HARNESS_TURN_COMMIT" || state != "COMMITTED" {
			return ErrDenied
		}
		var known bool
		if e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)", t, p, a.ID, world.key).Scan(&known); e != nil || !known {
			return ErrDenied
		}
	} else {
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		var raw []byte
		var receipt string
		e = u.tx.QueryRowContext(ctx, "SELECT proof,receipt_key FROM harness_cutoffs WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, a.ID).Scan(&raw, &receipt)
		if e != nil {
			return e
		}
		var proof cutoffProof
		if json.Unmarshal(raw, &proof) != nil || proof.Scope != s || proof.Task != a.ID || proof.WorldReceipt != turnIdentity(a.ID, "world-commit") || proof.CheckedAt.Before(proof.SettleBy) || proof.CheckedAt.Before(proof.ExecuteBy) {
			return ErrDenied
		}
		r, e := queryReceipt(ctx, u.tx, receipt)
		_, h, _ := harnessPayload(proof)
		if e != nil || !settlementReceiptScope(r, s, a.ID) || r.operation != "HARNESS_CUTOFF" || !bytes.Equal(r.hash, h[:]) {
			return ErrDenied
		}
	}
	for _, round := range a.Rounds {
		var current string
		if e = u.tx.QueryRowContext(ctx, "SELECT state FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? AND round_number=?", t, p, a.ID, round.Attempt, round.Phase, round.Number).Scan(&current); e != nil {
			return e
		}
		pr, e := queryReceipt(ctx, u.tx, round.Prepare)
		if e != nil || !settlementReceiptScope(pr, s, a.ID) || pr.operation != "HARNESS_LOCAL_PREPARE" {
			return ErrDenied
		}
		rr, e := queryReceipt(ctx, u.tx, round.Receipt)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if current == "COMMITTED" && e != nil {
			return ErrDenied
		}
		if e == nil && !settlementReceiptScope(rr, s, a.ID) {
			return ErrDenied
		}
	}
	if env == "ACCOUNTING_ONLY" {
		var original, next uint64
		var receipt string
		if e = u.tx.QueryRowContext(ctx, "SELECT original_generation,retired_generation,receipt_key FROM harness_task_retirements WHERE tenant_id=? AND product_id=? AND task_id=? AND world_instance_id=?", t, p, a.ID, s.WorldInstanceID).Scan(&original, &next, &receipt); e != nil {
			return e
		}
		if original == 0 || next != original+1 || next != generation {
			return ErrDenied
		}
		r, e := queryReceipt(ctx, u.tx, receipt)
		if e != nil || r.operation != "executeInstanceMigration" || r.world.String != s.WorldInstanceID {
			return ErrDenied
		}
	} else if u.worldAccounting.Operation == "INSTANCE_MIGRATION" {
		return ErrDenied
	}
	return nil
}
func (u *unit) SealWorldAccounting(ctx context.Context, s domain.Scope) error {
	p := u.worldAccounting
	if p == nil {
		return nil
	}
	if p.Operation != "executeInstanceMigration" || !u.commandOnly {
		return ErrDenied
	}
	hash, e := u.AssertWorldAccounting(ctx, s)
	if e != nil {
		return e
	}
	for _, a := range p.Tasks {
		if a.Kind != "TURN" {
			continue
		}
		var state string
		var gen uint64
		if e = u.tx.QueryRowContext(ctx, "SELECT state,narrative_generation FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, a.ID).Scan(&state, &gen); e != nil {
			return e
		}
		if state == "ACCOUNTING_ONLY" {
			continue
		}
		if gen == ^uint64(0) {
			return ErrDenied
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_task_retirements(tenant_id,product_id,world_instance_id,task_id,original_generation,retired_generation,receipt_key) VALUES(?,?,?,?,?,?,?)", s.TenantID, s.ProductID, s.WorldInstanceID, a.ID, gen, gen+1, p.Receipt); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='ACCOUNTING_ONLY',narrative_generation=narrative_generation+1 WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, a.ID); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE outbox SET status='RETIRED',lease_owner=NULL,lease_expires_at=NULL WHERE tenant_id=? AND product_id=? AND task_id=? AND status='PENDING'", s.TenantID, s.ProductID, a.ID); e != nil {
			return e
		}
		// Preserve real COMMITTED history. No attempt or local PENDING is relabelled.
		if _, e = u.tx.ExecContext(ctx, "UPDATE tasks SET fence=fence+1,lease_owner=NULL,lease_expires_at=NULL WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, a.ID); e != nil {
			return e
		}
	}
	members := append([]accountingTask(nil), p.Tasks...)
	for n := range members {
		members[n].Retirement = ""
	}
	raw, _, e := harnessPayload(members)
	if e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_world_seals(tenant_id,product_id,world_instance_id,installation_id,installation_generation,set_hash,receipt_key,document) VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE installation_id=VALUES(installation_id),installation_generation=VALUES(installation_generation),set_hash=VALUES(set_hash),receipt_key=VALUES(receipt_key),document=VALUES(document)", s.TenantID, s.ProductID, s.WorldInstanceID, p.Identity.Installation, p.Identity.Generation, hash, p.Receipt, raw)
	return e
}

// Fixed terminal-only history proof. It cannot authorize original candidate IO,
// dispatch, an old lease or another world write.
func (u *unit) AssertCommittedHistory(ctx context.Context, s domain.Scope, task string) (bool, error) {
	if !u.approvedHistoryRead || !u.commandOnly || u.purpose != ExecutionPurpose || u.taskID != task || u.scope != s.ProductScope || u.world != s.WorldInstanceID {
		return false, ErrDenied
	}
	var state string
	var gen uint64
	var raw []byte
	if e := u.RequireLock(LockKey{Rank: 50, IDs: []string{s.TenantID, s.ProductID, task}}); e != nil {
		return false, e
	}
	if e := u.tx.QueryRowContext(ctx, "SELECT state,narrative_generation FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&state, &gen); e != nil {
		return false, e
	}
	if state != "COMMITTED" && state != "ACCOUNTING_ONLY" {
		return false, ErrDenied
	}
	if e := u.tx.QueryRowContext(ctx, "SELECT document FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?", s.TenantID, s.ProductID, task, turnIdentity(task, "world-commit")).Scan(&raw); e != nil {
		return false, e
	}
	var committed domain.CandidateCommit
	if json.Unmarshal(raw, &committed) != nil || committed.Metadata.Identity.Scope != s {
		return false, ErrDenied
	}
	key := turnIdentity(task, "world-commit")
	if e := u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}}); e != nil {
		return false, e
	}
	receipt, e := queryReceipt(ctx, u.tx, key)
	if e != nil || !settlementReceiptScope(receipt, s, task) || receipt.operation != "HARNESS_TURN_COMMIT" {
		return false, ErrDenied
	}
	if state == "COMMITTED" {
		if gen != committed.NarrativeGeneration {
			return false, ErrDenied
		}
	} else {
		var original, next uint64
		if e = u.tx.QueryRowContext(ctx, "SELECT original_generation,retired_generation,receipt_key FROM harness_task_retirements WHERE tenant_id=? AND product_id=? AND task_id=? AND world_instance_id=?", s.TenantID, s.ProductID, task, s.WorldInstanceID).Scan(&original, &next, &key); e != nil {
			return false, e
		}
		if next != gen || original != committed.NarrativeGeneration || next != original+1 {
			return false, ErrDenied
		}
		if e = u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}}); e != nil {
			return false, e
		}
		receipt, e = queryReceipt(ctx, u.tx, key)
		if e != nil || receipt.operation != "executeInstanceMigration" || receipt.world.String != s.WorldInstanceID || receipt.owner != s.SubjectID {
			return false, ErrDenied
		}
		var hash string
		var valid bool
		if e = u.tx.QueryRowContext(ctx, "SELECT z.document,z.set_hash,z.installation_generation=i.generation,z.receipt_key FROM harness_world_seals z JOIN memory_installation i ON i.installation_id=z.installation_id WHERE z.tenant_id=? AND z.product_id=? AND z.world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&raw, &hash, &valid, &key); e != nil || !valid {
			return false, ErrDenied
		}
		if e = u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}}); e != nil {
			return false, e
		}
		receipt, e = queryReceipt(ctx, u.tx, key)
		if e != nil || receipt.operation != "executeInstanceMigration" || receipt.world.String != s.WorldInstanceID || receipt.owner != s.SubjectID {
			return false, ErrDenied
		}
		var members []accountingTask
		if json.Unmarshal(raw, &members) != nil {
			return false, ErrDenied
		}
		_, h, _ := harnessPayload(members)
		if hash != hex.EncodeToString(h[:]) {
			return false, ErrDenied
		}
		found := false
		for _, member := range members {
			if member.ID == task && member.Kind == "TURN" && member.Owner == s.SubjectID {
				for _, a := range member.Attempts {
					if a.Attempt == committed.Metadata.Identity.AttemptID && a.Request == committed.Metadata.Identity.RequestKey && strings.EqualFold(a.RequestHash, committed.Metadata.Identity.RequestHash) {
						found = true
					}
				}
			}
		}
		if !found {
			return false, ErrDenied
		}
	}
	var current domain.Versions
	if e = u.tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&current.ReleaseID, &current.WorldRevision, &current.ControlVersion, &current.PrivacyEpoch, &current.ViewEpoch); e != nil {
		return false, e
	}
	chain, e := loadCommittedTransitions(ctx, u.tx, s)
	if e != nil {
		return false, e
	}
	expected := committed.Versions
	for _, v := range chain {
		if v.Before.ControlVersion < committed.Versions.ControlVersion {
			continue
		}
		if v.Before.ControlVersion >= current.ControlVersion {
			break
		}
		for _, k := range v.locks(s) {
			if e = u.RequireLock(k); e != nil {
				return false, e
			}
		}
		if v.Scope != s || v.Before.ReleaseID != expected.ReleaseID || v.Before.ControlVersion != expected.ControlVersion || v.Before.ViewEpoch != expected.ViewEpoch || v.Before.PrivacyEpoch != expected.PrivacyEpoch {
			return false, ErrDenied
		}
		rr, e := queryReceipt(ctx, u.tx, v.Receipt)
		if e != nil || !settlementReceiptScope(rr, s, v.Task) || rr.operation != v.Operation || !bytes.Equal(rr.hash, v.PayloadHash[:]) {
			return false, ErrDenied
		}
		if v.Action == "MIGRATE" {
			if v.Operation != "INSTANCE_MIGRATION" || v.Event.Kind != "CONTENT_MIGRATION" || v.Event.FromRelease != expected.ReleaseID || v.Event.TargetRelease == "" || v.Event.Policy != "r0-presentation-preserve-v1" || v.Event.PlanID != v.Plan {
				return false, ErrDenied
			}
			expected.ReleaseID = v.Event.TargetRelease
			expected.ViewEpoch++
		} else if v.Operation != "INSTANCE_CONTROL" || v.Event.Kind != "CONTROL" || v.Event.Action != v.Action || (v.Action != "PAUSE" && v.Action != "RESUME" && v.Action != "END") {
			return false, ErrDenied
		}
		expected.ControlVersion++
	}
	expected.WorldRevision = current.WorldRevision
	if expected != current {
		return false, ErrDenied
	}
	return true, nil
}

func (u *unit) checkAccountingOriginals(ctx context.Context, s domain.Scope, a accountingTask) error {
	var raw []byte
	var project, quote, accepted string
	e := u.tx.QueryRowContext(ctx, "SELECT e.document,t.project_id,h.quote_id,h.accepted_commit_key FROM harness_envelopes e JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", s.TenantID, s.ProductID, a.ID).Scan(&raw, &project, &quote, &accepted)
	if e != nil {
		return e
	}
	var in HarnessIntake
	if json.Unmarshal(raw, &in) != nil || in.TaskID != a.ID || in.QuoteID != quote {
		return ErrDenied
	}
	r, e := queryReceipt(ctx, u.tx, accepted)
	if e != nil || r.operation != "HARNESS_INTAKE" || r.world.String != s.WorldInstanceID || r.owner != s.SubjectID {
		return ErrDenied
	}
	expected := map[string]Reservation{}
	for _, h := range in.ParentHolds {
		expected[h.ReservationID] = h
	}
	for _, x := range a.Attempts {
		if x.Amount < 0 || x.Authorization == "" {
			return ErrDenied
		}
		auth := turnIdentity(a.ID, "authorize")
		if x.Slot == 1 {
			auth = turnIdentity(x.Attempt, "authorize")
		}
		r, e = queryReceipt(ctx, u.tx, auth)
		if e != nil || !accountingPhaseReceipt(r, s, a.ID, "AUTHORIZE") {
			return ErrDenied
		}
		if x.Started {
			r, e = queryReceipt(ctx, u.tx, x.Marker)
			if e != nil || !accountingPhaseReceipt(r, s, a.ID, "SEND") {
				return ErrDenied
			}
			var id domain.MockRequestIdentity
			if e = u.tx.QueryRowContext(ctx, "SELECT request_identity FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", s.TenantID, s.ProductID, a.ID, x.Attempt).Scan(&raw); e != nil {
				return e
			}
			if json.Unmarshal(raw, &id) != nil || id.Scope != s || id.TaskID != a.ID || id.AttemptID != x.Attempt || id.AuthorizationID != x.Authorization || id.MarkerKey != x.Marker || id.RequestKey != x.Request || id.RouteID != x.Route || id.QuoteID != quote || !strings.EqualFold(id.RequestHash, x.RequestHash) {
				return ErrDenied
			}
		} else if x.Marker != "" {
			return ErrDenied
		}
		for _, h := range r0Accounts(s, project) {
			if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
				h.Amount = x.Amount
				h.ReservationID = turnIdentity(x.Attempt, "supplier", h.Kind)
				expected[h.ReservationID] = h
			}
		}
	}
	if len(expected) != len(a.Holds) {
		return ErrDenied
	}
	for _, h := range a.Holds {
		if expected[h.ReservationID] != h {
			return ErrDenied
		}
	}
	return nil
}

func accountingPhaseReceipt(r receiptRow, s domain.Scope, task, phase string) bool {
	return r.tenant == s.TenantID && r.product == s.ProductID && r.owner == s.SubjectID && r.task == task && r.world.Valid && r.world.String == s.WorldInstanceID && r.kind == "WORLD" && r.phase == phase
}
