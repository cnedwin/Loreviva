package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

// Only the fixed complete-body rejection command can create this same-Tx guard.
type rehearsalNegativeGuard struct {
	outcome  domain.RehearsalNegativeOutcome
	evidence OriginalResultEvidence
	applied  *unit
}

func (s *Store) CommitRehearsalNegative(ctx context.Context, lease domain.TaskLease, worker, attempt string) (err error) {
	phase := "negative evidence"
	defer func() {
		if err != nil {
			err = fmt.Errorf("%s: %w", phase, err)
		}
	}()
	if s.OriginalResultReader == nil || s.GenerationContext == nil || s.OriginalResultProgram == nil || lease.WorldInstanceID == nil {
		return ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer close()
	evidence, e := s.OriginalResultReader.ReadAttemptEvidence(ctx, lease, worker, attempt)
	if e != nil {
		return e
	}
	defer evidence.Close()
	meta := evidence.Metadata()
	code := s.OriginalResultProgram.ValidateOriginalCandidate(evidence.Bytes())
	// An invalid first result still owns the possible shared repair slot.
	if code != "REFUSAL" && code != "STRUCTURE_INVALID" {
		return ErrDenied
	}
	phase = "negative original barrier"
	r, e := s.GenerationContext.OriginalResultBarrier(ctx, lease, worker)
	if e != nil {
		return e
	}
	if r.rehearsalExecution == nil {
		return ErrDenied
	}
	ctx, service, release, e := s.turnQueryAdmission(ctx, DispatchRequest{BarrierRequest: r})
	if e != nil {
		return e
	}
	defer release()
	if service == nil {
		return ErrDenied
	}
	r.originalQueryService = service
	r.recoveryEvidence = evidence
	if e = s.prepareOriginalResultBarrier(ctx, &r, attempt); e != nil {
		return e
	}
	n := domain.RehearsalNegativeOutcome{State: "REFUSED", Metadata: meta, Versions: *r.Versions, ReceiptKey: turnIdentity(lease.TaskID, "negative-outcome"), ValidationKey: turnIdentity(attempt, "validation")}
	var raw []byte
	e = s.DB.QueryRowContext(ctx, "SELECT document FROM harness_recovery_approvals WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", lease.TenantID, lease.ProductID, lease.TaskID, attempt).Scan(&raw)
	if e == nil {
		var plan domain.RecoveryPlan
		if json.Unmarshal(raw, &plan) != nil || plan.AttemptID != attempt {
			return ErrDenied
		}
		n.OriginalDependency = &plan.Original
	} else if e != sql.ErrNoRows {
		return e
	}
	if e = s.DB.QueryRowContext(ctx, "SELECT narrative_generation+1 FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&n.NarrativeGeneration); e != nil {
		return e
	}
	if code == "STRUCTURE_INVALID" {
		if n.OriginalDependency == nil {
			return ErrDenied
		}
		n.State = "FAILED"
	}
	r.Operation = "REHEARSAL_CHILD_REJECT"
	r.CommandKey = n.ReceiptKey
	r.CommitKey = n.ReceiptKey
	_, r.PayloadHash, e = harnessPayload(n)
	if e != nil {
		return e
	}
	r.rehearsalNegative = &rehearsalNegativeGuard{outcome: n, evidence: evidence}
	r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{lease.TenantID, lease.ProductID, lease.TaskID, attempt, meta.Identity.AuthorizationID}})
	phase = "negative transaction"
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := s.checkRehearsalNegativeTx(ctx, u, r); e != nil {
			return fmt.Errorf("negative before apply: %w", e)
		}
		if e := advanceTurn(ctx, u, "REJECTED"); e != nil {
			return e
		}
		raw, e := json.Marshal(n)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_rehearsal_negative_outcomes VALUES(?,?,?,?,?,?,?,?,?)", lease.TenantID, lease.ProductID, lease.TaskID, r.rehearsalExecution.root.TaskID, r.rehearsalExecution.child.index, attempt, n.ReceiptKey, r.PayloadHash[:], raw); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='ACCOUNTING_ONLY',narrative_generation=narrative_generation+1 WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID); e != nil {
			return e
		}
		res, e := u.tx.ExecContext(ctx, "UPDATE harness_admin_rehearsals SET next_input=next_input+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND next_input=? AND fsm_state='ISOLATED'", lease.TenantID, lease.ProductID, r.rehearsalExecution.root.TaskID, r.rehearsalExecution.child.index)
		if e != nil {
			return e
		}
		count, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if count != 1 {
			return ErrDenied
		}
		if e = tx.SetTaskState(ctx, "FAILED"); e != nil {
			return e
		}
		r.rehearsalNegative.applied = u
		return nil
	})
	return e
}

func (s *Store) checkRehearsalNegativeTx(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalNegative
	if p == nil || r.rehearsalExecution == nil || r.Operation != "REHEARSAL_CHILD_REJECT" || p.evidence == nil || r.originalQueryService == nil {
		return ErrDenied
	}
	n := p.outcome
	if p.evidence.Metadata() != n.Metadata || s.OriginalResultProgram.ValidateOriginalCandidate(p.evidence.Bytes()) != negativeValidationCode(n) {
		return ErrDenied
	}
	if e := checkResultInstallation(ctx, u, r, *r.originalQueryService); e != nil {
		return e
	}
	if e := checkActiveCandidate(ctx, u, r, n.Metadata.Identity.AttemptID); e != nil {
		return e
	}
	if e := s.checkRehearsalNegativeEvidenceTx(ctx, u, r, n); e != nil {
		return e
	}
	if e := s.GenerationContext.ValidateResultSourcesTx(ctx, u, *r.WorldScope, r.TaskID, n.Metadata, p.evidence); e != nil {
		return e
	}
	if p.applied != nil {
		if e := checkRehearsalNegativeApplied(ctx, u.tx, r); e != nil {
			return fmt.Errorf("negative applied: %w", e)
		}
		return nil
	}
	var safe bool
	e := u.tx.QueryRowContext(ctx, "SELECT NOT EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=?) AND NOT EXISTS(SELECT 1 FROM harness_local_rounds WHERE tenant_id=? AND product_id=? AND task_id=? AND (state='PENDING' OR (phase='WORLD_COMMIT' AND state='COMMITTED')))", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&safe)
	if e != nil {
		return e
	}
	if !safe {
		return ErrDenied
	}
	return nil
}

func checkRehearsalNegativeRetirement(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	p := r.rehearsalNegative
	if p == nil || p.applied == nil || p.applied.tx != tx || r.Operation != "REHEARSAL_CHILD_REJECT" {
		return ErrDenied
	}
	var raw []byte
	var valid bool
	e := tx.QueryRowContext(ctx, "SELECT n.document,e.state='ACCOUNTING_ONLY' AND e.narrative_generation=? AND e.execute_by>UTC_TIMESTAMP(6) AND e.settle_by>UTC_TIMESTAMP(6) AND t.state='FAILED' AND h.fsm_state='REJECTED' FROM harness_rehearsal_negative_outcomes n JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) WHERE n.tenant_id=? AND n.product_id=? AND n.task_id=? AND n.root_task_id=? AND n.input_index=?", p.outcome.NarrativeGeneration, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.rehearsalExecution.root.TaskID, r.rehearsalExecution.child.index).Scan(&raw, &valid)
	if e != nil {
		return e
	}
	var n domain.RehearsalNegativeOutcome
	if !valid || json.Unmarshal(raw, &n) != nil || !reflect.DeepEqual(n, p.outcome) {
		return ErrDenied
	}
	return nil
}

func checkRehearsalNegativeApplied(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	p := r.rehearsalNegative
	if p == nil || p.applied == nil || p.applied.tx != tx || r.rehearsalExecution == nil || r.Operation != "REHEARSAL_CHILD_REJECT" {
		return ErrDenied
	}
	n, e := readRehearsalNegativeTx(ctx, tx, r)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(n, p.outcome) {
		return ErrDenied
	}
	return nil
}

func readRehearsalNegativeTx(ctx context.Context, tx *sql.Tx, r BarrierRequest) (n domain.RehearsalNegativeOutcome, e error) {
	var raw, storedHash []byte
	var key string
	var generation uint64
	e = tx.QueryRowContext(ctx, "SELECT n.document,n.receipt_key,e.narrative_generation,n.command_hash FROM harness_rehearsal_negative_outcomes n JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE n.tenant_id=? AND n.product_id=? AND n.task_id=? AND n.root_task_id=? AND n.input_index=? AND t.owner_subject_id=? AND h.world_instance_id=? AND t.state='FAILED' AND h.fsm_state='REJECTED' AND e.state='ACCOUNTING_ONLY' AND n.attempt_id=JSON_UNQUOTE(JSON_EXTRACT(n.document,'$.Metadata.Identity.AttemptID'))", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.rehearsalExecution.root.TaskID, r.rehearsalExecution.child.index, r.OwnerSubjectID, r.WorldScope.WorldInstanceID).Scan(&raw, &key, &generation, &storedHash)
	if e != nil {
		return n, e
	}
	if json.Unmarshal(raw, &n) != nil || n.ReceiptKey != key || key != turnIdentity(r.TaskID, "negative-outcome") || n.NarrativeGeneration != generation || generation < 2 {
		return n, ErrDenied
	}
	receipt, e := queryReceipt(ctx, tx, key)
	if e != nil {
		return n, e
	}
	expected := r
	expected.commandOnly = false
	expected.Operation = "REHEARSAL_CHILD_REJECT"
	expected.CommandKey = key
	expected.CommitKey = key
	_, expected.PayloadHash, e = harnessPayload(n)
	if e != nil {
		return n, e
	}
	if !receipt.matches(expected) || !bytes.Equal(storedHash, expected.PayloadHash[:]) {
		return n, ErrDenied
	}
	return n, nil
}

func (s *Store) checkRehearsalNegativeEvidenceTx(ctx context.Context, u *unit, r BarrierRequest, n domain.RehearsalNegativeOutcome) error {
	if negativeValidationCode(n) == "" || n.Metadata.Identity.Scope != *r.WorldScope || n.Metadata.Identity.TaskID != r.TaskID || n.ValidationKey != turnIdentity(n.Metadata.Identity.AttemptID, "validation") {
		return ErrDenied
	}
	if n.State == "FAILED" {
		var valid bool
		if e := u.tx.QueryRowContext(ctx, "SELECT a.slot=1 AND e.calls_allocated=2 AND p.extra_attempt_id=a.attempt_id AND p.original_attempt_id=? FROM attempts a JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_recovery_approvals p USING(tenant_id,product_id,task_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=?", n.OriginalDependency.Identity.AttemptID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, n.Metadata.Identity.AttemptID).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrDenied
		}
	}
	metas := []domain.MockResultMetadata{n.Metadata}
	if n.OriginalDependency != nil {
		metas = append([]domain.MockResultMetadata{*n.OriginalDependency}, metas...)
	}
	for _, meta := range metas {
		if !meta.BodyAvailable || meta.State != "SUCCEEDED" || meta.Identity.Scope != *r.WorldScope || meta.Identity.TaskID != r.TaskID {
			return ErrDenied
		}
		if e := u.RequireLock(LockKey{Rank: 60, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID}}); e != nil {
			return e
		}
		var raw []byte
		var code, key, hash string
		if e := u.tx.QueryRowContext(ctx, "SELECT metadata,validation_code,receipt_key,body_hash FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&raw, &code, &key, &hash); e != nil {
			return e
		}
		var saved domain.MockResultMetadata
		if json.Unmarshal(raw, &saved) != nil || saved != meta || hash != meta.BodyHash || (meta == n.Metadata && code != negativeValidationCode(n)) {
			return ErrDenied
		}
		receipt, e := queryReceipt(ctx, u.tx, key)
		if e != nil {
			return e
		}
		expected := r
		expected.commandOnly = false
		expected.Operation = "HARNESS_RESULT_VALIDATE"
		expected.CommandKey = turnIdentity(meta.Identity.AttemptID, "validation")
		expected.CommitKey = key
		_, expected.PayloadHash, e = harnessPayload(struct {
			Metadata domain.MockResultMetadata
			Code     string
		}{meta, code})
		if e != nil {
			return e
		}
		if !receipt.matches(expected) {
			return ErrDenied
		}
		var sent bool
		if e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM attempts a JOIN dispatch_authorizations d USING(tenant_id,product_id,task_id,attempt_id) JOIN commit_receipts c ON c.commit_key=a.marker_commit_key AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.task_id=a.task_id AND c.owner_subject_id=d.owner_subject_id WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=? AND d.authorization_id=? AND a.request_key=? AND a.request_hash=UNHEX(?) AND a.route=? AND a.marker_commit_key=? AND a.send_started_at IS NOT NULL AND c.operation='TURN_DISPATCH' AND c.phase='SEND' AND c.world_instance_id=? AND c.scope_kind='WORLD')", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID, meta.Identity.AuthorizationID, meta.Identity.RequestKey, meta.Identity.RequestHash, meta.Identity.RouteID, meta.Identity.MarkerKey, r.WorldScope.WorldInstanceID).Scan(&sent); e != nil {
			return e
		}
		if !sent {
			return ErrDenied
		}
	}
	return nil
}

func (s *Store) inspectRehearsalChildOutcomeTx(ctx context.Context, u *unit, r BarrierRequest, count int) (domain.RehearsalChildOutcome, error) {
	var exists bool
	if e := u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_negative_outcomes WHERE tenant_id=? AND product_id=? AND task_id=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&exists); e != nil {
		return domain.RehearsalChildOutcome{}, e
	}
	if !exists {
		c, e := s.inspectRehearsalChildResultTx(ctx, u, r, count)
		return domain.RehearsalWorldOutcome(c), e
	}
	n, e := readRehearsalNegativeTx(ctx, u.tx, r)
	if e != nil {
		return domain.RehearsalChildOutcome{}, e
	}
	expected := 1
	if n.OriginalDependency != nil {
		expected = 2
	}
	if count != expected {
		return domain.RehearsalChildOutcome{}, ErrDenied
	}
	if e = s.checkRehearsalNegativeEvidenceTx(ctx, u, r, n); e != nil {
		return domain.RehearsalChildOutcome{}, e
	}
	if e = s.RehearsalInputOwner.InspectRehearsalNegativeSourcesTx(ctx, u, *r.WorldScope, r.TaskID, *r.Versions, n); e != nil {
		return domain.RehearsalChildOutcome{}, e
	}
	return domain.RehearsalRejectedOutcome(n), nil
}

func negativeValidationCode(n domain.RehearsalNegativeOutcome) string {
	if n.State == "REFUSED" {
		return "REFUSAL"
	}
	if n.State == "FAILED" && n.OriginalDependency != nil {
		return "STRUCTURE_INVALID"
	}
	return ""
}
