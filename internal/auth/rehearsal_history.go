package auth

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
)

func (o *Operator) AuthorizeCommittedRehearsalTask(ctx context.Context, bearer, product, task string) (domain.ProductScope, error) {
	if e := o.committedReadIdentity(ctx, bearer, product); e != nil {
		return domain.ProductScope{}, e
	}
	var allowed bool
	if e := o.store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_actions WHERE credential_id=? AND action='task.read')", o.credentialID).Scan(&allowed); e != nil || !allowed {
		return domain.ProductScope{}, ErrForbidden
	}
	v, e := o.store.ReadDraftRehearsalTask(ctx, o.scope, task)
	if e != nil || v.TaskState != "COMMITTED" || v.ResultAssetID == nil {
		return domain.ProductScope{}, ErrNotFound
	}
	return o.scope, nil
}

// S57's fixed report asset target; no caller kind/history/purpose selector.
// Authentication is current, and the actual root COMMIT/source proof is
// independent of the old purchase permission and execution lease.
func (o *Operator) AuthorizeCommittedRehearsalReportAsset(ctx context.Context, bearer, product, asset string) (domain.ProductScope, error) {
	if e := o.committedReadIdentity(ctx, bearer, product); e != nil {
		return domain.ProductScope{}, e
	}
	if domain.ValidateID(asset) != nil {
		return domain.ProductScope{}, ErrNotFound
	}
	var allowed bool
	if e := o.store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_actions WHERE credential_id=? AND action='content.read') AND EXISTS(SELECT 1 FROM operator_actions WHERE credential_id=? AND action='asset.read')", o.credentialID, o.credentialID).Scan(&allowed); e != nil || !allowed {
		return domain.ProductScope{}, ErrForbidden
	}
	var task string
	e := o.store.DB.QueryRowContext(ctx, "SELECT r.task_id FROM harness_rehearsal_reports r JOIN assets a USING(tenant_id,product_id,asset_id) WHERE r.tenant_id=? AND r.product_id=? AND r.asset_id=? AND a.owner_subject_id=?", o.scope.TenantID, o.scope.ProductID, asset, o.scope.SubjectID).Scan(&task)
	if e != nil {
		return domain.ProductScope{}, ErrNotFound
	}
	proof, e := o.store.InspectCommittedRehearsalReport(ctx, o.scope, task)
	if e != nil || string(proof.Asset.AssetID) != asset {
		return domain.ProductScope{}, ErrNotFound
	}
	return o.scope, nil
}
