package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"unicode/utf8"
)

// Immutable server decision, never a caller supplied completion or repair flag.
type candidateRepairPlan struct {
	ApprovalID, AttemptID, Reason string
	Original                      domain.MockResultMetadata
	Base, Binding                 domain.DraftCandidateMaterial
}

func candidateAttemptID(task, attempt string) bool {
	return attempt == turnIdentity(task, "candidate-attempt", "0") || attempt == turnIdentity(task, "candidate-attempt", "1")
}
func candidateSendKey(d DispatchRequest) string {
	if d.Slot == 0 {
		return turnIdentity(d.TaskID, "candidate-send")
	}
	return turnIdentity(d.AttemptID, "send")
}

type candidateRepairSnapshot struct {
	Base, Binding domain.DraftCandidateMaterial
	Original      domain.MockResultMetadata
	ApprovalID    string
}

func (s *Store) candidateSnapshotTx(ctx context.Context, u *unit, d DispatchRequest, m domain.DraftCandidateMaterial) ([]byte, error) {
	m.Canonical = nil
	if d.Slot == 0 {
		return json.Marshal(m)
	}
	var raw []byte
	var p candidateRepairPlan
	if e := u.tx.QueryRowContext(ctx, "SELECT document FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &p) != nil || !sameCandidateBinding(p.Binding, m) {
		return nil, ErrDenied
	}
	return json.Marshal(candidateRepairSnapshot{Base: p.Base, Binding: m, Original: p.Original, ApprovalID: p.ApprovalID})
}
func (s *Store) PrepareDraftCandidateSend(ctx context.Context, l domain.TaskLease, worker string) (DispatchRequest, error) {
	d, e := s.ActiveDraftCandidateDispatch(ctx, l, worker)
	if e != nil {
		return d, e
	}
	d.CommandKey = candidateSendKey(d)
	d.CommitKey = d.CommandKey
	return d, nil
}
func (s *Store) CandidateOriginalDispatch(ctx context.Context, d DispatchRequest) (DispatchRequest, error) {
	if !d.candidateExecution || d.WorldScope != nil {
		return DispatchRequest{}, ErrDenied
	}
	lease := domain.TaskLease{ProductScope: d.ProductScope, OwnerSubjectID: d.OwnerSubjectID, TaskID: d.TaskID, Fence: d.Fence}
	out, e := s.candidateOriginalDispatch(ctx, lease, d.WorkerID, false)
	out.CommandKey = candidateSendKey(out)
	out.CommitKey = out.CommandKey
	return out, e
}
func (s *Store) ActiveDraftCandidateDispatch(ctx context.Context, lease domain.TaskLease, worker string) (DispatchRequest, error) {
	d, e := s.candidateOriginalDispatch(ctx, lease, worker, false)
	if e != nil {
		return d, e
	}
	var raw []byte
	e = s.DB.QueryRowContext(ctx, "SELECT document FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return s.PrepareDraftCandidateDispatch(ctx, lease, worker)
	}
	if e != nil {
		return d, e
	}
	var plan candidateRepairPlan
	if json.Unmarshal(raw, &plan) != nil || plan.AttemptID != turnIdentity(lease.TaskID, "candidate-attempt", "1") || plan.ApprovalID != turnIdentity(lease.TaskID, "candidate-recovery") || plan.Reason != "STRUCTURE_REPAIR" || plan.Original.Identity.TaskID != lease.TaskID || plan.Original.Identity.AttemptID != d.AttemptID || plan.Original.Identity.Scope.ProductScope != lease.ProductScope || plan.Original.Identity.Kind != "CANDIDATE" {
		return DispatchRequest{}, ErrDenied
	}
	receipt, e := s.QueryCommitScoped(ctx, lease.ProductScope, nil, lease.OwnerSubjectID, plan.ApprovalID)
	if e != nil {
		return DispatchRequest{}, e
	}
	if !receipt.Committed {
		return DispatchRequest{}, ErrCommitUnknown
	}
	row, e := queryReceipt(ctx, s.DB, plan.ApprovalID)
	if e != nil {
		return DispatchRequest{}, e
	}
	expected := d.BarrierRequest
	expected.Operation = "CANDIDATE_REPAIR_APPROVE"
	expected.CommandKey = plan.ApprovalID
	expected.CommitKey = plan.ApprovalID
	_, expected.PayloadHash, e = harnessPayload(plan.Original)
	if e != nil {
		return DispatchRequest{}, e
	}
	if !row.matches(expected) {
		return DispatchRequest{}, ErrConflict
	}
	return candidateRepairDispatch(d, plan), nil
}
func candidateRepairDispatch(d DispatchRequest, p candidateRepairPlan) DispatchRequest {
	d.Slot = 1
	d.AttemptID = p.AttemptID
	d.candidateAttempt = p.AttemptID
	d.AuthorizationID = turnIdentity(d.AttemptID, "authorization")
	d.RequestKey = turnIdentity(d.AttemptID, "request")
	d.RequestHash = mustArrayHash(p.Binding.RequestHash)
	d.Route = p.Binding.RouteHash
	d.CommandKey = turnIdentity(d.AttemptID, "authorize")
	d.CommitKey = d.CommandKey
	d.harnessRecoveryAttempt = d.AttemptID
	for i := range d.Reservations {
		d.Reservations[i].ReservationID = turnIdentity(d.AttemptID, "supplier", d.Reservations[i].Kind)
	}
	return d
}

// Only the registered Files owner can supply a live, locked dependency.
func (s *Store) WithCandidateOriginalEvidence(d DispatchRequest, e OriginalResultEvidence) (DispatchRequest, error) {
	if !d.candidateExecution || d.Slot != 1 || s.DraftCandidateReader == nil {
		return d, ErrDenied
	}
	_, m, err := s.DraftCandidateReader.CandidateOriginalBytes(e, d.ProductScope, d.TaskID, turnIdentity(d.TaskID, "candidate-attempt", "0"))
	if err != nil {
		return d, err
	}
	d.recoveryEvidence = e
	d.Locks = append(d.Locks, LockKey{Rank: 60, IDs: []string{d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, m.Identity.AttemptID, m.Identity.AuthorizationID}}, LockKey{Rank: 90, Kind: "asset", IDs: []string{d.ProductScope.TenantID, d.ProductScope.ProductID, "staging." + m.BodyStorage}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{m.Identity.MarkerKey}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(m.Identity.AttemptID, "candidate-validation")}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(d.TaskID, "candidate-recovery")}})
	return d, nil
}
func (s *Store) prepareCandidateRecoveryDispatch(ctx context.Context, d *DispatchRequest) (func(), error) {
	if s.DraftCandidateReader == nil {
		return nil, ErrDenied
	}
	original, e := s.CandidateOriginalDispatch(ctx, *d)
	if e != nil {
		return nil, e
	}
	ev, e := s.DraftCandidateReader.ReadPendingCandidateEvidence(ctx, original)
	if e != nil {
		return nil, e
	}
	attached, e := s.WithCandidateOriginalEvidence(*d, ev)
	if e != nil {
		ev.Close()
		return nil, e
	}
	*d = attached
	return ev.Close, nil
}
func buildCandidateRepair(base domain.DraftCandidateMaterial, body []byte, meta domain.MockResultMetadata) (domain.DraftCandidateMaterial, error) {
	if !utf8.Valid(body) || len(body) == 0 {
		return base, ErrDenied
	}
	var prompt map[string]json.RawMessage
	if json.Unmarshal(base.Canonical, &prompt) != nil {
		return base, ErrDenied
	}
	recovery, e := json.Marshal(struct{ Reason, OriginalAttempt, OriginalHash, OriginalBody string }{"STRUCTURE_REPAIR", meta.Identity.AttemptID, meta.BodyHash, string(body)})
	if e != nil {
		return base, e
	}
	prompt["Recovery"] = recovery
	base.Canonical, e = json.Marshal(prompt)
	if e != nil {
		return base, e
	}
	if uint64(utf8.RuneCount(base.Canonical)) > base.InputBound || base.InputBound > MockInputLimit {
		return base, ErrDenied
	}
	hash := sha256.Sum256(base.Canonical)
	base.RequestHash = hex.EncodeToString(hash[:])
	return base, nil
}
func (s *Store) candidateRepairReasonTx(ctx context.Context, u *unit, r BarrierRequest, base domain.DraftCandidateMaterial, plan candidateRepairPlan) (domain.DraftCandidateMaterial, error) {
	if s.DraftCandidateReader == nil || r.recoveryEvidence == nil || plan.Reason != "STRUCTURE_REPAIR" || plan.AttemptID != turnIdentity(r.TaskID, "candidate-attempt", "1") || plan.ApprovalID != turnIdentity(r.TaskID, "candidate-recovery") {
		return base, ErrDenied
	}
	body, meta, e := s.DraftCandidateReader.CandidateOriginalBytes(r.recoveryEvidence, r.ProductScope, r.TaskID, turnIdentity(r.TaskID, "candidate-attempt", "0"))
	if e != nil {
		return base, e
	}
	if meta != plan.Original {
		return base, ErrDenied
	}
	original := DispatchRequest{BarrierRequest: r, AttemptID: meta.Identity.AttemptID, AuthorizationID: meta.Identity.AuthorizationID, RequestKey: meta.Identity.RequestKey, RequestHash: mustArrayHash(meta.Identity.RequestHash), Route: meta.Identity.RouteID, HarnessQuoteID: meta.Identity.QuoteID}
	original.candidateAttempt = original.AttemptID
	original.candidateOriginalRead = true
	original.recoveryEvidence = nil
	original.harnessRecoveryAttempt = ""
	original.CommandKey = meta.Identity.MarkerKey
	original.CommitKey = original.CommandKey
	original.phase = ""
	original.Operation = "CANDIDATE_DISPATCH"
	for _, a := range r0CandidateAccounts(r.ProductScope, base.ProjectID) {
		if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
			a.ReservationID = turnIdentity(original.AttemptID, "supplier", a.Kind)
			original.Reservations = append(original.Reservations, a)
		}
	}
	if r.originalQueryService == nil {
		return base, ErrDenied
	}
	if _, e = s.checkCandidateResultTx(ctx, u, original, meta.BodyStorage, meta.BodyHash, *r.originalQueryService); e != nil {
		return base, e
	}
	var queryRaw []byte
	var queryMeta domain.MockResultMetadata
	if e := u.tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, original.AttemptID).Scan(&queryRaw); e != nil {
		return base, e
	}
	if json.Unmarshal(queryRaw, &queryMeta) != nil || queryMeta != meta {
		return base, ErrDenied
	}
	var code, hash, key, version string
	var raw []byte
	e = u.tx.QueryRowContext(ctx, "SELECT validation_code,body_hash,receipt_key,validator_version,metadata FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, original.AttemptID).Scan(&code, &hash, &key, &version, &raw)
	if e != nil {
		return base, e
	}
	var known domain.MockResultMetadata
	if json.Unmarshal(raw, &known) != nil || known != meta || code != "STRUCTURE_INVALID" || hash != meta.BodyHash || version != "R0_DRAFT_FIELDS_V1" || key != turnIdentity(original.AttemptID, "candidate-validation") {
		return base, ErrDenied
	}
	receipt, e := queryReceipt(ctx, u.tx, key)
	if e != nil {
		return base, e
	}
	expected := r
	expected.Operation = "CANDIDATE_RESULT_VALIDATE"
	expected.phase = ""
	expected.CommitKey = key
	expected.CommandKey = key
	_, expected.PayloadHash, e = harnessPayload(struct {
		Metadata domain.MockResultMetadata
		Program  string
	}{meta, "R0_DRAFT_FIELDS_V1"})
	if e != nil {
		return base, e
	}
	if !receipt.matches(expected) {
		return base, ErrConflict
	}
	current, e := s.DraftCandidateOwner.ValidateCandidateTx(ctx, u, r.ProductScope, base.Selection, body)
	if e != nil {
		return base, e
	}
	if current != code {
		return base, ErrDenied
	}
	return buildCandidateRepair(base, body, meta)
}
func (s *Store) candidateRepairMaterialTx(ctx context.Context, u *unit, r BarrierRequest, base domain.DraftCandidateMaterial) (domain.DraftCandidateMaterial, error) {
	var raw []byte
	var p candidateRepairPlan
	e := u.tx.QueryRowContext(ctx, "SELECT document FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.candidateAttempt).Scan(&raw)
	if e != nil {
		return base, e
	}
	if json.Unmarshal(raw, &p) != nil {
		return base, ErrDenied
	}
	proof := base
	proof.Canonical = nil
	if !sameCandidateBinding(proof, p.Base) {
		return base, ErrConflict
	}
	receipt, e := queryReceipt(ctx, u.tx, p.ApprovalID)
	if e != nil {
		return base, e
	}
	expected := r
	expected.phase = ""
	expected.Operation = "CANDIDATE_REPAIR_APPROVE"
	expected.CommitKey = p.ApprovalID
	expected.CommandKey = p.ApprovalID
	_, expected.PayloadHash, e = harnessPayload(p.Original)
	if e != nil {
		return base, e
	}
	if !receipt.matches(expected) {
		return base, ErrConflict
	}
	out, e := s.candidateRepairReasonTx(ctx, u, r, base, p)
	if e != nil {
		return out, e
	}
	proof = out
	proof.Canonical = nil
	if !sameCandidateBinding(proof, p.Binding) {
		return out, ErrConflict
	}
	return out, nil
}
func (s *Store) PrepareDraftCandidateRecovery(ctx context.Context, lease domain.TaskLease, worker string) (DispatchRequest, error) {
	var zero DispatchRequest
	if s.DraftCandidateReader == nil {
		return zero, ErrDenied
	}
	d, e := s.candidateOriginalDispatch(ctx, lease, worker, false)
	if e != nil {
		return zero, e
	}
	d.CommandKey = candidateSendKey(d)
	d.CommitKey = d.CommandKey
	ev, e := s.DraftCandidateReader.ReadPendingCandidateEvidence(ctx, d)
	if e != nil {
		return zero, e
	}
	defer ev.Close()
	meta := ev.Metadata()
	r := candidateResultBarrier(d, meta.BodyStorage, meta.BodyHash)
	p := candidateRepairPlan{ApprovalID: turnIdentity(d.TaskID, "candidate-recovery"), AttemptID: turnIdentity(d.TaskID, "candidate-attempt", "1"), Reason: "STRUCTURE_REPAIR", Original: meta}
	attached := d
	attached.Slot = 1
	attached.AttemptID = p.AttemptID
	attached, e = s.WithCandidateOriginalEvidence(attached, ev)
	if e != nil {
		return zero, e
	}
	r.Locks = append(r.Locks, attached.Locks...)
	r.recoveryEvidence = ev
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, p.AttemptID, turnIdentity(p.AttemptID, "authorization")}})
	ctx, service, release, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return zero, e
	}
	defer release()
	r.originalQueryService = service
	r.candidateRepairApproval = &p
	r.Operation = "CANDIDATE_REPAIR_APPROVE"
	r.CommandKey = p.ApprovalID
	r.CommitKey = p.ApprovalID
	_, r.PayloadHash, e = harnessPayload(meta)
	if e != nil {
		return zero, e
	}
	_, replay, e := s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		var exists bool
		if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return ErrDenied
		}
		base, e := s.candidateMaterialTx(ctx, u, r)
		if e != nil {
			return e
		}
		extra, e := s.candidateRepairReasonTx(ctx, u, r, base, p)
		if e != nil {
			return e
		}
		base.Canonical = nil
		extra.Canonical = nil
		p.Base = base
		p.Binding = extra
		raw, e := json.Marshal(p)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_candidate_recoveries VALUES(?,?,?,?,?,?,?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, p.AttemptID, raw, p.ApprovalID); e != nil {
			return e
		}
		return advanceTurn(ctx, u, "EXTRA_APPROVED")
	})
	if e != nil {
		return zero, e
	}
	if replay {
		return zero, ErrDenied
	}
	return candidateRepairDispatch(d, p), nil
}
