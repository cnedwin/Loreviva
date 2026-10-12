package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

// PrepareTurnRecovery is the sole program-owned extra-slot decision. It accepts
// no reason, completion flag, body, account or authorization from a caller.
func (s *Store) PrepareTurnRecovery(ctx context.Context, lease domain.TaskLease, worker string) (DispatchRequest, error) {
	var zero DispatchRequest
	if s.GenerationContext == nil || s.CandidateOwner == nil || s.OriginalResultReader == nil || s.OriginalResultProgram == nil {
		return zero, ErrDenied
	}
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return zero, e
	}
	defer done()
	evidence, e := s.OriginalResultReader.ReadOriginalEvidence(ctx, lease, worker)
	if e != nil {
		return zero, e
	}
	defer evidence.Close()
	meta := evidence.Metadata()
	r, e := s.GenerationContext.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return zero, e
	}
	ctx, service, release, e := s.turnQueryAdmission(ctx, DispatchRequest{BarrierRequest: r})
	if e != nil {
		return zero, e
	}
	defer release()
	if service == nil {
		return zero, ErrDenied
	}
	r.originalQueryService = service
	r.recoveryEvidence = evidence
	if e = s.prepareOriginalResultBarrier(ctx, &r, meta.Identity.AttemptID); e != nil {
		return zero, e
	}
	plan := domain.RecoveryPlan{AttemptID: turnIdentity(r.TaskID, "attempt", "1"), ApprovalID: turnIdentity(r.TaskID, "recovery"), Original: meta}
	r.Operation = "HARNESS_RECOVERY_APPROVE"
	r.CommandKey = plan.ApprovalID
	r.CommitKey = r.CommandKey
	if e = s.planRecoveryEvidence(ctx, &r, plan, evidence); e != nil {
		return zero, e
	}
	_, r.PayloadHash, e = harnessPayload(meta)
	if e != nil {
		return zero, e
	}
	_, replay, e := s.barrier(ctx, r, func(tx Tx) error {
		var exists bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return ErrDenied
		}
		if e := s.checkCandidateRoundAuthority(ctx, tx, r, meta); e != nil {
			return e
		}
		if e := s.recoveryReasonTx(ctx, tx, r, evidence, &plan, true); e != nil {
			return e
		}
		_, snapshot, e := s.GenerationContext.BuildRecoveryContextTx(ctx, tx, *r.WorldScope, r.TaskID, plan, evidence)
		if e != nil {
			return e
		}
		plan = *snapshot.Recovery
		raw, e := json.Marshal(plan)
		if e != nil {
			return e
		}
		u := tx.(*unit)
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_recovery_approvals VALUES(?,?,?,?,?,?,?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, plan.AttemptID, raw, r.CommitKey); e != nil {
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
	return s.recoveryDispatch(ctx, r, plan)
}

func (s *Store) planRecoveryEvidence(ctx context.Context, r *BarrierRequest, plan domain.RecoveryPlan, evidence OriginalResultEvidence) error {
	if r.WorldScope == nil || s.OriginalResultReader == nil || s.OriginalResultProgram == nil || s.CandidateOwner == nil {
		return ErrDenied
	}
	t, p := r.ProductScope.TenantID, r.ProductScope.ProductID
	r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{plan.ApprovalID}})
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{t, p, r.TaskID, plan.Original.Identity.AttemptID, plan.Original.Identity.AuthorizationID}}, LockKey{Rank: 60, IDs: []string{t, p, r.TaskID, plan.AttemptID, turnIdentity(plan.AttemptID, "authorization")}})
	body, meta, e := s.OriginalResultReader.RecoveryOriginalBytes(evidence, *r.WorldScope, r.TaskID, plan.Original.Identity.AttemptID)
	if e != nil {
		return e
	}
	if meta != plan.Original {
		return ErrDenied
	}
	if s.OriginalResultProgram.ValidateOriginalCandidate(body) == "STRUCTURE_VALID" {
		keys, e := s.CandidateOwner.PlanOriginalCandidate(ctx, *r.WorldScope, r.TaskID, turnIdentity(r.TaskID, "result"), body)
		if e != nil {
			return e
		}
		r.Locks = append(r.Locks, keys...)
	}
	return nil
}

func (s *Store) recoveryReasonTx(ctx context.Context, tx Tx, r BarrierRequest, evidence OriginalResultEvidence, plan *domain.RecoveryPlan, choose bool) error {
	meta := evidence.Metadata()
	if meta != plan.Original || meta.Identity.AttemptID == plan.AttemptID {
		return ErrDenied
	}
	var code, hash string
	if e := tx.QueryRowContext(ctx, "SELECT validation_code,body_hash FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&code, &hash); e != nil {
		return e
	}
	if code != s.OriginalResultProgram.ValidateOriginalCandidate(evidence.Bytes()) || hash != meta.BodyHash {
		return ErrDenied
	}
	var pending, committed bool
	if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND state='PENDING'),EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&pending, &committed); e != nil {
		return e
	}
	if pending {
		return ErrCommitUnknown
	}
	if committed {
		return ErrDenied
	}
	reason := "STRUCTURE_REPAIR"
	if code == "STRUCTURE_VALID" {
		reason = "STATE_CONFLICT_RECOMPUTE"
		var phase, state, why, bodyHash, key string
		var round uint64
		var raw []byte
		// WORLD_COMMIT is later than APPROVAL; only the latest round of that phase counts.
		if e := tx.QueryRowContext(ctx, "SELECT phase,round_number,state,reason,body_hash,original_receipt_key,conflict_document FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? ORDER BY phase DESC,round_number DESC LIMIT 1", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&phase, &round, &state, &why, &bodyHash, &key, &raw); e != nil {
			return e
		}
		if state != "ROLLED_BACK" || why != "RESOURCE_CONFLICT" || bodyHash != meta.BodyHash {
			return ErrDenied
		}
		if _, e := queryReceipt(ctx, tx.(*unit).tx, key); !errors.Is(e, sql.ErrNoRows) {
			if e != nil {
				return e
			}
			return ErrConflict
		}
		var old CandidateResourceConflict
		if json.Unmarshal(raw, &old) != nil {
			return ErrDenied
		}
		if e := s.CandidateOwner.ValidateOriginalRecoveryCandidateTx(ctx, tx, *r.WorldScope, r.TaskID, meta.Identity.AttemptID, evidence.Bytes()); e != nil {
			return e
		}
		_, e := s.CandidateOwner.EvaluateOriginalCandidateTx(ctx, tx, *r.WorldScope, r.TaskID, meta.Identity.AttemptID, evidence.Bytes())
		var current *CandidateResourceConflict
		if !errors.As(e, &current) || *current != old {
			return ErrDenied
		}
		if choose {
			plan.OriginalPhase = phase
			plan.OriginalRound = round
		} else if plan.OriginalPhase != phase || plan.OriginalRound != round {
			return ErrConflict
		}
	} else if code != "STRUCTURE_INVALID" {
		return ErrDenied
	}
	if choose {
		plan.Reason = reason
	} else if plan.Reason != reason {
		return ErrConflict
	}
	return nil
}

func (s *Store) recoveryDispatch(ctx context.Context, r BarrierRequest, plan domain.RecoveryPlan) (DispatchRequest, error) {
	d, e := s.OriginalTurnDispatch(ctx, r)
	if e != nil {
		return d, e
	}
	d.Slot = 1
	d.AttemptID = plan.AttemptID
	d.AuthorizationID = turnIdentity(plan.AttemptID, "authorization")
	d.RequestKey = turnIdentity(plan.AttemptID, "request")
	d.Route = plan.Binding.RouteID
	h, e := hex.DecodeString(plan.Binding.RequestHash)
	if e != nil || len(h) != 32 {
		return DispatchRequest{}, ErrDenied
	}
	copy(d.RequestHash[:], h)
	d.CommandKey = turnIdentity(plan.AttemptID, "authorize")
	d.CommitKey = d.CommandKey
	d.recoveryEvidence = nil
	d.harnessOriginalResultAttempt = ""
	d.harnessRecoveryAttempt = plan.AttemptID
	for i := range d.Reservations {
		d.Reservations[i].ReservationID = turnIdentity(plan.AttemptID, "supplier", d.Reservations[i].Kind)
	}
	return d, nil
}

// Reacquire real original0 bytes at each purchase boundary, outside all DB locks.
func (s *Store) prepareRecoveryDispatch(ctx context.Context, d *DispatchRequest) (func(), error) {
	if d.Slot == 0 {
		if d.candidateExecution {
			evidence, e := s.candidateOriginalSources(ctx, *d)
			if e != nil {
				return nil, e
			}
			return evidence.Close, nil
		}
		return func() {}, nil
	}
	if d.candidateExecution {
		return s.prepareCandidateRecoveryDispatch(ctx, d)
	}
	if d.Slot != 1 || d.WorldScope == nil || s.GenerationContext == nil || s.OriginalResultReader == nil || s.CandidateOwner == nil || s.OriginalResultProgram == nil {
		return nil, ErrDenied
	}
	var raw []byte
	var plan domain.RecoveryPlan
	if e := s.DB.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &plan) != nil || plan.AttemptID != d.AttemptID || plan.Binding.RequestHash != hex.EncodeToString(d.RequestHash[:]) || plan.Binding.RouteID != d.Route {
		return nil, ErrDenied
	}
	expected, e := s.recoveryDispatch(ctx, d.BarrierRequest, plan)
	if e != nil {
		return nil, e
	}
	if d.AuthorizationID != expected.AuthorizationID || d.RequestKey != expected.RequestKey || d.HarnessQuoteID != expected.HarnessQuoteID || d.Route != expected.Route || !reflect.DeepEqual(d.Reservations, expected.Reservations) {
		return nil, ErrDenied
	}
	lease := domain.TaskLease{ProductScope: d.ProductScope, WorldInstanceID: &d.WorldScope.WorldInstanceID, OwnerSubjectID: d.OwnerSubjectID, TaskID: d.TaskID, Fence: d.Fence}
	evidence, e := s.OriginalResultReader.ReadOriginalEvidence(ctx, lease, d.WorkerID)
	if e != nil {
		return nil, e
	}
	d.recoveryEvidence = evidence
	d.harnessRecoveryAttempt = d.AttemptID
	d.harnessOriginalResultAttempt = ""
	if e = s.planRecoveryEvidence(ctx, &d.BarrierRequest, plan, evidence); e != nil {
		evidence.Close()
		return nil, e
	}
	return evidence.Close, nil
}

func (s *Store) checkRecoveryDispatchTx(ctx context.Context, tx Tx, r BarrierRequest) (domain.GenerationMaterial, domain.OriginalContextSnapshot, error) {
	var material domain.GenerationMaterial
	var snapshot domain.OriginalContextSnapshot
	var raw []byte
	var plan domain.RecoveryPlan
	if r.recoveryEvidence == nil {
		return material, snapshot, ErrDenied
	}
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.harnessRecoveryAttempt).Scan(&raw); e != nil {
		return material, snapshot, e
	}
	if json.Unmarshal(raw, &plan) != nil {
		return material, snapshot, ErrDenied
	}
	receipt, e := queryReceipt(ctx, tx.(*unit).tx, plan.ApprovalID)
	if e != nil {
		return material, snapshot, e
	}
	expected := r
	expected.Operation = "HARNESS_RECOVERY_APPROVE"
	expected.phase = ""
	expected.CommandKey = plan.ApprovalID
	expected.CommitKey = plan.ApprovalID
	_, expected.PayloadHash, e = harnessPayload(plan.Original)
	if e != nil {
		return material, snapshot, e
	}
	if !receipt.matches(expected) {
		return material, snapshot, ErrConflict
	}
	if e := s.GenerationContext.ValidateResultSourcesTx(ctx, tx, *r.WorldScope, r.TaskID, plan.Original, nil); e != nil {
		return material, snapshot, e
	}
	if e := s.recoveryReasonTx(ctx, tx, r, r.recoveryEvidence, &plan, false); e != nil {
		return material, snapshot, e
	}
	material, snapshot, e = s.GenerationContext.BuildRecoveryContextTx(ctx, tx, *r.WorldScope, r.TaskID, plan, r.recoveryEvidence)
	if e != nil {
		return material, snapshot, e
	}
	if *snapshot.Recovery != plan {
		return material, snapshot, ErrConflict
	}
	return material, snapshot, nil
}

func checkActiveCandidate(ctx context.Context, tx Tx, r BarrierRequest, attempt string) error {
	var active string
	e := tx.QueryRowContext(ctx, "SELECT extra_attempt_id FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&active)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if active != attempt {
		return ErrDenied
	}
	return nil
}

// RecoveryTurnDispatch is an exact metadata lookup. Every send or narrative
// continuation still enters its own current owning barrier and real byte reader.
func (s *Store) RecoveryTurnDispatch(ctx context.Context, lease domain.TaskLease, worker string) (DispatchRequest, error) {
	if s.GenerationContext == nil {
		return DispatchRequest{}, ErrDenied
	}
	r, e := s.GenerationContext.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return DispatchRequest{}, e
	}
	var raw []byte
	var plan domain.RecoveryPlan
	if e = s.DB.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&raw); e != nil {
		return DispatchRequest{}, e
	}
	if json.Unmarshal(raw, &plan) != nil || plan.Original.Identity.Scope != *r.WorldScope || plan.Original.Identity.TaskID != r.TaskID {
		return DispatchRequest{}, ErrDenied
	}
	receipt, e := s.QueryCommitScoped(ctx, r.ProductScope, r.WorldScope, r.OwnerSubjectID, plan.ApprovalID)
	if e != nil {
		return DispatchRequest{}, e
	}
	if !receipt.Committed {
		return DispatchRequest{}, ErrCommitUnknown
	}
	if e = s.prepareOriginalResultBarrier(ctx, &r, plan.Original.Identity.AttemptID); e != nil {
		return DispatchRequest{}, e
	}
	return s.recoveryDispatch(ctx, r, plan)
}

// Completed original semantics remain a real governed dependency. Purchased
// conflict need not remain present; only the original frozen-world semantics are
// repeated here, never the purchase decision or a new call allocation.
func (s *Store) checkRecoveryDependencyTx(ctx context.Context, tx Tx, r BarrierRequest) error {
	var raw []byte
	var plan domain.RecoveryPlan
	e := tx.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.harnessOriginalResultAttempt).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if json.Unmarshal(raw, &plan) != nil || s.OriginalResultReader == nil || s.OriginalResultProgram == nil || s.CandidateOwner == nil {
		return ErrDenied
	}
	body, meta, e := s.OriginalResultReader.RecoveryOriginalBytes(r.recoveryEvidence, *r.WorldScope, r.TaskID, plan.Original.Identity.AttemptID)
	if e != nil {
		return e
	}
	if meta != plan.Original {
		return ErrDenied
	}
	code := s.OriginalResultProgram.ValidateOriginalCandidate(body)
	if plan.Reason == "STRUCTURE_REPAIR" && code == "STRUCTURE_INVALID" {
		return nil
	}
	if plan.Reason != "STATE_CONFLICT_RECOMPUTE" || code != "STRUCTURE_VALID" {
		return ErrDenied
	}
	return s.CandidateOwner.ValidateOriginalRecoveryCandidateTx(ctx, tx, *r.WorldScope, r.TaskID, plan.Original.Identity.AttemptID, body)
}
