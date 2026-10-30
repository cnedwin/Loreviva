package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func response(w http.ResponseWriter, status int, value any) {
	if binary, ok := value.(packageBinary); ok {
		if binary.metadata != "" {
			w.Header().Set("Loreviva-Package-File", binary.metadata)
		}
		w.Header().Set("Content-Type", binary.media)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+binary.name+"\"")
		w.Header().Set("Content-Length", strconv.Itoa(len(binary.body)))
		w.WriteHeader(status)
		_, _ = w.Write(binary.body)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Commit uncertainty directs recovery to the original key; this response does not establish rollback or authorize a replacement request.
func failure(w http.ResponseWriter, e error) {
	status := 503
	code := "DEPENDENCY_UNAVAILABLE"
	message := "Dependency unavailable; request not accepted."
	recovery := "WAIT_RETRY_AFTER"
	retry := true
	switch {
	case errors.Is(e, auth.ErrUnauthenticated):
		status = 401
		code = "UNAUTHENTICATED"
		message = "Operator authentication required."
		recovery = "AUTHENTICATE"
		retry = false
	case errors.Is(e, errUnsupportedMedia) || errors.Is(e, errArchiveTooLarge):
		status = 400
		code = "INVALID_REQUEST"
		message = "Request not accepted."
		recovery = "FIX_REQUEST"
		retry = false
	case errors.Is(e, auth.ErrForbidden) || errors.Is(e, store.ErrDenied):
		status = 403
		code = "FORBIDDEN"
		message = "Action not granted."
		recovery = "FIX_REQUEST"
		retry = false
	case errors.Is(e, auth.ErrNotFound) || errors.Is(e, content.ErrNotFound) || errors.Is(e, files.ErrNotFound):
		status = 404
		code = "NOT_FOUND"
		message = "Object missing or not visible."
		recovery = "FIX_REQUEST"
		retry = false
	case errors.Is(e, store.ErrConflict):
		status = 409
		code = "IDEMPOTENCY_CONFLICT"
		message = "Stable request conflicts with original."
		recovery = "USE_ORIGINAL_KEY"
		retry = false
	case errors.Is(e, store.ErrCommitUnknown):
		status = 409
		code = "EXTERNAL_UNKNOWN"
		message = "Local commit outcome unknown; retry original key to recover original result."
		recovery = "USE_ORIGINAL_KEY"
		retry = false
	case errors.Is(e, content.ErrBlocked) || errors.Is(e, files.ErrMetadata):
		status = 422
		code = "VALIDATION_FAILED"
		message = "Requested content operation is blocked."
		recovery = "FIX_REQUEST"
		retry = false
	case errors.Is(e, wire.ErrJSON) || errors.Is(e, memory.ErrInvalid):
		status = 400
		code = "INVALID_REQUEST"
		message = "Original request is invalid or exceeds limits."
		recovery = "FIX_REQUEST"
		retry = false
	}
	out := map[string]any{"code": code, "message": message, "correlation_id": "local-request", "retryable": retry, "recovery_action": recovery}
	var conflict *content.Conflict
	if errors.As(e, &conflict) {
		status = 409
		out["code"] = "SNAPSHOT_STALE"
		out["message"] = "Draft revision changed; preserve local edits and compare current revision."
		out["retryable"] = false
		out["recovery_action"] = "FIX_REQUEST"
		out["conflict"] = map[string]any{"draft_id": conflict.DraftID, "expected_revision": conflict.Expected, "current_revision": conflict.Current, "current_content_hash": conflict.Hash}
	}
	var fields *content.FieldErrors
	if errors.As(e, &fields) {
		status = 422
		out["code"] = "VALIDATION_FAILED"
		out["message"] = "Definition fields require correction."
		out["field_errors"] = fields.Findings
		out["retryable"] = false
		out["recovery_action"] = "FIX_REQUEST"
	}
	var pending *memory.PreservationPending
	if errors.As(e, &pending) {
		status = 503
		out["code"] = "DEPENDENCY_UNAVAILABLE"
		out["message"] = "Governance preservation unconfirmed; query the original task."
		out["retryable"] = false
		out["recovery_action"] = "QUERY_ORIGINAL_TASK"
		location := "/world/v1/admin/instances/" + url.PathEscape(pending.InstanceID) + "/memory-governance/" + url.PathEscape(pending.TaskID) + "?product_id=" + url.QueryEscape(pending.ProductID) + "&data_subject_id=" + url.QueryEscape(pending.DataSubjectID) + "&actor_instance_id=" + url.QueryEscape(pending.ActorID)
		w.Header().Set("Operation-Location", location)
		w.Header().Set("Governance-Location", location)
	}
	response(w, status, out)
}

type adminWork func(context.Context, domain.ProductScope, *http.Request, []byte) (int, any, error)

// After decoding the request, the entry action is checked and bound to a private HTTP proof; owning operations still enforce their transaction and source gates.
func (r *Runtime) admin(schema, action string, work adminWork) http.HandlerFunc {
	return r.adminWith(schema, action, work, false)
}

// adminControl serves a read-only report of what this runtime can do, and the
// controls an operator needs when it cannot do everything. The operator action
// is still required, but not every worker adapter has to be ready: reporting
// which jobs are unimplemented, or cancelling work on a degraded runtime, is
// exactly what an operator does when an adapter is missing.
func (r *Runtime) adminControl(schema, action string, work adminWork) http.HandlerFunc {
	return r.adminWith(schema, action, work, true)
}

func (r *Runtime) adminWith(schema, action string, work adminWork, skipReadiness bool) http.HandlerFunc {
	return func(w http.ResponseWriter, q *http.Request) {
		requestedAction := action
		for _, v := range q.URL.Query() {
			if len(v) != 1 {
				failure(w, wire.ErrJSON)
				return
			}
		}
		for _, name := range []string{"draft_id", "release_id", "task_id", "instance_id", "asset_id"} {
			if id := q.PathValue(name); id != "" && domain.ValidateID(id) != nil {
				failure(w, content.ErrNotFound)
				return
			}
		}
		product := q.URL.Query().Get("product_id")
		var raw []byte
		if schema != "" {
			kind, _, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
			if e != nil || kind != "application/json" {
				failure(w, wire.ErrJSON)
				return
			}
			q.Body = http.MaxBytesReader(w, q.Body, wire.MaxBytes)
			raw, e = io.ReadAll(q.Body)
			if e != nil || generated.Validate(schema, raw) != nil {
				failure(w, wire.ErrJSON)
				return
			}
			var data map[string]json.RawMessage
			if e = json.Unmarshal(raw, &data); e != nil {
				failure(w, wire.ErrJSON)
				return
			}
			_ = json.Unmarshal(data["product_id"], &product)
			if requestedAction == "content.command" {
				var command string
				_ = json.Unmarshal(data["action"], &command)
				switch command {
				case "REMOVE_BINDING":
					requestedAction = "content.edit"
				case "COPY":
					requestedAction = "content.copy"
				case "ARCHIVE":
					requestedAction = "content.archive"
				case "RESTORE":
					requestedAction = "content.restore"
				case "DELETE":
					requestedAction = "content.delete"
				default:
					failure(w, content.ErrBlocked)
					return
				}
			}
			if domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
				failure(w, wire.ErrJSON)
				return
			}
		}
		scope, e := r.Auth.Authorize(q.Context(), q.Header.Get("Authorization"), product, requestedAction)
		if e != nil {
			failure(w, e)
			return
		}
		originalGovernance := schema == "" && requestedAction == "memory.govern"
		if schema == "AdminMemoryGovernanceRequestV1" || schema == "AdminMemoryUsePolicyRequestV1" {
			originalGovernance = r.originalMemoryRecovery(q.Context(), scope, q, raw, schema)
		}
		if e = r.readiness(q.Context(), originalGovernance); e != nil && !skipReadiness {
			failure(w, e)
			return
		}
		ctx := q.Context()
		ctx, e = r.Store.AuthorizeHTTPAction(ctx, scope, requestedAction)
		if e != nil {
			failure(w, e)
			return
		}
		if raw != nil {
			ctx = content.WithOriginalPayload(ctx, raw)
		}
		status, value, e := work(ctx, scope, q, raw)
		if e != nil {
			failure(w, e)
			return
		}
		response(w, status, value)
	}
}
func decode[T any](raw []byte) T { var v T; _ = json.Unmarshal(raw, &v); return v }
func expected(v generated.ExactUnsigned) (uint64, error) {
	n, e := strconv.ParseUint(string(v), 10, 64)
	if e != nil {
		return 0, wire.ErrJSON
	}
	return n, nil
}
func labels(v []generated.SupportLabel) []string {
	out := []string{}
	for _, s := range v {
		out = append(out, string(s))
	}
	return out
}
func (r *Runtime) Handler() http.Handler {
	if r.Memory != nil {
		_ = r.Memory.BindIntegrationGovernance()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, q *http.Request) { response(w, 200, map[string]any{"status": "alive"}) })
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, q *http.Request) {
		if e := r.Readiness(q.Context()); e != nil {
			failure(w, e)
			return
		}
		response(w, 200, map[string]any{"status": "ready", "gate": "M1_FRESH_SYNTHETIC", "mock_only": true})
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, q *http.Request) {
		response(w, 200, map[string]any{"name": "Loreviva", "version": "0.0.0-r0", "contract_version": "1.4.0", "source_hash": generated.SourceHash, "mock_only": true})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		_, _ = io.WriteString(w, "<!doctype html><html lang=en><meta charset=utf-8><title>Loreviva</title><body><h1>Loreviva</h1><p>The AI Platform for Living Worlds</p><p>Synthetic Mock workspace.</p></body></html>")
	})
	root := "/world/v1/admin"
	mux.HandleFunc("GET "+root+"/content-drafts", r.admin("", "content.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		limit := pageOptions(q).Limit
		v, e := r.Content.ListDrafts(ctx, s, q.URL.Query().Get("project_id"), q.URL.Query().Get("root_kind"), q.URL.Query().Get("state"), limit, q.URL.Query().Get("cursor"))
		return 200, v, e
	}))
	mux.HandleFunc("POST "+root+"/content-drafts", r.admin("CreateDraftRequest", "content.edit", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v := decode[generated.CreateDraftRequest](b)
		project := ""
		if v.ProjectID != nil {
			project = string(*v.ProjectID)
		}
		d, e := r.Content.Create(ctx, s, string(v.RootKind), project, v.Input, q.Header.Get("Idempotency-Key"))
		return 201, d, e
	}))
	mux.HandleFunc("GET "+root+"/content-drafts/{draft_id}", r.admin("", "content.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		d, e := r.Content.GetDraft(ctx, s, q.PathValue("draft_id"), q.URL.Query().Get("revision"))
		return 200, d, e
	}))
	mux.HandleFunc("PUT "+root+"/content-drafts/{draft_id}", r.admin("SaveDraftRequest", "content.edit", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v := decode[generated.SaveDraftRequest](b)
		rev, e := expected(v.ExpectedDraftRevision)
		if e != nil {
			return 0, nil, e
		}
		d, e := r.Content.SaveDraft(ctx, s, q.PathValue("draft_id"), v.Input, rev, q.Header.Get("Idempotency-Key"))
		return 200, d, e
	}))
	mux.HandleFunc("GET "+root+"/content-drafts/{draft_id}/revisions", r.admin("", "content.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		v, e := r.Content.Revisions(ctx, s, q.PathValue("draft_id"), pageOptions(q))
		return 200, v, e
	}))
	mux.HandleFunc("POST "+root+"/content-drafts/{draft_id}/impact-checks", r.admin("ImpactRequest", "content.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v := decode[generated.ImpactRequest](b)
		rev, e := expected(v.ExpectedDraftRevision)
		if e != nil {
			return 0, nil, e
		}
		selected := []string{}
		for _, id := range v.ObjectIDS {
			selected = append(selected, string(id))
		}
		d, e := r.Content.Impact(ctx, s, q.PathValue("draft_id"), rev, string(v.Action), q.Header.Get("Idempotency-Key"), selected...)
		return 201, d, e
	}))
	mux.HandleFunc("POST "+root+"/content-drafts/{draft_id}/commands", r.admin("DraftCommandRequest", "content.command", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v := decode[generated.DraftCommandRequest](b)
		rev, e := expected(v.ExpectedDraftRevision)
		if e != nil {
			return 0, nil, e
		}
		restore := ""
		if v.RestoreRevision != nil {
			restore = string(*v.RestoreRevision)
		}
		name := ""
		if v.CopyName != nil {
			name = *v.CopyName
		}
		d, e := r.Content.Command(ctx, s, q.PathValue("draft_id"), rev, string(v.Action), string(v.ImpactReportID), restore, name, q.Header.Get("Idempotency-Key"))
		status := 200
		if v.Action == "COPY" {
			status = 201
		}
		return status, d, e
	}))
	mux.HandleFunc("POST "+root+"/content-drafts/{draft_id}/publish-checks", r.admin("PublishCheckRequest", "content.check", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v := decode[generated.PublishCheckRequest](b)
		rev, e := expected(v.ExpectedDraftRevision)
		if e != nil {
			return 0, nil, e
		}
		d, e := r.Content.CheckPublish(ctx, s, q.PathValue("draft_id"), rev, labels(v.RequestedSupportLabels), q.Header.Get("Idempotency-Key"))
		return 201, d, e
	}))
	mux.HandleFunc("POST "+root+"/content-drafts/{draft_id}/releases", r.admin("PublishRequest", "content.publish", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		v := decode[generated.PublishRequest](b)
		if len(v.RehearsalReportIDS) > 0 {
			return 0, nil, content.ErrBlocked
		}
		rev, e := expected(v.ExpectedDraftRevision)
		if e != nil {
			return 0, nil, e
		}
		d, e := r.Content.Publish(ctx, s, q.PathValue("draft_id"), rev, string(v.CheckReportID), labels(v.RequestedSupportLabels), q.Header.Get("Idempotency-Key"))
		return 201, d, e
	}))
	mux.HandleFunc("GET "+root+"/releases", r.admin("", "content.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		v, e := r.Content.ListReleases(ctx, s, q.URL.Query().Get("project_id"), q.URL.Query().Get("draft_id"), pageOptions(q))
		return 200, v, e
	}))
	mux.HandleFunc("GET "+root+"/releases/{release_id}", r.admin("", "content.read", func(ctx context.Context, s domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		v, e := r.Content.GetRelease(ctx, s, q.PathValue("release_id"))
		return 200, v, e
	}))
	r.taskRoutes(mux)
	r.adminReadRoutes(mux)
	r.settingsRoutes(mux)
	r.assetRoutes(mux)
	mux.HandleFunc("GET "+root+"/capabilities", r.adminControl("", "content.read", r.capabilities))
	r.packageRoutes(mux)
	r.instanceRoutes(mux)
	mux.HandleFunc("POST /world/v1/instances", r.createIntegrationInstance)
	r.integrationMemoryRoutes(mux)
	r.integrationSessionRoutes(mux)
	r.integrationRuntimeRoutes(mux)
	r.integrationDeliveryRoutes(mux)
	r.memoryRoutes(mux)
	r.unsupportedRoutes(mux)
	r.integrationCapabilitiesRoutes(mux)
	studio := studioHandler()
	// Static Studio and health/version routes bypass this operation wrapper. Other requests enter the runtime guard, and admin requests attach the authenticated principal before dispatch.
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/studio" || strings.HasPrefix(q.URL.Path, "/studio/") {
			studio.ServeHTTP(w, q)
			return
		}
		if q.URL.Path == "/health" || q.URL.Path == "/version" {
			mux.ServeHTTP(w, q)
			return
		}
		if deliveryPattern(q, mux) {
			mux.ServeHTTP(w, q)
			return
		}
		ctx, release, e := r.Store.EnterOperation(q.Context())
		if e != nil {
			failure(w, e)
			return
		}
		defer release()
		if strings.HasPrefix(q.URL.Path, "/world/v1/admin/") {
			ctx, _, e = r.Auth.Authenticate(ctx, q.Header.Get("Authorization"))
			if e != nil {
				failure(w, e)
				return
			}
		}
		if strings.HasPrefix(q.URL.Path, "/world/v1/assets/") {
			if next, _, authErr := r.Auth.Authenticate(ctx, q.Header.Get("Authorization")); authErr == nil {
				ctx = next
			}
		}
		mux.ServeHTTP(w, q.WithContext(ctx))
	})
}
func (r *Runtime) unsupportedRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /world/v1/admin/content-drafts/{draft_id}/candidates", r.admin("CandidateRequest", "content.assist", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		if r.Candidates == nil || r.Worker.Turns != r.Candidates.Executor || !r.Worker.Turns.CandidateReady() {
			return 0, nil, errors.New("candidate handler unavailable; not accepted")
		}
		out, e := r.Candidates.Submit(ctx, s, q.PathValue("draft_id"), q.Header.Get("Idempotency-Key"), decode[generated.CandidateRequest](b))
		return 202, out, e
	}))
	m.HandleFunc("POST /world/v1/admin/content-drafts/{draft_id}/rehearsals", r.admin("RehearsalRequest", "content.rehearse", func(ctx context.Context, s domain.ProductScope, q *http.Request, b []byte) (int, any, error) {
		if r.Worker == nil || r.Worker.Turns == nil || !r.Worker.Turns.RehearsalReady() {
			return 0, nil, errors.New("rehearsal handler unavailable; not accepted")
		}
		out, e := r.Store.AcceptDraftRehearsal(ctx, s, q.PathValue("draft_id"), q.Header.Get("Idempotency-Key"), decode[generated.RehearsalRequest](b))
		return 202, out, e
	}))
}

func pageOptions(q *http.Request) content.PageOptions {
	n, e := strconv.Atoi(q.URL.Query().Get("limit"))
	if e != nil && q.URL.Query().Get("limit") != "" {
		n = -1
	}
	return content.PageOptions{Limit: n, Cursor: q.URL.Query().Get("cursor")}
}
