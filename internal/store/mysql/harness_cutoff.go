package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"strings"
	"time"
)

type SettlementAuthority interface {
	EnterDeadlineSettlement(context.Context) (context.Context, domain.SettlementServiceIdentity, func(), error)
}
type HarnessCutoffResult struct{ State, RiskCaseID, ReceiptKey string }
type cutoffAttempt struct {
	Attempt, Authorization, Marker string
	Request, Route, RequestHash    string
	Started                        bool
	Slot                           int
	Amount                         int64
}
type cutoffHold struct {
	Reservation
	Purpose, State string
}
type cutoffRound struct {
	Attempt, Phase, State, Receipt, Prepare string
	Number                                  uint64
	ReceiptKnown                            bool
}
type cutoffProof struct {
	MockHead                       *domain.MockHead `json:",omitempty"`
	Scope                          domain.Scope
	Task, Quote, WorldReceipt      string
	AcceptedReceipt                string
	CheckedAt, ExecuteBy, SettleBy time.Time
	Attempts                       []cutoffAttempt
	Holds                          []cutoffHold
	Rounds                         []cutoffRound
	NarrativeGeneration            uint64
}

func settlementServiceTx(ctx context.Context, tx *sql.Tx, id domain.SettlementServiceIdentity) error {
	if id.Installation == "" || id.Generation == 0 || id.ServiceID != "r0-mock-settlement" || id.Action != "attempt.cost.settle" || id.Policy != "R0_BOUNDED_SETTLEMENT_V1" {
		return ErrDenied
	}
	var generation uint64
	if e := tx.QueryRowContext(ctx, "SELECT generation FROM memory_installation WHERE installation_id=? FOR UPDATE", id.Installation).Scan(&generation); e != nil {
		return e
	}
	if generation != id.Generation {
		return ErrDenied
	}
	var valid bool
	e := tx.QueryRowContext(ctx, "SELECT s.enabled=1 AND i.generation=? FROM harness_settlement_services s JOIN memory_installation i ON i.installation_id=s.installation_id WHERE s.installation_id=? AND s.service_id=? AND s.action=? AND s.policy=? FOR UPDATE", id.Generation, id.Installation, id.ServiceID, id.Action, id.Policy).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}

// These locks are the same real world/task/attempt/account rows used by the
// narrative UoW. All provider and filesystem IO stays outside this transaction.
func (s *Store) lockSettlement(ctx context.Context, tx *sql.Tx, scope domain.Scope, task string, service domain.SettlementServiceIdentity, extra []LockKey) (cutoffProof, string, error) {
	out := cutoffProof{Scope: scope, Task: task, WorldReceipt: turnIdentity(task, "world-commit")}
	t, p, w, sub := scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID
	product := w == ""
	if product {
		out.WorldReceipt = turnIdentity(task, "candidate-commit")
	}
	var got string
	for _, v := range []struct {
		q    string
		args []any
	}{
		{"SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE", []any{t}},
		{"SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE", []any{t, p}},
	} {
		if e := tx.QueryRowContext(ctx, v.q, v.args...).Scan(&got); e != nil {
			return out, "", e
		}
	}
	if e := settlementServiceTx(ctx, tx, service); e != nil {
		return out, "", e
	}
	if e := tx.QueryRowContext(ctx, "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", t, p, sub).Scan(&got); e != nil {
		return out, "", e
	}
	if !product {
		if e := tx.QueryRowContext(ctx, "SELECT world_instance_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", t, p, w).Scan(&got); e != nil {
			return out, "", e
		}
	}
	var owner, world, state, project string
	taskQuery := "SELECT owner_subject_id,world_instance_id,project_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? AND task_kind='TURN' FOR UPDATE"
	if product {
		taskQuery = "SELECT t.owner_subject_id,COALESCE(t.world_instance_id,''),t.project_id FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='CANDIDATE' FOR UPDATE"
	}
	if e := tx.QueryRowContext(ctx, taskQuery, t, p, task).Scan(&owner, &world, &project); e != nil {
		return out, "", e
	}
	if owner != sub || world != w {
		return out, "", ErrDenied
	}
	var bound bool
	if e := tx.QueryRowContext(ctx, "SELECT installation_id=? AND service_id=? AND policy=? FROM harness_settlement_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", service.Installation, service.ServiceID, service.Policy, t, p, task).Scan(&bound); e != nil {
		return out, "", e
	}
	if !bound {
		return out, "", ErrDenied
	}
	var calls int
	if e := tx.QueryRowContext(ctx, "SELECT quote_id,state,execute_by,settle_by,narrative_generation,calls_allocated FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", t, p, task).Scan(&out.Quote, &state, &out.ExecuteBy, &out.SettleBy, &out.NarrativeGeneration, &calls); e != nil {
		return out, "", e
	}
	if product {
		var raw []byte
		if e := tx.QueryRowContext(ctx, "SELECT accepted_commit_key,binding FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", t, p, task, sub).Scan(&out.AcceptedReceipt, &raw); e != nil {
			return out, "", e
		}
		var material domain.DraftCandidateMaterial
		if json.Unmarshal(raw, &material) != nil || material.ProjectID != project || out.Quote != turnIdentity(task, "quote") || out.AcceptedReceipt != turnIdentity(task, "accepted") {
			return out, "", ErrConflict
		}
	} else {
		if e := tx.QueryRowContext(ctx, "SELECT accepted_commit_key FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND world_instance_id=? AND owner_subject_id=? AND quote_id=?", t, p, task, w, sub, out.Quote).Scan(&out.AcceptedReceipt); e != nil {
			return out, "", e
		}
	}
	// Task50 excludes every attempt insertion, extra approval and hold allocation;
	// collect the complete bounded set before taking any rank60 or account locks.
	rows, e := tx.QueryContext(ctx, "SELECT a.attempt_id,d.authorization_id,COALESCE(a.marker_commit_key,''),a.slot,h.supplier_atoms,a.request_key,a.route,HEX(a.request_hash),a.send_started_at IS NOT NULL FROM attempts a JOIN dispatch_authorizations d USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_attempt_allocations h USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", t, p, task)
	if e != nil {
		return out, "", e
	}
	for rows.Next() {
		var a cutoffAttempt
		if e = rows.Scan(&a.Attempt, &a.Authorization, &a.Marker, &a.Slot, &a.Amount, &a.Request, &a.Route, &a.RequestHash, &a.Started); e != nil {
			rows.Close()
			return out, "", e
		}
		out.Attempts = append(out.Attempts, a)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, "", e
	}
	var actual int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, task).Scan(&actual); e != nil {
		return out, "", e
	}
	if actual != calls || len(out.Attempts) != calls || calls > 2 {
		return out, "", ErrConflict
	}
	keys := []LockKey{}
	for _, a := range out.Attempts {
		keys = append(keys, LockKey{Rank: 60, IDs: []string{t, p, task, a.Attempt, a.Authorization}})
	}
	keys, e = PlanLocks(keys)
	if e != nil {
		return out, "", e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return out, "", e
		}
		if e = tx.QueryRowContext(ctx, "SELECT attempt_id FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? FOR UPDATE", t, p, task, k.IDs[3]).Scan(&got); e != nil {
			return out, "", e
		}
		if s.Trace != nil {
			s.Trace(k)
		}
	}
	rows, e = tx.QueryContext(ctx, "SELECT kind,scope_key,account_id,reservation_id,amount,purpose,status FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? LIMIT 9", t, p, task)
	if e != nil {
		return out, "", e
	}
	for rows.Next() {
		var h cutoffHold
		if e = rows.Scan(&h.Kind, &h.ScopeKey, &h.AccountID, &h.ReservationID, &h.Amount, &h.Purpose, &h.State); e != nil {
			rows.Close()
			return out, "", e
		}
		out.Holds = append(out.Holds, h)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, "", e
	}
	parentCount := 4
	if product {
		parentCount = 3
	}
	if len(out.Holds) != parentCount+2*calls {
		return out, "", ErrConflict
	}
	// Compare every real account and amount to the immutable intake and allocation.
	var raw []byte
	var in HarnessIntake
	if e = tx.QueryRowContext(ctx, "SELECT document FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, task).Scan(&raw); e != nil {
		return out, "", e
	}
	if json.Unmarshal(raw, &in) != nil || in.TaskID != task || in.QuoteID != out.Quote {
		return out, "", ErrConflict
	}
	expected := map[string]cutoffHold{}
	for _, h := range in.ParentHolds {
		expected[h.Kind+"/PARENT"] = cutoffHold{Reservation: h, Purpose: "PARENT"}
	}
	accounts := r0Accounts(scope, project)
	if product {
		accounts = r0CandidateAccounts(scope.ProductScope, project)
	}
	for _, a := range out.Attempts {
		for _, kind := range []string{"supplier_global", "supplier_tenant"} {
			for _, account := range accounts {
				if account.Kind == kind {
					expected[kind+"/"+a.Attempt] = cutoffHold{Reservation: Reservation{Kind: kind, ScopeKey: account.ScopeKey, AccountID: account.AccountID, Amount: a.Amount}, Purpose: a.Attempt}
				}
			}
		}
	}
	holds := []Reservation{}
	for _, h := range out.Holds {
		want, ok := expected[h.Kind+"/"+h.Purpose]
		if !ok || want.Kind != h.Kind || want.ScopeKey != h.ScopeKey || want.AccountID != h.AccountID || want.Amount != h.Amount || (h.Purpose == "PARENT" && want.ReservationID != h.ReservationID) || (h.Purpose != "PARENT" && h.ReservationID != turnIdentity(h.Purpose, "supplier", h.Kind)) {
			return out, "", ErrConflict
		}
		delete(expected, h.Kind+"/"+h.Purpose)
		holds = append(holds, h.Reservation)
	}
	if len(expected) != 0 {
		return out, "", ErrConflict
	}
	keys, e = PlanLocks(holdLocks(holds))
	if e != nil {
		return out, "", e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return out, "", e
		}
		if k.Rank == 70 {
			e = tx.QueryRowContext(ctx, "SELECT account_id FROM budget_accounts WHERE kind=? AND scope_key=? AND account_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1]).Scan(&got)
		} else {
			e = tx.QueryRowContext(ctx, "SELECT reservation_id FROM reservations WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&got)
		}
		if e != nil {
			return out, "", e
		}
		if s.Trace != nil {
			s.Trace(k)
		}
	}
	// The rank60 task/attempt closure protects rounds; acquire all exact append
	// receipt locks before checking whether a local phase actually committed.
	rows, e = tx.QueryContext(ctx, "SELECT attempt_id,phase,round_number,state,original_receipt_key FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY attempt_id,phase,round_number LIMIT 81", t, p, task)
	if e != nil {
		return out, "", e
	}
	keys = []LockKey{{Rank: 110, Kind: "commit_receipts", IDs: []string{out.WorldReceipt}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(task, "cutoff")}}}
	for rows.Next() {
		var r cutoffRound
		if e = rows.Scan(&r.Attempt, &r.Phase, &r.Number, &r.State, &r.Receipt); e != nil {
			rows.Close()
			return out, "", e
		}
		r.Prepare = turnIdentity(r.Attempt, r.Phase, "prepare", fmt.Sprint(r.Number))
		out.Rounds = append(out.Rounds, r)
		keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{r.Receipt}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{r.Prepare}})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, "", e
	}
	if len(out.Rounds) > 80 {
		return out, "", ErrConflict
	}
	if product && len(out.Rounds) != 0 {
		return out, "", ErrConflict
	}
	keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{out.AcceptedReceipt}})
	for _, a := range out.Attempts {
		authorize := turnIdentity(task, "authorize")
		if product {
			authorize = turnIdentity(task, "candidate-authorize")
		}
		if a.Slot == 1 {
			authorize = turnIdentity(a.Attempt, "authorize")
		}
		keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{authorize}})
		if a.Started {
			if a.Marker == "" {
				return out, "", ErrConflict
			}
			keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{a.Marker}})
		} else if a.Marker != "" {
			return out, "", ErrConflict
		}
	}
	keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(task, "meter-cutoff")}})
	keys = append(keys, extra...)
	for _, h := range out.Holds {
		keys = append(keys, LockKey{Rank: 110, Kind: "postings", IDs: []string{t, p, turnIdentity(task, "cutoff", h.ReservationID)}})
		keys = append(keys, LockKey{Rank: 110, Kind: "postings", IDs: []string{t, p, turnIdentity(task, "meter-posting", h.ReservationID)}})
	}
	keys, e = PlanLocks(keys)
	if e != nil {
		return out, "", e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return out, "", e
		}
		if s.Trace != nil {
			s.Trace(k)
		}
	}
	accepted, e := queryReceipt(ctx, tx, out.AcceptedReceipt)
	if e != nil {
		return out, "", e
	}
	intakeOperation := "HARNESS_INTAKE"
	if product {
		intakeOperation = "CANDIDATE_INTAKE"
	}
	if accepted.tenant != t || accepted.product != p || accepted.owner != sub || !settlementReceiptDimension(accepted, scope) || accepted.operation != intakeOperation || accepted.phase != "COMMIT" {
		return out, "", ErrConflict
	}
	for _, a := range out.Attempts {
		authorize := turnIdentity(task, "authorize")
		if product {
			authorize = turnIdentity(task, "candidate-authorize")
		}
		if a.Slot == 1 {
			authorize = turnIdentity(a.Attempt, "authorize")
		}
		auth, e := queryReceipt(ctx, tx, authorize)
		if e != nil {
			return out, "", e
		}
		if auth.tenant != t || auth.product != p || auth.owner != sub || auth.task != task || !settlementReceiptDimension(auth, scope) || auth.phase != "AUTHORIZE" {
			return out, "", ErrConflict
		}
		if !a.Started {
			continue
		}
		var raw []byte
		var installation string
		if e = tx.QueryRowContext(ctx, "SELECT request_identity,installation_id FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", t, p, task, a.Attempt).Scan(&raw, &installation); e != nil {
			return out, "", e
		}
		var original domain.MockRequestIdentity
		if json.Unmarshal(raw, &original) != nil || original.Validate() != nil || (product && original.Kind != "CANDIDATE") || original.Scope != scope || original.TaskID != task || original.AttemptID != a.Attempt || original.AuthorizationID != a.Authorization || original.QuoteID != out.Quote || original.MarkerKey != a.Marker || original.RequestKey != a.Request || original.RouteID != a.Route || original.RequestHash != strings.ToLower(a.RequestHash) || installation != service.Installation {
			return out, "", ErrConflict
		}
		if hash, e := hex.DecodeString(original.RequestHash); e != nil || len(hash) != 32 {
			return out, "", ErrConflict
		}
		sent, e := queryReceipt(ctx, tx, a.Marker)
		if e != nil {
			return out, "", e
		}
		if sent.tenant != t || sent.product != p || sent.owner != sub || sent.task != task || !settlementReceiptDimension(sent, scope) || sent.phase != "SEND" {
			return out, "", ErrConflict
		}
	}
	for i, r := range out.Rounds {
		pr, e := queryReceipt(ctx, tx, r.Prepare)
		if e != nil {
			return out, "", e
		}
		if !settlementReceiptScope(pr, scope, task) || pr.operation != "HARNESS_LOCAL_PREPARE" || pr.command != r.Prepare {
			return out, "", ErrConflict
		}
		receipt, e := queryReceipt(ctx, tx, r.Receipt)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return out, "", e
		}
		if e == nil {
			wantOperation, wantKey := "HARNESS_CANDIDATE_APPROVE", turnIdentity(r.Attempt, "approval")
			if r.Phase == "WORLD_COMMIT" {
				wantOperation, wantKey = "HARNESS_TURN_COMMIT", out.WorldReceipt
			}
			if !settlementReceiptScope(receipt, scope, task) || receipt.operation != wantOperation || r.Receipt != wantKey {
				return out, "", ErrConflict
			}
			out.Rounds[i].ReceiptKnown = true
		}
		if r.State == "COMMITTED" && e != nil {
			return out, "", ErrConflict
		}
	}
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&out.CheckedAt); e != nil {
		return out, "", e
	}
	return out, state, nil
}

// CutoffHarnessTask never queries a provider or opens a body. A successful
// proof says no WORLD commit exists now and future narrative is retired; it
// deliberately does not turn old PENDING rounds into invented rollbacks.
func (s *Store) CutoffHarnessTask(ctx context.Context, scope domain.Scope, task string) (HarnessCutoffResult, error) {
	if s.SettlementAuthority == nil || scope.Validate() != nil || domain.ValidateID(task) != nil {
		return HarnessCutoffResult{}, ErrDenied
	}
	return s.cutoffOriginalTask(ctx, scope, task)
}

// Product candidate scope is proven again from the actual registered original
// task and intake. This port never creates a world or grants content authority.
func (s *Store) CutoffDraftCandidate(ctx context.Context, scope domain.ProductScope, task string) (HarnessCutoffResult, error) {
	if s.SettlementAuthority == nil || scope.Validate() != nil || domain.ValidateID(task) != nil {
		return HarnessCutoffResult{}, ErrDenied
	}
	return s.cutoffOriginalTask(ctx, domain.Scope{ProductScope: scope}, task)
}
func (s *Store) cutoffOriginalTask(ctx context.Context, scope domain.Scope, task string) (HarnessCutoffResult, error) {
	var out HarnessCutoffResult
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, releaseID, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	provider, ok := s.ProviderEvidence.(settlementContinuityOwner)
	if !ok {
		return out, ErrDenied
	}
	evidence, e := provider.ReadSettlementContinuity(ctx)
	if e != nil {
		return out, e
	}
	defer evidence.Close()
	head, e := provider.CheckSettlementContinuity(evidence, releaseID)
	if e != nil {
		return out, e
	}
	tx, closeTx, e := s.beginSettlementTx(ctx)
	if e != nil {
		return out, e
	}
	defer closeTx()
	proof, state, e := s.lockSettlement(ctx, tx, scope, task, releaseID, nil)
	if e != nil {
		return out, e
	}
	if current, e := provider.CheckSettlementContinuity(evidence, releaseID); e != nil || current != head {
		return out, ErrDenied
	}
	proof.MockHead = &head
	t, p := scope.TenantID, scope.ProductID
	worldReceipt, e := queryReceipt(ctx, tx, proof.WorldReceipt)
	if e == nil {
		operation, commitQuery := "HARNESS_TURN_COMMIT", "SELECT EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)"
		if scope.WorldInstanceID == "" {
			operation = "CANDIDATE_COMMIT"
			commitQuery = "SELECT EXISTS(SELECT 1 FROM harness_candidate_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)"
		}
		if (state != "COMMITTED" && state != "ACCOUNTING_ONLY") || !settlementReceiptScope(worldReceipt, scope, task) || worldReceipt.operation != operation {
			return out, ErrConflict
		}
		var joint bool
		if e = tx.QueryRowContext(ctx, commitQuery, t, p, task, proof.WorldReceipt).Scan(&joint); e != nil || !joint {
			return out, ErrConflict
		}
		return s.cutoffCommittedMeter(ctx, tx, proof, releaseID, evidence)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if scope.WorldInstanceID == "" {
		// Missing one side of a real joint commit is corruption, never permission
		// to reinterpret the historical business outcome as uncommitted.
		var joint bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_candidate_commits WHERE tenant_id=? AND product_id=? AND task_id=?) OR EXISTS(SELECT 1 FROM service_events WHERE tenant_id=? AND product_id=? AND task_id=?)", t, p, task, t, p, task).Scan(&joint); e != nil {
			return out, e
		}
		if joint {
			return out, ErrConflict
		}
	}
	if state == "CUTOFF" || state == "ACCOUNTING_ONLY" {
		out.State = "CUTOFF"
		e = tx.QueryRowContext(ctx, "SELECT risk_case_id,receipt_key FROM harness_cutoffs WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, task).Scan(&out.RiskCaseID, &out.ReceiptKey)
		if e != nil {
			return out, e
		}
		receipt, e := queryReceipt(ctx, tx, out.ReceiptKey)
		if e != nil || receipt.operation != "HARNESS_CUTOFF" || !settlementReceiptScope(receipt, scope, task) {
			return HarnessCutoffResult{}, ErrConflict
		}
		return out, nil
	}
	if state != "ACTIVE" || proof.CheckedAt.Before(proof.SettleBy) || proof.CheckedAt.Before(proof.ExecuteBy) {
		return out, ErrDenied
	}
	for _, h := range proof.Holds {
		if h.State != "HELD" {
			return out, ErrConflict
		}
	}
	out = HarnessCutoffResult{State: "CUTOFF", RiskCaseID: turnIdentity(task, "risk"), ReceiptKey: turnIdentity(task, "cutoff")}
	// Monotonic per-parent retirement shares task50 with every execution writer.
	if _, e = tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='CUTOFF',narrative_generation=narrative_generation+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE'", t, p, task); e != nil {
		return out, e
	}
	proof.NarrativeGeneration++
	if e = ensureOriginalRisk(ctx, tx, proof); e != nil {
		return out, e
	}
	sent := map[string]bool{}
	anySent := false
	for _, a := range proof.Attempts {
		sent[a.Attempt] = a.Started
		anySent = anySent || a.Started
	}
	if !anySent {
		if _, e = tx.ExecContext(ctx, "UPDATE harness_risk_cases SET unresolved=0 WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, task); e != nil {
			return out, e
		}
	}
	for _, h := range proof.Holds {
		status := "HELD"
		switch h.Kind {
		case "customer", "project", "world":
			status = "RELEASED"
		case "platform_risk":
			status = "RELEASED"
			if anySent {
				status = "RISK"
			}
		case "supplier_global", "supplier_tenant":
			if !sent[h.Purpose] {
				status = "RELEASED"
			}
		}
		if status == "HELD" {
			continue
		}
		if _, e = tx.ExecContext(ctx, "UPDATE reservations SET status=?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", status, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
			return out, e
		}
		if status == "RELEASED" {
			if _, e = tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND held>=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID, h.Amount); e != nil {
				return out, e
			}
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,?,?,1)", t, p, turnIdentity(task, "cutoff", h.ReservationID), task, scope.SubjectID, h.Kind, h.ScopeKey, h.AccountID, h.Amount, status); e != nil {
			return out, e
		}
	}
	raw, hash, e := harnessPayload(proof)
	if e != nil {
		return out, e
	}
	kind, world := settlementDimension(scope)
	if _, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,?,?,?,'HARNESS_CUTOFF',?,?,'COMMIT')", out.ReceiptKey, t, p, scope.SubjectID, kind, world, task, out.ReceiptKey, hash[:]); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_cutoffs VALUES(?,?,?,?,?,?,?,?,?,?)", t, p, task, out.RiskCaseID, out.ReceiptKey, releaseID.Installation, releaseID.Generation, proof.NarrativeGeneration, proof.CheckedAt, raw); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_cutoff_work SET done=1,last_failure='' WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, task); e != nil {
		return out, e
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = settlementServiceTx(ctx, tx, releaseID); e != nil {
		return out, e
	}
	if current, e := provider.CheckSettlementContinuity(evidence, releaseID); e != nil || current != head {
		return out, ErrDenied
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return out, e
	}
	return out, nil
}

// RunHarnessCutoffs is one bounded page, not a new task system. A failed item
// keeps its original identity and next retry; a successful replay is also done.
func (s *Store) RunHarnessCutoffs(ctx context.Context) (int, int, error) {
	if s.SettlementAuthority == nil {
		return 0, 0, ErrDenied
	}
	ctx, id, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return 0, 0, e
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, e := s.DB.QueryContext(ctx, "SELECT t.tenant_id,t.product_id,t.owner_subject_id,COALESCE(t.world_instance_id,''),w.task_id,j.operation FROM harness_cutoff_work w JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_settlement_bindings b USING(tenant_id,product_id,task_id) WHERE w.done=0 AND w.next_run_at<=UTC_TIMESTAMP(6) AND e.settle_by<=UTC_TIMESTAMP(6) AND b.installation_id=? AND b.service_id=? AND b.policy=? AND ((t.scope_kind='WORLD' AND t.task_kind='TURN' AND j.operation='TURN') OR (t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation IN ('CANDIDATE','REHEARSAL'))) ORDER BY w.next_run_at,w.tenant_id,w.product_id,w.task_id LIMIT 100", id.Installation, id.ServiceID, id.Policy)
	if e != nil {
		return 0, 0, e
	}
	type item struct {
		scope     domain.Scope
		task      string
		operation string
	}
	items := []item{}
	for rows.Next() {
		var i item
		if e = rows.Scan(&i.scope.TenantID, &i.scope.ProductID, &i.scope.SubjectID, &i.scope.WorldInstanceID, &i.task, &i.operation); e != nil {
			rows.Close()
			return 0, 0, e
		}
		items = append(items, i)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, 0, e
	}
	done, failed := 0, 0
	for _, i := range items {
		if e = ctx.Err(); e != nil {
			return done, failed, e
		}
		itemCtx, itemCancel := context.WithTimeout(ctx, 5*time.Second)
		if i.operation == "REHEARSAL" {
			_, e = s.CutoffDraftRehearsal(itemCtx, i.scope.ProductScope, i.task)
		} else if i.scope.WorldInstanceID == "" {
			_, e = s.CutoffDraftCandidate(itemCtx, i.scope.ProductScope, i.task)
		} else {
			_, e = s.CutoffHarnessTask(itemCtx, i.scope, i.task)
		}
		if e == nil {
			done++
		} else {
			failed++
		}
		record := s.recordCutoffRun(itemCtx, i.scope, i.task, e == nil)
		itemCancel()
		if record != nil {
			return done, failed, record
		}
	}
	return done, failed, nil
}
func (s *Store) recordCutoffRun(ctx context.Context, scope domain.Scope, task string, done bool) error {
	ctx, id, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return e
	}
	defer release()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	// This queue metadata writes under the same closed service and original task;
	// it grants no outcome, no purchase and no claim about a failed transaction.
	if e = settlementServiceTx(ctx, tx, id); e != nil {
		return e
	}
	var bound bool
	kind, world := settlementDimension(scope)
	// Original binding/scope/owner are immutable. This read grants no settlement
	// outcome and must not reacquire the very task lock whose timeout is being
	// recorded. The actual queue UPDATE serializes its own work row; all financial
	// transitions still require lockSettlement's complete task/attempt closure.
	if e = tx.QueryRowContext(ctx, "SELECT b.installation_id=? AND b.service_id=? AND b.policy=? AND t.owner_subject_id=? AND t.scope_kind=? AND t.world_instance_id<=>? FROM harness_settlement_bindings b JOIN tasks t USING(tenant_id,product_id,task_id) WHERE b.tenant_id=? AND b.product_id=? AND b.task_id=?", id.Installation, id.ServiceID, id.Policy, scope.SubjectID, kind, world, scope.TenantID, scope.ProductID, task).Scan(&bound); e != nil {
		return e
	}
	if !bound {
		return ErrDenied
	}
	if done {
		_, e = tx.ExecContext(ctx, "UPDATE harness_cutoff_work SET done=1,last_failure='' WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task)
	} else {
		_, e = tx.ExecContext(ctx, "UPDATE harness_cutoff_work SET failures=failures+1,last_failure='RETRY_REQUIRED',next_run_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND) WHERE tenant_id=? AND product_id=? AND task_id=? AND done=0", scope.TenantID, scope.ProductID, task)
	}
	if e != nil {
		return e
	}
	return s.costCommit(ctx, tx)
}

func (s *Store) turnSettlementAdmission(ctx context.Context, d DispatchRequest) (context.Context, *domain.SettlementServiceIdentity, func(), error) {
	var actual bool
	if e := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?) OR EXISTS(SELECT 1 FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID).Scan(&actual); e != nil {
		return ctx, nil, nil, e
	}
	if !actual {
		return ctx, nil, func() {}, nil
	}
	if s.SettlementAuthority == nil {
		return ctx, nil, nil, ErrDenied
	}
	next, id, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	return next, &id, release, e
}

func settlementReceiptScope(r receiptRow, scope domain.Scope, task string) bool {
	return r.tenant == scope.TenantID && r.product == scope.ProductID && r.owner == scope.SubjectID && r.task == task && settlementReceiptDimension(r, scope) && r.phase == "COMMIT"
}
func settlementDimension(scope domain.Scope) (string, any) {
	if scope.WorldInstanceID == "" {
		return "PRODUCT", nil
	}
	return "WORLD", scope.WorldInstanceID
}
func settlementReceiptDimension(r receiptRow, scope domain.Scope) bool {
	if scope.WorldInstanceID == "" {
		return r.kind == "PRODUCT" && !r.world.Valid
	}
	return r.kind == "WORLD" && r.world.Valid && r.world.String == scope.WorldInstanceID
}

// beginSettlementTx bounds the actual server lock wait as well as the caller's
// five-second context. Closing a canceled driver connection alone does not prove
// MySQL has stopped its waiting statement and released earlier locks.
func (s *Store) beginSettlementTx(ctx context.Context) (*sql.Tx, func(), error) {
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return nil, nil, e
	}
	var original uint64
	if e = conn.QueryRowContext(ctx, "SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&original); e != nil {
		conn.Close()
		return nil, nil, e
	}
	if _, e = conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout=4"); e != nil {
		conn.Close()
		return nil, nil, e
	}
	restore := func() {
		reset, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if _, err := conn.ExecContext(reset, "SET SESSION innodb_lock_wait_timeout=?", original); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}
	tx, e := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		restore()
		return nil, nil, e
	}
	return tx, func() { _ = tx.Rollback(); restore() }, nil
}
