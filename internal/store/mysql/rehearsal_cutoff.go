package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
	"strconv"
	"time"
)

type rehearsalSettlementChild struct {
	Task, World, Accepted, State string
	Index, Calls                 int
	Generation                   uint64
	Attempts                     []cutoffAttempt
	Rounds                       []cutoffRound
}
type rehearsalSettlementHold struct {
	cutoffHold
	Task string
}
type rehearsalSettlementProof struct {
	MockHead domain.MockHead
	Root     cutoffProof
	Binding  domain.RehearsalBinding
	State    string
	Children []rehearsalSettlementChild
	Holds    []rehearsalSettlementHold
}

type settlementContinuityOwner interface {
	ReadSettlementContinuity(context.Context) (domain.SettlementContinuityEvidence, error)
	CheckSettlementContinuity(domain.SettlementContinuityEvidence, domain.SettlementServiceIdentity) (domain.MockHead, error)
}

// Only this fixed owner can retire an original rehearsal's complete narrative
// family. It consumes settlement authority, never content/body authority.
func (s *Store) CutoffDraftRehearsal(ctx context.Context, scope domain.ProductScope, task string) (out HarnessCutoffResult, err error) {
	if scope.Validate() != nil || domain.ValidateID(task) != nil || s.SettlementAuthority == nil {
		return out, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, id, close, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	provider, ok := s.ProviderEvidence.(settlementContinuityOwner)
	if !ok {
		return out, ErrDenied
	}
	evidence, e := provider.ReadSettlementContinuity(ctx)
	if e != nil {
		return out, e
	}
	defer evidence.Close()
	head, e := provider.CheckSettlementContinuity(evidence, id)
	if e != nil {
		return out, e
	}
	tx, closeTx, e := s.beginSettlementTx(ctx)
	if e != nil {
		return out, e
	}
	defer closeTx()
	proof, e := s.lockRehearsalSettlement(ctx, tx, scope, task, id, nil)
	if e != nil {
		return out, e
	}
	proof.MockHead = head
	if current, e := provider.CheckSettlementContinuity(evidence, id); e != nil || current != head {
		return out, ErrDenied
	}
	if proof.State == "COMMITTED" {
		flat := proof.Root
		flat.MockHead = &head
		flat.Holds = nil
		for _, h := range proof.Holds {
			flat.Holds = append(flat.Holds, h.cutoffHold)
		}
		for _, c := range proof.Children {
			flat.Attempts = append(flat.Attempts, c.Attempts...)
		}
		return s.cutoffCommittedMeter(ctx, tx, flat, id, evidence)
	}
	if proof.State == "CUTOFF" {
		out = HarnessCutoffResult{State: "CUTOFF", RiskCaseID: turnIdentity(task, "risk"), ReceiptKey: turnIdentity(task, "cutoff")}
		saved, e := queryReceipt(ctx, tx, out.ReceiptKey)
		if e != nil {
			return out, e
		}
		var raw []byte
		if e = tx.QueryRowContext(ctx, "SELECT proof FROM harness_cutoffs WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?", scope.TenantID, scope.ProductID, task, out.ReceiptKey).Scan(&raw); e != nil {
			return out, e
		}
		var original rehearsalSettlementProof
		if json.Unmarshal(raw, &original) != nil {
			return out, ErrConflict
		}
		_, hash, e := harnessPayload(original)
		if e != nil || saved.operation != "REHEARSAL_CUTOFF" || !settlementReceiptScope(saved, domain.Scope{ProductScope: scope}, task) || !bytes.Equal(saved.hash, hash[:]) {
			return out, ErrConflict
		}
		return out, nil
	}
	if proof.State != "ACTIVE" || proof.Root.CheckedAt.Before(proof.Root.ExecuteBy) || proof.Root.CheckedAt.Before(proof.Root.SettleBy) {
		return out, ErrDenied
	}
	for _, h := range proof.Holds {
		if h.State != "HELD" {
			return out, ErrConflict
		}
	}
	sent := map[string]bool{}
	anySent := false
	for _, c := range proof.Children {
		for _, a := range c.Attempts {
			sent[a.Attempt] = a.Started
			anySent = anySent || a.Started
		}
	}
	out = HarnessCutoffResult{State: "CUTOFF", RiskCaseID: turnIdentity(task, "risk"), ReceiptKey: turnIdentity(task, "cutoff")}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='CUTOFF',narrative_generation=narrative_generation+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE'", scope.TenantID, scope.ProductID, task); e != nil {
		return out, e
	}
	proof.State = "CUTOFF"
	proof.Root.NarrativeGeneration++
	for i, c := range proof.Children {
		if c.State != "COMMITTED" && c.State != "ACCOUNTING_ONLY" {
			if _, e = tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='CUTOFF',narrative_generation=narrative_generation+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE'", scope.TenantID, scope.ProductID, c.Task); e != nil {
				return out, e
			}
			proof.Children[i].State = "CUTOFF"
			proof.Children[i].Generation++
		}
	}
	if e = ensureOriginalRisk(ctx, tx, proof.Root); e != nil {
		return out, e
	}
	if !anySent {
		if _, e = tx.ExecContext(ctx, "UPDATE harness_risk_cases SET unresolved=0 WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task); e != nil {
			return out, e
		}
	}
	for i, h := range proof.Holds {
		state := "HELD"
		switch h.Kind {
		case "customer", "project":
			state = "RELEASED"
		case "platform_risk":
			state = "RELEASED"
			if anySent {
				state = "RISK"
			}
		case "supplier_global", "supplier_tenant":
			if !sent[h.Purpose] {
				state = "RELEASED"
			}
		}
		if state == "HELD" {
			continue
		}
		proof.Holds[i].State = state
		for j := range proof.Root.Holds {
			if proof.Root.Holds[j].ReservationID == h.ReservationID {
				proof.Root.Holds[j].State = state
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE reservations SET status=?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", state, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
			return out, e
		}
		if state == "RELEASED" {
			if _, e = tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND held>=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID, h.Amount); e != nil {
				return out, e
			}
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,?,?,1)", scope.TenantID, scope.ProductID, turnIdentity(task, "cutoff", h.ReservationID), h.Task, scope.SubjectID, h.Kind, h.ScopeKey, h.AccountID, h.Amount, state); e != nil {
			return out, e
		}
	}
	raw, hash, e := harnessPayload(proof)
	if e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,'PRODUCT',?,'REHEARSAL_CUTOFF',?,?,'COMMIT')", out.ReceiptKey, scope.TenantID, scope.ProductID, scope.SubjectID, task, out.ReceiptKey, hash[:]); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_cutoffs VALUES(?,?,?,?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, task, out.RiskCaseID, out.ReceiptKey, id.Installation, id.Generation, proof.Root.NarrativeGeneration, proof.Root.CheckedAt, raw); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_cutoff_work SET done=1,last_failure='' WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task); e != nil {
		return out, e
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = settlementServiceTx(ctx, tx, id); e != nil {
		return out, e
	}
	current, e := readRehearsalSettlement(ctx, tx, scope, task, id)
	if e != nil {
		return out, e
	}
	current.Root.CheckedAt = proof.Root.CheckedAt
	current.MockHead = head
	if !reflect.DeepEqual(current, proof) {
		return out, ErrConflict
	}
	if current, e := provider.CheckSettlementContinuity(evidence, id); e != nil || current != head {
		return out, ErrDenied
	}
	for _, h := range proof.Holds {
		if h.State == "HELD" {
			continue
		}
		var valid bool
		if e = tx.QueryRowContext(ctx, "SELECT task_id=? AND owner_subject_id=? AND kind=? AND scope_key=? AND account_id=? AND amount=? AND status=? FROM postings WHERE tenant_id=? AND product_id=? AND record_id=?", h.Task, scope.SubjectID, h.Kind, h.ScopeKey, h.AccountID, h.Amount, h.State, scope.TenantID, scope.ProductID, turnIdentity(task, "cutoff", h.ReservationID)).Scan(&valid); e != nil || !valid {
			return out, ErrConflict
		}
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return out, e
	}
	return out, nil
}

// This private read is used before and after the same complete lock set. It
// never opens content or trusts task-state alone as a business commit.
func readRehearsalSettlement(ctx context.Context, tx *sql.Tx, scope domain.ProductScope, task string, id domain.SettlementServiceIdentity) (out rehearsalSettlementProof, err error) {
	t, p, sub := scope.TenantID, scope.ProductID, scope.SubjectID
	out.Root = cutoffProof{Scope: domain.Scope{ProductScope: scope}, Task: task, WorldReceipt: turnIdentity(task, "report-commit")}
	var raw, command, document []byte
	var key, project string
	var calls int
	var valid bool
	e := tx.QueryRowContext(ctx, "SELECT a.binding,a.command_hash,a.original_key,a.accepted_commit_key,e.quote_id,e.document,e.state,e.execute_by,e.settle_by,e.narrative_generation,e.calls_allocated,t.project_id,t.owner_subject_id=a.owner_subject_id AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='REHEARSAL' AND b.installation_id=? AND b.service_id=? AND b.policy=? FROM harness_admin_rehearsals a JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_settlement_bindings b USING(tenant_id,product_id,task_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=?", id.Installation, id.ServiceID, id.Policy, t, p, task, sub).Scan(&raw, &command, &key, &out.Root.AcceptedReceipt, &out.Root.Quote, &document, &out.State, &out.Root.ExecuteBy, &out.Root.SettleBy, &out.Root.NarrativeGeneration, &calls, &project, &valid)
	if e != nil {
		return out, e
	}
	var intake HarnessIntake
	if !valid || json.Unmarshal(raw, &out.Binding) != nil || len(out.Binding.Inputs) < 1 || len(out.Binding.Inputs) > 20 || out.Binding.ProjectID != project || json.Unmarshal(document, &intake) != nil || intake.TaskID != task || intake.QuoteID != out.Root.Quote || out.Root.Quote != turnIdentity(task, "quote") || !bytes.Equal(command, intake.RequestHash[:]) || turnIdentity(t, p, sub, "REHEARSAL", key) != task {
		return out, ErrConflict
	}
	accepted, e := queryReceipt(ctx, tx, out.Root.AcceptedReceipt)
	if e != nil {
		return out, e
	}
	if out.Root.AcceptedReceipt != turnIdentity(task, "accepted") || accepted.command != key || accepted.operation != "REHEARSAL_INTAKE" || accepted.phase != "COMMIT" || accepted.tenant != t || accepted.product != p || accepted.owner != sub || !settlementReceiptDimension(accepted, out.Root.Scope) || !bytes.Equal(accepted.hash, command) {
		return out, ErrConflict
	}
	var initialized bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_worlds w JOIN world_controls c ON c.tenant_id=w.tenant_id AND c.product_id=w.product_id AND c.world_instance_id=w.world_instance_id WHERE w.tenant_id=? AND w.product_id=? AND w.root_task_id=? AND w.world_instance_id=? AND w.release_id=? AND w.initialization_receipt=?)", t, p, task, turnIdentity(task, "rehearsal-world"), turnIdentity(task, "rehearsal-version"), turnIdentity(task, "rehearsal-init")).Scan(&initialized); e != nil {
		return out, e
	}
	initReceipt, initErr := queryReceipt(ctx, tx, turnIdentity(task, "rehearsal-init"))
	if initErr == nil {
		_, h, e := harnessPayload(out.Binding)
		if e != nil {
			return out, e
		}
		if !initialized || !settlementReceiptScope(initReceipt, out.Root.Scope, task) || initReceipt.operation != "REHEARSAL_INIT" || !bytes.Equal(initReceipt.hash, h[:]) {
			return out, ErrConflict
		}
	} else if !errors.Is(initErr, sql.ErrNoRows) || initialized {
		return out, ErrConflict
	}
	rows, e := tx.QueryContext(ctx, "SELECT c.input_index,c.child_task_id,c.world_instance_id,c.accepted_receipt,e.state,e.narrative_generation,e.calls_allocated,t.owner_subject_id=? AND t.scope_kind='WORLD' AND t.world_instance_id=c.world_instance_id AND t.task_kind='TURN' AND j.operation='REHEARSAL_TURN' AND t.project_id=? AND h.actor_id=c.actor_id AND h.input_source_id=c.input_source_id AND h.input_hash=c.input_hash AND h.accepted_commit_key=c.accepted_receipt AND b.installation_id=? AND b.service_id=? AND b.policy=? FROM harness_rehearsal_children c JOIN tasks t ON t.tenant_id=c.tenant_id AND t.product_id=c.product_id AND t.task_id=c.child_task_id JOIN registered_jobs j ON j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id JOIN harness_envelopes e ON e.tenant_id=t.tenant_id AND e.product_id=t.product_id AND e.task_id=t.task_id JOIN harness_turns h ON h.tenant_id=t.tenant_id AND h.product_id=t.product_id AND h.task_id=t.task_id JOIN harness_settlement_bindings b ON b.tenant_id=t.tenant_id AND b.product_id=t.product_id AND b.task_id=t.task_id WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=? ORDER BY c.input_index LIMIT 21", sub, project, id.Installation, id.ServiceID, id.Policy, t, p, task)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var c rehearsalSettlementChild
		if e = rows.Scan(&c.Index, &c.Task, &c.World, &c.Accepted, &c.State, &c.Generation, &c.Calls, &valid); e != nil {
			rows.Close()
			return out, e
		}
		if !valid || c.Index != len(out.Children) || c.Index >= len(out.Binding.Inputs) || c.Task != turnIdentity(task, "rehearsal-child", strconv.Itoa(c.Index)) || c.World != turnIdentity(task, "rehearsal-world") || c.Accepted != turnIdentity(c.Task, "accepted") || c.Calls < 0 || c.Calls > 2 {
			rows.Close()
			return out, ErrConflict
		}
		out.Children = append(out.Children, c)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	total := 0
	var worldTasks int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE tenant_id=? AND product_id=? AND world_instance_id=?", t, p, turnIdentity(task, "rehearsal-world")).Scan(&worldTasks); e != nil {
		return out, e
	}
	if worldTasks != len(out.Children) || (len(out.Children) > 0 && !initialized) {
		return out, ErrConflict
	}
	for i := range out.Children {
		c := &out.Children[i]
		total += c.Calls
		receipt, e := queryReceipt(ctx, tx, c.Accepted)
		if e != nil {
			return out, e
		}
		_, hash, e := harnessPayload(struct {
			Root    string
			Index   int
			Binding domain.RehearsalBinding
		}{task, c.Index, out.Binding})
		if e != nil {
			return out, e
		}
		if !settlementReceiptScope(receipt, out.Root.Scope, task) || receipt.operation != "REHEARSAL_CHILD_START" || receipt.command != c.Accepted || !bytes.Equal(receipt.hash, hash[:]) {
			return out, ErrConflict
		}
		arows, e := tx.QueryContext(ctx, "SELECT a.attempt_id,d.authorization_id,COALESCE(a.marker_commit_key,''),a.slot,h.supplier_atoms,a.request_key,a.route,HEX(a.request_hash),a.send_started_at IS NOT NULL FROM attempts a JOIN dispatch_authorizations d USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_attempt_allocations h USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", t, p, c.Task)
		if e != nil {
			return out, e
		}
		for arows.Next() {
			var a cutoffAttempt
			if e = arows.Scan(&a.Attempt, &a.Authorization, &a.Marker, &a.Slot, &a.Amount, &a.Request, &a.Route, &a.RequestHash, &a.Started); e != nil {
				arows.Close()
				return out, e
			}
			if a.Slot != len(c.Attempts) || a.Amount != 0 || (!a.Started && a.Marker != "") || (a.Started && a.Marker == "") {
				arows.Close()
				return out, ErrConflict
			}
			c.Attempts = append(c.Attempts, a)
		}
		e = arows.Err()
		arows.Close()
		if e != nil {
			return out, e
		}
		if len(c.Attempts) != c.Calls {
			return out, ErrConflict
		}
		for _, a := range c.Attempts {
			authorize := turnIdentity(c.Task, "authorize")
			if a.Slot == 1 {
				authorize = turnIdentity(a.Attempt, "authorize")
			}
			ar, e := queryReceipt(ctx, tx, authorize)
			if e != nil {
				return out, e
			}
			cs := domain.Scope{ProductScope: scope, WorldInstanceID: c.World}
			if ar.tenant != t || ar.product != p || ar.owner != sub || ar.task != c.Task || !settlementReceiptDimension(ar, cs) || ar.phase != "AUTHORIZE" {
				return out, ErrConflict
			}
			if a.Started {
				var raw []byte
				if e = tx.QueryRowContext(ctx, "SELECT request_identity FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", t, p, c.Task, a.Attempt).Scan(&raw); e != nil {
					return out, e
				}
				var original domain.MockRequestIdentity
				if json.Unmarshal(raw, &original) != nil || original.Validate() != nil || original.Scope != cs || original.TaskID != c.Task || original.AttemptID != a.Attempt || original.AuthorizationID != a.Authorization || original.MarkerKey != a.Marker || original.RequestKey != a.Request || original.RouteID != a.Route || !bytes.EqualFold([]byte(original.RequestHash), []byte(a.RequestHash)) {
					return out, ErrConflict
				}
				if e = checkOriginalBillRequest(ctx, tx, original, id); e != nil {
					return out, e
				}
			}
		}
		rr, e := tx.QueryContext(ctx, "SELECT attempt_id,phase,round_number,state,original_receipt_key FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY attempt_id,phase,round_number LIMIT 41", t, p, c.Task)
		if e != nil {
			return out, e
		}
		for rr.Next() {
			var r cutoffRound
			if e = rr.Scan(&r.Attempt, &r.Phase, &r.Number, &r.State, &r.Receipt); e != nil {
				rr.Close()
				return out, e
			}
			r.Prepare = turnIdentity(r.Attempt, r.Phase, "prepare", fmt.Sprint(r.Number))
			c.Rounds = append(c.Rounds, r)
		}
		e = rr.Err()
		rr.Close()
		if e != nil {
			return out, e
		}
		if len(c.Rounds) > 40 {
			return out, ErrConflict
		}
		for j, r := range c.Rounds {
			pr, e := queryReceipt(ctx, tx, r.Prepare)
			if e != nil {
				return out, e
			}
			cs := domain.Scope{ProductScope: scope, WorldInstanceID: c.World}
			if !settlementReceiptScope(pr, cs, c.Task) || pr.operation != "HARNESS_LOCAL_PREPARE" {
				return out, ErrConflict
			}
			cr, e := queryReceipt(ctx, tx, r.Receipt)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return out, e
			}
			if e == nil {
				if !settlementReceiptScope(cr, cs, c.Task) {
					return out, ErrConflict
				}
				c.Rounds[j].ReceiptKnown = true
			}
			if r.State == "COMMITTED" && e != nil {
				return out, ErrConflict
			}
		}
		cr, e := queryReceipt(ctx, tx, turnIdentity(c.Task, "world-commit"))
		var committed bool
		if qe := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)", t, p, c.Task, turnIdentity(c.Task, "world-commit")).Scan(&committed); qe != nil {
			return out, qe
		}
		if e == nil {
			if !committed || c.State != "COMMITTED" || cr.operation != "HARNESS_TURN_COMMIT" || !settlementReceiptScope(cr, domain.Scope{ProductScope: scope, WorldInstanceID: c.World}, c.Task) {
				return out, ErrConflict
			}
		} else if !errors.Is(e, sql.ErrNoRows) || committed || c.State == "COMMITTED" {
			return out, ErrConflict
		}

		if c.State == "ACCOUNTING_ONLY" {
			var exact bool
			key := turnIdentity(c.Task, "negative-outcome")
			if e = tx.QueryRowContext(ctx, "SELECT n.root_task_id=? AND n.input_index=? AND n.receipt_key=? AND cr.command_key=n.receipt_key AND cr.payload_hash=n.command_hash AND cr.operation='REHEARSAL_CHILD_REJECT' AND cr.phase='COMMIT' AND cr.scope_kind='WORLD' AND cr.world_instance_id=? AND cr.owner_subject_id=? AND t.state='FAILED' AND h.fsm_state='REJECTED' AND JSON_EXTRACT(n.document,'$.NarrativeGeneration')=? FROM harness_rehearsal_negative_outcomes n JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN commit_receipts cr ON cr.commit_key=n.receipt_key AND cr.tenant_id=n.tenant_id AND cr.product_id=n.product_id AND cr.task_id=n.task_id WHERE n.tenant_id=? AND n.product_id=? AND n.task_id=?", task, c.Index, key, c.World, sub, c.Generation, t, p, c.Task).Scan(&exact); e != nil {
				return out, e
			}
			if !exact {
				return out, ErrConflict
			}
		}
	}
	if total != calls || total > 2*len(out.Binding.Inputs) {
		return out, ErrConflict
	}
	expected := map[string]rehearsalSettlementHold{}
	for _, a := range r0CandidateAccounts(scope, project) {
		if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
			for _, c := range out.Children {
				for _, at := range c.Attempts {
					h := a
					h.ReservationID = turnIdentity(at.Attempt, "supplier", a.Kind)
					expected[h.ReservationID] = rehearsalSettlementHold{cutoffHold{Reservation: h, Purpose: at.Attempt}, c.Task}
				}
			}
		} else {
			a.ReservationID = turnIdentity(task, "parent", a.Kind)
			expected[a.ReservationID] = rehearsalSettlementHold{cutoffHold{Reservation: a, Purpose: "PARENT"}, task}
		}
	}
	rows, e = tx.QueryContext(ctx, "SELECT r.task_id,r.kind,r.scope_key,r.account_id,r.reservation_id,r.amount,r.purpose,r.status FROM reservations r WHERE r.tenant_id=? AND r.product_id=? AND (r.task_id=? OR EXISTS(SELECT 1 FROM harness_rehearsal_children c WHERE c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.child_task_id=r.task_id AND c.root_task_id=?)) ORDER BY r.kind,r.scope_key,r.account_id,r.reservation_id LIMIT 84", t, p, task, task)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var h rehearsalSettlementHold
		if e = rows.Scan(&h.Task, &h.Kind, &h.ScopeKey, &h.AccountID, &h.ReservationID, &h.Amount, &h.Purpose, &h.State); e != nil {
			rows.Close()
			return out, e
		}
		want, ok := expected[h.ReservationID]
		want.State = h.State
		if !ok || want != h {
			rows.Close()
			return out, ErrConflict
		}
		delete(expected, h.ReservationID)
		out.Holds = append(out.Holds, h)
		if h.Task == task {
			out.Root.Holds = append(out.Root.Holds, h.cutoffHold)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(expected) != 0 {
		return out, ErrConflict
	}
	report, e := queryReceipt(ctx, tx, out.Root.WorldReceipt)
	var joint bool
	if qe := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_reports r JOIN assets a ON a.tenant_id=r.tenant_id AND a.product_id=r.product_id AND a.asset_id=r.asset_id JOIN service_events s ON s.tenant_id=r.tenant_id AND s.product_id=r.product_id AND s.task_id=r.task_id WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.state='COMMITTED' AND r.receipt_key=? AND r.commit_hash=(SELECT payload_hash FROM commit_receipts WHERE commit_key=r.receipt_key) AND r.asset_id=? AND a.storage_key=r.storage_key)", t, p, task, out.Root.WorldReceipt, turnIdentity(task, "report-asset")).Scan(&joint); qe != nil {
		return out, qe
	}
	if e == nil {
		if !joint || out.State != "COMMITTED" || report.operation != "REHEARSAL_REPORT_COMMIT" || !settlementReceiptScope(report, out.Root.Scope, task) {
			return out, ErrConflict
		}
	} else if !errors.Is(e, sql.ErrNoRows) || joint || out.State == "COMMITTED" {
		return out, ErrConflict
	}
	if errors.Is(e, sql.ErrNoRows) {
		var partial bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM service_events WHERE tenant_id=? AND product_id=? AND task_id=?) OR EXISTS(SELECT 1 FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=? AND state='COMMITTED')", t, p, task, t, p, task).Scan(&partial); e != nil {
			return out, e
		}
		if partial {
			return out, ErrConflict
		}
	}
	return out, nil
}

func (s *Store) lockRehearsalSettlement(ctx context.Context, tx *sql.Tx, scope domain.ProductScope, task string, id domain.SettlementServiceIdentity, extra []LockKey) (rehearsalSettlementProof, error) {
	// The pre-read is a lock plan, not authority. The complete set is compared
	// again after the sorted locks, including the world that may not exist yet.
	pre, e := readRehearsalSettlement(ctx, tx, scope, task, id)
	if e != nil {
		return rehearsalSettlementProof{}, e
	}
	keys := []LockKey{{Rank: 10, IDs: []string{scope.TenantID}}, {Rank: 20, IDs: []string{scope.TenantID, scope.ProductID}}, {Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}, {Rank: 40, IDs: []string{scope.TenantID, scope.ProductID, turnIdentity(task, "rehearsal-world")}}, harnessTaskKey(scope, task)}
	for _, c := range pre.Children {
		keys = append(keys, harnessTaskKey(scope, c.Task))
		for _, a := range c.Attempts {
			keys = append(keys, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, c.Task, a.Attempt, a.Authorization}})
		}
	}
	for _, h := range pre.Holds {
		keys = append(keys, holdLocks([]Reservation{h.Reservation})...)
		keys = append(keys, LockKey{Rank: 110, Kind: "postings", IDs: []string{scope.TenantID, scope.ProductID, turnIdentity(task, "cutoff", h.ReservationID)}})
		keys = append(keys, LockKey{Rank: 110, Kind: "postings", IDs: []string{scope.TenantID, scope.ProductID, turnIdentity(task, "meter-posting", h.ReservationID)}})
	}
	receiptKeys := []string{pre.Root.AcceptedReceipt, pre.Root.WorldReceipt, turnIdentity(task, "cutoff"), turnIdentity(task, "rehearsal-init")}
	for _, c := range pre.Children {
		receiptKeys = append(receiptKeys, c.Accepted, turnIdentity(c.Task, "world-commit"), turnIdentity(c.Task, "negative-outcome"))
		for _, a := range c.Attempts {
			key := turnIdentity(c.Task, "authorize")
			if a.Slot == 1 {
				key = turnIdentity(a.Attempt, "authorize")
			}
			receiptKeys = append(receiptKeys, key)
			if a.Started {
				receiptKeys = append(receiptKeys, a.Marker)
			}
		}
		for _, r := range c.Rounds {
			receiptKeys = append(receiptKeys, r.Prepare, r.Receipt)
		}
	}
	for _, key := range receiptKeys {
		keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}})
	}
	keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(task, "meter-cutoff")}})
	keys = append(keys, extra...)
	keys, e = PlanLocks(keys)
	if e != nil {
		return rehearsalSettlementProof{}, e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return rehearsalSettlementProof{}, e
		}
		var got string
		switch k.Rank {
		case 10:
			e = tx.QueryRowContext(ctx, "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE", scope.TenantID).Scan(&got)
		case 20:
			e = tx.QueryRowContext(ctx, "SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE", scope.TenantID, scope.ProductID).Scan(&got)
			if e == nil {
				e = settlementServiceTx(ctx, tx, id)
			}
		case 30:
			e = tx.QueryRowContext(ctx, "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&got)
		case 40:
			e = tx.QueryRowContext(ctx, "SELECT world_instance_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&got)
			if errors.Is(e, sql.ErrNoRows) {
				e = nil
			}
		case 50:
			e = tx.QueryRowContext(ctx, "SELECT task_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&got)
		case 60:
			e = tx.QueryRowContext(ctx, "SELECT attempt_id FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3]).Scan(&got)
		case 70:
			e = tx.QueryRowContext(ctx, "SELECT account_id FROM budget_accounts WHERE kind=? AND scope_key=? AND account_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1]).Scan(&got)
		case 80:
			e = tx.QueryRowContext(ctx, "SELECT reservation_id FROM reservations WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&got)
		}
		if e != nil {
			return rehearsalSettlementProof{}, e
		}
		if s.Trace != nil {
			s.Trace(k)
		}
	}
	proof, e := readRehearsalSettlement(ctx, tx, scope, task, id)
	if e != nil {
		return rehearsalSettlementProof{}, e
	}
	if !reflect.DeepEqual(pre, proof) {
		return rehearsalSettlementProof{}, ErrReplan
	}
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&proof.Root.CheckedAt); e != nil {
		return rehearsalSettlementProof{}, e
	}
	return proof, nil
}
