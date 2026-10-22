package app

import (
	"context"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"net/http"
)

func (r *Runtime) taskRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /world/v1/admin/tasks/{task_id}", r.readAdminTaskMetadata)
	m.HandleFunc("POST /world/v1/admin/tasks/{task_id}/cancel", r.adminControl("AdminCancelRequest", "task.cancel", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		id := q.PathValue("task_id")
		if _, _, e := r.Store.ReadRegisteredAdminTask(ctx, s, id); e != nil {
			return 0, nil, content.ErrNotFound
		}
		var migrationWorld string
		e := r.Store.DB.QueryRowContext(ctx, "SELECT COALESCE(world_instance_id,'') FROM tasks WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND task_id=? AND task_kind='MIGRATION'", s.TenantID, s.ProductID, s.SubjectID, id).Scan(&migrationWorld)
		if e == nil && migrationWorld != "" {
			out, e := r.World.CancelMigration(ctx, domain.Scope{ProductScope: s, WorldInstanceID: migrationWorld}, id, q.Header.Get("Idempotency-Key"), decode[generated.AdminCancelRequest](b))
			return 202, out, e
		}
		raw, e := content.Command(ctx, r.Store, s, "cancelAdminTask", q.Header.Get("Idempotency-Key"), id, "AdminCancelReceipt", decode[generated.AdminCancelRequest](b), []store.LockKey{{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}}}, func(tx store.Tx) (any, error) {
			if e := tx.CancelRegisteredTask(ctx, id); e != nil {
				return nil, e
			}
			return generated.AdminCancelReceipt{TaskID: generated.Identity(id), CancelState: "CANCEL_REQUESTED", CorrelationID: generated.Identity(id)}, nil
		})
		return 202, raw, e
	}))
}
