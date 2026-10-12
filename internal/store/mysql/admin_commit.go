package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

// This document contains provenance and safe references, never candidate text.
type draftCandidateCommit struct {
	Metadata                       domain.MockResultMetadata
	OriginalDependency             *domain.MockResultMetadata `json:",omitempty"`
	Binding                        domain.DraftCandidateMaterial
	AssetID, ReceiptKey, Validator string
}
type candidateCommitGuard struct {
	dispatch DispatchRequest
	evidence OriginalResultEvidence
	metadata domain.MockResultMetadata
	costs    []domain.MockResultMetadata
}

// Resolves only the original local business COMMIT. It grants no body access.
func (s *Store) QueryDraftCandidateCommit(ctx context.Context, scope domain.ProductScope, task string) (CommitReceipt, error) {
	if scope.Validate() != nil || domain.ValidateID(task) != nil {
		return CommitReceipt{}, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer release()
	return queryDraftCandidateCommitRow(ctx, s.DB, scope, task)
}
func queryDraftCandidateCommitRow(ctx context.Context, q rowQuerier, scope domain.ProductScope, task string) (CommitReceipt, error) {
	var raw []byte
	var key string
	e := q.QueryRowContext(ctx, "SELECT c.receipt_key,c.document FROM harness_candidate_commits c JOIN harness_admin_candidates a USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND a.owner_subject_id=?", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&key, &raw)
	if e != nil {
		return CommitReceipt{}, e
	}
	var c draftCandidateCommit
	if json.Unmarshal(raw, &c) != nil || key != turnIdentity(task, "candidate-commit") || c.ReceiptKey != key || c.Metadata.Identity.TaskID != task || c.Metadata.Identity.Scope.ProductScope != scope || c.Metadata.Identity.Kind != "CANDIDATE" || c.Metadata.Identity.Scope.WorldInstanceID != "" || !candidateAttemptID(task, c.Metadata.Identity.AttemptID) {
		return CommitReceipt{}, ErrConflict
	}
	receipt, e := queryCommitScopedRow(ctx, q, scope, nil, scope.SubjectID, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	row, e := queryReceipt(ctx, q, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	want := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_COMMIT", CommandKey: key, CommitKey: key}
	_, want.PayloadHash, e = harnessPayload(c.Metadata)
	if e != nil {
		return CommitReceipt{}, e
	}
	if !receipt.Committed || !row.matches(want) {
		return CommitReceipt{}, ErrConflict
	}
	return receipt, nil
}

func (s *Store) CommitDraftCandidate(ctx context.Context, lease domain.TaskLease, worker string) (CommitReceipt, error) {
	if lease.WorldInstanceID != nil || lease.SubjectID != lease.OwnerSubjectID || s.DraftCandidateReader == nil || s.DraftCandidateOwner == nil {
		return CommitReceipt{}, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer release()
	if receipt, e := s.QueryDraftCandidateCommit(ctx, lease.ProductScope, lease.TaskID); e == nil {
		return receipt, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return CommitReceipt{}, e
	}
	d, e := s.ActiveDraftCandidateDispatch(ctx, lease, worker)
	if e != nil {
		return CommitReceipt{}, e
	}
	d.CommandKey = candidateSendKey(d)
	d.CommitKey = d.CommandKey
	evidence, e := s.DraftCandidateReader.ReadPendingCandidateEvidence(ctx, d)
	if e != nil {
		return CommitReceipt{}, e
	}
	if evidence == nil {
		return CommitReceipt{}, ErrDenied
	}
	defer evidence.Close()
	meta := evidence.Metadata()
	if d.Slot == 1 {
		d, e = s.WithCandidateOriginalEvidence(d, evidence)
		if e != nil {
			return CommitReceipt{}, e
		}
	}
	ctx, service, queryRelease, e := s.turnQueryAdmission(ctx, d)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer queryRelease()
	if service == nil {
		return CommitReceipt{}, ErrDenied
	}
	scope := d.ProductScope
	r := candidateResultBarrier(d, meta.BodyStorage, meta.BodyHash)
	// A business candidate must be the active attempt; original dependency reads
	// have a separate fixed path and cannot resurrect a retired candidate.
	r.candidateOriginalRead = false
	r.originalQueryService = service
	r.candidateCommitGuard = &candidateCommitGuard{dispatch: d, evidence: evidence, metadata: meta}
	costs := []domain.MockResultMetadata{meta}
	if d.Slot == 1 {
		_, original, e := s.DraftCandidateReader.CandidateOriginalBytes(evidence, scope, d.TaskID, turnIdentity(d.TaskID, "candidate-attempt", "0"))
		if e != nil {
			return CommitReceipt{}, e
		}
		costs = []domain.MockResultMetadata{original, meta}
	}
	r.candidateCommitGuard.costs = append([]domain.MockResultMetadata{}, costs...)
	holds := []Reservation{}
	rows, e := s.DB.QueryContext(ctx, "SELECT kind,scope_key,account_id,reservation_id,amount FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? ORDER BY kind,scope_key,account_id,reservation_id LIMIT 8", scope.TenantID, scope.ProductID, d.TaskID)
	if e != nil {
		return CommitReceipt{}, e
	}
	for rows.Next() {
		var h Reservation
		if e = rows.Scan(&h.Kind, &h.ScopeKey, &h.AccountID, &h.ReservationID, &h.Amount); e != nil {
			rows.Close()
			return CommitReceipt{}, e
		}
		holds = append(holds, h)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return CommitReceipt{}, e
	}
	if len(holds) != 3+2*len(costs) {
		return CommitReceipt{}, ErrDenied
	}
	asset, serviceID := turnIdentity(d.TaskID, "candidate-asset"), turnIdentity(d.TaskID, "service")
	r.Locks = append(r.Locks, holdLocks(holds)...)
	r.Locks = append(r.Locks, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, asset}}, LockKey{Rank: 110, Kind: "service_events", IDs: []string{scope.TenantID, scope.ProductID, serviceID}})
	for _, cost := range costs {
		r.Locks = append(r.Locks, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, d.TaskID, cost.Identity.AttemptID, cost.Identity.AuthorizationID}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(cost.Identity.AttemptID, "candidate-validation")}})
	}
	for _, h := range holds {
		r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "postings", IDs: []string{scope.TenantID, scope.ProductID, turnIdentity(d.TaskID, "posting", h.ReservationID)}})
	}
	r.Operation = "CANDIDATE_COMMIT"
	r.CommandKey = turnIdentity(d.TaskID, "candidate-commit")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(meta)
	if e != nil {
		return CommitReceipt{}, e
	}
	return s.WithBarrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		material, e := s.checkCandidateCommitTx(ctx, u, r)
		if e != nil {
			return e
		}
		if e = settleCandidateCosts(ctx, u, r, material, costs, holds, serviceID); e != nil {
			return e
		}
		var now time.Time
		if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return e
		}
		record := generated.FileRecord{Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, AssetID: generated.Identity(asset), State: "READY", Name: "candidate-fields.json", MediaType: "text/plain", SizeBytes: generated.ExactUnsigned(strconv.Itoa(len(evidence.Bytes()))), Sha256: meta.BodyHash, Purpose: "MEDIA", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "R0 candidate result", License: "original governed source restrictions", AllowedUses: []string{}}, AudiencePolicy: "PRODUCT_EDITORS", OwnerSubjectID: generated.Identity(scope.SubjectID), CreatedAt: now.UTC().Format(time.RFC3339Nano)}
		if e = typedJSON("FileRecord", record); e != nil {
			return e
		}
		fileRaw, e := json.Marshal(record)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO assets(tenant_id,product_id,asset_id,owner_subject_id,state,storage_key,document) VALUES(?,?,?,?,'READY',?,?)", scope.TenantID, scope.ProductID, asset, scope.SubjectID, meta.BodyStorage, fileRaw); e != nil {
			return e
		}
		for _, cost := range costs {
			result, e := u.tx.ExecContext(ctx, "UPDATE harness_candidate_results SET asset_id=? WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND storage_key=? AND body_hash=? AND delete_intent=0 AND asset_id IS NULL", asset, scope.TenantID, scope.ProductID, d.TaskID, cost.Identity.AttemptID, cost.BodyStorage, cost.BodyHash)
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return ErrConflict
			}
		}
		material.Canonical = nil
		committed := draftCandidateCommit{Metadata: meta, Binding: material, AssetID: asset, ReceiptKey: r.CommitKey, Validator: "R0_DRAFT_FIELDS_V1"}
		if len(costs) == 2 {
			original := costs[0]
			committed.OriginalDependency = &original
		}
		raw, e := json.Marshal(committed)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_candidate_commits(tenant_id,product_id,task_id,attempt_id,asset_id,receipt_key,document) VALUES(?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, d.TaskID, d.AttemptID, asset, r.CommitKey, raw); e != nil {
			return e
		}
		for _, next := range []string{"LOCAL_APPROVAL_READY", "APPROVED", "LOCAL_COMMIT_READY", "COMMITTED"} {
			if e = advanceTurn(ctx, u, next); e != nil {
				return e
			}
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='COMMITTED' WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, d.TaskID); e != nil {
			return e
		}
		return tx.SetTaskState(ctx, "COMMITTED")
	})
}

func (s *Store) checkCandidateCommitTx(ctx context.Context, u *unit, r BarrierRequest) (domain.DraftCandidateMaterial, error) {
	var empty domain.DraftCandidateMaterial
	g := r.candidateCommitGuard
	if g == nil || g.evidence == nil || r.originalQueryService == nil {
		return empty, ErrDenied
	}
	body, meta := g.evidence.Bytes(), g.evidence.Metadata()
	hash := sha256.Sum256(body)
	if meta != g.metadata || meta.Identity != mockIdentity(g.dispatch) || hex.EncodeToString(hash[:]) != meta.BodyHash {
		return empty, ErrDenied
	}
	material, e := s.checkCandidateResultTx(ctx, u, g.dispatch, meta.BodyStorage, meta.BodyHash, *r.originalQueryService)
	if e != nil {
		return empty, e
	}
	// Repeat the active gate even though the fixed source inspection of slot0 is
	// also usable as a dependency read after repair.
	if _, e = s.candidateMaterialTx(ctx, u, r); e != nil {
		return empty, e
	}
	var raw []byte
	var code, key string
	if e = u.tx.QueryRowContext(ctx, "SELECT metadata,validation_code,receipt_key FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND validator_version='R0_DRAFT_FIELDS_V1'", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, meta.Identity.AttemptID).Scan(&raw, &code, &key); e != nil {
		return empty, e
	}
	var saved domain.MockResultMetadata
	if json.Unmarshal(raw, &saved) != nil || saved != meta || code != "STRUCTURE_VALID" || key != turnIdentity(meta.Identity.AttemptID, "candidate-validation") {
		return empty, ErrDenied
	}
	receipt, e := queryReceipt(ctx, u.tx, key)
	if e != nil {
		return empty, e
	}
	expected := r
	expected.Operation = "CANDIDATE_RESULT_VALIDATE"
	expected.CommandKey = key
	expected.CommitKey = key
	_, expected.PayloadHash, e = harnessPayload(struct {
		Metadata domain.MockResultMetadata
		Program  string
	}{meta, "R0_DRAFT_FIELDS_V1"})
	if e != nil {
		return empty, e
	}
	if !receipt.matches(expected) {
		return empty, ErrConflict
	}
	code, e = s.DraftCandidateOwner.ValidateCandidateTx(ctx, u, r.ProductScope, material.Selection, body)
	if e != nil {
		return empty, e
	}
	if code != "STRUCTURE_VALID" {
		return empty, ErrDenied
	}
	if e = checkCandidateCostsTx(ctx, u, r, g.costs); e != nil {
		return empty, e
	}
	return material, nil
}

func checkCandidateCostsTx(ctx context.Context, u *unit, r BarrierRequest, costs []domain.MockResultMetadata) error {
	if len(costs) < 1 || len(costs) > 2 {
		return ErrDenied
	}
	for _, cost := range costs {
		if !validOriginalUsageMetadata(minimalOriginalMetadata(cost)) || cost.State != "SUCCEEDED" || cost.Identity.TaskID != r.TaskID || cost.Identity.Scope.ProductScope != r.ProductScope || cost.Identity.Kind != "CANDIDATE" {
			return ErrDenied
		}
		var raw []byte
		var saved domain.MockResultMetadata
		if e := u.tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND provider_state='SUCCEEDED'", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, cost.Identity.AttemptID).Scan(&raw); e != nil {
			return e
		}
		if json.Unmarshal(raw, &saved) != nil || saved != cost {
			return ErrConflict
		}
	}
	return nil
}

func settleCandidateCosts(ctx context.Context, u *unit, r BarrierRequest, m domain.DraftCandidateMaterial, costs []domain.MockResultMetadata, holds []Reservation, serviceID string) error {
	scope := r.ProductScope
	var eligible bool
	if e := u.tx.QueryRowContext(ctx, "SELECT e.state='ACTIVE' AND e.execute_by>UTC_TIMESTAMP(6) AND e.settle_by>UTC_TIMESTAMP(6) AND e.calls_allocated=? AND q.document->'$.ServiceAtoms'=0 AND q.document->'$.SupplierAtoms'=0 FROM harness_envelopes e JOIN harness_quotes q ON q.tenant_id=e.tenant_id AND q.product_id=e.product_id AND q.quote_id=e.quote_id WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", len(costs), scope.TenantID, scope.ProductID, r.TaskID).Scan(&eligible); e != nil {
		return e
	}
	if !eligible {
		return ErrDenied
	}
	expected := map[string]Reservation{}
	purposes := map[string]string{}
	for _, a := range r0CandidateAccounts(scope, m.ProjectID) {
		if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
			for _, cost := range costs {
				h := a
				h.ReservationID = turnIdentity(cost.Identity.AttemptID, "supplier", a.Kind)
				expected[h.ReservationID] = h
				purposes[h.ReservationID] = cost.Identity.AttemptID
			}
		} else {
			a.ReservationID = turnIdentity(r.TaskID, "parent", a.Kind)
			expected[a.ReservationID] = a
			purposes[a.ReservationID] = "PARENT"
		}
	}
	if len(expected) != len(holds) {
		return ErrDenied
	}
	for _, cost := range costs {
		if !validOriginalUsageMetadata(minimalOriginalMetadata(cost)) || cost.State != "SUCCEEDED" || cost.Identity.TaskID != r.TaskID || cost.Identity.Scope.ProductScope != scope || cost.Identity.Kind != "CANDIDATE" {
			return ErrDenied
		}
		var raw []byte
		var saved domain.MockResultMetadata
		if e := u.tx.QueryRowContext(ctx, "SELECT metadata FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND provider_state='SUCCEEDED'", scope.TenantID, scope.ProductID, r.TaskID, cost.Identity.AttemptID).Scan(&raw); e != nil {
			return e
		}
		if json.Unmarshal(raw, &saved) != nil || saved != cost {
			return ErrConflict
		}
	}
	for _, h := range holds {
		want, ok := expected[h.ReservationID]
		if !ok || want != h {
			return ErrDenied
		}
		var amount int64
		var state, purpose string
		if e := u.tx.QueryRowContext(ctx, "SELECT amount,status,purpose FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? AND kind=? AND scope_key=? AND account_id=? AND reservation_id=?", scope.TenantID, scope.ProductID, r.TaskID, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID).Scan(&amount, &state, &purpose); e != nil {
			return e
		}
		if amount != 0 || state != "HELD" || purpose != purposes[h.ReservationID] {
			return ErrDenied
		}
	}
	if e := bindOriginalServiceMeteringTx(ctx, u, r, serviceID, costs); e != nil {
		return e
	}
	for _, h := range holds {
		status := originalCostHoldState(h, costs)
		if status == "HELD" {
			continue
		}
		if _, e := u.tx.ExecContext(ctx, "UPDATE reservations SET status=?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", status, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,0,?,1)", scope.TenantID, scope.ProductID, turnIdentity(r.TaskID, "posting", h.ReservationID), r.TaskID, scope.SubjectID, h.Kind, h.ScopeKey, h.AccountID, status); e != nil {
			return e
		}
	}
	return nil
}
