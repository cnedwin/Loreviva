package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

var errOriginalResultInspected = errors.New("original result inspected; rollback read transaction")

// InspectOriginalResult performs only a fixed original-result authority check.
// The sentinel rolls back temporary command/lock rows, with no receipt or FSM write.
func (s *Store) InspectOriginalResult(ctx context.Context, r BarrierRequest) (domain.MockResultMetadata, error) {
	if r.WorldScope == nil {
		return domain.MockResultMetadata{}, ErrDenied
	}
	id, e := s.OriginalMockIdentity(ctx, *r.WorldScope, r.TaskID)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	return s.InspectAttemptResult(ctx, r, id.AttemptID)
}
func (s *Store) InspectAttemptResult(ctx context.Context, r BarrierRequest, attempt string) (domain.MockResultMetadata, error) {
	return s.InspectAttemptResultWithOriginal(ctx, r, attempt, nil)
}
func (s *Store) InspectAttemptResultWithOriginal(ctx context.Context, r BarrierRequest, attempt string, evidence OriginalResultEvidence) (domain.MockResultMetadata, error) {
	r.recoveryEvidence = evidence
	var out domain.MockResultMetadata
	if r.WorldScope == nil || s.GenerationContext == nil || s.ProviderEvidence == nil {
		return out, ErrDenied
	}
	ctx, service, release, e := s.turnQueryAdmission(ctx, DispatchRequest{BarrierRequest: r})
	if e != nil {
		return out, e
	}
	defer release()
	if service == nil {
		return out, ErrDenied
	}
	r.originalQueryService = service
	id, e := s.AttemptMockIdentity(ctx, *r.WorldScope, r.TaskID, attempt)
	if e != nil {
		return out, e
	}
	if e = s.prepareOriginalResultBarrier(ctx, &r, id.AttemptID); e != nil {
		return out, e
	}
	r.Operation = "HARNESS_RESULT_READ"
	r.CommandKey = turnIdentity(id.AttemptID, "read", r.WorkerID)
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, _ = harnessPayload(id)
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, id.AttemptID, id.AuthorizationID}})
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := checkResultInstallation(ctx, tx, r, *service); e != nil {
			return e
		}
		var raw []byte
		var valid bool
		if e := tx.QueryRowContext(ctx, "SELECT q.metadata,(a.state='SUCCEEDED' AND a.send_started_at IS NOT NULL AND a.marker_commit_key=? AND a.request_key=? AND a.request_hash=? AND a.route=? AND e.state='ACTIVE' AND e.execute_by>UTC_TIMESTAMP(6)) FROM harness_original_queries q JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE q.tenant_id=? AND q.product_id=? AND q.task_id=? AND q.attempt_id=?", id.MarkerKey, id.RequestKey, mustHash(id.RequestHash), id.RouteID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, id.AttemptID).Scan(&raw, &valid); e != nil {
			return e
		}
		if !valid || json.Unmarshal(raw, &out) != nil || out.Identity != id || out.State != "SUCCEEDED" || !out.BodyAvailable || !validOriginalUsageMetadata(minimalOriginalMetadata(out)) || out.BodyStorage == "" || len(mustHash(out.BodyHash)) != 32 {
			return ErrDenied
		}
		if e := s.GenerationContext.ValidateResultSourcesTx(ctx, tx, *r.WorldScope, r.TaskID, out, r.recoveryEvidence); e != nil {
			return e
		}
		return errOriginalResultInspected
	})
	if !errors.Is(e, errOriginalResultInspected) {
		if e == nil {
			e = ErrDenied
		}
		return domain.MockResultMetadata{}, e
	}
	// Independent provider freshness is checked outside every database lock.
	live, e := s.ProviderEvidence.OriginalMetadata(ctx, id)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	if live != out {
		return domain.MockResultMetadata{}, ErrConflict
	}
	return out, nil
}

func checkResultInstallation(ctx context.Context, tx Tx, r BarrierRequest, service domain.QueryServiceIdentity) error {
	var valid bool
	if e := tx.QueryRowContext(ctx, "SELECT installation_id=? AND service_id=? AND policy=? FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", service.Installation, service.ServiceID, service.Policy, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.harnessOriginalResultAttempt).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}
