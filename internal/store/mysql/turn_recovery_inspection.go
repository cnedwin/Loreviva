package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

var errRecoveryCandidateInspected = errors.New("complete original recovery candidate inspected")

// InspectOriginalRecoveryCandidate checks the complete original semantics only.
// It does not approve an extra slot, change FSM, or authorize a world commit.
func (s *Store) InspectOriginalRecoveryCandidate(ctx context.Context, lease domain.TaskLease, worker string) error {
	if s.CandidateOwner == nil || s.OriginalResultReader == nil || s.GenerationContext == nil {
		return ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer release()
	evidence, e := s.OriginalResultReader.ReadOriginalEvidence(ctx, lease, worker)
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
	ctx, service, done, e := s.turnQueryAdmission(ctx, DispatchRequest{BarrierRequest: r})
	if e != nil {
		return e
	}
	defer done()
	if service == nil {
		return ErrDenied
	}
	r.originalQueryService = service
	if e = s.prepareOriginalResultBarrier(ctx, &r, meta.Identity.AttemptID); e != nil {
		return e
	}
	locks, e := s.CandidateOwner.PlanOriginalCandidate(ctx, *r.WorldScope, r.TaskID, turnIdentity(r.TaskID, "result"), body)
	if e != nil {
		return e
	}
	r.Locks = append(r.Locks, locks...)
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID}})
	r.Operation = "HARNESS_RECOVERY_INSPECT"
	r.CommandKey = turnIdentity(meta.Identity.AttemptID, "recovery-inspect")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(meta)
	if e != nil {
		return e
	}
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := s.checkCandidateRoundAuthority(ctx, tx, r, meta); e != nil {
			return e
		}
		if e := s.CandidateOwner.ValidateOriginalRecoveryCandidateTx(ctx, tx, *r.WorldScope, r.TaskID, meta.Identity.AttemptID, body); e != nil {
			return e
		}
		return errRecoveryCandidateInspected
	})
	if errors.Is(e, errRecoveryCandidateInspected) {
		return nil
	}
	if e == nil {
		return ErrDenied
	}
	return e
}
