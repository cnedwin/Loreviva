package maintenance

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func InitializeLocalAdministration(ctx context.Context, s *store.Store) error {
	if e := Compatible(ctx, s); e != nil {
		return e
	}
	return s.InitializeLocalAdministration(ctx)
}
func RegisterLocalCredential(ctx context.Context, s *store.Store, scope domain.ProductScope, id, secret string) error {
	if e := Compatible(ctx, s); e != nil {
		return e
	}
	return s.RegisterLocalCredential(ctx, scope, id, secret)
}
