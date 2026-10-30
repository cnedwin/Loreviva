package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
)

var transferUses = []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}

func transferScope() domain.ProductScope {
	return domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
}
func transferFixture(t *testing.T, r *app.Runtime) packageFixture {
	t.Helper()
	server := httptest.NewServer(r.Handler())
	t.Cleanup(server.Close)
	return packageFixture{server: server, token: runtimeToken, store: r.Store, scope: transferScope()}
}
func transferCheck(t *testing.T, r *app.Runtime, d generated.ContentDraft, key string) generated.PublishCheckReport {
	t.Helper()
	n, _ := content.Revision(string(d.DraftRevision))
	report, e := r.Content.CheckPublish(runtimeContext(t, r), transferScope(), string(d.DraftID), n, []string{"MOCK_DEMO"}, key)
	if e != nil || !report.Passed {
		t.Fatalf("actual publish check: %v %+v", e, report.Findings)
	}
	return report
}
func transferPublish(t *testing.T, r *app.Runtime, d generated.ContentDraft, key string) generated.Release {
	t.Helper()
	check := transferCheck(t, r, d, key+"-check")
	n, _ := content.Revision(string(d.DraftRevision))
	release, e := r.Content.Publish(runtimeContext(t, r), transferScope(), string(d.DraftID), n, string(check.ReportID), []string{"MOCK_DEMO"}, key+"-publish")
	if e != nil {
		t.Fatal("publish actual draft", e)
	}
	return release
}

// Installation A creates a newly licensed definition and a real governed file;
// the immutable seeded release is never edited to make an export pass.
func transferOriginal(t *testing.T, edits ...func(*generated.DraftInput)) (*app.Runtime, generated.Release, generated.PackageExportReceipt, []byte) {
	t.Helper()
	r, id, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := transferScope()
	d, e := r.Content.GetDraft(ctx, scope, id, "")
	if e != nil {
		t.Fatal(e)
	}
	rights := generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "Synthetic transfer file", License: "CC0-1.0", AllowedUses: transferUses}
	body := []byte("Synthetic authoring source, with no players or runtime state.")
	sum := sha256.Sum256(body)
	file, e := r.Files.Upload(ctx, scope, bytes.NewReader(body), generated.AssetUploadMetadata{ProductID: "coffee", Name: "authoring.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: generated.ExactUnsigned(fmt.Sprint(len(body))), ExpectedSha256: hex.EncodeToString(sum[:]), AudiencePolicy: "PRODUCT_READERS", Rights: rights}, "p4-original-file")
	if e != nil || file.State != "READY" {
		t.Fatal("normal original upload", e)
	}
	for i, raw := range d.Input.Objects {
		var object map[string]any
		json.Unmarshal(raw, &object)
		object["rights"] = rights
		d.Input.Objects[i] = mustPreviewJSON(t, object)
	}
	def := generated.AssetDefinition{ObjectID: "transfer-original-file", Kind: "ASSET", Name: "Authoring source", Description: "Static reusable source", Tags: []string{}, Rights: rights, AudiencePolicy: "PRODUCT_READERS", Version: "1", SourceRefs: []string{}, DependencyRefs: []generated.DefinitionRef{}, AssetID: file.AssetID, AssetSha256: file.Sha256, SizeBytes: file.SizeBytes, MediaType: file.MediaType, ClientCapabilities: []generated.Identity{}, Fallback: "BLOCK"}
	d.Input.Objects = append(d.Input.Objects, mustPreviewJSON(t, def))
	d.Input.Description = "Narrative preserves coffee-world, mia and mock-local verbatim."
	for _, edit := range edits {
		edit(&d.Input)
	}
	d, e = r.Content.SaveDraft(ctx, scope, id, d.Input, 1, "p4-original-license")
	if e != nil {
		t.Fatal(e)
	}
	release := transferPublish(t, r, d, "p4-source")
	request := generated.PackageExportRequest{ProductID: "coffee", ReleaseID: release.ReleaseID, PackageID: "synthetic.roundtrip", PackageVersion: "1.0.0", AuthorDisplayName: "Synthetic creator", RightsAttested: true}
	receipt, e := r.Content.ExportPackage(ctx, scope, request, "p4-export-original")
	if e != nil {
		t.Fatal("actual original export", e)
	}
	_, archive, e := r.Store.ReadPackageExport(ctx, scope, string(receipt.ExportID))
	if e != nil {
		t.Fatal("actual authorized archive", e)
	}
	if wepkg.Check(archive, wepkg.DefaultLimits).Verdict != "PASS" {
		t.Fatal("export not a full valid package")
	}
	again, e := r.Content.ExportPackage(ctx, scope, request, "p4-export-original")
	if e != nil || receipt != again {
		t.Fatal("export original-key replay", e)
	}
	return r, release, receipt, archive
}

// Extract through the actual binary endpoint, then upload through the ordinary
// file owner. A package itself never creates an asset or grants local rights.
func transferExtract(t *testing.T, f packageFixture, archive []byte, path string) ([]byte, generated.PackageFileInfo) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	p, _ := w.CreateFormFile("archive", "world.wepkg")
	p.Write(archive)
	w.WriteField("options", string(mustPreviewJSON(t, generated.PackageFileOptions{FilePath: path})))
	w.Close()
	q := httptest.NewRequest("POST", "/world/v1/admin/content-packages/file-bytes?product_id=coffee", &body)
	q.Header.Set("Content-Type", w.FormDataContentType())
	q.Header.Set("Idempotency-Key", "p4-read-file")
	q.Header.Set("Authorization", "Bearer "+f.token)
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, q)
	if out.Code != 200 {
		t.Fatalf("extract original package file %d: %s", out.Code, out.Body.String())
	}
	raw, e := base64.StdEncoding.DecodeString(out.Header().Get("Loreviva-Package-File"))
	var info generated.PackageFileInfo
	if e != nil || generated.Decode("PackageFileInfo", raw, &info) != nil || info.FilePath != path {
		t.Fatal("bound file metadata", e, string(raw))
	}
	sum := sha256.Sum256(out.Body.Bytes())
	if info.Sha256 != hex.EncodeToString(sum[:]) || string(info.SizeBytes) != fmt.Sprint(out.Body.Len()) || len(info.SourceRights.AllowedUses) != 0 {
		t.Fatal("binary metadata or local rights differ")
	}
	return out.Body.Bytes(), info
}
func transferImport(t *testing.T, r *app.Runtime, archive []byte, key string) (packageFixture, generated.ContentDraft) {
	t.Helper()
	f := transferFixture(t, r)
	discovered := previewPost(t, f, archive, map[string]any{"stage": "DISCOVER"}, f.token)
	var discovery map[string]any
	if discovered.Code != 200 || json.Unmarshal(discovered.Body.Bytes(), &discovery) != nil || discovery["status"] != "NEEDS_MAPPING" {
		t.Fatal("actual second-install discovery", discovered.Body.String())
	}
	req := discovery["requirements"].(map[string]any)
	rights := []any{}
	for _, v := range req["objects"].([]any) {
		rights = append(rights, map[string]any{"object_id": v.(map[string]any)["object_id"], "allowed_uses": transferUses})
	}
	models := []any{}
	caps := map[string]string{}
	for _, v := range req["capabilities"].([]any) {
		c := v.(map[string]any)
		id := c["capability_id"].(string)
		if c["family"] == "MODEL" {
			models = append(models, map[string]any{"capability_id": id, "settings": map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}}})
		} else {
			if id == "text" {
				caps[id] = "TEXT"
			} else if id == "sign_custody" {
				caps[id] = "SIGN_CUSTODY"
			}
		}
	}
	assets := []any{}
	for i, v := range req["files"].([]any) {
		row := v.(map[string]any)
		path := row["file_path"].(string)
		raw, info := transferExtract(t, f, archive, path)
		uses := info.SourceRights
		uses.AllowedUses = transferUses
		record, e := r.Files.Upload(runtimeContext(t, r), transferScope(), bytes.NewReader(raw), generated.AssetUploadMetadata{ProductID: "coffee", Name: fmt.Sprintf("second-%d.txt", i), Purpose: "KNOWLEDGE", DeclaredType: info.MediaType, ExpectedSize: info.SizeBytes, ExpectedSha256: info.Sha256, AudiencePolicy: "PRODUCT_READERS", Rights: uses}, fmt.Sprintf("%s-file-%d", key, i))
		if e != nil || record.State != "READY" {
			t.Fatal("second normal READY asset upload", e)
		}
		assets = append(assets, map[string]any{"file_path": path, "asset_id": record.AssetID})
	}
	if len(assets) == 0 {
		t.Fatal("second-install loop did not include a physical file")
	}
	options := map[string]any{"stage": "RESOLVE", "entrypoint": discovery["entrypoint"], "namespace": discovery["namespace"], "dependency_mappings": []any{}, "additional_locks": []any{}, "model_mappings": models, "local_capabilities": caps, "asset_mappings": assets, "rights": rights, "rights_attested": true}
	resolved := previewPost(t, f, archive, options, f.token)
	var result map[string]any
	json.Unmarshal(resolved.Body.Bytes(), &result)
	if resolved.Code != 200 || result["status"] != "RESOLVED" {
		t.Fatalf("actual second-install mapping %d: %s", resolved.Code, resolved.Body.String())
	}
	imported := importedResult(t, importPost(t, f, archive, map[string]any{"mapping": options, "expected_preview_sha256": result["preview_sha256"]}, key, context.Background()))
	return f, importDraft(t, f, imported.Receipt.DraftID)
}
func transferRehearse(t *testing.T, r *app.Runtime, d generated.ContentDraft, key string, assertions []string, notApplicable ...bool) string {
	t.Helper()
	slot := ""
	for _, raw := range d.Input.Objects {
		var o map[string]any
		json.Unmarshal(raw, &o)
		if o["object_id"] == string(d.Input.RootObjectID) {
			slot, _ = o["primary_actor_slot"].(string)
		}
	}
	if slot == "" {
		t.Fatal("actual imported primary slot missing")
	}
	var req generated.RehearsalRequest
	raw := mustPreviewJSON(t, map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "mode": "MOCK", "fixture": "COFFEE", "inputs": []any{map[string]any{"actor_slot": slot, "text": ""}}, "max_cost_units": "0", "assertions": assertions})
	if e := generated.Decode("RehearsalRequest", raw, &req); e != nil {
		t.Fatal(e)
	}
	task, e := r.Store.AcceptDraftRehearsal(runtimeContext(t, r), transferScope(), string(d.DraftID), key, req)
	if e != nil {
		t.Fatal("real imported Mock admission", e)
	}
	if e = r.Worker.Once(runtimeContext(t, r)); e != nil {
		t.Fatal("real imported Mock execution", e)
	}
	body, e := r.Files.ReadCommittedRehearsalReport(runtimeContext(t, r), transferScope(), string(task.TaskID))
	if e != nil {
		t.Fatal("original report owner", e)
	}
	var report struct {
		Assertions []domain.RehearsalAssertion `json:"assertions"`
		Inputs     []struct {
			State string `json:"state"`
		} `json:"inputs"`
	}
	json.Unmarshal(body, &report)
	if len(report.Inputs) != 1 || report.Inputs[0].State != "COMMITTED" {
		t.Fatalf("original report input not committed: %s", body)
	}
	for _, a := range report.Assertions {
		if len(notApplicable) > 0 && notApplicable[0] && a.Kind == "SIGN_CUSTODY" && a.State == "NOT_RUN" {
			continue
		}
		if a.State != "PASS" {
			t.Fatalf("actual applicable assertion %s=%s", a.Kind, a.State)
		}
	}
	return string(task.TaskID)
}
func transferApproval(d generated.ContentDraft, check generated.PublishCheckReport, task string) generated.PackageApprovalRequest {
	out := generated.PackageApprovalRequest{ProductID: "coffee", ExpectedDraftRevision: d.DraftRevision, CheckReportID: check.ReportID, ApprovalNote: "Synthetic current revision reviewed", RightsAttested: true}
	if task != "" {
		id := generated.Identity(task)
		out.RehearsalTaskID = &id
	}
	return out
}
func transferCount(t *testing.T, r *app.Runtime, table string) int {
	t.Helper()
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestPackageTransferSecondInstallation(t *testing.T) {
	original, release, receipt, archive := transferOriginal(t)
	before := mustPreviewJSON(t, release)
	second, scope, _ := runtimeFixture(t)
	f, d := transferImport(t, second, archive, "p4-second-import")
	if d.State != "ISOLATED_IMPORT" {
		t.Fatal("initial isolation lost")
	}
	for i, raw := range d.Input.Objects {
		var object map[string]any
		json.Unmarshal(raw, &object)
		if generation, ok := object["generation"].(map[string]any); ok && generation["mode"] != "NONE" {
			object["generation"] = map[string]any{"mode": "INHERIT", "required_capabilities": []string{}, "allowed_external_uses": []string{"NONE"}}
			d.Input.Objects[i] = mustPreviewJSON(t, object)
		}
	}
	for i := range d.Input.ModuleAgentBindings {
		d.Input.ModuleAgentBindings[i].Generation = generated.GenerationSettings{Mode: "INHERIT", RequiredCapabilities: []generated.Identity{}, AllowedExternalUses: []string{"NONE"}}
	}
	var saveError error
	d, saveError = second.Content.SaveDraft(runtimeContext(t, second), scope, string(d.DraftID), d.Input, 1, "p4-second-inherit")
	if saveError != nil {
		t.Fatal(saveError)
	}
	oldIDs := map[string]bool{}
	for _, raw := range release.Input.Objects {
		var o map[string]any
		json.Unmarshal(raw, &o)
		oldIDs[o["object_id"].(string)] = true
	}
	for _, raw := range d.Input.Objects {
		var o map[string]any
		json.Unmarshal(raw, &o)
		if oldIDs[o["object_id"].(string)] {
			t.Fatal("installation reused original object identity")
		}
	}
	if d.Input.Description != release.Input.Description {
		t.Fatal("narrative strings were rewritten")
	}
	check := transferCheck(t, second, d, "p4-second-isolated-check")
	ctx := runtimeContext(t, second)
	if _, e := second.Content.ApprovePackageDraft(ctx, scope, string(d.DraftID), transferApproval(d, check, ""), "p4-missing-report"); e == nil {
		t.Fatal("WORLD approval accepted no real rehearsal")
	}
	partial := transferRehearse(t, second, d, "p4-partial-rehearsal", []string{"ISOLATED"})
	if _, e := second.Content.ApprovePackageDraft(ctx, scope, string(d.DraftID), transferApproval(d, check, partial), "p4-partial-approval"); e == nil {
		t.Fatal("partial report gave approval")
	}
	task := transferRehearse(t, second, d, "p4-full-rehearsal", []string{"ISOLATED", "RIGHTS", "DUTIES", "SIGN_CUSTODY"})
	request := transferApproval(d, check, task)
	// Deliberate fixture corruption bypasses the normal file writer. The final
	// held proof must detect it even when every SQL metadata row is unchanged.
	var reportStorage string
	if e := second.Store.DB.QueryRow("SELECT storage_key FROM harness_rehearsal_reports WHERE task_id=?", task).Scan(&reportStorage); e != nil {
		t.Fatal(e)
	}
	reportPath := filepath.Join(second.Files.RootPath, scope.TenantID, scope.ProductID, reportStorage+".part")
	originalReport, e := os.ReadFile(reportPath)
	if e != nil {
		t.Fatal(e)
	}
	tampered := false
	second.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_package_adoptions").Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			tampered = true
			return os.WriteFile(reportPath, []byte("synthetic corruption"), 0600)
		}
		return nil
	}
	_, bad := second.Content.ApprovePackageDraft(ctx, scope, string(d.DraftID), request, "p4-physical-corruption")
	second.Store.BeforeCommit = nil
	if e = os.WriteFile(reportPath, originalReport, 0600); e != nil {
		t.Fatal(e)
	}
	if !tampered || bad == nil || transferCount(t, second, "content_package_adoptions") != 0 {
		t.Fatal("physical final proof did not roll back", tampered, bad)
	}
	approved, e := second.Content.ApprovePackageDraft(ctx, scope, string(d.DraftID), request, "p4-approve")
	if e != nil {
		t.Fatal("genuine current world approval", e)
	}
	if approved.State != "DRAFT" || approved.DraftRevision == d.DraftRevision || approved.ContentHash != d.ContentHash {
		t.Fatal("approval changed content or failed revision transition")
	}
	replay, e := second.Content.ApprovePackageDraft(ctx, scope, string(d.DraftID), request, "p4-approve")
	if e != nil || !bytes.Equal(mustPreviewJSON(t, replay), mustPreviewJSON(t, approved)) {
		t.Fatal("approval original replay", e)
	}
	if transferCount(t, second, "content_package_adoptions") != 1 || transferCount(t, second, "content_package_import_lineage") == 0 {
		t.Fatal("approval removed lineage or duplicated adoption")
	}
	installed := transferPublish(t, second, approved, "p4-second")
	if installed.ReleaseID == release.ReleaseID {
		t.Fatal("second installation reused release")
	}
	var setting json.RawMessage
	runtimeHTTP(t, second, "POST", "/settings/commands", "p4-current-default-change", map[string]any{"scope": map[string]any{"scope_type": "PRODUCT", "tenant_id": "demo-local", "product_id": "coffee"}, "kind": "SET_DEFAULTS", "expected_revision": "0", "defaults": map[string]any{"product_pack_ref": nil, "module_defaults": []any{map[string]any{"module_id": "INTERACTION", "generation_settings": map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}, "parameters": map[string]any{"temperature": 0.4}}}}}}, 200, &setting)
	routeCheck := transferCheck(t, second, approved, "p4-changed-route-check")
	if routeCheck.RouteSnapshotHash == check.RouteSnapshotHash {
		t.Fatal("route change did not alter the actual basis")
	}
	approvedRevision, _ := content.Revision(string(approved.DraftRevision))
	if _, e = second.Content.Publish(ctx, scope, string(approved.DraftID), approvedRevision, string(routeCheck.ReportID), []string{"MOCK_DEMO"}, "p4-approval-route-stale"); e == nil {
		t.Fatal("changed route consumed an old approval")
	}
	copy := importLifecycle(t, f, approved, "COPY", "p4-copy", "")
	if copy.State != "ISOLATED_IMPORT" {
		t.Fatal("copy inherited revision approval")
	}
	archived := importLifecycle(t, f, copy, "ARCHIVE", "p4-archive", "")
	restored := importLifecycle(t, f, archived, "RESTORE", "p4-restore", "1")
	if restored.State != "ISOLATED_IMPORT" {
		t.Fatal("restore inherited revision approval")
	}
	n, _ := content.Revision(string(approved.DraftRevision))
	edited, e := second.Content.SaveDraft(ctx, scope, string(approved.DraftID), approved.Input, n, "p4-post-approval-save")
	if e != nil || edited.State != "ISOLATED_IMPORT" {
		t.Fatal("save inherited revision approval", e)
	}
	stale := transferCheck(t, second, edited, "p4-stale-check")
	rev, _ := content.Revision(string(edited.DraftRevision))
	if _, e = second.Content.Publish(ctx, scope, string(edited.DraftID), rev, string(stale.ReportID), []string{"MOCK_DEMO"}, "p4-stale-publish"); e == nil {
		t.Fatal("new revision consumed old approval")
	}
	current, e := original.Content.GetRelease(runtimeContext(t, original), transferScope(), string(release.ReleaseID))
	if e != nil || !bytes.Equal(before, mustPreviewJSON(t, current)) {
		t.Fatal("original immutable release changed", e)
	}
	if _, e = original.Store.DB.Exec("UPDATE releases SET available=0 WHERE release_id=?", release.ReleaseID); e != nil {
		t.Fatal(e)
	}
	if _, _, e = original.Store.ReadPackageExport(runtimeContext(t, original), transferScope(), string(receipt.ExportID)); e == nil {
		t.Fatal("old receipt bypassed current source withdrawal")
	}
	t.Log("Two independent initialized databases and file roots; original ZIP → file extraction → normal READY upload → explicit mapping → isolated import → genuine partial refusal/full Mock → exact revision approval → new check and immutable release. Original unchanged.")
}

func TestPackageTransferCommitUnknownAndFinalRollback(t *testing.T) {
	r, release, _, _ := transferOriginal(t)
	ctx := runtimeContext(t, r)
	scope := transferScope()
	request := generated.PackageExportRequest{ProductID: "coffee", ReleaseID: release.ReleaseID, PackageID: "synthetic.unknown", PackageVersion: "1.0.0", AuthorDisplayName: "Synthetic creator", RightsAttested: true}
	before := transferCount(t, r, "content_package_exports")
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_package_exports").Scan(&n); e != nil {
			return e
		}
		if n > before {
			_, e := tx.ExecContext(ctx, "UPDATE releases SET available=0 WHERE release_id=?", release.ReleaseID)
			return e
		}
		return nil
	}
	if _, e := r.Content.ExportPackage(ctx, scope, request, "p4-final-source-withdrawal"); e == nil {
		t.Fatal("late withdrawal committed export")
	}
	r.Store.BeforeCommit = nil
	if transferCount(t, r, "content_package_exports") != before {
		t.Fatal("export survived rollback")
	}
	r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic lost reply after actual commit")
	}
	if _, e := r.Content.ExportPackage(ctx, scope, request, "p4-unknown"); e == nil {
		t.Fatal("lost commit reply was reported as known success")
	}
	r.Store.Commit = nil
	replay, e := r.Content.ExportPackage(ctx, scope, request, "p4-unknown")
	if e != nil {
		t.Fatal("original unknown recovery", e)
	}
	if transferCount(t, r, "content_package_exports") != before+1 {
		t.Fatal("UNKNOWN repeated export")
	}
	if _, _, e = r.Store.ReadPackageExport(ctx, scope, string(replay.ExportID)); e != nil {
		t.Fatal("durable recovered original download", e)
	}
	if e = r.Store.Migrate(ctx); e != nil {
		t.Fatal("migration10 retry", e)
	}
	var n int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=10").Scan(&n); e != nil || n != 1 {
		t.Fatal("migration10 not exactly once", e, n)
	}
}

func TestPackageTransferStaticApprovalFinalAuthority(t *testing.T) {
	f := newPackageFixture(t, "synthetic-p4-static-token-at-least-32")
	archive := notePreviewArchive(t)
	options := importOptions(t, f, archive)
	options["mapping"].(map[string]any)["rights"].([]any)[0].(map[string]any)["allowed_uses"] = transferUses
	mapping := options["mapping"]
	resolved := previewPost(t, f, archive, mapping, f.token)
	var p map[string]any
	json.Unmarshal(resolved.Body.Bytes(), &p)
	options["expected_preview_sha256"] = p["preview_sha256"]
	result := importedResult(t, importPost(t, f, archive, options, "p4-static", context.Background()))
	d := importDraft(t, f, result.Receipt.DraftID)
	// Use the same authenticated HTTP owner as a normal creator; no rehearsal is
	// represented as a static definition check.
	raw := previewAdminJSON(t, f, "/world/v1/admin/content-drafts/"+string(d.DraftID)+"/publish-checks", map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "requested_support_labels": []string{"MOCK_DEMO"}}, "p4-static-check")
	var checked generated.PublishCheckReport
	json.Unmarshal(raw, &checked)
	if !checked.Passed {
		t.Fatal("static check", string(raw))
	}
	for _, mode := range []string{"authority", "expired-check"} {
		hit := false
		f.store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
			var n int
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_package_adoptions").Scan(&n); e != nil {
				return e
			}
			if n > 0 {
				hit = true
				if mode == "authority" {
					_, e := tx.ExecContext(ctx, "UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'")
					return e
				}
				_, e := tx.ExecContext(ctx, "UPDATE content_checks SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.expires_at','2000-01-01T00:00:00Z') WHERE report_id=?", checked.ReportID)
				return e
			}
			return nil
		}
		out := importJSON(t, f, "POST", "/world/v1/admin/content-drafts/"+string(d.DraftID)+"/isolation-approvals", "p4-static-"+mode, transferApproval(d, checked, ""))
		f.store.BeforeCommit = nil
		if !hit || out.Code < 400 {
			t.Fatal("late approval guard not exercised", mode, hit, out.Code, out.Body.String())
		}
		var count int
		f.store.DB.QueryRow("SELECT COUNT(*) FROM content_package_adoptions").Scan(&count)
		if count != 0 {
			t.Fatal("approval survived rollback", mode)
		}
	}
	var out *httptest.ResponseRecorder
	out = importJSON(t, f, "POST", "/world/v1/admin/content-drafts/"+string(d.DraftID)+"/isolation-approvals", "p4-static-pass", transferApproval(d, checked, ""))
	if out.Code != 201 {
		t.Fatal("static approval", out.Code, out.Body.String())
	}
}

func TestPackageTransferNonApplicableCustodyDoesNotBlockApproval(t *testing.T) {
	_, _, _, archive := transferOriginal(t, func(in *generated.DraftInput) {
		for i, raw := range in.Objects {
			var object map[string]any
			json.Unmarshal(raw, &object)
			if object["kind"] == "RULE" && object["primitive"] == "SIGN_CUSTODY" {
				object["strength"] = "SOFT"
				in.Objects[i] = mustPreviewJSON(t, object)
			}
		}
	})
	second, scope, _ := runtimeFixture(t)
	_, d := transferImport(t, second, archive, "p4-no-hard-sign-import")
	check := transferCheck(t, second, d, "p4-no-hard-sign-check")
	task := transferRehearse(t, second, d, "p4-no-hard-sign-rehearsal", []string{"ISOLATED", "RIGHTS", "DUTIES", "SIGN_CUSTODY"}, true)
	approved, e := second.Content.ApprovePackageDraft(runtimeContext(t, second), scope, string(d.DraftID), transferApproval(d, check, task), "p4-no-hard-sign-approve")
	if e != nil || approved.State != "DRAFT" {
		t.Fatal("non-applicable sign blocked a genuine review", e)
	}
}
