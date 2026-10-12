package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

// Safe metadata for the already authenticated task reader. This grants no byte
// access: current S55 source/read permissions are enforced by Files separately.
func (s *Store) ReadDraftCandidateTask(ctx context.Context, scope domain.ProductScope, task string) (generated.AdminTaskView, error) {
	var out generated.AdminTaskView
	if scope.Validate() != nil || domain.ValidateID(task) != nil {
		return out, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	return readDraftCandidateTaskRow(ctx, s.DB, scope, task)
}
func readDraftCandidateTaskRow(ctx context.Context, q rowQuerier, scope domain.ProductScope, task string) (generated.AdminTaskView, error) {
	var out generated.AdminTaskView
	var state, envelope string
	e := q.QueryRowContext(ctx, "SELECT t.state,e.state FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_admin_candidates a USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND a.owner_subject_id=t.owner_subject_id AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='CANDIDATE'", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&state, &envelope)
	if e != nil {
		return out, e
	}
	out = generated.AdminTaskView{Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, TaskID: generated.Identity(task), OwnerSubjectID: generated.Identity(scope.SubjectID), TaskKind: "CANDIDATE", TaskState: state, MockOnly: true, Findings: []generated.FieldError{}}
	if _, e = queryDraftCandidateCommitRow(ctx, q, scope, task); e == nil {
		var asset string
		if e = q.QueryRowContext(ctx, "SELECT asset_id FROM harness_candidate_commits WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&asset); e != nil {
			return out, e
		}
		if asset != turnIdentity(task, "candidate-asset") {
			return out, ErrConflict
		}
		var raw []byte
		if e = q.QueryRowContext(ctx, "SELECT document FROM harness_candidate_commits WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&raw); e != nil {
			return out, e
		}
		var c draftCandidateCommit
		if json.Unmarshal(raw, &c) != nil || c.AssetID != asset || state != "COMMITTED" {
			return out, ErrConflict
		}
		if e = inspectTaskResultAsset(ctx, q, scope, asset, c.Metadata.BodyStorage, c.Metadata.BodyHash); e != nil {
			return out, e
		}
		id := generated.Identity(asset)
		out.TaskState = "COMMITTED"
		out.ResultAssetID = &id
		return out, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if state == "COMMITTED" {
		return out, ErrConflict
	}
	if envelope != "CUTOFF" {
		return out, nil
	}
	var raw []byte
	var key string
	if e = q.QueryRowContext(ctx, "SELECT proof,receipt_key FROM harness_cutoffs WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&raw, &key); e != nil {
		return out, e
	}
	var proof cutoffProof
	want := domain.Scope{ProductScope: scope}
	if json.Unmarshal(raw, &proof) != nil || proof.Scope != want || proof.Task != task || proof.WorldReceipt != turnIdentity(task, "candidate-commit") || key != turnIdentity(task, "cutoff") || proof.CheckedAt.Before(proof.SettleBy) || proof.CheckedAt.Before(proof.ExecuteBy) {
		return out, ErrConflict
	}
	r, e := queryReceipt(ctx, q, key)
	if e != nil {
		return out, e
	}
	_, hash, e := harnessPayload(proof)
	if e != nil {
		return out, e
	}
	if !settlementReceiptScope(r, want, task) || r.operation != "HARNESS_CUTOFF" || !bytes.Equal(r.hash, hash[:]) {
		return out, ErrConflict
	}
	if state != "CANCELLED" && state != "REVOKED" {
		out.TaskState = "FAILED"
	}
	return out, nil
}

// Metadata binding only. This does not require or grant byte/read-use rights.
func inspectTaskResultAsset(ctx context.Context, q rowQuerier, scope domain.ProductScope, asset, storage, hash string) error {
	var actual, owner string
	var raw []byte
	if e := q.QueryRowContext(ctx, "SELECT owner_subject_id,storage_key,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, asset).Scan(&owner, &actual, &raw); e != nil {
		return e
	}
	var record generated.FileRecord
	if generated.Decode("FileRecord", raw, &record) != nil || owner != scope.SubjectID || actual != storage || len(hash) != 64 || string(record.AssetID) != asset || string(record.OwnerSubjectID) != scope.SubjectID || record.Scope != (generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}) || record.Sha256 != hash {
		return ErrConflict
	}
	return nil
}
