package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
)

type rehearsalReadKey struct{}
type rehearsalReadAdmission struct {
	store   *Store
	root    domain.TaskLease
	worker  string
	index   int
	report  bool
	binding domain.RehearsalBinding
	closed  atomic.Bool
}

// This opaque lifetime owns only ordered physical source locks. Current DB
// eligibility is independently reconstructed by every inspection.
func (s *Store) EnterRehearsalResultRead(ctx context.Context, root domain.TaskLease, worker string, index int) (context.Context, func(), error) {
	if held, ok := ctx.Value(rehearsalReadKey{}).(*rehearsalReadAdmission); ok {
		if held.store != s || held.closed.Load() || held.root != root || held.worker != worker || (held.index != index && (!held.report || index < 0 || index >= len(held.binding.Inputs))) {
			return nil, nil, ErrDenied
		}
		return ctx, func() {}, nil
	}
	if index < 0 || index >= 20 || root.WorldInstanceID != nil || s.Guard == nil {
		return nil, nil, ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, nil, e
	}
	world := turnIdentity(root.TaskID, "rehearsal-world")
	child := domain.TaskLease{ProductScope: root.ProductScope, WorldInstanceID: &world, OwnerSubjectID: root.OwnerSubjectID, TaskID: turnIdentity(root.TaskID, "rehearsal-child", strconv.Itoa(index))}
	r, e := s.rehearsalChildBarrier(ctx, root, child, worker)
	if e != nil {
		close()
		return nil, nil, e
	}
	p := r.rehearsalExecution
	if p.child.index != index {
		close()
		return nil, nil, ErrDenied
	}
	evidence, e := s.DraftRehearsalReader.ReadRehearsalSourceEvidence(ctx, root.ProductScope, p.initialization.binding, p.initialization.request)
	if e != nil {
		close()
		return nil, nil, e
	}
	held := &rehearsalReadAdmission{store: s, root: root, worker: worker, index: index, binding: p.initialization.binding}
	var once sync.Once
	return context.WithValue(ctx, rehearsalReadKey{}, held), func() { once.Do(func() { held.closed.Store(true); evidence.Close(); close() }) }, nil
}
func (s *Store) rehearsalReadSourcesHeld(ctx context.Context, r BarrierRequest) bool {
	held, ok := ctx.Value(rehearsalReadKey{}).(*rehearsalReadAdmission)
	p := r.rehearsalExecution
	return ok && !held.closed.Load() && held.store == s && p != nil && held.root == p.root && held.worker == p.worker && (held.index == p.child.index || held.report && p.child.index >= 0 && p.child.index < len(held.binding.Inputs)) && reflect.DeepEqual(held.binding, p.initialization.binding)
}

// Owns the one original creative-source lock set for the complete report.
// It does not accept a caller child set, count, body or authority flag.
func (s *Store) EnterRehearsalReportRead(ctx context.Context, root domain.TaskLease, worker string) (context.Context, int, func(), error) {
	if ctx.Value(rehearsalReadKey{}) != nil {
		return nil, 0, nil, ErrDenied
	}
	ctx, close, e := s.EnterRehearsalResultRead(ctx, root, worker, 0)
	if e != nil {
		return nil, 0, nil, e
	}
	held := ctx.Value(rehearsalReadKey{}).(*rehearsalReadAdmission)
	n := len(held.binding.Inputs)
	if n < 1 || n > 20 {
		close()
		return nil, 0, nil, ErrDenied
	}
	if _, e = s.InspectRehearsalChildOutcome(ctx, root, worker, 0); e != nil {
		close()
		return nil, 0, nil, e
	}
	var next, count int
	e = s.DB.QueryRowContext(ctx, "SELECT a.next_input,(SELECT COUNT(*) FROM harness_rehearsal_children c WHERE c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.root_task_id=a.task_id) FROM harness_admin_rehearsals a WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=?", root.TenantID, root.ProductID, root.TaskID, root.SubjectID).Scan(&next, &count)
	if e != nil {
		close()
		return nil, 0, nil, e
	}
	if next != n || count != n {
		close()
		return nil, 0, nil, ErrDenied
	}
	held.report = true
	return ctx, n, close, nil
}

// InspectRehearsalChildResult is a pure owning read for the current root only.
// It cannot resume a child or return an execution/send capability.
func (s *Store) InspectRehearsalChildResult(ctx context.Context, root domain.TaskLease, worker string, index int) (domain.CandidateCommit, error) {
	c, e := s.InspectRehearsalChildOutcome(ctx, root, worker, index)
	if e != nil {
		return domain.CandidateCommit{}, e
	}
	if c.World == nil || c.Negative != nil {
		return domain.CandidateCommit{}, ErrDenied
	}
	return *c.World, nil
}
func (s *Store) InspectRehearsalChildOutcome(ctx context.Context, root domain.TaskLease, worker string, index int) (out domain.RehearsalChildOutcome, err error) {
	if index < 0 || index >= 20 || root.WorldInstanceID != nil || s.RehearsalInputOwner == nil || s.ProviderEvidence == nil {
		return out, ErrDenied
	}
	ctx, close, e := s.EnterRehearsalResultRead(ctx, root, worker, index)
	if e != nil {
		return out, e
	}
	defer close()
	world := turnIdentity(root.TaskID, "rehearsal-world")
	child := domain.TaskLease{ProductScope: root.ProductScope, WorldInstanceID: &world, OwnerSubjectID: root.OwnerSubjectID, TaskID: turnIdentity(root.TaskID, "rehearsal-child", strconv.Itoa(index))}
	r, e := s.rehearsalChildBarrier(ctx, root, child, worker)
	if e != nil {
		return out, e
	}
	if r.rehearsalExecution.child.index != index {
		return out, ErrDenied
	}
	r.rehearsalReportRead = true
	r.commandOnly = true
	r.Operation = "REHEARSAL_CHILD_RESULT_READ"
	r.CommandKey = turnIdentity(child.TaskID, "parent-result-read")
	r.CommitKey = r.CommandKey
	r.Locks = append(r.Locks, harnessTaskKey(root.ProductScope, child.TaskID))
	rows, e := s.DB.QueryContext(ctx, "SELECT a.attempt_id,z.authorization_id FROM attempts a JOIN dispatch_authorizations z USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", root.TenantID, root.ProductID, child.TaskID)
	if e != nil {
		return out, e
	}
	n := 0
	for rows.Next() {
		var attempt, auth string
		if e = rows.Scan(&attempt, &auth); e != nil {
			rows.Close()
			return out, e
		}
		n++
		r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{root.TenantID, root.ProductID, child.TaskID, attempt, auth}})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if n < 1 || n > 2 {
		return out, ErrDenied
	}
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		var err error
		out, err = s.inspectRehearsalChildOutcomeTx(ctx, port.(*unit), r, n)
		if err != nil {
			return err
		}
		return errApprovedInspected
	})
	if !errors.Is(e, errApprovedInspected) {
		if e == nil {
			e = ErrDenied
		}
		return domain.RehearsalChildOutcome{}, e
	}
	metas := []domain.MockResultMetadata{out.Metadata}
	if out.OriginalDependency != nil {
		metas = append(metas, *out.OriginalDependency)
	}
	for _, meta := range metas {
		actual, e := s.ProviderEvidence.OriginalMetadata(ctx, meta.Identity)
		if e != nil {
			return domain.RehearsalChildOutcome{}, e
		}
		if actual != meta {
			return domain.RehearsalChildOutcome{}, ErrConflict
		}
	}
	return out, nil
}

func (s *Store) checkRehearsalReportRead(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalExecution
	if !r.rehearsalReportRead || !r.commandOnly || p == nil || r.Operation != "REHEARSAL_CHILD_RESULT_READ" {
		return ErrDenied
	}
	rr := BarrierRequest{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, TaskID: p.root.TaskID, rehearsalInitialization: &p.initialization}
	if e := s.checkRehearsalInitialization(ctx, u, rr); e != nil {
		return e
	}
	if e := checkRehearsalChildIdentity(ctx, u, r); e != nil {
		return e
	}
	if e := checkCurrentLease(ctx, u.tx, BarrierRequest{ProductScope: r.ProductScope, TaskID: p.root.TaskID, WorkerID: p.worker, Fence: p.root.Fence}); e != nil {
		return e
	}
	state := "ISOLATED"
	if u.rehearsalReport != nil && u.rehearsalReport.applied != nil {
		if e := checkRehearsalReportApplied(ctx, u); e != nil {
			return e
		}
		state = "COMMITTED"
	}
	var valid bool
	e := u.tx.QueryRowContext(ctx, "SELECT c.root_generation=e.narrative_generation AND a.fsm_state=? AND c.input_index<a.next_input FROM harness_rehearsal_children c JOIN harness_envelopes e ON e.tenant_id=c.tenant_id AND e.product_id=c.product_id AND e.task_id=c.root_task_id JOIN harness_admin_rehearsals a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.task_id=c.root_task_id JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id JOIN commit_receipts cr ON cr.commit_key=c.accepted_receipt AND cr.tenant_id=c.tenant_id AND cr.product_id=c.product_id AND cr.task_id=c.root_task_id AND cr.owner_subject_id=h.owner_subject_id AND cr.command_key=c.accepted_receipt AND cr.payload_hash=h.command_hash AND cr.operation='REHEARSAL_CHILD_START' AND cr.phase='COMMIT' AND cr.scope_kind='PRODUCT' AND cr.world_instance_id IS NULL WHERE c.tenant_id=? AND c.product_id=? AND c.child_task_id=? AND c.root_task_id=?", state, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.root.TaskID).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}

// Fixed same-Tx reader. The caller owns the entire planned set and a read-only child view.
func (s *Store) inspectRehearsalChildResultTx(ctx context.Context, u *unit, r BarrierRequest, n int) (out domain.CandidateCommit, err error) {
	if r.rehearsalExecution == nil || !u.rehearsalReportRead {
		return out, ErrDenied
	}
	root := r.rehearsalExecution.root
	child := domain.TaskLease{TaskID: r.TaskID}
	world := r.WorldScope.WorldInstanceID
	var e error
	var raw, approvalRaw []byte
	var receiptKey string
	e = u.tx.QueryRowContext(ctx, "SELECT c.document,c.receipt_key,a.document FROM harness_turn_commits c JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) JOIN approved_results z ON z.tenant_id=c.tenant_id AND z.product_id=c.product_id AND z.task_id=c.task_id AND z.record_id=c.result_id JOIN harness_candidate_approvals a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.task_id=c.task_id AND a.attempt_id=JSON_UNQUOTE(JSON_EXTRACT(c.document,'$.Metadata.Identity.AttemptID')) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND t.owner_subject_id=? AND t.state='COMMITTED' AND h.fsm_state='COMMITTED' AND z.status='COMMITTED'", root.TenantID, root.ProductID, child.TaskID, world, root.SubjectID, root.SubjectID).Scan(&raw, &receiptKey, &approvalRaw)
	if e != nil {
		return out, e
	}
	var approval domain.CandidateApproval
	if json.Unmarshal(raw, &out) != nil || json.Unmarshal(approvalRaw, &approval) != nil || out.Metadata.Identity.Scope != *r.WorldScope || out.Metadata.Identity.TaskID != child.TaskID || out.ReceiptKey != receiptKey || out.ResultID != turnIdentity(child.TaskID, "result") || approval.Metadata != out.Metadata || approval.ResultID != out.ResultID {
		return out, ErrDenied
	}
	receipt, e := queryReceipt(ctx, u.tx, receiptKey)
	if e != nil {
		return out, e
	}
	_, hash, e := harnessPayload(out.Metadata)
	if e != nil {
		return out, e
	}
	expected := r
	expected.commandOnly = false
	expected.Operation = "HARNESS_TURN_COMMIT"
	expected.CommandKey = turnIdentity(child.TaskID, "world-commit")
	expected.CommitKey = receiptKey
	expected.PayloadHash = hash
	// The actual commit key is stable per original task; compare the full scope,
	// operation, phase and immutable metadata payload, not a bare row existence.
	if !receipt.matches(expected) {
		return out, ErrDenied
	}
	if e = checkActiveCandidate(ctx, u, r, out.Metadata.Identity.AttemptID); e != nil {
		return out, e
	}
	var active bool
	if e = u.tx.QueryRowContext(ctx, "SELECT state='COMMITTED' AND narrative_generation=? FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", out.NarrativeGeneration, root.TenantID, root.ProductID, child.TaskID).Scan(&active); e != nil {
		return out, e
	}
	if !active || out.NarrativeGeneration == 0 {
		return out, ErrDenied
	}
	var approvalKey string
	if e = u.tx.QueryRowContext(ctx, "SELECT receipt_key FROM harness_candidate_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", root.TenantID, root.ProductID, child.TaskID, out.Metadata.Identity.AttemptID).Scan(&approvalKey); e != nil {
		return out, e
	}
	ar, e := queryReceipt(ctx, u.tx, approvalKey)
	if e != nil {
		return out, e
	}
	ae := expected
	ae.Operation = "HARNESS_CANDIDATE_APPROVE"
	ae.CommandKey = turnIdentity(out.Metadata.Identity.AttemptID, "approval")
	ae.CommitKey = approvalKey
	if !ar.matches(ae) {
		return out, ErrDenied
	}
	metas := []domain.MockResultMetadata{out.Metadata}
	if out.OriginalDependency != nil {
		metas = append([]domain.MockResultMetadata{*out.OriginalDependency}, metas...)
	}
	if len(metas) != n {
		return out, ErrDenied
	}
	for _, meta := range metas {
		if meta.Identity.Scope != *r.WorldScope || meta.Identity.TaskID != child.TaskID || !meta.BodyAvailable || meta.BodyStorage == "" {
			return out, ErrDenied
		}
		if e := u.RequireLock(LockKey{Rank: 60, IDs: []string{root.TenantID, root.ProductID, child.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID}}); e != nil {
			return out, e
		}
		var saved []byte
		var code, key, bodyHash string
		if e := u.tx.QueryRowContext(ctx, "SELECT metadata,validation_code,receipt_key,body_hash FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", root.TenantID, root.ProductID, child.TaskID, meta.Identity.AttemptID).Scan(&saved, &code, &key, &bodyHash); e != nil {
			return out, e
		}
		var m domain.MockResultMetadata
		if json.Unmarshal(saved, &m) != nil || m != meta || bodyHash != meta.BodyHash || (meta == out.Metadata && code != "STRUCTURE_VALID") {
			return out, ErrDenied
		}
		vr, e := queryReceipt(ctx, u.tx, key)
		if e != nil {
			return out, e
		}
		_, vh, e := harnessPayload(struct {
			Metadata domain.MockResultMetadata
			Code     string
		}{meta, code})
		if e != nil {
			return out, e
		}
		ve := expected
		ve.Operation = "HARNESS_RESULT_VALIDATE"
		ve.CommandKey = turnIdentity(meta.Identity.AttemptID, "validation")
		ve.CommitKey = key
		ve.PayloadHash = vh
		if !vr.matches(ve) {
			return out, ErrDenied
		}
		var sent bool
		if e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attempts a JOIN dispatch_authorizations d USING(tenant_id,product_id,task_id,attempt_id) JOIN commit_receipts c ON c.commit_key=a.marker_commit_key AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.task_id=a.task_id AND c.owner_subject_id=d.owner_subject_id WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=? AND d.authorization_id=? AND a.request_key=? AND a.request_hash=UNHEX(?) AND a.route=? AND a.marker_commit_key=? AND a.send_started_at IS NOT NULL AND c.operation='TURN_DISPATCH' AND c.phase='SEND' AND c.world_instance_id=? AND c.scope_kind='WORLD')", root.TenantID, root.ProductID, child.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID, meta.Identity.RequestKey, meta.Identity.RequestHash, meta.Identity.RouteID, meta.Identity.MarkerKey, world).Scan(&sent); e != nil {
			return out, e
		}
		if !sent {
			return out, ErrDenied
		}
	}
	if e = s.RehearsalInputOwner.InspectRehearsalChildResultTx(ctx, u, *r.WorldScope, child.TaskID, *r.Versions, out); e != nil {
		return out, e
	}
	return out, nil
}
