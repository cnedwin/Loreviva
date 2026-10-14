package auth

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
)

// Committed task metadata and asset access use different checks: the asset path requires current content/asset actions and the matching committed task target.
func (o *Operator) AuthorizeCommittedCandidateTask(ctx context.Context, bearer, product, task string) (domain.ProductScope, error) {
	ctx, o, e := o.committedReadOperator(ctx, bearer, product)
	if e != nil {
		return domain.ProductScope{}, e
	}
	if e := o.store.CheckOperator(ctx, o.scope, "task.read"); e != nil {
		return domain.ProductScope{}, ErrForbidden
	}
	v, e := o.store.ReadDraftCandidateTask(ctx, o.scope, task)
	if e != nil || v.TaskState != "COMMITTED" || v.ResultAssetID == nil {
		return domain.ProductScope{}, ErrNotFound
	}
	return o.scope, nil
}

func (o *Operator) AuthorizeCommittedCandidateAsset(ctx context.Context, bearer, product, asset string) (domain.ProductScope, error) {
	ctx, o, e := o.committedReadOperator(ctx, bearer, product)
	if e != nil {
		return domain.ProductScope{}, e
	}
	if domain.ValidateID(asset) != nil {
		return domain.ProductScope{}, ErrNotFound
	}
	if e := o.store.CheckOperator(ctx, o.scope, "content.read", "asset.read"); e != nil {
		return domain.ProductScope{}, ErrForbidden
	}
	var task string
	e = o.store.DB.QueryRowContext(ctx, "SELECT c.task_id FROM harness_candidate_commits c JOIN assets a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.asset_id=c.asset_id WHERE c.tenant_id=? AND c.product_id=? AND c.asset_id=? AND a.owner_subject_id=?", o.scope.TenantID, o.scope.ProductID, asset, o.scope.SubjectID).Scan(&task)
	if e != nil {
		return domain.ProductScope{}, ErrNotFound
	}
	v, e := o.store.ReadDraftCandidateTask(ctx, o.scope, task)
	if e != nil || v.TaskState != "COMMITTED" || v.ResultAssetID == nil || string(*v.ResultAssetID) != asset {
		return domain.ProductScope{}, ErrNotFound
	}
	return o.scope, nil
}
