package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

// A current relevant resource differs from the immutable actual sent snapshot.
// This classification alone does not authorize a new attempt.
var ErrCandidateResourceConflict = errors.New("candidate relevant resource changed since original input")

type OriginalCandidateOwner interface {
	EvaluateSkillActionsTx(context.Context, Tx, domain.Scope, string, string, []byte) ([]domain.CandidateResource, [32]byte, error)
	ValidateOriginalRecoveryCandidateTx(context.Context, Tx, domain.Scope, string, string, []byte) error
	PlanOriginalCandidate(context.Context, domain.Scope, string, string, []byte) ([]LockKey, error)
	EvaluateOriginalCandidateTx(context.Context, Tx, domain.Scope, string, string, []byte) ([]domain.CandidateResource, error)
	ApplyOriginalCandidateTx(context.Context, Tx, domain.Scope, string, string, string, domain.Versions, []byte) (domain.CandidateCommit, error)
}

func (s *Store) ApproveOriginalTurn(ctx context.Context, lease domain.TaskLease, worker string) error {
	if lease.WorldInstanceID == nil {
		return ErrDenied
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	id, e := s.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e != nil {
		return e
	}
	return s.ApproveAttemptTurn(ctx, lease, worker, id.AttemptID)
}
func (s *Store) ApproveAttemptTurn(ctx context.Context, lease domain.TaskLease, worker, attempt string) error {
	if s.CandidateOwner == nil || s.OriginalResultReader == nil || s.GenerationContext == nil {
		return ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer release()
	evidence, e := s.OriginalResultReader.ReadAttemptEvidence(ctx, lease, worker, attempt)
	if e != nil {
		return e
	}
	if evidence == nil {
		return ErrDenied
	}
	defer evidence.Close()
	body, meta := evidence.Bytes(), evidence.Metadata()
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != meta.BodyHash {
		return ErrConflict
	}
	r, e := s.GenerationContext.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return e
	}
	ctx, service, queryRelease, e := s.turnQueryAdmission(ctx, DispatchRequest{BarrierRequest: r})
	if e != nil {
		return e
	}
	defer queryRelease()
	if service == nil {
		return ErrDenied
	}
	r.originalQueryService = service
	r.recoveryEvidence = evidence
	if e = s.prepareOriginalResultBarrier(ctx, &r, meta.Identity.AttemptID); e != nil {
		return e
	}
	result := turnIdentity(r.TaskID, "result")
	locks, e := s.CandidateOwner.PlanOriginalCandidate(ctx, *r.WorldScope, r.TaskID, result, body)
	if e != nil {
		return e
	}
	r.Locks = append(r.Locks, locks...)
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID}})
	r.Operation = "HARNESS_CANDIDATE_APPROVE"
	r.CommandKey = turnIdentity(meta.Identity.AttemptID, "approval")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(meta)
	if e != nil {
		return e
	}
	r.localCandidateRound, e = s.prepareCandidateRound(ctx, r, meta, "APPROVAL")
	if e != nil {
		return e
	}
	if r.localCandidateRound == nil {
		return nil
	}
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := useCandidateRound(ctx, tx, r); e != nil {
			return e
		}
		if e := checkResultInstallation(ctx, tx, r, *service); e != nil {
			return e
		}
		if e := s.GenerationContext.ValidateResultSourcesTx(ctx, tx, *r.WorldScope, r.TaskID, meta, r.recoveryEvidence); e != nil {
			return e
		}
		var hash, code string
		if e := tx.QueryRowContext(ctx, "SELECT body_hash,validation_code FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&hash, &code); e != nil {
			return e
		}
		if hash != meta.BodyHash || code != "STRUCTURE_VALID" {
			return ErrDenied
		}
		resources, e := s.evaluateProgramSkills(ctx, tx.(*unit), r, meta, body)
		if e != nil {
			bindCandidateConflict(r, e)
			return e
		}
		approval := domain.CandidateApproval{Metadata: meta, Versions: *r.Versions, ResultID: result, Resources: resources}
		raw, e := json.Marshal(approval)
		if e != nil {
			return e
		}
		u := tx.(*unit)
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_candidate_approvals(tenant_id,product_id,task_id,attempt_id,document,receipt_key) VALUES(?,?,?,?,?,?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, raw, r.CommitKey); e != nil {
			return e
		}
		return advanceTurn(ctx, u, "APPROVED")
	})
	return s.recordCandidateRollback(ctx, r, meta, e)
}
