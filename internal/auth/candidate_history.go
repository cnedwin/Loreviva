package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"loreviva.local/loreviva/internal/domain"
	"strings"
)

// This private authentication prefix does not authorize an operation. Its
// fixed consumers require their actual committed target and their
// distinct current actions. General Operator.Check/Authorize are unchanged.
func (o *Operator) committedReadIdentity(ctx context.Context, bearer, product string) error {
	if o == nil || !o.resolved || o.scope.Validate() != nil || !strings.HasPrefix(bearer, "Bearer ") {
		return ErrUnauthenticated
	}
	h := sha256.Sum256([]byte(strings.TrimPrefix(bearer, "Bearer ")))
	if subtle.ConstantTimeCompare(h[:], o.configured[:]) != 1 {
		return ErrUnauthenticated
	}
	var stored []byte
	var enabled bool
	e := o.store.DB.QueryRowContext(ctx, "SELECT c.token_sha256,c.enabled AND t.enabled AND p.enabled AND s.enabled FROM operator_credentials c JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) JOIN subject_governance s ON s.tenant_id=c.tenant_id AND s.product_id=c.product_id AND s.subject_id=c.subject_id WHERE c.credential_id=? AND c.tenant_id=? AND c.product_id=? AND c.subject_id=?", o.credentialID, o.scope.TenantID, o.scope.ProductID, o.scope.SubjectID).Scan(&stored, &enabled)
	if e != nil || !enabled || subtle.ConstantTimeCompare(stored, o.configured[:]) != 1 {
		return ErrUnauthenticated
	}
	if product != o.scope.ProductID {
		return ErrNotFound
	}
	return nil
}

func (o *Operator) AuthorizeCommittedCandidateTask(ctx context.Context, bearer, product, task string) (domain.ProductScope, error) {
	if e := o.committedReadIdentity(ctx, bearer, product); e != nil {
		return domain.ProductScope{}, e
	}
	var allowed bool
	if e := o.store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_actions WHERE credential_id=? AND action='task.read')", o.credentialID).Scan(&allowed); e != nil || !allowed {
		return domain.ProductScope{}, ErrForbidden
	}
	v, e := o.store.ReadDraftCandidateTask(ctx, o.scope, task)
	if e != nil || v.TaskState != "COMMITTED" || v.ResultAssetID == nil {
		return domain.ProductScope{}, ErrNotFound
	}
	return o.scope, nil
}

func (o *Operator) AuthorizeCommittedCandidateAsset(ctx context.Context, bearer, product, asset string) (domain.ProductScope, error) {
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
	e := o.store.DB.QueryRowContext(ctx, "SELECT c.task_id FROM harness_candidate_commits c JOIN assets a ON a.tenant_id=c.tenant_id AND a.product_id=c.product_id AND a.asset_id=c.asset_id WHERE c.tenant_id=? AND c.product_id=? AND c.asset_id=? AND a.owner_subject_id=?", o.scope.TenantID, o.scope.ProductID, asset, o.scope.SubjectID).Scan(&task)
	if e != nil {
		return domain.ProductScope{}, ErrNotFound
	}
	v, e := o.store.ReadDraftCandidateTask(ctx, o.scope, task)
	if e != nil || v.TaskState != "COMMITTED" || v.ResultAssetID == nil || string(*v.ResultAssetID) != asset {
		return domain.ProductScope{}, ErrNotFound
	}
	return o.scope, nil
}
