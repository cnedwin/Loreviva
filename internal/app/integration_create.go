package app

import (
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"net/http"
)

// CREATE is a Service-owned metadata operation. It neither authenticates as
// an Operator nor grants the player's independent positive-use permission.
func (r *Runtime) createIntegrationInstance(w http.ResponseWriter, q *http.Request) {
	ctx, _, e := r.Store.AuthenticateIntegration(q.Context(), q.Header.Get("Authorization"), domain.IntegrationService)
	if e != nil {
		response(w, 401, map[string]any{"code": "UNAUTHENTICATED", "message": "Current Service authentication required.", "correlation_id": "local-request", "retryable": false, "recovery_action": "AUTHENTICATE"})
		return
	}
	profile := q.Header.Get("Loreviva-Client-Contract")
	media, _, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if e != nil || media != "application/json" || len(q.URL.Query()) != 0 || len(q.Header.Values("Loreviva-Client-Contract")) > 1 || profile != "" && profile != "public_v1" && profile != "integration_r0_v1" || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
		failure(w, wire.ErrJSON)
		return
	}
	q.Body = http.MaxBytesReader(w, q.Body, wire.MaxBytes)
	raw, e := io.ReadAll(q.Body)
	var request generated.CreateInstanceRequest
	if e != nil || generated.Decode("CreateInstanceRequest", raw, &request) != nil {
		failure(w, wire.ErrJSON)
		return
	}
	out, e := r.World.CreateIntegrationInstance(ctx, q.Header.Get("Idempotency-Key"), request)
	if e != nil {
		failure(w, e)
		return
	}
	response(w, http.StatusCreated, out)
}
