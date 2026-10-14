package memory

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

// WriteSelfConsentTx is the owning PRIVATE_RESPONSE permission write. It runs
// only inside the caller's sealed Studio transaction and never grants another
// subject, memory source, use, or administrator action.
func (s *MemoryService) WriteSelfConsentTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, enabled bool, expires time.Time) error {
	if e := store.CheckOperatorTx(ctx, tx, scope, "memory.generation.consent"); e != nil {
		return e
	}
	if e := tx.RequireLock(store.LockKey{Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "INSERT INTO memory_subject_permissions(tenant_id,product_id,caller_subject_id,data_subject_id,action,allowed_use,expires_at,enabled) VALUES(?,?,?,?,'memory.generation.use','PRIVATE_RESPONSE',?,?) ON DUPLICATE KEY UPDATE expires_at=VALUES(expires_at),enabled=VALUES(enabled)", scope.TenantID, scope.ProductID, scope.SubjectID, scope.SubjectID, expires, enabled); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, "UPDATE subject_governance SET permission_version=permission_version+1 WHERE tenant_id=? AND product_id=? AND subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID)
	return e
}
