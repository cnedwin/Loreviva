package runtime

import (
	"context"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func (r *Runtime) CancelMigration(ctx context.Context, s domain.Scope, id, key string, q generated.AdminCancelRequest) (generated.AdminCancelReceipt, error) {
	var out generated.AdminCancelReceipt
	i, e := r.GetInstance(ctx, s)
	if e != nil {
		return out, e
	}
	v, e := versions(i.Versions)
	if e != nil {
		return out, e
	}
	var fence uint64
	e = r.Store.DB.QueryRowContext(ctx, "SELECT fence FROM tasks WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=? AND task_id=? AND task_kind='MIGRATION'", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID, id).Scan(&fence)
	if e != nil {
		return out, content.ErrNotFound
	}
	raw, e := content.BoundCommand(ctx, r.Store, s.ProductScope, "cancelInstanceMigration", key, id, "AdminCancelReceipt", q, []store.LockKey{{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}}}, store.CommandBarrierRequest{WorldScope: &s, Versions: &v, Purpose: store.MigrationPurpose, ExpectedWorldStatus: "PAUSED", Runtime: &store.RuntimeBinding{Action: "CANCEL_MIGRATION", TargetTaskID: id}}, func(tx store.Tx) (any, error) {
		if e := tx.CancelRuntimeMigration(ctx, id, fence); e != nil {
			return nil, e
		}
		return generated.AdminCancelReceipt{TaskID: generated.Identity(id), CancelState: "CANCEL_REQUESTED", CorrelationID: generated.Identity(id)}, nil
	})
	if e == nil {
		e = generated.Decode("AdminCancelReceipt", raw, &out)
	}
	return out, e
}
