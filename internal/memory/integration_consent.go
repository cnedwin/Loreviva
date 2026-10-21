package memory

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

// This typed writer grants only the real player's PRIVATE_RESPONSE use. Store
// checks the sealed same-Tx choice before executing its two exact row writes.
// It cannot grant a source, another subject, PUBLIC_RESPONSE or RESEARCH use.
func (s *MemoryService) WritePlayerSelfConsentTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, enabled bool, expires time.Time) error {
	return s.Store.WriteIntegrationConsentPermissionTx(ctx, tx, scope, enabled, expires)
}
