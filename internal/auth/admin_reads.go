package auth

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
)

// Fixed S59 readers authenticate against the same current credential registry.
// Neither method mints execution, asset/body, or cost authority.
func (o *Operator) AuthorizeDraftGenerationResolution(ctx context.Context, bearer, product string) (domain.ProductScope, error) {
	ctx, o, e := o.committedReadOperator(ctx, bearer, product)
	if e != nil {
		return domain.ProductScope{}, e
	}
	if e := o.store.CheckOperator(ctx, o.scope, "content.read"); e != nil {
		return domain.ProductScope{}, ErrForbidden
	}
	return o.scope, nil
}
func (o *Operator) AuthorizeAdminTaskList(ctx context.Context, bearer, product string) (domain.ProductScope, error) {
	return o.adminTaskReadIdentity(ctx, bearer, product)
}

// S60 admits only metadata. The Store subsequently proves the exact registered
// ADMIN target; neither this authentication nor its result grants byte access.
func (o *Operator) AuthorizeRegisteredAdminTaskRead(ctx context.Context, bearer, product string) (domain.ProductScope, error) {
	return o.adminTaskReadIdentity(ctx, bearer, product)
}
func (o *Operator) adminTaskReadIdentity(ctx context.Context, bearer, product string) (domain.ProductScope, error) {
	ctx, o, e := o.committedReadOperator(ctx, bearer, product)
	if e != nil {
		return domain.ProductScope{}, e
	}
	if e := o.store.CheckOperator(ctx, o.scope, "task.read"); e != nil {
		return domain.ProductScope{}, ErrForbidden
	}
	return o.scope, nil
}
