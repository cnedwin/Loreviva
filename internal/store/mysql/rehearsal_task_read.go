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

// Safe task metadata, independently authenticated by the actual task.read
// consumer. A report reference never grants Content/Memory/Files body rights.
func (s *Store) ReadDraftRehearsalTask(ctx context.Context, scope domain.ProductScope, task string) (out generated.AdminTaskView, err error) {
	if scope.Validate() != nil || domain.ValidateID(task) != nil {
		return out, ErrDenied
	}
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	return readDraftRehearsalTaskRow(ctx, s.DB, scope, task)
}
func readDraftRehearsalTaskRow(ctx context.Context, q rowQuerier, scope domain.ProductScope, task string) (out generated.AdminTaskView, err error) {
	var state string
	e := q.QueryRowContext(ctx, "SELECT t.state FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND a.owner_subject_id=t.owner_subject_id AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='REHEARSAL'", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&state)
	if e != nil {
		return out, e
	}
	out = generated.AdminTaskView{Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, TaskID: generated.Identity(task), OwnerSubjectID: generated.Identity(scope.SubjectID), TaskKind: "REHEARSAL", TaskState: state, MockOnly: true, Findings: []generated.FieldError{}}
	if _, e = queryRehearsalReportCommitRow(ctx, q, scope, task); e == nil {
		if state != "COMMITTED" {
			return out, ErrConflict
		}
		id := generated.Identity(turnIdentity(task, "report-asset"))
		var storage, hash string
		if e = q.QueryRowContext(ctx, "SELECT storage_key,body_hash FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=? AND asset_id=?", scope.TenantID, scope.ProductID, task, id).Scan(&storage, &hash); e != nil {
			return out, e
		}
		if e = inspectTaskResultAsset(ctx, q, scope, string(id), storage, hash); e != nil {
			return out, e
		}
		out.ResultAssetID = &id
		return out, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if state == "COMMITTED" {
		return out, ErrConflict
	}
	var envelope string
	var generation uint64
	if e = q.QueryRowContext(ctx, "SELECT state,narrative_generation FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&envelope, &generation); e != nil {
		return out, e
	}
	if envelope == "CUTOFF" {
		key := turnIdentity(task, "cutoff")
		var raw []byte
		if e = q.QueryRowContext(ctx, "SELECT proof FROM harness_cutoffs WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=? AND narrative_generation=?", scope.TenantID, scope.ProductID, task, key, generation).Scan(&raw); e != nil {
			return out, e
		}
		var proof rehearsalSettlementProof
		if json.Unmarshal(raw, &proof) != nil || proof.State != "CUTOFF" || proof.Root.Scope != (domain.Scope{ProductScope: scope}) || proof.Root.Task != task || proof.Root.NarrativeGeneration != generation || proof.Root.CheckedAt.Before(proof.Root.SettleBy) || proof.Root.CheckedAt.Before(proof.Root.ExecuteBy) {
			return out, ErrConflict
		}
		_, hash, e := harnessPayload(proof)
		if e != nil {
			return out, e
		}
		receipt, e := queryReceipt(ctx, q, key)
		if e != nil {
			return out, e
		}
		if !settlementReceiptScope(receipt, proof.Root.Scope, task) || receipt.operation != "REHEARSAL_CUTOFF" || receipt.command != key || !bytes.Equal(receipt.hash, hash[:]) {
			return out, ErrConflict
		}
		if state != "CANCELLED" {
			out.TaskState = "FAILED"
		}
	}
	return out, nil
}
