package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// CandidateService is only installed with the real local executor. Qualification,
// sources and budgets are revalidated by the fixed Store owning intake.
type CandidateService struct{ Executor *Executor }

func (x *Executor) CandidateReady() bool {
	return x != nil && x.Turns != nil && x.Turns.Store != nil && x.Files != nil && x.Journal != nil && x.Participant != nil
}

func (s *CandidateService) Submit(ctx context.Context, scope domain.ProductScope, draft, key string, request generated.CandidateRequest) (generated.AdminTaskReceipt, error) {
	if s == nil || !s.Executor.CandidateReady() {
		return generated.AdminTaskReceipt{}, store.ErrDenied
	}
	return s.Executor.Turns.Store.AcceptDraftCandidate(ctx, scope, draft, key, request)
}

// ExecuteDraftCandidate never issues a permit for an existing original marker.
// Each round is one of the two immutable attempts in the original envelope.
func (x *Executor) ExecuteDraftCandidate(ctx context.Context, lease domain.TaskLease, worker string) (err error) {
	phase := "admission"
	defer func() {
		if err != nil {
			err = fmt.Errorf("CANDIDATE %s: %w", phase, err)
		}
	}()
	if !x.CandidateReady() || lease.WorldInstanceID != nil {
		return store.ErrDenied
	}
	ctx, release, e := x.Participant.Enter(ctx, maintenance.HarnessWriter)
	if e != nil {
		return e
	}
	defer release()
	s := x.Turns.Store
	ctx, e = s.EnterDraftTaskIssuer(ctx, lease, worker)
	if e != nil {
		return e
	}
	if _, e = s.QueryDraftCandidateCommit(ctx, lease.ProductScope, lease.TaskID); e == nil {
		return nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	for round := 0; round < 2; round++ {
		phase = "active original"
		d, e := s.ActiveDraftCandidateDispatch(ctx, lease, worker)
		if e != nil {
			return e
		}
		id, e := s.CandidateAttemptMockIdentity(ctx, lease.ProductScope, lease.TaskID, d.AttemptID)
		if e == nil {
			phase = "original query"
			meta, err := x.originalResult(ctx, id)
			if err != nil {
				return err
			}
			if meta.State != "SUCCEEDED" {
				return ErrMockReplyLost
			}
		} else if errors.Is(e, sql.ErrNoRows) {
			phase = "authorize"
			if _, e = s.AuthorizeDispatch(ctx, d); e != nil {
				return e
			}
			phase = "send marker"
			d, e = s.PrepareDraftCandidateSend(ctx, lease, worker)
			if e != nil {
				return e
			}
			a, e := s.StartSend(ctx, d, d.AttemptID)
			if e != nil {
				return e
			}
			if !a.CanSend || a.OriginalSend() == nil {
				// A concurrent/uncertain original marker cannot mint a new permit.
				return ErrMockReplyLost
			}
			phase = "generate"
			if e = x.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				return e
			}
			if x.Fault == MockDropOriginalReply {
				return ErrMockReplyLost
			}
			phase = "original reply"
			if _, e = s.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				return e
			}
		} else {
			return e
		}
		phase = "validation"
		if e = s.ValidateDraftCandidate(ctx, lease, worker); e != nil {
			return e
		}
		code, e := s.ReadDraftCandidateValidation(ctx, lease.ProductScope, lease.TaskID, d.AttemptID)
		if e != nil {
			return e
		}
		if code == "STRUCTURE_INVALID" && d.Slot == 0 {
			phase = "repair approval"
			if _, e = s.PrepareDraftCandidateRecovery(ctx, lease, worker); e != nil {
				return e
			}
			continue
		}
		if code != "STRUCTURE_VALID" {
			return store.ErrDenied
		}
		phase = "business commit"
		_, e = s.CommitDraftCandidate(ctx, lease, worker)
		return e
	}
	return store.ErrDenied
}
