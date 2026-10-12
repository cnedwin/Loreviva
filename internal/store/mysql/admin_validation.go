package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
)

type DraftCandidateReader interface {
	ReadCandidateSourceEvidence(context.Context, domain.ProductScope, domain.DraftCandidateMaterial) (CandidateSourceEvidence, error)
	ReadPendingCandidateEvidence(context.Context, DispatchRequest) (OriginalResultEvidence, error)
	CandidateOriginalBytes(OriginalResultEvidence, domain.ProductScope, string, string) ([]byte, domain.MockResultMetadata, error)
}

func (s *Store) ValidateDraftCandidate(ctx context.Context, lease domain.TaskLease, worker string) error {
	if s.DraftCandidateReader == nil || s.DraftCandidateOwner == nil {
		return ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer release()
	d, e := s.ActiveDraftCandidateDispatch(ctx, lease, worker)
	if e != nil {
		return e
	}
	d.CommandKey = candidateSendKey(d)
	d.CommitKey = d.CommandKey
	evidence, e := s.DraftCandidateReader.ReadPendingCandidateEvidence(ctx, d)
	if e != nil {
		return fmt.Errorf("candidate validation evidence: %w", e)
	}
	if evidence == nil {
		return ErrDenied
	}
	defer evidence.Close()
	meta, body := evidence.Metadata(), evidence.Bytes()
	if d.Slot == 1 {
		d, e = s.WithCandidateOriginalEvidence(d, evidence)
		if e != nil {
			return e
		}
	}
	h := sha256.Sum256(body)
	if meta.Identity != mockIdentity(d) || hex.EncodeToString(h[:]) != meta.BodyHash {
		return ErrDenied
	}
	if _, e = s.QueryDraftCandidateAttemptValidation(ctx, lease.ProductScope, lease.TaskID, d.AttemptID); e == nil {
		return nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	ctx, service, queryRelease, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return e
	}
	defer queryRelease()
	if service == nil {
		return ErrDenied
	}
	r := candidateResultBarrier(d, meta.BodyStorage, meta.BodyHash)
	r.originalQueryService = service
	r.Operation = "CANDIDATE_RESULT_VALIDATE"
	r.CommandKey = turnIdentity(d.AttemptID, "candidate-validation")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(struct {
		Metadata domain.MockResultMetadata
		Program  string
	}{meta, "R0_DRAFT_FIELDS_V1"})
	if e != nil {
		return e
	}
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		m, e := s.checkCandidateResultTx(ctx, u, d, meta.BodyStorage, meta.BodyHash, *service)
		if e != nil {
			return e
		}
		var raw []byte
		if e = u.tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID).Scan(&raw); e != nil {
			return e
		}
		var current domain.MockResultMetadata
		if json.Unmarshal(raw, &current) != nil || current != meta {
			return ErrDenied
		}
		code, e := s.DraftCandidateOwner.ValidateCandidateTx(ctx, u, d.ProductScope, m.Selection, body)
		if e != nil {
			return e
		}
		if code != "STRUCTURE_VALID" && code != "STRUCTURE_INVALID" && code != "REFUSAL" {
			return ErrDenied
		}
		if e = advanceTurn(ctx, u, "RESULT_KNOWN"); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_original_validations(tenant_id,product_id,task_id,attempt_id,body_hash,metadata,validation_code,validator_version,receipt_key) VALUES(?,?,?,?,?,?,?,'R0_DRAFT_FIELDS_V1',?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.AttemptID, meta.BodyHash, raw, code, r.CommitKey); e != nil {
			return e
		}
		return advanceTurn(ctx, u, "VALIDATED")
	})
	if e != nil {
		return fmt.Errorf("candidate validation transaction: %w", e)
	}
	return nil
}

// Only resolves this local program transaction. No bytes, completion of the
// candidate business command, or repair/dispatch permission are returned.
func (s *Store) QueryDraftCandidateValidation(ctx context.Context, scope domain.ProductScope, task string) (CommitReceipt, error) {
	return s.QueryDraftCandidateAttemptValidation(ctx, scope, task, turnIdentity(task, "candidate-attempt", "0"))
}

func (s *Store) ReadDraftCandidateValidation(ctx context.Context, scope domain.ProductScope, task, attempt string) (string, error) {
	if _, e := s.QueryDraftCandidateAttemptValidation(ctx, scope, task, attempt); e != nil {
		return "", e
	}
	var code string
	e := s.DB.QueryRowContext(ctx, "SELECT validation_code FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND validator_version='R0_DRAFT_FIELDS_V1'", scope.TenantID, scope.ProductID, task, attempt).Scan(&code)
	return code, e
}
func (s *Store) QueryDraftCandidateAttemptValidation(ctx context.Context, scope domain.ProductScope, task, attempt string) (CommitReceipt, error) {
	if scope.Validate() != nil || domain.ValidateID(task) != nil || !candidateAttemptID(task, attempt) {
		return CommitReceipt{}, ErrDenied
	}
	var key string
	var raw []byte
	e := s.DB.QueryRowContext(ctx, "SELECT v.receipt_key,v.metadata FROM harness_original_validations v JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND c.owner_subject_id=? AND v.attempt_id=? AND v.validator_version='R0_DRAFT_FIELDS_V1'", scope.TenantID, scope.ProductID, task, scope.SubjectID, attempt).Scan(&key, &raw)
	if e != nil {
		return CommitReceipt{}, e
	}
	var meta domain.MockResultMetadata
	if key != turnIdentity(attempt, "candidate-validation") || json.Unmarshal(raw, &meta) != nil || meta.Identity.AttemptID != attempt || meta.Identity.TaskID != task || meta.Identity.Kind != "CANDIDATE" || meta.Identity.Scope.ProductScope != scope || meta.Identity.Scope.WorldInstanceID != "" {
		return CommitReceipt{}, ErrConflict
	}
	receipt, e := s.QueryCommitScoped(ctx, scope, nil, scope.SubjectID, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	row, e := queryReceipt(ctx, s.DB, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	expected := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_RESULT_VALIDATE", CommandKey: key, CommitKey: key}
	_, expected.PayloadHash, e = harnessPayload(struct {
		Metadata domain.MockResultMetadata
		Program  string
	}{meta, "R0_DRAFT_FIELDS_V1"})
	if e != nil {
		return CommitReceipt{}, e
	}
	if !row.matches(expected) || !receipt.Committed {
		return CommitReceipt{}, ErrConflict
	}
	return receipt, nil
}
