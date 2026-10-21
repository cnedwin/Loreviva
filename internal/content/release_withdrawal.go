package content

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// WithdrawRelease is an internal owning port, not a public wire command. It
// returns only the original commit receipt; retained publication bodies stay private.
func (s *Service) WithdrawRelease(ctx context.Context, scope domain.ProductScope, releaseID, expectedContentHash, key string) (store.CommitReceipt, error) {
	return s.Store.WithdrawContentRelease(ctx, scope, releaseID, expectedContentHash, key)
}
