package app

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"net/http"
)

// Kind selection is checked by the real authenticator. A SERVICE cannot use
// the CLIENT fallback to gain a player's body, self-consent or TURN authority.
func (r *Runtime) integrationRuntimeCaller(w http.ResponseWriter, q *http.Request, serviceOnly bool) (context.Context, bool) {
	profile, e := integrationProfile(q)
	if e != nil {
		failure(w, e)
		return nil, false
	}
	ctx, p, e := r.Store.AuthenticateIntegration(q.Context(), q.Header.Get("Authorization"), domain.IntegrationService)
	if e == nil {
		return ctx, true
	}
	if serviceOnly {
		integrationUnauthorized(w)
		return nil, false
	}
	ctx, p, e = r.Store.AuthenticateIntegration(q.Context(), q.Header.Get("Authorization"), domain.IntegrationClient)
	if e != nil {
		integrationUnauthorized(w)
		return nil, false
	}
	if p.Profile != profile {
		response(w, 409, map[string]any{"code": "VIEW_RESET", "message": "A new Session is required for a different profile.", "correlation_id": "local-request", "retryable": false, "recovery_action": "RESET_VIEW"})
		return nil, false
	}
	return ctx, true
}

func (r *Runtime) integrationRuntimeRoutes(m *http.ServeMux) {
	// Existing task_id wire locator has the source TurnId meaning; register once.
	m.HandleFunc(turnPattern, r.integrationDeliveryHTTP)

	m.HandleFunc("GET /world/v1/tasks/{task_id}", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationRuntimeCaller(w, q, false)
		if !ok {
			return
		}
		if len(q.URL.Query()) != 0 {
			failure(w, wire.ErrJSON)
			return
		}
		out, e := r.Store.ReadIntegrationTask(ctx, q.PathValue("task_id"))
		if errors.Is(e, sql.ErrNoRows) {
			e = content.ErrNotFound
		}
		if e != nil {
			failure(w, e)
			return
		}
		response(w, 200, out)
	})
	for _, path := range []string{"POST /world/v1/tasks/{task_id}/cancel", "POST /world/v1/turns/{task_id}/cancel"} {
		m.HandleFunc(path, func(w http.ResponseWriter, q *http.Request) {
			var ctx context.Context
			var ok bool
			if path == "POST /world/v1/turns/{task_id}/cancel" {
				// The TURN wire never inherits SERVICE's separate CONTROL task authority.
				ctx, ok = r.integrationClient(w, q)
				if !ok {
					return
				}
				if _, e := r.Store.IntegrationTurnScope(ctx, q.PathValue("task_id")); e != nil {
					failure(w, e)
					return
				}
			} else {
				ctx, ok = r.integrationRuntimeCaller(w, q, false)
				if !ok {
					return
				}
			}
			body, e := integrationBody(w, q, "CancelRequest")
			if e != nil {
				failure(w, e)
				return
			}
			out, e := r.Store.CancelIntegrationTask(ctx, q.PathValue("task_id"), q.Header.Get("Idempotency-Key"), decode[generated.CancelRequest](body))
			if errors.Is(e, sql.ErrNoRows) {
				e = content.ErrNotFound
			}
			if e != nil {
				failure(w, e)
				return
			}
			response(w, 202, out)
		})
	}
	m.HandleFunc("POST /world/v1/instances/{instance_id}/controls", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationRuntimeCaller(w, q, false)
		if !ok {
			return
		}
		b, e := integrationBody(w, q, "ControlRequest")
		if e != nil {
			failure(w, e)
			return
		}
		out, e := r.World.ControlIntegrationInstance(ctx, q.PathValue("instance_id"), q.Header.Get("Idempotency-Key"), decode[generated.ControlRequest](b))
		if e != nil {
			failure(w, e)
			return
		}
		response(w, 202, out)
	})
	m.HandleFunc("POST /world/v1/quotes", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationRuntimeCaller(w, q, true)
		if !ok {
			return
		}
		b, e := integrationBody(w, q, "QuoteRequest")
		if e != nil {
			failure(w, e)
			return
		}
		out, e := r.Store.QuoteIntegrationTurn(ctx, q.Header.Get("Idempotency-Key"), decode[generated.QuoteRequest](b))
		if e != nil {
			failure(w, e)
			return
		}
		response(w, 201, out)
	})
	m.HandleFunc("POST /world/v1/instances/{instance_id}/turns", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationClient(w, q)
		if !ok {
			return
		}
		b, e := integrationBody(w, q, "TurnRequest")
		if e != nil {
			failure(w, e)
			return
		}
		out, e := r.Store.AcceptIntegrationTurn(ctx, q.PathValue("instance_id"), q.Header.Get("Idempotency-Key"), decode[generated.TurnRequest](b))
		if e != nil {
			failure(w, e)
			return
		}
		response(w, 202, out)
	})
	m.HandleFunc("GET /world/v1/instances/{instance_id}/turn-intake-receipt", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationClient(w, q)
		if !ok {
			return
		}
		if len(q.URL.Query()) != 0 || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
			failure(w, wire.ErrJSON)
			return
		}
		out, e := r.Store.ReadIntegrationIntakeReceipt(ctx, q.PathValue("instance_id"), q.Header.Get("Idempotency-Key"))
		if errors.Is(e, sql.ErrNoRows) {
			e = content.ErrNotFound
		}
		if e != nil {
			failure(w, e)
			return
		}
		response(w, 200, out)
	})
}
