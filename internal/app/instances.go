package app

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	"net/http"
	"strconv"
)

func (r *Runtime) instanceRoutes(m *http.ServeMux) {
	scope := func(s domain.ProductScope, q *http.Request) domain.Scope {
		return domain.Scope{ProductScope: s, WorldInstanceID: q.PathValue("instance_id")}
	}
	m.HandleFunc("POST /world/v1/admin/instances/{instance_id}/migration-previews", r.admin("MigrationPreviewRequest", "runtime.migrate", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v, e := r.World.PreviewInstanceMigration(ctx, scope(s, q), decode[generated.MigrationPreviewRequest](b), q.Header.Get("Idempotency-Key"))
		return 201, v, e
	}))
	m.HandleFunc("POST /world/v1/admin/instances/{instance_id}/migrations", r.admin("MigrationExecuteRequest", "runtime.migrate", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v, e := r.World.ExecuteInstanceMigration(ctx, scope(s, q), decode[generated.MigrationExecuteRequest](b), q.Header.Get("Idempotency-Key"))
		return 202, v, e
	}))
	m.HandleFunc("POST /world/v1/admin/instances", r.admin("AdminCreateInstanceRequest", "runtime.create", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v, e := r.World.CreateInstance(ctx, s, world.CreateInstanceCommand{Request: decode[generated.AdminCreateInstanceRequest](b), Key: q.Header.Get("Idempotency-Key")})
		return 201, v, e
	}))
	m.HandleFunc("GET /world/v1/admin/instances", r.admin("", "runtime.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		n, _ := strconv.Atoi(q.URL.Query().Get("limit"))
		v, e := r.World.ListInstances(ctx, s, n, q.URL.Query().Get("cursor"))
		return 200, v, e
	}))
	m.HandleFunc("GET /world/v1/admin/instances/{instance_id}", r.admin("", "runtime.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		v, e := r.World.GetInstance(ctx, scope(s, q))
		return 200, v, e
	}))
	m.HandleFunc("GET /world/v1/admin/instances/{instance_id}/actors", r.admin("", "runtime.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		v, e := r.World.Actors(ctx, scope(s, q))
		return 200, v, e
	}))
	m.HandleFunc("POST /world/v1/admin/instances/{instance_id}/controls", r.admin("AdminControlRequest", "runtime.control", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v, e := r.World.Control(ctx, scope(s, q), world.ControlCommand{Request: decode[generated.AdminControlRequest](b), Key: q.Header.Get("Idempotency-Key")})
		return 202, v, e
	}))
}
