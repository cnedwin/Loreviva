package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

func (s *Store) PrepareDraftCandidateDispatch(ctx context.Context, lease domain.TaskLease, worker string) (DispatchRequest, error) {
	return s.candidateOriginalDispatch(ctx, lease, worker, true)
}
func (s *Store) candidateOriginalDispatch(ctx context.Context, lease domain.TaskLease, worker string, bind bool) (DispatchRequest, error) {
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return DispatchRequest{}, e
	}
	defer release()
	if lease.WorldInstanceID != nil || lease.SubjectID != lease.OwnerSubjectID || s.DraftCandidateOwner == nil {
		return DispatchRequest{}, ErrDenied
	}
	g, e := s.candidateGates(ctx, lease.ProductScope)
	if e != nil {
		return DispatchRequest{}, e
	}
	var raw []byte
	var quote string
	e = s.DB.QueryRowContext(ctx, "SELECT c.binding,e.quote_id FROM harness_admin_candidates c JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND c.owner_subject_id=?", lease.TenantID, lease.ProductID, lease.TaskID, lease.SubjectID).Scan(&raw, &quote)
	if e != nil {
		return DispatchRequest{}, e
	}
	var m domain.DraftCandidateMaterial
	if json.Unmarshal(raw, &m) != nil {
		return DispatchRequest{}, ErrDenied
	}
	key := turnIdentity(lease.TaskID, "candidate-context")
	r := BarrierRequest{ProductScope: lease.ProductScope, OwnerSubjectID: lease.OwnerSubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, GateVersions: g, Operation: "CANDIDATE_CONTEXT", CommandKey: key, CommitKey: key, candidateExecution: true}
	_, r.PayloadHash, e = harnessPayload(m)
	if e != nil {
		return DispatchRequest{}, e
	}
	if bind {
		_, _, e = s.barrier(ctx, r, func(tx Tx) error { return advanceTurn(ctx, tx.(*unit), "CONTEXT_BOUND") })
	}
	if e != nil {
		return DispatchRequest{}, e
	}
	attempt := turnIdentity(lease.TaskID, "candidate-attempt", "0")
	r.candidateAttempt = attempt
	r.Operation = "CANDIDATE_DISPATCH"
	r.CommandKey = turnIdentity(lease.TaskID, "candidate-authorize")
	r.CommitKey = r.CommandKey
	d := DispatchRequest{BarrierRequest: r, HarnessQuoteID: quote, AttemptID: attempt, AuthorizationID: turnIdentity(attempt, "authorization"), RequestKey: turnIdentity(attempt, "request"), Route: m.RouteHash}
	hash, e := hex.DecodeString(m.RequestHash)
	if e != nil || len(hash) != 32 {
		return DispatchRequest{}, ErrDenied
	}
	copy(d.RequestHash[:], hash)
	for _, a := range r0CandidateAccounts(lease.ProductScope, m.ProjectID) {
		if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
			a.ReservationID = turnIdentity(attempt, "supplier", a.Kind)
			d.Reservations = append(d.Reservations, a)
		}
	}
	return d, nil
}
func (s *Store) planCandidateExecution(ctx context.Context, r *BarrierRequest) error {
	if r.WorldScope != nil || r.commandOnly || !r.candidateExecution {
		return nil
	}
	if s.DraftCandidateOwner == nil {
		return ErrDenied
	}
	var raw []byte
	if e := s.DB.QueryRowContext(ctx, "SELECT selection FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.OwnerSubjectID).Scan(&raw); e != nil {
		return e
	}
	var selection domain.DraftCandidateSelection
	if json.Unmarshal(raw, &selection) != nil {
		return ErrDenied
	}
	keys, e := s.DraftCandidateOwner.PlanCandidateLocks(ctx, r.ProductScope, selection)
	if e != nil {
		return e
	}
	r.Locks = append(r.Locks, keys...)
	return nil
}
// Keep the original route policy and attempt identity, but rebuild current source material before use; returning a dispatch does not grant a new send permit.
func (s *Store) candidateMaterialTx(ctx context.Context, u *unit, r BarrierRequest) (domain.DraftCandidateMaterial, error) {
	var out domain.DraftCandidateMaterial
	if !r.candidateExecution || r.WorldScope != nil || r.commandOnly || s.DraftCandidateOwner == nil {
		return out, ErrDenied
	}
	var raw, selected []byte
	if e := u.tx.QueryRowContext(ctx, "SELECT c.binding,c.selection FROM harness_admin_candidates c JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND c.owner_subject_id=? AND t.task_kind='ADMIN' AND j.operation='CANDIDATE' AND t.world_instance_id IS NULL", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.OwnerSubjectID).Scan(&raw, &selected); e != nil {
		return out, e
	}
	var original domain.DraftCandidateMaterial
	var selection domain.DraftCandidateSelection
	if json.Unmarshal(raw, &original) != nil || json.Unmarshal(selected, &selection) != nil {
		return out, ErrDenied
	}
	u.routePolicyFrozen = true
	u.routePolicy = original.RoutePolicy
	out, e := s.DraftCandidateOwner.BuildCandidateTx(ctx, u, r.ProductScope, selection)
	if e != nil {
		return out, e
	}
	proof := out
	proof.Canonical = nil
	rebuilt, _ := json.Marshal(proof)
	originalBytes, _ := json.Marshal(original)
	if !bytes.Equal(originalBytes, rebuilt) {
		return out, ErrConflict
	}
	if r.candidateAttempt == turnIdentity(r.TaskID, "candidate-attempt", "1") {
		out, e = s.candidateRepairMaterialTx(ctx, u, r, out)
		if e != nil {
			return out, e
		}
	} else if !r.candidateOriginalRead {
		var extra string
		e := u.tx.QueryRowContext(ctx, "SELECT extra_attempt_id FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&extra)
		if e == nil {
			return out, ErrDenied
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
	}
	if r.phase == "AUTHORIZE" || r.phase == "SEND" {
		if r.harnessDispatchHash != mustArrayHash(out.RequestHash) || r.harnessDispatchRoute != out.RouteHash {
			return out, ErrConflict
		}
	}
	return out, nil
}
func mustArrayHash(s string) (out [32]byte) { copy(out[:], mustHash(s)); return }
