package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
)

type ApprovedDraftCandidate struct {
	Asset   generated.FileRecord
	Results []domain.MockResultMetadata // immutable original 0, then optional 1
	Sources []CandidateSourceAsset
}

func candidateReadActionsTx(ctx context.Context, tx *sql.Tx, scope domain.ProductScope) error {
	var allowed bool
	e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_credentials c WHERE c.tenant_id=? AND c.product_id=? AND c.subject_id=? AND c.enabled=1 AND c.credential_id='owner' AND EXISTS(SELECT 1 FROM operator_actions a WHERE a.credential_id=c.credential_id AND a.action='content.read') AND EXISTS(SELECT 1 FROM operator_actions a WHERE a.credential_id=c.credential_id AND a.action='asset.read'))", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&allowed)
	if e != nil {
		return e
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

// The only mint for the private S55 barrier. This returns a current read
// projection, never a reusable execution or file capability.
func (s *Store) InspectApprovedDraftCandidate(ctx context.Context, scope domain.ProductScope, task string) (ApprovedDraftCandidate, error) {
	var out ApprovedDraftCandidate
	if scope.Validate() != nil || domain.ValidateID(task) != nil || s.Guard == nil || s.DraftCandidateOwner == nil || s.ProviderEvidence == nil {
		return out, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	var raw []byte
	if e = s.DB.QueryRowContext(ctx, "SELECT c.document FROM harness_candidate_commits c JOIN harness_admin_candidates a USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND a.owner_subject_id=?", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&raw); e != nil {
		return out, e
	}
	var c draftCandidateCommit
	if json.Unmarshal(raw, &c) != nil || c.ReceiptKey != turnIdentity(task, "candidate-commit") || c.AssetID != turnIdentity(task, "candidate-asset") || c.Validator != "R0_DRAFT_FIELDS_V1" {
		return out, ErrDenied
	}
	results := []domain.MockResultMetadata{c.Metadata}
	if c.OriginalDependency != nil {
		results = []domain.MockResultMetadata{*c.OriginalDependency, c.Metadata}
	}
	var gates GateVersions
	var enabled bool
	if e = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled FROM subject_governance s JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled); e != nil {
		return out, e
	}
	if !enabled {
		return out, ErrDenied
	}
	keys, e := s.DraftCandidateOwner.PlanCandidateHistoryLocks(ctx, scope, c.Binding)
	if e != nil {
		return out, e
	}
	keys = append(keys, LockKey{Rank: 50, IDs: []string{scope.TenantID, scope.ProductID, task}}, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, c.AssetID}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{c.ReceiptKey}})
	for i, m := range results {
		id := m.Identity
		attempt := turnIdentity(task, "candidate-attempt", strconv.Itoa(i))
		if id.Scope.ProductScope != scope || id.Scope.WorldInstanceID != "" || id.Kind != "CANDIDATE" || id.TaskID != task || id.AttemptID != attempt || id.AuthorizationID != turnIdentity(attempt, "authorization") || id.RequestKey != turnIdentity(attempt, "request") || m.State != "SUCCEEDED" || !m.BodyAvailable || len(mustHash(m.BodyStorage)) != 16 || len(mustHash(m.BodyHash)) != 32 {
			return out, ErrDenied
		}
		keys = append(keys, LockKey{Rank: 60, IDs: []string{scope.TenantID, scope.ProductID, task, id.AttemptID, id.AuthorizationID}}, LockKey{Rank: 90, Kind: "asset", IDs: []string{scope.TenantID, scope.ProductID, "staging." + m.BodyStorage}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{id.MarkerKey}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(attempt, "candidate-validation")}})
	}
	if len(results) == 2 {
		keys = append(keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(task, "candidate-recovery")}})
	}
	key := turnIdentity(task, "candidate-history-read")
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_HISTORY_READ", CommandKey: key, CommitKey: key, GateVersions: gates, Locks: keys, commandOnly: true, candidateHistoryRead: &c}
	inspected := errors.New("candidate history inspected without mutation")
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		var current []byte
		var registered bool
		var attempt, asset, key string
		if e := u.tx.QueryRowContext(ctx, "SELECT c.document,c.attempt_id,c.asset_id,c.receipt_key,t.state='COMMITTED' AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='CANDIDATE' FROM harness_candidate_commits c JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND t.owner_subject_id=?", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&current, &attempt, &asset, &key, &registered); e != nil {
			return e
		}
		var stored draftCandidateCommit
		if json.Unmarshal(current, &stored) != nil || !reflect.DeepEqual(c, stored) || !registered || attempt != c.Metadata.Identity.AttemptID || asset != c.AssetID || key != c.ReceiptKey {
			return ErrDenied
		}
		business := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_COMMIT", CommandKey: c.ReceiptKey, CommitKey: c.ReceiptKey}
		_, business.PayloadHash, _ = harnessPayload(c.Metadata)
		if e := candidateHistoryReceipt(ctx, u, business); e != nil {
			return e
		}
		if !allOriginalUsageKnown(results) {
			if e := checkOriginalServiceMeteringTx(ctx, u, scope, task, turnIdentity(task, "service"), c.ReceiptKey, results); e != nil {
				return e
			}
		}
		if e := s.DraftCandidateOwner.InspectCandidateSourcesTx(ctx, u, scope, c.Binding); e != nil {
			return e
		}
		var original domain.DraftCandidateMaterial
		if e := u.tx.QueryRowContext(ctx, "SELECT binding FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&current); e != nil {
			return e
		}
		if json.Unmarshal(current, &original) != nil || len(original.Sources) == 0 {
			return ErrDenied
		}
		for i, m := range results {
			if e := s.inspectCandidateHistoryAttempt(ctx, u, scope, task, c, original, m, uint8(i)); e != nil {
				return e
			}
		}
		var state, storage, owner string
		if e := u.tx.QueryRowContext(ctx, "SELECT state,storage_key,owner_subject_id,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, c.AssetID).Scan(&state, &storage, &owner, &current); e != nil {
			return e
		}
		if generated.Decode("FileRecord", current, &out.Asset) != nil || state != "READY" || out.Asset.State != "READY" || storage != c.Metadata.BodyStorage || owner != scope.SubjectID || string(out.Asset.OwnerSubjectID) != owner || out.Asset.Scope.TenantID != scope.TenantID || out.Asset.Scope.ProductID != scope.ProductID || string(out.Asset.AssetID) != c.AssetID || out.Asset.Sha256 != c.Metadata.BodyHash || out.Asset.AudiencePolicy != "PRODUCT_EDITORS" {
			return ErrDenied
		}
		out.Results = results
		out.Sources, e = candidateSourceAssetsTx(ctx, u, scope, c.Binding)
		if e != nil {
			return e
		}
		return inspected
	})
	if !errors.Is(e, inspected) {
		if e == nil {
			e = ErrDenied
		}
		return ApprovedDraftCandidate{}, e
	}
	// Independent installation/head check occurs outside all database locks.
	for _, m := range results {
		current, e := s.ProviderEvidence.OriginalMetadata(ctx, m.Identity)
		if e != nil {
			return ApprovedDraftCandidate{}, e
		}
		if current != m {
			return ApprovedDraftCandidate{}, ErrDenied
		}
	}
	return out, nil
}

func candidateHistoryReceipt(ctx context.Context, u *unit, want BarrierRequest) error {
	if e := u.RequireLock(LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{want.CommitKey}}); e != nil {
		return e
	}
	row, e := queryReceipt(ctx, u.tx, want.CommitKey)
	if e != nil {
		return e
	}
	if !row.matches(want) {
		return ErrDenied
	}
	return nil
}

func (s *Store) inspectCandidateHistoryAttempt(ctx context.Context, u *unit, scope domain.ProductScope, task string, c draftCandidateCommit, original domain.DraftCandidateMaterial, m domain.MockResultMetadata, slot uint8) error {
	id := m.Identity
	d := DispatchRequest{BarrierRequest: BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_DISPATCH", CommandKey: id.MarkerKey, CommitKey: id.MarkerKey, phase: "SEND"}, AttemptID: id.AttemptID, AuthorizationID: id.AuthorizationID, RequestKey: id.RequestKey, RequestHash: mustArrayHash(id.RequestHash), Route: id.RouteID, HarnessQuoteID: id.QuoteID, Slot: slot}
	if id.MarkerKey != candidateSendKey(d) {
		return ErrDenied
	}
	a, e := readAttempt(ctx, u.tx, d)
	if e != nil {
		return e
	}
	if !a.matches(d) || !a.started.Valid || !a.marker.Valid || a.marker.String != id.MarkerKey {
		return ErrDenied
	}
	for _, h := range r0CandidateAccounts(scope, original.ProjectID) {
		if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
			h.ReservationID = turnIdentity(id.AttemptID, "supplier", h.Kind)
			d.Reservations = append(d.Reservations, h)
		}
	}
	d.PayloadHash, e = dispatchFingerprint(d)
	if e != nil {
		return e
	}
	if e = candidateHistoryReceipt(ctx, u, d.BarrierRequest); e != nil {
		return e
	}
	var frozen, identity, metaRaw, resultRaw []byte
	var hash, asset string
	if e = u.tx.QueryRowContext(ctx, "SELECT a.document,q.request_identity,q.metadata,r.source_binding,r.body_hash,r.asset_id FROM harness_candidate_attempts a JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_candidate_results r USING(tenant_id,product_id,task_id,attempt_id) JOIN file_staging f USING(tenant_id,product_id,storage_key) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=? AND r.storage_key=? AND r.delete_intent=0 AND f.state='ACTIVE'", scope.TenantID, scope.ProductID, task, id.AttemptID, m.BodyStorage).Scan(&frozen, &identity, &metaRaw, &resultRaw, &hash, &asset); e != nil {
		return e
	}
	var storedID domain.MockRequestIdentity
	var storedMeta domain.MockResultMetadata
	var bound, bodyBinding domain.DraftCandidateMaterial
	if json.Unmarshal(identity, &storedID) != nil || storedID != id || json.Unmarshal(metaRaw, &storedMeta) != nil || storedMeta != m || json.Unmarshal(resultRaw, &bodyBinding) != nil || hash != m.BodyHash || asset != c.AssetID {
		return ErrDenied
	}
	if slot == 0 {
		if json.Unmarshal(frozen, &bound) != nil || !sameCandidateBinding(bound, original) {
			return ErrDenied
		}
	} else {
		var snap candidateRepairSnapshot
		var plan candidateRepairPlan
		var raw []byte
		if e = u.tx.QueryRowContext(ctx, "SELECT document FROM harness_candidate_recoveries WHERE tenant_id=? AND product_id=? AND task_id=? AND extra_attempt_id=?", scope.TenantID, scope.ProductID, task, id.AttemptID).Scan(&raw); e != nil {
			return e
		}
		if json.Unmarshal(frozen, &snap) != nil || json.Unmarshal(raw, &plan) != nil || c.OriginalDependency == nil || snap.Original != *c.OriginalDependency || plan.Original != snap.Original || plan.Reason != "STRUCTURE_REPAIR" || plan.AttemptID != id.AttemptID || plan.ApprovalID != turnIdentity(task, "candidate-recovery") || snap.ApprovalID != plan.ApprovalID || !sameCandidateBinding(snap.Base, original) || !sameCandidateBinding(plan.Base, original) || !sameCandidateBinding(snap.Binding, plan.Binding) {
			return ErrDenied
		}
		bound = snap.Binding
		want := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_REPAIR_APPROVE", CommandKey: plan.ApprovalID, CommitKey: plan.ApprovalID}
		_, want.PayloadHash, _ = harnessPayload(plan.Original)
		if e = candidateHistoryReceipt(ctx, u, want); e != nil {
			return e
		}
	}
	if !sameCandidateBinding(bound, bodyBinding) || bound.RequestHash != id.RequestHash || bound.RouteHash != id.RouteID {
		return ErrDenied
	}
	if id.AttemptID == c.Metadata.Identity.AttemptID && !sameCandidateBinding(bound, c.Binding) {
		return ErrDenied
	}
	if e = s.DraftCandidateOwner.InspectCandidateSourcesTx(ctx, u, scope, bound); e != nil {
		return e
	}
	var code, validator, key string
	if e = u.tx.QueryRowContext(ctx, "SELECT metadata,body_hash,validation_code,validator_version,receipt_key FROM harness_original_validations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", scope.TenantID, scope.ProductID, task, id.AttemptID).Scan(&metaRaw, &hash, &code, &validator, &key); e != nil {
		return e
	}
	expectedCode := "STRUCTURE_VALID"
	if slot == 0 && c.OriginalDependency != nil {
		expectedCode = "STRUCTURE_INVALID"
	}
	if json.Unmarshal(metaRaw, &storedMeta) != nil || storedMeta != m || hash != m.BodyHash || code != expectedCode || validator != "R0_DRAFT_FIELDS_V1" || key != turnIdentity(id.AttemptID, "candidate-validation") {
		return ErrDenied
	}
	want := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "CANDIDATE_RESULT_VALIDATE", CommandKey: key, CommitKey: key}
	_, want.PayloadHash, _ = harnessPayload(struct {
		Metadata domain.MockResultMetadata
		Program  string
	}{m, "R0_DRAFT_FIELDS_V1"})
	return candidateHistoryReceipt(ctx, u, want)
}
