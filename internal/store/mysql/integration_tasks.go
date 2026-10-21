package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
	"time"
)

// This plan is assembled from the current CLIENT and the persisted original
// intake. It is private to the Store and cannot be constructed by a wire Scope.
func (s *Store) integrationTaskPlan(ctx context.Context, q creationRows, id string) (*integrationTurnPlan, error) {
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || v.store != s || (v.principal.Kind != domain.IntegrationClient && v.principal.Kind != domain.IntegrationService) || domain.ValidateID(id) != nil {
		return nil, ErrDenied
	}
	var kind string
	if e := q.QueryRowContext(ctx, "SELECT task_kind FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", v.principal.Tenant, v.principal.Product, id, v.principal.StableSubject).Scan(&kind); e != nil {
		return nil, e
	}
	if kind == "CONTROL" {
		return s.integrationControlTaskPlan(ctx, q, v, id)
	}
	if kind != "TURN" || v.principal.Kind != domain.IntegrationClient {
		return nil, ErrDenied
	}
	a, e := s.inspectIntegrationTurnRead(ctx, q, v, v.principal.World)
	if e != nil {
		return nil, e
	}
	a.task = id
	var hash []byte
	e = q.QueryRowContext(ctx, "SELECT original_key,quote_id,command_hash FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND world_instance_id=?", a.scope.TenantID, a.scope.ProductID, id, a.scope.SubjectID, a.scope.WorldInstanceID).Scan(&a.key, &a.quote, &hash)
	if e != nil {
		return nil, e
	}
	if len(hash) != 32 || id != turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, "TURN", a.key) {
		return nil, ErrDenied
	}
	copy(a.hash[:], hash)
	e = q.QueryRowContext(ctx, "SELECT accepted_use_generation,accepted_consent_revision,accepted_use_expires_at FROM integration_task_principals WHERE tenant_id=? AND product_id=? AND task_id=?", a.scope.TenantID, a.scope.ProductID, id).Scan(&a.use.Generation, &a.use.Revision, &a.use.Expires)
	if e != nil {
		return nil, e
	}
	a.keys = append(a.keys, LockKey{Rank: 40, IDs: []string{a.scope.TenantID, a.scope.ProductID, a.scope.WorldInstanceID}}, harnessTaskKey(a.scope.ProductScope, id), harnessTaskKey(a.scope.ProductScope, a.quote), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(id, "accepted")}})
	if e = a.verifyIntake(ctx, q, a.intakeRequest(), id, true, true); e != nil {
		return nil, e
	}
	return a, nil
}
func (a *integrationTurnPlan) currentTaskRead(ctx context.Context, q creationRows) error {
	n, e := a.store.integrationTaskPlan(ctx, q, a.task)
	if e != nil || !reflect.DeepEqual(n.control, a.control) || !reflect.DeepEqual(n.session, a.session) || !reflect.DeepEqual(n.eligibility, a.eligibility) || !reflect.DeepEqual(n.service, a.service) || n.account != a.account || n.key != a.key || n.quote != a.quote || n.hash != a.hash || n.use != a.use {
		return ErrDenied
	}
	return nil
}

// Metadata has no body or positive-use grant. Read and final readback share the
// task lock, current participant proof and original immutable intake identity.
func (s *Store) ReadIntegrationTask(ctx context.Context, id string) (generated.TaskView, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return generated.TaskView{}, e
	}
	defer done()
	if v, ok := ctx.Value(integrationContextKey{}).(currentIntegration); ok && v.store == s && v.principal.Kind == domain.IntegrationClient && domain.ValidateID(id) == nil {
		var kind string
		if e := s.DB.QueryRowContext(ctx, "SELECT task_kind FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", v.principal.Tenant, v.principal.Product, id, v.principal.StableSubject).Scan(&kind); e == nil && kind == "GOVERNANCE" {
			return s.ReadIntegrationGovernanceTask(ctx, id)
		}
	}
	a, e := s.integrationTaskPlan(ctx, s.DB, id)
	if e != nil {
		return generated.TaskView{}, e
	}
	tx, e := s.integrationMemoryReadTx(ctx, a.keys)
	if e != nil {
		return generated.TaskView{}, e
	}
	defer tx.Rollback()
	if e = a.currentTaskRead(ctx, tx); e != nil {
		return generated.TaskView{}, e
	}
	out, e := integrationTaskView(ctx, tx, a)
	if e != nil {
		return out, e
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return generated.TaskView{}, e
		}
	}
	if e = a.currentTaskRead(ctx, tx); e != nil {
		return generated.TaskView{}, e
	}
	after, e := integrationTaskView(ctx, tx, a)
	if e != nil || !reflect.DeepEqual(after, out) {
		return generated.TaskView{}, ErrDenied
	}
	return out, typedJSON("TaskView", out)
}
func integrationTaskView(ctx context.Context, q creationRows, a *integrationTurnPlan) (generated.TaskView, error) {
	if a.control != nil {
		return integrationControlTaskView(ctx, q, a)
	}
	out := generated.TaskView{Attempts: []generated.AttemptSummary{}, CheckpointState: "NONE"}
	var state, release string
	var revision, control, privacy, view uint64
	var settle time.Time
	e := q.QueryRowContext(ctx, "SELECT t.state,w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,h.settle_by FROM tasks t JOIN world_controls w USING(tenant_id,product_id,world_instance_id) JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=?", a.scope.TenantID, a.scope.ProductID, a.task, a.scope.SubjectID).Scan(&state, &release, &revision, &control, &privacy, &view, &settle)
	if e != nil {
		return out, e
	}
	out.Task = generated.TaskReceipt{TaskID: a.task, TaskKind: "TURN", TaskState: state, Scope: turnScope(a.scope), CorrelationID: a.task}
	out.Versions = generated.Versions{ReleaseID: release, WorldRevision: strconv.FormatUint(revision, 10), ControlVersion: strconv.FormatUint(control, 10), PrivacyEpoch: strconv.FormatUint(privacy, 10), ViewEpoch: strconv.FormatUint(view, 10)}
	deadline := settle.Format(time.RFC3339Nano)
	out.Billing = generated.BillingSummary{RiskBearer: "PLATFORM", ServiceSettlementState: "RESERVED", ServiceUnit: "WE_CREDIT", SupplierCostCurrency: "USD", ReservationSettleBy: &deadline}
	var held, consumed, supplier int64
	e = q.QueryRowContext(ctx, "SELECT COALESCE(SUM(CASE WHEN kind='customer' AND account_id=? AND status='HELD' THEN amount ELSE 0 END),0),COALESCE(SUM(CASE WHEN kind='customer' AND account_id=? AND status='SETTLED' THEN amount ELSE 0 END),0),COALESCE(SUM(CASE WHEN kind='supplier_global' AND status='HELD' THEN amount ELSE 0 END),0) FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=?", a.account, a.account, a.scope.TenantID, a.scope.ProductID, a.task).Scan(&held, &consumed, &supplier)
	if e != nil {
		return out, e
	}
	out.Billing.CustomerReservedUnits = strconv.FormatInt(held, 10)
	out.Billing.CustomerConsumedUnits = strconv.FormatInt(consumed, 10)
	out.Billing.SupplierSafetyReservedUnits = strconv.FormatInt(supplier, 10)
	turn := generated.TurnView{Task: out.Task, Billing: out.Billing}
	if e = projectTurnSettlementTx(ctx, q, a.scope, a.task, &turn); e != nil {
		return out, e
	}
	out.Task = turn.Task
	out.Billing = turn.Billing
	rows, e := q.QueryContext(ctx, "SELECT a.attempt_id,a.state,a.route,a.send_started_at IS NOT NULL,COALESCE(q.provider_state='SUCCEEDED' AND JSON_EXTRACT(q.metadata,'$.UsageKnown')=true,0),q.attempt_id IS NOT NULL FROM attempts a LEFT JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot", a.scope.TenantID, a.scope.ProductID, a.task)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var v generated.AttemptSummary
		var sent, known, query bool
		if e = rows.Scan(&v.AttemptID, &v.AttemptState, &v.RouteID, &sent, &known, &query); e != nil {
			rows.Close()
			return out, e
		}
		v.PurchaseResponsibility = "MOCK"
		v.QueryCapability = "UNSUPPORTED"
		if query {
			v.QueryCapability = "SUPPORTED"
		}
		v.CancelState = "NOT_REQUESTED"
		if state == "CANCELLED" && sent {
			v.CancelState = "UNSUPPORTED"
		}
		v.UsageState = "UNKNOWN"
		if known {
			v.UsageState = "AUTHORITATIVE"
		}
		out.Attempts = append(out.Attempts, v)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return out, rowErr
	}
	for i := range out.Attempts {
		if out.Attempts[i].UsageState != "AUTHORITATIVE" {
			known, err := originalMeterUsageKnown(ctx, q, a.scope, a.task, string(out.Attempts[i].AttemptID))
			if err != nil {
				return out, err
			}
			if known {
				out.Attempts[i].UsageState = "AUTHORITATIVE"
			}
		}
	}
	return out, nil
}

// Cancellation changes only the local task flag. A recorded send keeps its
// original external state, holds and narrow accounting/query/cleanup authority.
func (s *Store) CancelIntegrationTask(ctx context.Context, id, key string, in generated.CancelRequest) (generated.CancelReceipt, error) {
	var out generated.CancelReceipt
	raw, e := json.Marshal(in)
	if e != nil || generated.Validate("CancelRequest", raw) != nil || domain.ValidateIdempotencyKey(key) != nil {
		return out, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	a, e := s.integrationTaskPlan(ctx, s.DB, id)
	if e != nil {
		return out, e
	}
	allowed := a.control != nil && a.principal.principal.Kind == domain.IntegrationService
	for _, x := range a.session.Actions {
		allowed = allowed || a.control == nil && x == "TURN" || a.control != nil && x == "CONTROL_"+a.control.document.Action
	}
	if !allowed {
		return out, ErrDenied
	}
	_, hash, e := harnessPayload(struct {
		Scope   domain.Scope
		Task    string
		Request generated.CancelRequest
	}{a.scope, id, in})
	if e != nil {
		return out, e
	}
	receipt := turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, "cancelIntegrationTask", key)
	keys := append([]LockKey{}, a.keys...)
	keys = append(keys, LockKey{Rank: 90, Kind: "content_check", IDs: []string{a.scope.TenantID, a.scope.ProductID, receipt}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{receipt}})
	tx, e := s.integrationMemoryReadTx(ctx, keys)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if e = a.currentTaskRead(ctx, tx); e != nil {
		return out, e
	}
	proof, e := queryReceipt(ctx, tx, receipt)
	expected := BarrierRequest{ProductScope: a.scope.ProductScope, WorldScope: &a.scope, OwnerSubjectID: a.scope.SubjectID, TaskID: id, Operation: "cancelIntegrationTask", CommandKey: key, CommitKey: receipt, PayloadHash: hash}
	if e == nil {
		if !proof.matches(expected) {
			return out, ErrConflict
		}
		stored, readErr := readIntegrationCancel(ctx, tx, a, receipt)
		if readErr != nil || generated.Decode("CancelReceipt", stored, &out) != nil || out.Task.TaskID != id {
			return out, ErrConflict
		}
		if s.BeforeReadFinish != nil {
			if e = s.BeforeReadFinish(ctx, tx); e != nil {
				return generated.CancelReceipt{}, e
			}
		}
		if e = a.currentTaskRead(ctx, tx); e != nil {
			return generated.CancelReceipt{}, e
		}
		finalProof, proofErr := queryReceipt(ctx, tx, receipt)
		finalBody, bodyErr := readIntegrationCancel(ctx, tx, a, receipt)
		if proofErr != nil || !finalProof.matches(expected) || bodyErr != nil || !bytes.Equal(stored, finalBody) {
			return generated.CancelReceipt{}, ErrDenied
		}
		return out, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	view, e := integrationTaskView(ctx, tx, a)
	if e != nil {
		return out, e
	}
	if view.Task.TaskState == "COMMITTED" {
		return out, ErrConflict
	}
	var sent, unknown int
	e = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(send_started_at IS NOT NULL),0),COALESCE(SUM(a.state='UNKNOWN' OR q.provider_state='UNKNOWN'),0) FROM attempts a LEFT JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=?", a.scope.TenantID, a.scope.ProductID, id).Scan(&sent, &unknown)
	if e != nil {
		return out, e
	}
	res, e := tx.ExecContext(ctx, "UPDATE tasks SET cancelled=1,state='CANCELLED' WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND state<>'COMMITTED'", a.scope.TenantID, a.scope.ProductID, id, a.scope.SubjectID)
	if e != nil {
		return out, e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return out, ErrConflict
	}
	view.Task.TaskState = "CANCELLED"
	out = generated.CancelReceipt{Task: view.Task, Billing: view.Billing, LocalCancelState: "CANCELLED", ExternalCancelState: "NOT_SENT"}
	if sent > 0 {
		out.LocalCancelState = "REQUESTED"
		out.ExternalCancelState = "UNSUPPORTED"
	}
	if unknown > 0 {
		out.ExternalCancelState = "UNKNOWN"
	}
	if e = typedJSON("CancelReceipt", out); e != nil {
		return out, e
	}
	body, _ := json.Marshal(out)
	_, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,'WORLD',?,?,'cancelIntegrationTask',?,?,'COMMIT')", receipt, a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, id, key, hash[:])
	if e != nil {
		return out, e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO admin_results(tenant_id,product_id,result_id,owner_subject_id,schema_name,document) VALUES(?,?,?,?,'CancelReceipt',?)", a.scope.TenantID, a.scope.ProductID, receipt, a.scope.SubjectID, body)
	if e != nil {
		return out, e
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return generated.CancelReceipt{}, e
		}
	}
	if e = a.currentTaskRead(ctx, tx); e != nil {
		return generated.CancelReceipt{}, e
	}
	final, e := queryReceipt(ctx, tx, receipt)
	if e != nil || !final.matches(expected) {
		return generated.CancelReceipt{}, ErrDenied
	}
	var cancelled bool
	e = tx.QueryRowContext(ctx, "SELECT cancelled=1 AND state='CANCELLED' FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", a.scope.TenantID, a.scope.ProductID, id, a.scope.SubjectID).Scan(&cancelled)
	if e != nil || !cancelled {
		return generated.CancelReceipt{}, ErrDenied
	}
	finalRaw, e := readIntegrationCancel(ctx, tx, a, receipt)
	var decoded generated.CancelReceipt
	if e != nil || json.Unmarshal(finalRaw, &decoded) != nil || !reflect.DeepEqual(decoded, out) {
		return generated.CancelReceipt{}, ErrDenied
	}
	if e = tx.Commit(); e != nil {
		return generated.CancelReceipt{}, ErrCommitUnknown
	}
	return out, nil
}

// Compare only concrete original payload bytes; a new lawful Session may read
// the same historical intake, but may never rewrite its hash or accepted tuple.
func (s *Store) integrationReplayAccepted(ctx context.Context, world, key string, request generated.TurnRequest) (generated.TurnAccepted, error) {
	original, e := s.readIntegrationIntakeReceipt(ctx, world, key, &request)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	return original.Acceptance, nil
}

// Result IO receives no worker lease. Its concrete read seal requires the
// current CLIENT, original accepted generation, own body permission and the
// existing source/route inspection before and after Files reads any bytes.
func (s *Store) checkIntegrationApprovedRead(ctx context.Context, q creationRows, r BarrierRequest) error {
	a := r.integrationTurn
	if a == nil || a.store != s || a.action != "APPROVED_READ" || a.lease != nil || ctx.Value(integrationTurnKey{}) != a || ctx.Value(operatorContextKey{}) != nil || !r.commandOnly || !r.approvedHistoryRead || r.Operation != "HARNESS_APPROVED_READ" || r.TaskID != a.task || r.ProductScope != a.scope.ProductScope || r.WorldScope == nil || *r.WorldScope != a.scope || r.OwnerSubjectID != a.scope.SubjectID {
		return ErrDenied
	}
	if e := a.currentTaskRead(ctx, q); e != nil {
		return e
	}
	choice, e := a.inspectCurrentChoice(ctx, q, a.choice)
	if a.choice == nil || e != nil || choice.Use.Generation != a.use.Generation {
		return ErrDenied
	}
	var body bool
	e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.private_body.read' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))", a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.SubjectID).Scan(&body)
	if e != nil || !body {
		return ErrDenied
	}
	return nil
}

// Task outputs use the existing command result store, not the closed registry
// receipt family. The original COMMIT separately binds the complete request.
func readIntegrationCancel(ctx context.Context, q creationRows, a *integrationTurnPlan, receipt string) ([]byte, error) {
	var body []byte
	e := q.QueryRowContext(ctx, "SELECT document FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? AND owner_subject_id=? AND schema_name='CancelReceipt'", a.scope.TenantID, a.scope.ProductID, receipt, a.scope.SubjectID).Scan(&body)
	return body, e
}

// CONTROL metadata preserves the actual command owner. SERVICE metadata never
// becomes the player's private-body authority, nor accepts a caller Scope.
func (s *Store) integrationControlTaskPlan(ctx context.Context, q creationRows, v currentIntegration, id string) (*integrationTurnPlan, error) {
	var raw []byte
	var d integrationControlDocument
	if e := q.QueryRowContext(ctx, "SELECT document FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", v.principal.Tenant, v.principal.Product, id).Scan(&raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &d) != nil || d.Task != id || d.Scope.SubjectID != v.principal.StableSubject || d.Scope.TenantID != v.principal.Tenant || d.Scope.ProductID != v.principal.Product {
		return nil, ErrDenied
	}
	c, e := s.inspectIntegrationControl(ctx, q, v, d.Scope.WorldInstanceID, d.Action, false, true)
	if e != nil {
		return nil, e
	}
	c.document = d
	c.raw = raw
	c.hash, e = integrationControlHash(d)
	if e != nil {
		return nil, e
	}
	if e = c.checkOriginal(ctx, q, false); e != nil {
		return nil, e
	}
	a := &integrationTurnPlan{store: s, control: c, principal: v, service: c.service, session: c.session, eligibility: c.eligibility, scope: d.Scope, task: id, key: d.Key, hash: c.hash}
	if e = q.QueryRowContext(ctx, "SELECT account_id FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=?", v.principal.Tenant, v.principal.Product, v.principal.Customer).Scan(&a.account); e != nil {
		return nil, e
	}
	a.keys = append(append([]LockKey{}, c.keys...), LockKey{Rank: 40, IDs: []string{d.Scope.TenantID, d.Scope.ProductID, d.Scope.WorldInstanceID}}, harnessTaskKey(d.Scope.ProductScope, id), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{d.Receipt}})
	return a, nil
}
func integrationControlTaskView(ctx context.Context, q creationRows, a *integrationTurnPlan) (generated.TaskView, error) {
	out := generated.TaskView{Attempts: []generated.AttemptSummary{}, CheckpointState: "NONE"}
	var state, release string
	var rev, cv, pe, ve uint64
	e := q.QueryRowContext(ctx, "SELECT t.state,w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch FROM tasks t JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=?", a.scope.TenantID, a.scope.ProductID, a.task, a.scope.SubjectID).Scan(&state, &release, &rev, &cv, &pe, &ve)
	if e != nil {
		return out, e
	}
	out.Task = generated.TaskReceipt{TaskID: a.task, TaskKind: "CONTROL", TaskState: state, Scope: turnScope(a.scope), CorrelationID: a.task}
	out.Versions = generated.Versions{ReleaseID: release, WorldRevision: strconv.FormatUint(rev, 10), ControlVersion: strconv.FormatUint(cv, 10), PrivacyEpoch: strconv.FormatUint(pe, 10), ViewEpoch: strconv.FormatUint(ve, 10)}
	// CONTROL has no purchase or generation path; reject any unexpected ledger.
	var n int
	if e = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=?", a.scope.TenantID, a.scope.ProductID, a.task).Scan(&n); e != nil || n != 0 {
		return out, ErrDenied
	}
	out.Billing = generated.BillingSummary{CustomerConsumedUnits: "0", CustomerReservedUnits: "0", SupplierSafetyReservedUnits: "0", RiskBearer: "PLATFORM", ServiceSettlementState: "NOT_APPLICABLE", ServiceUnit: "WE_CREDIT", SupplierCostCurrency: "USD"}
	return out, nil
}

func (s *Store) IntegrationTurnScope(ctx context.Context, id string) (domain.Scope, error) {
	a, e := s.integrationTaskPlan(ctx, s.DB, id)
	if e != nil {
		return domain.Scope{}, e
	}
	if a.control != nil {
		return domain.Scope{}, ErrDenied
	}
	return a.scope, nil
}

// Plan the current body-use proof before any owning locks are acquired. Keep
// the original intake use unchanged across lawful same-generation renewals.
func (a *integrationTurnPlan) planApprovedChoice(ctx context.Context, q creationRows) error {
	if a.action != "APPROVED_READ" || a.control != nil || a.choice != nil {
		return ErrDenied
	}
	choice, e := a.inspectCurrentChoice(ctx, q, nil)
	if e != nil || choice.Use.Generation != a.use.Generation {
		return ErrDenied
	}
	a.choice = choice
	a.keys = append(a.keys, a.choiceLocks()...)
	return nil
}
