package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
)

type OriginalResultReader interface {
	RecoveryOriginalBytes(OriginalResultEvidence, domain.Scope, string, string) ([]byte, domain.MockResultMetadata, error)
	ReadAttemptEvidence(context.Context, domain.TaskLease, string, string) (OriginalResultEvidence, error)
	ReadOriginalEvidence(context.Context, domain.TaskLease, string) (OriginalResultEvidence, error)
}
type ApprovedTurnReader interface {
	ReadApprovedTurn(context.Context, domain.Scope, string) ([]byte, domain.CandidateCommit, error)
}

// Evidence is returned only by the installed owning Files adapter. No Store
// command accepts a caller-provided evidence value or completion classification.
type OriginalResultEvidence interface {
	Close()
	Bytes() []byte
	Metadata() domain.MockResultMetadata
}
type OriginalResultProgram interface{ ValidateOriginalCandidate([]byte) string }

// QueryOriginalValidation resolves only the original local validation COMMIT.
// It returns no result body or execution/extra-slot permission, including when
// the original governed body has since been deleted.
func (s *Store) QueryOriginalValidation(ctx context.Context, scope domain.Scope, task string) (CommitReceipt, error) {
	id, e := s.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		return CommitReceipt{}, e
	}
	return s.QueryAttemptValidation(ctx, scope, task, id.AttemptID)
}
func (s *Store) QueryAttemptValidation(ctx context.Context, scope domain.Scope, task, attempt string) (CommitReceipt, error) {
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer release()
	if scope.Validate() != nil || domain.ValidateID(task) != nil || domain.ValidateID(attempt) != nil {
		return CommitReceipt{}, ErrDenied
	}
	var key string
	e = s.DB.QueryRowContext(ctx, "SELECT v.receipt_key FROM harness_original_validations v JOIN harness_turns h USING(tenant_id,product_id,task_id) WHERE v.tenant_id=? AND v.product_id=? AND v.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND v.attempt_id=?", scope.TenantID, scope.ProductID, task, scope.WorldInstanceID, scope.SubjectID, attempt).Scan(&key)
	if e != nil {
		return CommitReceipt{}, e
	}
	receipt, e := s.QueryCommitScoped(ctx, scope.ProductScope, &scope, scope.SubjectID, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	row, e := queryReceipt(ctx, s.DB, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	if row.operation != "HARNESS_RESULT_VALIDATE" || row.phase != "COMMIT" || row.task != task || !receipt.Committed {
		return CommitReceipt{}, ErrConflict
	}
	return receipt, nil
}

// ValidateOriginalTurn consumes only the registered governed reader and program.
func (s *Store) ValidateOriginalTurn(ctx context.Context, lease domain.TaskLease, worker string) error {
	if lease.WorldInstanceID == nil {
		return ErrDenied
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	id, e := s.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e != nil {
		return e
	}
	return s.ValidateAttemptTurn(ctx, lease, worker, id.AttemptID)
}
func (s *Store) ValidateAttemptTurn(ctx context.Context, lease domain.TaskLease, worker, attempt string) error {
	if s.OriginalResultReader == nil || s.OriginalResultProgram == nil || s.GenerationContext == nil {
		return ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer release()
	if lease.WorldInstanceID == nil {
		return ErrDenied
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	evidence, e := s.OriginalResultReader.ReadAttemptEvidence(ctx, lease, worker, attempt)
	if e != nil {
		return e
	}
	if evidence == nil {
		return ErrDenied
	}
	defer evidence.Close()
	if receipt, e := s.QueryAttemptValidation(ctx, scope, lease.TaskID, attempt); e == nil {
		if receipt.TaskID != lease.TaskID {
			return ErrConflict
		}
		return nil
	} else if e != sql.ErrNoRows {
		return e
	}
	body, meta := evidence.Bytes(), evidence.Metadata()
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != meta.BodyHash {
		return ErrConflict
	}
	code := s.OriginalResultProgram.ValidateOriginalCandidate(body)
	if code != "STRUCTURE_VALID" && code != "STRUCTURE_INVALID" && code != "REFUSAL" {
		return ErrDenied
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
	r.Operation = "HARNESS_RESULT_VALIDATE"
	r.CommandKey = turnIdentity(meta.Identity.AttemptID, "validation")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(struct {
		Metadata domain.MockResultMetadata
		Code     string
	}{meta, code})
	if e != nil {
		return e
	}
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID}})
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		if e := checkResultInstallation(ctx, tx, r, *service); e != nil {
			return e
		}
		var raw []byte
		if e := tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&raw); e != nil {
			return e
		}
		var current domain.MockResultMetadata
		if json.Unmarshal(raw, &current) != nil || current != meta {
			return ErrConflict
		}
		if e := s.GenerationContext.ValidateResultSourcesTx(ctx, tx, *r.WorldScope, r.TaskID, meta, r.recoveryEvidence); e != nil {
			return e
		}
		u := tx.(*unit)
		if e := advanceTurn(ctx, u, "RESULT_KNOWN"); e != nil {
			return e
		}
		metadata, e := json.Marshal(meta)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_original_validations(tenant_id,product_id,task_id,attempt_id,body_hash,metadata,validation_code,validator_version,receipt_key) VALUES(?,?,?,?,?,?,?,'R0_CANDIDATE_STRUCTURE_V1',?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.BodyHash, metadata, code, r.CommitKey); e != nil {
			return e
		}
		return advanceTurn(ctx, u, "VALIDATED")
	})
	return e
}

// ReadAttemptValidation returns metadata only after resolving its exact local
// receipt. It is never body access, recovery approval or purchase authority.
func (s *Store) ReadAttemptValidation(ctx context.Context, scope domain.Scope, task, attempt string) (string, error) {
	receipt, e := s.QueryAttemptValidation(ctx, scope, task, attempt)
	if e != nil {
		return "", e
	}
	if !receipt.Committed {
		return "", ErrCommitUnknown
	}
	var code string
	e = s.DB.QueryRowContext(ctx, "SELECT validation_code FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", scope.TenantID, scope.ProductID, task, attempt).Scan(&code)
	if e != nil {
		return "", e
	}
	if code != "STRUCTURE_VALID" && code != "STRUCTURE_INVALID" && code != "REFUSAL" {
		return "", ErrDenied
	}
	return code, nil
}
