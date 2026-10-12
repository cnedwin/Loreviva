package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"loreviva.local/loreviva/internal/domain"
)

// CandidateResourceConflict is emitted only by the registered owning semantic
// evaluator while reading the locked actual resource and immutable sent input.
// It is classification, never a permit or proof that a transaction ended.
type CandidateResourceConflict struct{ Kind, ID, OriginalHash, CurrentHash string }

func (e *CandidateResourceConflict) Error() string { return ErrCandidateResourceConflict.Error() }
func (e *CandidateResourceConflict) Unwrap() error { return ErrCandidateResourceConflict }

// The permit never crosses the fixed Store operation. A restart or an unknown
// prepare ACK cannot reconstruct it from the durable PENDING row.
type candidateRound struct {
	phase, attempt  string
	number          uint64
	used            atomic.Bool
	commitAttempted bool
	conflict        *CandidateResourceConflict
}

// Only the fixed semantic call sites can bind conflict evidence to this exact
// transaction. A matching error from transport hooks is not semantic evidence.
func bindCandidateConflict(r BarrierRequest, cause error) {
	var conflict *CandidateResourceConflict
	if r.localCandidateRound != nil && errors.As(cause, &conflict) {
		copy := *conflict
		r.localCandidateRound.conflict = &copy
	}
}

type candidateRollback struct {
	round *candidateRound
	cause error
}

var errCandidateRoundCommitted = errors.New("original candidate phase receipt known")

func (e *candidateRollback) Error() string { return e.cause.Error() }
func (e *candidateRollback) Unwrap() error { return e.cause }

func candidatePhaseState(phase string) (string, string) {
	if phase == "APPROVAL" {
		return "VALIDATED", "LOCAL_APPROVAL_READY"
	}
	if phase == "WORLD_COMMIT" {
		return "APPROVED", "LOCAL_COMMIT_READY"
	}
	return "", ""
}

func (s *Store) checkCandidateRoundAuthority(ctx context.Context, tx Tx, r BarrierRequest, meta domain.MockResultMetadata) error {
	if r.originalQueryService == nil || s.GenerationContext == nil {
		return ErrDenied
	}
	if e := checkResultInstallation(ctx, tx, r, *r.originalQueryService); e != nil {
		return e
	}
	return s.GenerationContext.ValidateResultSourcesTx(ctx, tx, *r.WorldScope, r.TaskID, meta, r.recoveryEvidence)
}

func (s *Store) prepareCandidateRound(ctx context.Context, r BarrierRequest, meta domain.MockResultMetadata, phase string) (*candidateRound, error) {
	base, ready := candidatePhaseState(phase)
	if base == "" {
		return nil, ErrDenied
	}
	var previous uint64
	e := s.DB.QueryRowContext(ctx, "SELECT COALESCE(MAX(round_number),0) FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, phase).Scan(&previous)
	if e != nil {
		return nil, e
	}
	p := &candidateRound{phase: phase, attempt: meta.Identity.AttemptID, number: previous + 1}
	originalKey := r.CommitKey
	originalRequest := r
	r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{originalKey}})
	r.Operation = "HARNESS_LOCAL_PREPARE"
	r.CommandKey = turnIdentity(meta.Identity.AttemptID, phase, "prepare", fmt.Sprint(p.number))
	r.CommitKey = r.CommandKey
	_, replay, e := s.barrier(ctx, r, func(tx Tx) error {
		if e := checkActiveCandidate(ctx, tx, r, meta.Identity.AttemptID); e != nil {
			return e
		}
		if e := s.checkCandidateRoundAuthority(ctx, tx, r, meta); e != nil {
			return e
		}
		u := tx.(*unit)
		var pending int
		if e := u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND state='PENDING'", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&pending); e != nil {
			return e
		}
		if pending != 0 {
			return ErrCommitUnknown
		}
		var number uint64
		var state string
		e := u.tx.QueryRowContext(ctx, "SELECT round_number,state FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? ORDER BY round_number DESC LIMIT 1", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.attempt, phase).Scan(&number, &state)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if number != previous {
			return ErrConflict
		}
		if state == "COMMITTED" {
			receipt, e := queryReceipt(ctx, u.tx, originalKey)
			if e != nil {
				return e
			}
			if !receipt.matches(originalRequest) {
				return ErrConflict
			}
			return errCandidateRoundCommitted
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_local_rounds(tenant_id,product_id,task_id,attempt_id,phase,round_number,state,reason,body_hash,original_receipt_key,conflict_document) VALUES(?,?,?,?,?,?,'PENDING','',?,?,NULL)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.attempt, phase, p.number, meta.BodyHash, originalKey); e != nil {
			return e
		}
		return advanceTurn(ctx, u, ready)
	})
	if errors.Is(e, errCandidateRoundCommitted) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if replay {
		return nil, ErrCommitUnknown
	}
	return p, nil
}

func useCandidateRound(ctx context.Context, tx Tx, r BarrierRequest) error {
	p := r.localCandidateRound
	if p == nil || !p.used.CompareAndSwap(false, true) {
		return ErrDenied
	}
	if e := checkActiveCandidate(ctx, tx, r, p.attempt); e != nil {
		return e
	}
	u := tx.(*unit)
	var state string
	var latest uint64
	if e := u.tx.QueryRowContext(ctx, "SELECT state,round_number FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? ORDER BY round_number DESC LIMIT 1", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.attempt, p.phase).Scan(&state, &latest); e != nil {
		return e
	}
	if state != "PENDING" || latest != p.number {
		return ErrCommitUnknown
	}
	result, e := u.tx.ExecContext(ctx, "UPDATE harness_local_rounds SET state='COMMITTED' WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? AND round_number=? AND state='PENDING'", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.attempt, p.phase, p.number)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrDenied
	}
	return nil
}

func (s *Store) recordCandidateRollback(ctx context.Context, r BarrierRequest, meta domain.MockResultMetadata, outcome error) error {
	var proof *candidateRollback
	if !errors.As(outcome, &proof) || proof.round != r.localCandidateRound || proof.round.commitAttempted {
		return outcome
	}
	p := proof.round
	base, _ := candidatePhaseState(p.phase)
	reason := "LOCAL_FAILURE"
	conflict := p.conflict
	var raw []byte
	if conflict != nil && errors.Is(proof.cause, ErrCandidateResourceConflict) && conflict.OriginalHash != "" && conflict.CurrentHash != "" && conflict.OriginalHash != conflict.CurrentHash {
		reason = "RESOURCE_CONFLICT"
		raw, _ = json.Marshal(conflict)
	}
	r.localCandidateRound = nil
	r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{r.CommitKey}})
	r.Operation = "HARNESS_LOCAL_ROLLBACK"
	r.CommandKey = turnIdentity(p.attempt, p.phase, "rollback", fmt.Sprint(p.number))
	r.CommitKey = r.CommandKey
	_, e := s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := s.checkCandidateRoundAuthority(ctx, tx, r, meta); e != nil {
			return e
		}
		u := tx.(*unit)
		var key, state string
		var number uint64
		if e := u.tx.QueryRowContext(ctx, "SELECT original_receipt_key,state,round_number FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? ORDER BY round_number DESC LIMIT 1", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.attempt, p.phase).Scan(&key, &state, &number); e != nil {
			return e
		}
		if state != "PENDING" || number != p.number {
			return ErrDenied
		}
		if _, e := queryReceipt(ctx, u.tx, key); !errors.Is(e, sql.ErrNoRows) {
			if e != nil {
				return e
			}
			return ErrConflict
		}
		if _, e := u.tx.ExecContext(ctx, "UPDATE harness_local_rounds SET state='ROLLED_BACK',reason=?,conflict_document=? WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND phase=? AND round_number=? AND state='PENDING'", reason, raw, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, p.attempt, p.phase, p.number); e != nil {
			return e
		}
		return advanceTurn(ctx, u, base)
	})
	if e != nil {
		return errors.Join(outcome, e)
	}
	return outcome
}
