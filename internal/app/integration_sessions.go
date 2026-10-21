package app

import (
	"context"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"net/http"
)

// The selector is a closed wire profile, never a capability grant. Missing
// means public_v1, and a current CLIENT may not switch its original profile.
func integrationProfile(q *http.Request) (string, error) {
	p := q.Header.Get("Loreviva-Client-Contract")
	if p == "" {
		p = "public_v1"
	}
	if len(q.Header.Values("Loreviva-Client-Contract")) > 1 || (p != "public_v1" && p != "integration_r0_v1") {
		return "", wire.ErrJSON
	}
	return p, nil
}
func integrationBody(w http.ResponseWriter, q *http.Request, schema string) ([]byte, error) {
	media, _, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || len(q.URL.Query()) != 0 || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
		return nil, wire.ErrJSON
	}
	q.Body = http.MaxBytesReader(w, q.Body, wire.MaxBytes)
	raw, e := io.ReadAll(q.Body)
	if e != nil || generated.Validate(schema, raw) != nil {
		return nil, wire.ErrJSON
	}
	return raw, nil
}
func integrationUnauthorized(w http.ResponseWriter) {
	response(w, 401, map[string]any{"code": "UNAUTHENTICATED", "message": "Current matching integration authentication required.", "correlation_id": "local-request", "retryable": false, "recovery_action": "AUTHENTICATE"})
}
func (r *Runtime) integrationClient(w http.ResponseWriter, q *http.Request) (context.Context, bool) {
	profile, e := integrationProfile(q)
	if e != nil {
		failure(w, e)
		return nil, false
	}
	ctx, p, e := r.Store.AuthenticateIntegration(q.Context(), q.Header.Get("Authorization"), domain.IntegrationClient)
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
func (r *Runtime) integrationSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /world/v1/instances/{instance_id}/memories/{memory_id}", r.handleIntegrationMemory)
	mux.HandleFunc("POST /world/v1/client-sessions", func(w http.ResponseWriter, q *http.Request) {
		profile, e := integrationProfile(q)
		if e != nil {
			failure(w, e)
			return
		}
		ctx, _, e := r.Store.AuthenticateIntegration(q.Context(), q.Header.Get("Authorization"), domain.IntegrationService)
		if e != nil {
			integrationUnauthorized(w)
			return
		}
		raw, e := integrationBody(w, q, "ClientSessionRequest")
		if e != nil {
			failure(w, e)
			return
		}
		var input generated.ClientSessionRequest
		if generated.Decode("ClientSessionRequest", raw, &input) != nil {
			failure(w, wire.ErrJSON)
			return
		}
		out, e := r.Store.ExchangeIntegrationSession(ctx, q.Header.Get("Idempotency-Key"), profile, input)
		if e != nil {
			failure(w, e)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		response(w, 201, out)
	})
	mux.HandleFunc("GET /world/v1/self-consent", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationClient(w, q)
		if !ok {
			return
		}
		if len(q.URL.Query()) != 0 {
			failure(w, wire.ErrJSON)
			return
		}
		out, e := r.Store.ReadPlayerSelfConsent(ctx)
		if e != nil {
			failure(w, e)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		response(w, 200, out)
	})
	mux.HandleFunc("POST /world/v1/self-consent/commands", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationClient(w, q)
		if !ok {
			return
		}
		raw, e := integrationBody(w, q, "PlayerSelfConsentCommandRequest")
		if e != nil {
			failure(w, e)
			return
		}
		out, e := r.Store.ApplyPlayerSelfConsent(ctx, q.Header.Get("Idempotency-Key"), raw)
		if e != nil {
			failure(w, e)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		response(w, 200, out)
	})
}
