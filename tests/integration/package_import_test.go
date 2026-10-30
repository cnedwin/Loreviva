package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func importOptions(t *testing.T, f packageFixture, archive []byte) map[string]any {
	t.Helper()
	mapping := resolveNoteOptions(t, f, archive)
	resolved := previewPost(t, f, archive, mapping, f.token)
	var report map[string]any
	json.Unmarshal(resolved.Body.Bytes(), &report)
	if resolved.Code != 200 || report["status"] != "RESOLVED" {
		t.Fatalf("import prerequisite %d: %s", resolved.Code, resolved.Body.String())
	}
	return map[string]any{"mapping": mapping, "expected_preview_sha256": report["preview_sha256"]}
}
func importPost(t *testing.T, f packageFixture, archive []byte, options any, key string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, e := writer.CreateFormFile("archive", "world.wepkg")
	if e != nil {
		t.Fatal(e)
	}
	part.Write(archive)
	writer.WriteField("options", string(mustPreviewJSON(t, options)))
	writer.Close()
	q, _ := http.NewRequestWithContext(ctx, "POST", f.server.URL+"/world/v1/admin/content-packages/imports?product_id=coffee", &body)
	q.Header.Set("Authorization", "Bearer "+f.token)
	q.Header.Set("Idempotency-Key", key)
	q.Header.Set("Content-Type", writer.FormDataContentType())
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, q)
	return out
}
func importedResult(t *testing.T, out *httptest.ResponseRecorder) generated.PackageImportResult {
	t.Helper()
	if out.Code != 201 {
		t.Fatalf("import %d: %s", out.Code, out.Body.String())
	}
	var r generated.PackageImportResult
	if e := generated.Decode("PackageImportResult", out.Body.Bytes(), &r); e != nil {
		t.Fatal(e)
	}
	return r
}
func importRows(t *testing.T, f packageFixture) []int {
	t.Helper()
	out := []int{}
	for _, table := range []string{"content_package_imports", "content_package_import_lineage", "content_drafts", "content_revisions", "commands", "admin_results", "commit_receipts", "tasks"} {
		var n int
		if e := f.store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		out = append(out, n)
	}
	return out
}
func importDraft(t *testing.T, f packageFixture, id generated.Identity) generated.ContentDraft {
	t.Helper()
	out := f.get(t, "/world/v1/admin/content-drafts/"+string(id)+"?product_id=coffee")
	if out.Code != 200 {
		t.Fatalf("draft %d: %s", out.Code, out.Body.String())
	}
	var d generated.ContentDraft
	if e := generated.Decode("ContentDraft", out.Body.Bytes(), &d); e != nil {
		t.Fatal(e)
	}
	return d
}
func importJSON(t *testing.T, f packageFixture, method, path, key string, value any) *httptest.ResponseRecorder {
	t.Helper()
	q, _ := http.NewRequest(method, f.server.URL+path, bytes.NewReader(mustPreviewJSON(t, value)))
	q.Header.Set("Authorization", "Bearer "+f.token)
	q.Header.Set("Idempotency-Key", key)
	q.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, q)
	return out
}
func importLifecycle(t *testing.T, f packageFixture, d generated.ContentDraft, action, key string, history string) generated.ContentDraft {
	t.Helper()
	base := "/world/v1/admin/content-drafts/" + string(d.DraftID)
	raw := previewAdminJSON(t, f, base+"/impact-checks", map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "action": action, "object_ids": []string{}}, key+"-impact")
	var report generated.ImpactReport
	json.Unmarshal(raw, &report)
	if len(report.BlockingFindings) > 0 {
		t.Fatalf("impact blocked: %+v", report.BlockingFindings)
	}
	body := map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "action": action, "impact_report_id": report.ReportID}
	if history != "" {
		body["restore_revision"] = history
	}
	raw = previewAdminJSON(t, f, base+"/commands", body, key+"-command")
	if action == "COPY" {
		var r generated.CopyReceipt
		if e := generated.Decode("CopyReceipt", raw, &r); e != nil {
			t.Fatal(e)
		}
		return r.Draft
	}
	return importDraft(t, f, d.DraftID)
}

func TestPackageImportAtomicIdempotency(t *testing.T) {
	f := newPackageFixture(t, "synthetic-p3-atomic-operator-token-0001")
	archive := notePreviewArchive(t)
	options := importOptions(t, f, archive)
	first := importedResult(t, importPost(t, f, archive, options, "p3-original", context.Background()))
	d := importDraft(t, f, first.Receipt.DraftID)
	if d.State != "ISOLATED_IMPORT" || !first.Persisted {
		t.Fatal("import not isolated/persistent")
	}
	if d.Input.RootObjectID == generated.Identity("note") || first.Receipt.Namespace == options["mapping"].(map[string]any)["namespace"] {
		t.Fatal("client preview IDs became installation authority")
	}
	rows := importRows(t, f)
	if rows[0] != 1 || rows[1] != 2 || rows[2] != 1 || rows[3] != 1 || rows[7] != 0 {
		t.Fatalf("incomplete or asynchronous import: %v", rows)
	}
	for _, key := range []string{"p3-original", "p3-other-request"} {
		r := importedResult(t, importPost(t, f, archive, options, key, context.Background()))
		if r.Receipt.ImportID != first.Receipt.ImportID || r.Receipt.DraftID != first.Receipt.DraftID {
			t.Fatal("natural slot duplicated")
		}
	}
	if got := importRows(t, f); got[0] != 1 || got[2] != 1 || got[4] != rows[4]+1 || got[6] != rows[6]+1 {
		t.Fatalf("receipt/command atomicity: %v", got)
	}
	// New service owners share only durable database state, never a process cache.
	recoveredStore := store.New(f.store.DB)
	service := content.New(recoveredStore)
	operator := auth.New(recoveredStore, f.scope, "owner", f.token)
	ctx, _, e := operator.Authenticate(context.Background(), "Bearer "+f.token)
	if e != nil {
		t.Fatal(e)
	}
	got, e := service.GetPackageImportResult(ctx, f.scope, "p3-original")
	if e != nil || got.Receipt != first.Receipt {
		t.Fatal("restart recovery", e)
	}
	changed := map[string]any{}
	json.Unmarshal(mustPreviewJSON(t, options), &changed)
	changed["copy_key"] = "independent"
	if out := importPost(t, f, archive, changed, "p3-original", context.Background()); out.Code != 409 {
		t.Fatalf("same key payload changed: %d", out.Code)
	}
	copy := importedResult(t, importPost(t, f, archive, changed, "p3-new-copy", context.Background()))
	if copy.Receipt.DraftID == first.Receipt.DraftID || copy.Receipt.Namespace == first.Receipt.Namespace {
		t.Fatal("copy reused imported identities")
	}
	altered := revisePreviewArchive(t, archive, func(_ map[string]any, body map[string]any, _ map[string][]byte) {
		body["description"] = "Changed content, same package version"
	})
	if out := importPost(t, f, altered, options, "p3-changed-version", context.Background()); out.Code != 409 {
		t.Fatalf("natural-slot hash conflict %d: %s", out.Code, out.Body.String())
	}
	changed = map[string]any{}
	json.Unmarshal(mustPreviewJSON(t, options), &changed)
	changed["mapping"].(map[string]any)["rights"].([]any)[0].(map[string]any)["allowed_uses"] = []string{"EDIT", "COPY", "PUBLISH"}
	if out := importPost(t, f, archive, changed, "p3-changed-map", context.Background()); out.Code != 409 {
		t.Fatalf("mapping conflict %d", out.Code)
	}
	if out := f.get(t, "/world/v1/admin/content-packages/imports/p3-original?product_id=other"); out.Code == 200 {
		t.Fatal("cross-product receipt escaped")
	}
	if _, e = f.store.DB.Exec("UPDATE admin_results SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.request_sha256',REPEAT('a',64)) WHERE schema_name='PackageImportResult'"); e != nil {
		t.Fatal(e)
	}
	if out := f.get(t, "/world/v1/admin/content-packages/imports/p3-original?product_id=coffee"); out.Code == 200 {
		t.Fatal("altered result not bound to original command hash")
	}
}

func TestPackageImportCommitUnknownRecoversOriginal(t *testing.T) {
	f := newPackageFixture(t, "synthetic-p3-unknown-operator-token-0001")
	archive := notePreviewArchive(t)
	options := importOptions(t, f, archive)
	f.store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic reply lost after actual COMMIT")
	}
	out := importPost(t, f, archive, options, "p3-unknown-original", context.Background())
	if out.Code != 409 || !bytes.Contains(out.Body.Bytes(), []byte("UNKNOWN")) {
		t.Fatalf("unknown commit %d: %s", out.Code, out.Body.String())
	}
	f.store.Commit = nil
	before := importRows(t, f)
	recovered := f.get(t, "/world/v1/admin/content-packages/imports/p3-unknown-original?product_id=coffee")
	if recovered.Code != 200 {
		t.Fatalf("original lookup %d: %s", recovered.Code, recovered.Body.String())
	}
	importedResult(t, importPost(t, f, archive, options, "p3-unknown-original", context.Background()))
	if !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, importRows(t, f))) {
		t.Fatal("original recovery duplicated durable effects")
	}
}

func TestPackageImportFinalRollback(t *testing.T) {
	for _, mode := range []string{"credential-revoked", "cancelled", "source-withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			f := newPackageFixture(t, "synthetic-p3-rollback-operator-token-0001")
			archive := notePreviewArchive(t)
			options := importOptions(t, f, archive)
			var target generated.DependencyLock
			if mode == "source-withdrawn" {
				target = publishPreviewResource(t, f, "ROLE", []string{"EDIT", "COPY", "PUBLISH"}, "p3-source")
				archive = bindingPreviewArchive(t)
				mapping := resolveNoteOptions(t, f, archive)
				mapping["dependency_mappings"] = []any{map[string]any{"pointer": "/module_agent_bindings/0/role_ref", "target": target}}
				resolved := previewPost(t, f, archive, mapping, f.token)
				var r map[string]any
				json.Unmarshal(resolved.Body.Bytes(), &r)
				if r["status"] != "RESOLVED" {
					t.Fatal(resolved.Body.String())
				}
				options = map[string]any{"mapping": mapping, "expected_preview_sha256": r["preview_sha256"]}
			}
			before := importRows(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hit := false
			f.store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var n int
				if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_package_imports").Scan(&n); e != nil {
					return e
				}
				if n == 0 {
					return nil
				}
				hit = true
				switch mode {
				case "credential-revoked":
					_, e := tx.ExecContext(ctx, "UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'")
					return e
				case "source-withdrawn":
					_, e := tx.ExecContext(ctx, "UPDATE releases SET available=0 WHERE release_id=?", target.ReleaseID)
					return e
				default:
					cancel()
					return ctx.Err()
				}
			}
			out := importPost(t, f, archive, options, "p3-rollback-original", ctx)
			if !hit || out.Code == 201 {
				t.Fatalf("final guard not exercised %v/%d: %s", hit, out.Code, out.Body.String())
			}
			f.store.BeforeCommit = nil
			if !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, importRows(t, f))) {
				t.Fatal("partial draft/import/receipt survived rollback")
			}
		})
	}
}

func TestPackageImportLineageSurvivesLifecycle(t *testing.T) {
	f := newPackageFixture(t, "synthetic-p3-lineage-operator-token-0001")
	archive := notePreviewArchive(t)
	options := importOptions(t, f, archive)
	r := importedResult(t, importPost(t, f, archive, options, "p3-lineage", context.Background()))
	d := importDraft(t, f, r.Receipt.DraftID)
	copy := importLifecycle(t, f, d, "COPY", "p3-copy", "")
	if copy.State != "ISOLATED_IMPORT" {
		t.Fatal("COPY laundered isolation")
	}
	archived := importLifecycle(t, f, copy, "ARCHIVE", "p3-archive", "")
	if archived.State != "ARCHIVED" {
		t.Fatal("archive")
	}
	restored := importLifecycle(t, f, archived, "RESTORE", "p3-restore", "1")
	if restored.State != "ISOLATED_IMPORT" {
		t.Fatal("history restore laundered isolation")
	}
	// Ordinary create/save APIs cannot wash a known imported object identity.
	created := importJSON(t, f, "POST", "/world/v1/admin/content-drafts", "p3-reuse-create", map[string]any{"product_id": "coffee", "root_kind": "KNOWLEDGE", "input": d.Input})
	if created.Code != 201 {
		t.Fatalf("reuse create %d: %s", created.Code, created.Body.String())
	}
	var reused generated.ContentDraft
	generated.Decode("ContentDraft", created.Body.Bytes(), &reused)
	if reused.State != "ISOLATED_IMPORT" {
		t.Fatal("create washed known object")
	}
	clean := d.Input
	clean.RootObjectID = "clean-note"
	var obj map[string]any
	json.Unmarshal(clean.Objects[0], &obj)
	obj["object_id"] = "clean-note"
	clean.Objects = []generated.ContentObject{mustPreviewJSON(t, obj)}
	normal := importJSON(t, f, "POST", "/world/v1/admin/content-drafts", "p3-normal-create", map[string]any{"product_id": "coffee", "root_kind": "KNOWLEDGE", "input": clean})
	if normal.Code != 201 {
		t.Fatal(normal.Body.String())
	}
	var ordinary generated.ContentDraft
	generated.Decode("ContentDraft", normal.Body.Bytes(), &ordinary)
	if ordinary.State != "DRAFT" {
		t.Fatal("ordinary draft unexpectedly isolated")
	}
	saved := importJSON(t, f, "PUT", "/world/v1/admin/content-drafts/"+string(ordinary.DraftID), "p3-reuse-save", map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "input": d.Input})
	if saved.Code != 200 {
		t.Fatalf("reuse save %d: %s", saved.Code, saved.Body.String())
	}
	generated.Decode("ContentDraft", saved.Body.Bytes(), &ordinary)
	if ordinary.State != "ISOLATED_IMPORT" {
		t.Fatal("save washed known object")
	}
	// Replacing all object IDs still cannot remove the draft's sticky origin.
	saved = importJSON(t, f, "PUT", "/world/v1/admin/content-drafts/"+string(ordinary.DraftID), "p3-replace-objects", map[string]any{"product_id": "coffee", "expected_draft_revision": ordinary.DraftRevision, "input": clean})
	if saved.Code != 200 {
		t.Fatal(saved.Body.String())
	}
	generated.Decode("ContentDraft", saved.Body.Bytes(), &ordinary)
	if ordinary.State != "ISOLATED_IMPORT" {
		t.Fatal("object removal washed draft origin")
	}
	base := "/world/v1/admin/content-drafts/" + string(d.DraftID)
	checked := previewAdminJSON(t, f, base+"/publish-checks", map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "requested_support_labels": []string{"MOCK_DEMO"}}, "p3-import-check")
	var report generated.PublishCheckReport
	json.Unmarshal(checked, &report)
	out := importJSON(t, f, "POST", base+"/releases", "p3-publish-blocked", map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "check_report_id": report.ReportID, "rehearsal_report_ids": []string{}, "requested_support_labels": []string{"MOCK_DEMO"}, "approval_note": "Synthetic isolation boundary"})
	if out.Code < 400 {
		t.Fatal("imported draft published")
	}
}

func TestPackageImportMigrationRetry(t *testing.T) {
	f := newPackageFixture(t, "synthetic-p3-migration-operator-token-0001")
	if e := f.store.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	var n int
	if e := f.store.DB.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=9").Scan(&n); e != nil || n != 1 {
		t.Fatal("migration9 missing or duplicated", e, n)
	}
	for _, table := range []string{"content_package_imports", "content_package_import_lineage"} {
		if e := f.store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil || n != 0 {
			t.Fatal("migration creates business data", e)
		}
	}
}

func TestPackageImportBrowser(t *testing.T) {
	if os.Getenv("LOREVIVA_PACKAGE_IMPORT_BROWSER") != "1" {
		t.Skip("explicit owned import browser fixture required")
	}
	f := newPackageFixture(t, "synthetic-p3-browser-operator-token-0001")
	archive := notePreviewArchive(t)
	baseHandler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "POST" && q.URL.Path == "/world/v1/admin/content-packages/imports" {
			raw, e := io.ReadAll(q.Body)
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			q.Body = io.NopCloser(bytes.NewReader(raw))
			clone := q.Clone(q.Context())
			clone.Body = io.NopCloser(bytes.NewReader(raw))
			reader, e := clone.MultipartReader()
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			var received, options []byte
			for {
				part, e := reader.NextPart()
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Error(e)
					break
				}
				value, _ := io.ReadAll(part)
				if part.FormName() == "archive" {
					received = value
				}
				if part.FormName() == "options" {
					options = value
				}
			}
			if !bytes.Equal(received, archive) {
				t.Errorf("browser changed original archive bytes: %d/%d", len(received), len(archive))
			}
			if len(options) == 0 {
				t.Error("browser omitted import options")
			}
			recorded := httptest.NewRecorder()
			baseHandler.ServeHTTP(recorded, q)
			if recorded.Code == 201 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				w.Write([]byte("{"))
				return
			}
			for k, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(k, value)
				}
			}
			w.WriteHeader(recorded.Code)
			w.Write(recorded.Body.Bytes())
			return
		}
		baseHandler.ServeHTTP(w, q)
	})
	input := filepath.Join(t.TempDir(), "world.wepkg")
	if e := os.WriteFile(input, archive, 0600); e != nil {
		t.Fatal(e)
	}
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("LOREVIVA_TEST_NODE"), "scripts/package-import-browser.mjs")
	command.Dir = repo
	command.Env = append(os.Environ(), "LOREVIVA_PACKAGE_IMPORT_URL="+f.server.URL, "LOREVIVA_PACKAGE_IMPORT_BEARER="+f.token, "LOREVIVA_PACKAGE_IMPORT_ARCHIVE="+input)
	raw, e := command.CombinedOutput()
	t.Log(string(raw))
	if e != nil {
		t.Fatal("actual creator import browser", e)
	}
	rows := importRows(t, f)
	if rows[0] != 1 || rows[2] != 1 || rows[7] != 0 {
		t.Fatal(fmt.Sprintf("browser durable effects %v", rows))
	}
}
