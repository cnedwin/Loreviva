package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/settings"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"net/http"
	"strconv"
)

// Reject unknown or repeated query keys and enforce the exact TENANT/PRODUCT shape.
// Parsing this scope does not authorize it; the owning operation checks current authority.
func settingsScope(q *http.Request, extra ...string) (domain.SettingsScope, error) {
	values := q.URL.Query()
	allowed := map[string]bool{"scope_type": true, "tenant_id": true, "product_id": true}
	for _, key := range extra {
		allowed[key] = true
	}
	for key, v := range values {
		if !allowed[key] || len(v) != 1 {
			return domain.SettingsScope{}, wire.ErrJSON
		}
	}
	sc := domain.SettingsScope{Kind: values.Get("scope_type"), TenantID: values.Get("tenant_id")}
	if values.Has("product_id") {
		v := values.Get("product_id")
		sc.ProductID = &v
	}
	if sc.Validate() != nil {
		return sc, wire.ErrJSON
	}
	return sc, nil
}
// A stale revision and a changed registration both require a fresh snapshot, but remain distinct conflicts.
// Missing rows become not-found without exposing stored data.
func settingsFailure(w http.ResponseWriter, e error) {
	var conflict *store.StudioConflict
	if errors.As(e, &conflict) {
		response(w, 409, map[string]any{"code": "SNAPSHOT_STALE", "message": "Settings changed; preserve local edits and compare the current snapshot.", "correlation_id": "studio-settings", "retryable": false, "recovery_action": "FETCH_NEW_SNAPSHOT", "conflict": map[string]any{"target": conflict.Target, "expected_revision": strconv.FormatUint(conflict.Expected, 10), "current_revision": strconv.FormatUint(conflict.Current, 10)}})
		return
	}
	if errors.Is(e, store.ErrReplan) {
		response(w, 409, map[string]any{"code": "VIEW_RESET", "message": "Current registration changed; fetch a new snapshot.", "correlation_id": "studio-settings", "retryable": false, "recovery_action": "FETCH_NEW_SNAPSHOT"})
		return
	}
	if errors.Is(e, sql.ErrNoRows) {
		e = auth.ErrNotFound
	}
	failure(w, e)
}
func (r *Runtime) settingsRoutes(m *http.ServeMux) {
	service := settings.New(r.Store)
	handle := func(work func(*http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, q *http.Request) {
			// Ordinary settings depend on local persistence and governance, never on a
			// provider probe, model availability or a configured home-product owner.
			if e := maintenance.Compatible(q.Context(), r.Store); e != nil {
				settingsFailure(w, e)
				return
			}
			if e := maintenance.GovernanceReady(q.Context(), r.Store); e != nil {
				settingsFailure(w, e)
				return
			}
			value, e := work(q)
			if e != nil {
				settingsFailure(w, e)
				return
			}
			response(w, 200, value)
		}
	}
	m.HandleFunc("GET /world/v1/admin/settings", handle(func(q *http.Request) (any, error) {
		sc, e := settingsScope(q)
		if e != nil {
			return nil, e
		}
		return service.Read(q.Context(), sc)
	}))
	m.HandleFunc("GET /world/v1/admin/settings/members", handle(func(q *http.Request) (any, error) {
		sc, e := settingsScope(q, "limit", "cursor")
		if e != nil {
			return nil, e
		}
		limit := 50
		if raw := q.URL.Query().Get("limit"); raw != "" {
			limit, e = strconv.Atoi(raw)
			if e != nil || limit < 1 || limit > 100 {
				return nil, wire.ErrJSON
			}
		}
		return service.Members(q.Context(), sc, limit, q.URL.Query().Get("cursor"))
	}))
	m.HandleFunc("GET /world/v1/admin/settings/self-consent", handle(func(q *http.Request) (any, error) {
		sc, e := settingsScope(q)
		if e != nil {
			return nil, e
		}
		if sc.Kind != "PRODUCT" {
			return nil, wire.ErrJSON
		}
		return service.SelfConsent(q.Context(), sc)
	}))
	m.HandleFunc("GET /world/v1/admin/settings/commands/{command_key}", handle(func(q *http.Request) (any, error) {
		sc, e := settingsScope(q, "family")
		if e != nil {
			return nil, e
		}
		family := q.URL.Query().Get("family")
		if family != "SETTINGS" && family != "MEMBER" && family != "SELF_CONSENT" || domain.ValidateIdempotencyKey(q.PathValue("command_key")) != nil {
			return nil, wire.ErrJSON
		}
		return service.Receipt(q.Context(), sc, family, q.PathValue("command_key"))
	}))
	commands := []struct {
		path, schema string
		run          func(*http.Request, json.RawMessage) (any, error)
	}{
		{"/world/v1/admin/settings/commands", "StudioSettingsCommandRequest", func(q *http.Request, b json.RawMessage) (any, error) {
			return service.Command(q.Context(), q.Header.Get("Idempotency-Key"), b)
		}},
		{"/world/v1/admin/settings/members/commands", "StudioMemberCommandRequest", func(q *http.Request, b json.RawMessage) (any, error) {
			return service.MemberCommand(q.Context(), q.Header.Get("Idempotency-Key"), b)
		}},
		{"/world/v1/admin/settings/self-consent/commands", "StudioSelfConsentCommandRequest", func(q *http.Request, b json.RawMessage) (any, error) {
			return service.ConsentCommand(q.Context(), q.Header.Get("Idempotency-Key"), b)
		}},
	}
	for _, command := range commands {
		m.HandleFunc("POST "+command.path, handle(func(q *http.Request) (any, error) {
			media, _, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
			if e != nil || media != "application/json" || len(q.URL.Query()) != 0 || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
				return nil, wire.ErrJSON
			}
			raw, e := io.ReadAll(io.LimitReader(q.Body, wire.MaxBytes+1))
			if e != nil || len(raw) > wire.MaxBytes {
				return nil, wire.ErrJSON
			}
			var validated json.RawMessage
			if e = generated.Decode(command.schema, raw, &validated); e != nil {
				return nil, wire.ErrJSON
			}
			return command.run(q, validated)
		}))
	}
}
