package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"mime"
	"net/http"
	"net/url"
	"strconv"
)

func (r *Runtime) adminReadRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /world/v1/admin/content-drafts/{draft_id}/generation-resolution", r.resolveDraftGeneration)
	m.HandleFunc("GET /world/v1/admin/tasks", r.listAdminTasks)
}
func (r *Runtime) resolveDraftGeneration(w http.ResponseWriter, q *http.Request) {
	kind, _, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if e != nil || kind != "application/json" || len(q.URL.Query()) != 0 || domain.ValidateID(q.PathValue("draft_id")) != nil {
		failure(w, wire.ErrJSON)
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, q.Body, wire.MaxBytes))
	if e != nil {
		failure(w, wire.ErrJSON)
		return
	}
	var in generated.DraftGenerationResolutionRequest
	if e = generated.Decode("DraftGenerationResolutionRequest", raw, &in); e != nil {
		failure(w, wire.ErrJSON)
		return
	}
	scope, e := r.Auth.AuthorizeDraftGenerationResolution(q.Context(), q.Header.Get("Authorization"), string(in.ProductID))
	if e != nil {
		failure(w, e)
		return
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return
	}
	revision, e := expected(in.ExpectedDraftRevision)
	if e != nil || revision == 0 {
		failure(w, wire.ErrJSON)
		return
	}
	v, e := r.Content.InspectDraftGeneration(q.Context(), scope, q.PathValue("draft_id"), revision, string(in.ObjectID), in.ModuleID)
	if e != nil {
		failure(w, e)
		return
	}
	bound := int64(store.MockOutputLimit)
	if v.Settings.Parameters != nil && v.Settings.Parameters.MaxOutputTokens != nil && *v.Settings.Parameters.MaxOutputTokens > 0 && *v.Settings.Parameters.MaxOutputTokens < bound {
		bound = *v.Settings.Parameters.MaxOutputTokens
	}
	support := generated.DraftGenerationSupport{InputUpperBound: "16000", OutputUpperBound: generated.ExactUnsigned(strconv.FormatInt(bound, 10)), MaxModelCalls: json.RawMessage("2"), MockOnly: true, SupportedFields: v.Fields}
	result := map[string]any{"scope": generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, "draft_id": v.Draft.DraftID, "object_id": v.ObjectID, "draft_revision": v.Draft.DraftRevision, "draft_hash": v.Draft.ContentHash, "module_id": v.ModuleID, "owner_kind": v.OwnerKind, "state": v.State, "support": support}
	if v.Settings.Mode != "" {
		result["effective_settings"] = v.Settings
	}
	if v.RouteHash != "" {
		result["route_hash"] = v.RouteHash
	}
	encoded, e := json.Marshal(result)
	if e == nil {
		e = generated.Validate("DraftGenerationResolution", encoded)
	}
	if e != nil {
		failure(w, e)
		return
	}
	if _, e = r.Auth.AuthorizeDraftGenerationResolution(q.Context(), q.Header.Get("Authorization"), string(in.ProductID)); e != nil {
		failure(w, e)
		return
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return
	}
	response(w, 200, json.RawMessage(encoded))
}

type adminListCursor struct {
	Scope              domain.ProductScope
	Stamp, Exact, Last string
	Limit              int
}

func (r *Runtime) adminCursorCipher() (cipher.AEAD, error) {
	r.adminCursorOnce.Do(func() {
		key := make([]byte, 32)
		if _, r.adminCursorErr = rand.Read(key); r.adminCursorErr != nil {
			return
		}
		var block cipher.Block
		block, r.adminCursorErr = aes.NewCipher(key)
		if r.adminCursorErr == nil {
			r.adminCursor, r.adminCursorErr = cipher.NewGCM(block)
		}
	})
	return r.adminCursor, r.adminCursorErr
}
func (r *Runtime) listAdminTasks(w http.ResponseWriter, q *http.Request) {
	values := q.URL.Query()
	for k, v := range values {
		if len(v) != 1 || (k != "product_id" && k != "task_id" && k != "limit" && k != "cursor") {
			failure(w, wire.ErrJSON)
			return
		}
	}
	product, exact, token := values.Get("product_id"), values.Get("task_id"), values.Get("cursor")
	limit := 50
	var e error
	if v, ok := values["limit"]; ok {
		limit, e = strconv.Atoi(v[0])
		if e != nil {
			failure(w, wire.ErrJSON)
			return
		}
	}
	if domain.ValidateID(product) != nil || (exact != "" && domain.ValidateID(exact) != nil) || limit < 1 || limit > 100 || len(token) > 2048 || (exact != "" && token != "") {
		failure(w, wire.ErrJSON)
		return
	}
	scope, e := r.Auth.AuthorizeAdminTaskList(q.Context(), q.Header.Get("Authorization"), product)
	if e != nil {
		failure(w, e)
		return
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return
	}
	cipher, e := r.adminCursorCipher()
	if e != nil {
		failure(w, e)
		return
	}
	cursor := adminListCursor{Scope: scope, Limit: limit, Exact: exact}
	if token != "" {
		blob, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(blob) < cipher.NonceSize() {
			failure(w, wire.ErrJSON)
			return
		}
		raw, err := cipher.Open(nil, blob[:cipher.NonceSize()], blob[cipher.NonceSize():], []byte("S59-admin-task-list"))
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Scope != scope || cursor.Limit != limit || cursor.Exact != exact || domain.ValidateID(cursor.Last) != nil {
			failure(w, wire.ErrJSON)
			return
		}
	}
	page, e := r.Store.ListAdminTaskMetadata(q.Context(), scope, store.AdminTaskSelection{ExactID: exact, After: cursor.Last, Limit: limit})
	if e != nil {
		failure(w, e)
		return
	}
	if token != "" && cursor.Stamp != page.CurrentStamp {
		failure(w, wire.ErrJSON)
		return
	}
	out := generated.AdminTaskPage{Items: []generated.AdminTaskListItem{}}
	for _, item := range page.Items {
		var value any
		if item.Kind == "TURN" {
			value = generated.AdminTaskListWorldItem{Family: "WORLD", Scope: generated.WorldScope{TenantID: scope.TenantID, ProductID: scope.ProductID, WorldInstanceID: item.World}, TaskID: generated.Identity(item.ID), TaskKind: "TURN", TaskState: item.State, QueryURL: "/world/v1/admin/tasks?product_id=" + url.QueryEscape(scope.ProductID) + "&task_id=" + url.QueryEscape(item.ID), AccountingStatus: generated.AdminTaskAccountingStatus{ServiceSettlementState: item.Accounting.Service, SupplierReconciliationState: item.Accounting.Supplier, PlatformRiskState: item.Accounting.Risk}}
		} else {
			value = generated.AdminTaskListAdminItem{Family: "ADMIN", Scope: generated.ProductScope{TenantID: scope.TenantID, ProductID: scope.ProductID}, TaskID: generated.Identity(item.ID), TaskKind: generated.AdminTaskKind(item.Kind), TaskState: item.State, QueryURL: "/world/v1/admin/tasks/" + url.PathEscape(item.ID) + "?product_id=" + url.QueryEscape(scope.ProductID)}
		}
		raw, err := json.Marshal(value)
		if err != nil || generated.Validate("AdminTaskListItem", raw) != nil {
			failure(w, store.ErrConflict)
			return
		}
		out.Items = append(out.Items, raw)
	}
	if page.More {
		cursor.Last = page.Last
		cursor.Stamp = page.CurrentStamp
		raw, err := json.Marshal(cursor)
		if err != nil {
			failure(w, err)
			return
		}
		nonce := make([]byte, cipher.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			failure(w, err)
			return
		}
		encoded := base64.RawURLEncoding.EncodeToString(cipher.Seal(nonce, nonce, raw, []byte("S59-admin-task-list")))
		out.NextCursor = &encoded
	}
	if _, e = r.Auth.AuthorizeAdminTaskList(q.Context(), q.Header.Get("Authorization"), product); e != nil {
		failure(w, e)
		return
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return
	}
	response(w, 200, out)
}

func (r *Runtime) readAdminTaskMetadata(w http.ResponseWriter, q *http.Request) {
	values := q.URL.Query()
	if len(values) != 1 || len(values["product_id"]) != 1 || domain.ValidateID(q.PathValue("task_id")) != nil {
		failure(w, wire.ErrJSON)
		return
	}
	scope, e := r.Auth.AuthorizeRegisteredAdminTaskRead(q.Context(), q.Header.Get("Authorization"), values.Get("product_id"))
	if e != nil {
		failure(w, e)
		return
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return
	}
	view, e := r.Store.ReadAdminTaskMetadata(q.Context(), scope, q.PathValue("task_id"))
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			e = content.ErrNotFound
		}
		failure(w, e)
		return
	}
	if _, e = r.Auth.AuthorizeRegisteredAdminTaskRead(q.Context(), q.Header.Get("Authorization"), values.Get("product_id")); e != nil {
		failure(w, e)
		return
	}
	if e = r.committedArtifactReadReady(q.Context()); e != nil {
		failure(w, e)
		return
	}
	raw, e := json.Marshal(view)
	if e == nil {
		e = generated.Validate("AdminTaskView", raw)
	}
	if e != nil {
		failure(w, e)
		return
	}
	response(w, 200, json.RawMessage(raw))
}
