package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func previewPost(t *testing.T, f packageFixture, archive []byte, options any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("archive", "world.wepkg")
	if err != nil {
		t.Fatal(err)
	}
	part.Write(archive)
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	writer.WriteField("options", string(raw))
	writer.Close()
	request, _ := http.NewRequest("POST", f.server.URL+"/world/v1/admin/content-packages/preview?product_id=coffee", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", "preview-original-key-0001")
	request.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, request)
	return out
}
func TestPackagePreviewHTTPDiscoverHasNoBusinessEffects(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0001")
	before := countRows(t, f.store)
	archive := validPackage(t, portablePackageWorldBody(t))
	out := previewPost(t, f, archive, map[string]any{"stage": "DISCOVER"}, f.token)
	if out.Code != 200 {
		t.Fatalf("discovery HTTP status %d: %s", out.Code, out.Body.String())
	}
	if err := generated.Validate("PackageImportPreview", out.Body.Bytes()); err != nil {
		t.Fatalf("preview wire differs from its exact schema: %v", err)
	}
	var value map[string]any
	json.Unmarshal(out.Body.Bytes(), &value)
	if value["status"] != "NEEDS_MAPPING" || value["execution_ready"] != false || value["persisted"] != false {
		t.Fatalf("discovery did not separate declarations from local resolution: %s", out.Body.String())
	}
	if _, found := value["input"]; found || len(value["id_mappings"].([]any)) != 0 {
		t.Fatal("discovery returned local editing authority")
	}
	if len(value["requirements"].(map[string]any)["objects"].([]any)) == 0 {
		t.Fatal("GUI cannot build source-use mapping rows")
	}
	if after := countRows(t, f.store); after != before {
		t.Fatalf("discovery wrote business state: %v -> %v", before, after)
	}
}

func notePreviewArchive(t *testing.T) []byte {
	body := []byte(`{"body_version":"1.0","name":"Portable note","description":"Synthetic preview","tags":[],"root_object_id":"note","objects":[{"object_id":"note","kind":"KNOWLEDGE","name":"Note","description":"Synthetic","tags":[],"rights":{"license":"CC0-1.0","source_reference":"fixture:p2-http","redistribution_allowed":true,"allowed_uses":["WORLD_CONTENT"]},"audience_policy":"PRODUCT_READERS","version":"9","source_refs":[],"dependency_refs":[],"knowledge_type":"NOTE","text":"A source note","binding_refs":[]}],"module_agent_bindings":[],"skill_bindings":[]}`)
	return packageWithManifest(t, body, func(doc map[string]any) {
		doc["kind"] = "KNOWLEDGE"
		doc["entries"].([]any)[0].(map[string]any)["kind"] = "KNOWLEDGE"
		doc["requirements"].(map[string]any)["presentation_capabilities"] = []any{}
		raw, _ := json.Marshal(doc)
		var manifest wepkg.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		hash, err := wepkg.ContentHash(manifest)
		if err != nil {
			t.Fatal(err)
		}
		doc["content_sha256"] = hash
	})
}
func resolveNoteOptions(t *testing.T, f packageFixture, archive []byte) map[string]any {
	t.Helper()
	discovery := previewPost(t, f, archive, map[string]any{"stage": "DISCOVER"}, f.token)
	if discovery.Code != 200 {
		t.Fatalf("discovery status: %d", discovery.Code)
	}
	var value map[string]any
	json.Unmarshal(discovery.Body.Bytes(), &value)
	if value["status"] != "NEEDS_MAPPING" {
		t.Fatalf("note not statically discoverable: %s", discovery.Body.String())
	}
	return map[string]any{"stage": "RESOLVE", "entrypoint": value["entrypoint"], "namespace": value["namespace"], "dependency_mappings": []any{}, "additional_locks": []any{}, "model_mappings": []any{}, "local_capabilities": map[string]string{}, "asset_mappings": []any{}, "rights": []any{map[string]any{"object_id": "note", "allowed_uses": []string{"EDIT", "COPY"}}}, "rights_attested": true}
}
func previewBusinessCounts(t *testing.T, f packageFixture) []int {
	t.Helper()
	out := []int{}
	for _, table := range []string{"tasks", "assets", "content_drafts", "harness_turns", "commands", "commit_receipts", "lock_markers", "admin_results", "content_checks", "content_revisions", "world_projects"} {
		var n int
		if err := f.store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}
func TestPackagePreviewHTTPResolveNeverCommits(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0002")
	archive := notePreviewArchive(t)
	options := resolveNoteOptions(t, f, archive)
	before := previewBusinessCounts(t, f)
	committed := false
	f.store.Commit = func(ctx context.Context, tx *sql.Tx) error { committed = true; return tx.Commit() }
	out := previewPost(t, f, archive, options, f.token)
	if committed {
		t.Fatal("read-only preview invoked a commit-capable finish")
	}
	if out.Code != 200 {
		t.Fatalf("resolve status %d: %s", out.Code, out.Body.String())
	}
	if err := generated.Validate("PackageImportPreview", out.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	json.Unmarshal(out.Body.Bytes(), &value)
	if value["status"] != "RESOLVED" {
		t.Fatalf("complete owned local preview not resolved: %s", out.Body.String())
	}
	if value["execution_ready"] != false || value["persisted"] != false || value["input"] == nil {
		t.Fatal("preview/execution boundary changed")
	}
	after := previewBusinessCounts(t, f)
	if !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, after)) {
		t.Fatalf("preview persisted rows: %v -> %v", before, after)
	}
}
func mustPreviewJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func previewAdminJSON(t *testing.T, f packageFixture, path string, value any, key string) []byte {
	t.Helper()
	request, _ := http.NewRequest("POST", f.server.URL+path, bytes.NewReader(mustPreviewJSON(t, value)))
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, request)
	if out.Code < 200 || out.Code >= 300 {
		t.Fatalf("real prerequisite status %d at %s: %s", out.Code, path, out.Body.String())
	}
	return out.Body.Bytes()
}
func publishPreviewResource(t *testing.T, f packageFixture, kind string, uses []string, key string, edits ...func(*generated.DraftInput)) generated.DependencyLock {
	t.Helper()
	var input generated.DraftInput
	if kind == "ROLE" {
		raw, err := os.ReadFile("../../contracts/source/openapi.yaml")
		if err != nil {
			t.Fatal(err)
		}
		var source struct {
			Components struct {
				Schemas map[string]json.RawMessage `json:"schemas"`
			} `json:"components"`
		}
		json.Unmarshal(raw, &source)
		var schema struct {
			Examples []generated.DraftInput `json:"examples"`
		}
		json.Unmarshal(source.Components.Schemas["DraftInput"], &schema)
		for _, raw := range schema.Examples[0].Objects {
			var obj map[string]any
			json.Unmarshal(raw, &obj)
			if obj["kind"] == "ROLE" {
				obj["rights"] = map[string]any{"origin_kind": "SYNTHETIC", "source_label": "P2 synthetic role", "license": "CC0-1.0", "allowed_uses": uses}
				input = generated.DraftInput{Name: "P2 local role", Description: "Synthetic local dependency", Tags: []string{}, RootObjectID: generated.Identity(obj["object_id"].(string)), Objects: []generated.ContentObject{mustPreviewJSON(t, obj)}, DependencyLocks: []generated.DependencyLock{}, ModuleAgentBindings: []generated.ModuleAgentBinding{}, SkillBindings: []generated.SkillBinding{}}
				break
			}
		}
	} else {
		json.Unmarshal([]byte(`{"name":"Local note","description":"Synthetic local dependency","tags":[],"root_object_id":"local-note","objects":[{"object_id":"local-note","kind":"KNOWLEDGE","name":"Local note","description":"Synthetic","tags":[],"rights":{"origin_kind":"SYNTHETIC","source_label":"P2 synthetic","license":"CC0-1.0","allowed_uses":["EDIT","COPY","PUBLISH"]},"audience_policy":"PRODUCT_READERS","version":"1","source_refs":[],"dependency_refs":[],"knowledge_type":"NOTE","text":"Local source","binding_refs":[]}],"dependency_locks":[],"module_agent_bindings":[],"skill_bindings":[]}`), &input)
	}
	for _, edit := range edits {
		edit(&input)
	}
	var draft generated.ContentDraft
	json.Unmarshal(previewAdminJSON(t, f, "/world/v1/admin/content-drafts", map[string]any{"product_id": "coffee", "root_kind": kind, "input": input}, key+"-create"), &draft)
	var checked generated.PublishCheckReport
	json.Unmarshal(previewAdminJSON(t, f, "/world/v1/admin/content-drafts/"+string(draft.DraftID)+"/publish-checks", map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "requested_support_labels": []string{"MOCK_DEMO"}}, key+"-check"), &checked)
	if !checked.Passed {
		t.Fatalf("real local fixture not publishable: %+v", checked.Findings)
	}
	var release generated.Release
	json.Unmarshal(previewAdminJSON(t, f, "/world/v1/admin/content-drafts/"+string(draft.DraftID)+"/releases", map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "check_report_id": checked.ReportID, "rehearsal_report_ids": []string{}, "requested_support_labels": []string{"MOCK_DEMO"}, "approval_note": "Synthetic P2 prerequisite"}, key+"-publish"), &release)
	hash, err := content.Hash(input.Objects[0])
	if err != nil {
		t.Fatal(err)
	}
	return generated.DependencyLock{ReleaseID: release.ReleaseID, ObjectID: input.RootObjectID, Version: "1", ContentHash: hash}
}
func bindingPreviewArchive(t *testing.T) []byte {
	t.Helper()
	source := notePreviewArchive(t)
	reader, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, file := range reader.File {
		if file.Name == "definitions/coffee.json" {
			r, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(r)
			r.Close()
		}
	}
	var doc map[string]any
	json.Unmarshal(body, &doc)
	doc["module_agent_bindings"] = []any{map[string]any{"binding_id": "role-binding", "owner_object_id": "note", "module_id": "AUTHORING", "role_ref": map[string]any{"kind": "DEPENDENCY_EXPORT", "package_id": "deps.roles", "package_version": "1.0.0", "content_sha256": strings.Repeat("e", 64), "logical_id": "role"}, "enabled": false, "generation": map[string]any{"mode": "NONE"}}}
	body = mustPreviewJSON(t, doc)
	return packageWithManifest(t, body, func(m map[string]any) {
		m["kind"] = "KNOWLEDGE"
		m["entries"].([]any)[0].(map[string]any)["kind"] = "KNOWLEDGE"
		m["requirements"].(map[string]any)["presentation_capabilities"] = []any{}
		m["dependencies"] = []any{map[string]any{"package_id": "deps.roles", "package_version": "1.0.0", "content_sha256": strings.Repeat("e", 64), "logical_exports": []string{"role"}}}
		m["entries"].([]any)[0].(map[string]any)["references"] = []string{"role"}
		raw := mustPreviewJSON(t, m)
		var value wepkg.Manifest
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		hash, err := wepkg.ContentHash(value)
		if err != nil {
			t.Fatal(err)
		}
		m["content_sha256"] = hash
	})
}
func TestPackagePreviewHTTPLocalDependenciesAndRights(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0003")
	good := publishPreviewResource(t, f, "ROLE", []string{"EDIT", "COPY", "PUBLISH"}, "p2-role-good")
	noCopy := publishPreviewResource(t, f, "ROLE", []string{"EDIT", "PUBLISH"}, "p2-role-no-copy")
	wrongKind := publishPreviewResource(t, f, "KNOWLEDGE", nil, "p2-note-wrong-kind")
	archive := bindingPreviewArchive(t)
	options := resolveNoteOptions(t, f, archive)
	before := previewBusinessCounts(t, f)
	cases := []struct {
		name   string
		target generated.DependencyLock
		want   string
	}{{"exact_scoped_binding", good, "RESOLVED"}, {"wrong_reference_kind", wrongKind, "BLOCKED"}, {"source_copy_not_granted", noCopy, "BLOCKED"}}
	missing := good
	missing.ReleaseID = "not-visible-release"
	cases = append(cases, struct {
		name   string
		target generated.DependencyLock
		want   string
	}{"missing_or_foreign_release", missing, "BLOCKED"})
	wrongHash := good
	wrongHash.ContentHash = strings.Repeat("f", 64)
	cases = append(cases, struct {
		name   string
		target generated.DependencyLock
		want   string
	}{"wrong_local_content_hash", wrongHash, "BLOCKED"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options["dependency_mappings"] = []any{map[string]any{"pointer": "/module_agent_bindings/0/role_ref", "target": tc.target}}
			out := previewPost(t, f, archive, options, f.token)
			if out.Code != 200 {
				t.Fatalf("current local preview status %d: %s", out.Code, out.Body.String())
			}
			if err := generated.Validate("PackageImportPreview", out.Body.Bytes()); err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			json.Unmarshal(out.Body.Bytes(), &value)
			if value["status"] != tc.want {
				t.Fatalf("want %s got: %s", tc.want, out.Body.String())
			}
			if tc.want == "BLOCKED" {
				if _, found := value["input"]; found || len(value["id_mappings"].([]any)) != 0 {
					t.Fatal("blocked source leaked a partial candidate")
				}
			}
		})
	}
	options["dependency_mappings"] = []any{map[string]any{"pointer": "/module_agent_bindings/0/role_ref", "target": good}}
	if _, err := f.store.DB.Exec("UPDATE releases SET available=0 WHERE release_id=?", good.ReleaseID); err != nil {
		t.Fatal(err)
	}
	out := previewPost(t, f, archive, options, f.token)
	var value map[string]any
	json.Unmarshal(out.Body.Bytes(), &value)
	if value["status"] != "BLOCKED" {
		t.Fatal("previous mapping survived release withdrawal")
	}
	if after := previewBusinessCounts(t, f); !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, after)) {
		t.Fatalf("local matching wrote business rows: %v -> %v", before, after)
	}
}
func TestPackagePreviewHTTPFinalCredentialRecheck(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0004")
	archive := notePreviewArchive(t)
	options := resolveNoteOptions(t, f, archive)
	before := previewBusinessCounts(t, f)
	hit := false
	f.store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		hit = true
		_, err := tx.ExecContext(ctx, "UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'")
		return err
	}
	out := previewPost(t, f, archive, options, f.token)
	if !hit {
		t.Fatal("owning final check not reached")
	}
	if out.Code == 200 {
		t.Fatal("candidate escaped after final credential revocation")
	}
	if bytes.Contains(out.Body.Bytes(), []byte(`"input"`)) {
		t.Fatal("revoked response carried candidate body")
	}
	f.store.BeforeCommit = nil
	if after := previewBusinessCounts(t, f); !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, after)) {
		t.Fatal("failed preview committed rows")
	}
	var enabled bool
	if err := f.store.DB.QueryRow("SELECT enabled FROM operator_credentials WHERE credential_id='owner'").Scan(&enabled); err != nil || !enabled {
		t.Fatal("preview rollback failed to restore controlled fixture change", err)
	}
}

func TestPackagePreviewBrowser(t *testing.T) {
	if os.Getenv("LOREVIVA_PACKAGE_PREVIEW_BROWSER") != "1" {
		t.Skip("explicit owned creator-preview browser fixture required")
	}
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0005")
	target := publishPreviewResource(t, f, "ROLE", []string{"EDIT", "COPY", "PUBLISH"}, "p2-browser-role")
	archive := revisePreviewArchive(t, bindingPreviewArchive(t), func(manifest, body map[string]any, _ map[string][]byte) {
		body["module_agent_bindings"].([]any)[0].(map[string]any)["generation"] = map[string]any{"mode": "REQUIREMENT", "capability_id": "text"}
		requirements := manifest["requirements"].(map[string]any)
		requirements["model_capabilities"] = []any{map[string]any{"capability_id": "text", "required": true}}
		requirements["presentation_capabilities"] = []any{map[string]any{"capability_id": "text", "required": true}}
	})
	input := filepath.Join(t.TempDir(), "world.wepkg")
	if err := os.WriteFile(input, archive, 0600); err != nil {
		t.Fatal(err)
	}
	node := os.Getenv("LOREVIVA_TEST_NODE")
	output := os.Getenv("LOREVIVA_PACKAGE_PREVIEW_EVIDENCE")
	if node == "" || output == "" || os.Getenv("LOREVIVA_TEST_CHROMIUM") == "" {
		t.Fatal("verified browser runtime paths required")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	before := previewBusinessCounts(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "scripts/package-preview-browser.mjs")
	command.Dir = repo
	command.Env = append(os.Environ(), "LOREVIVA_PACKAGE_PREVIEW_URL="+f.server.URL, "LOREVIVA_PACKAGE_PREVIEW_BEARER="+f.token, "LOREVIVA_PACKAGE_PREVIEW_ARCHIVE="+input, "LOREVIVA_PACKAGE_PREVIEW_TARGET="+string(target.ReleaseID)+"/"+string(target.ObjectID))
	raw, err := command.CombinedOutput()
	t.Log(string(raw))
	if err != nil {
		t.Fatal("real creator preview browser", err)
	}
	if after := previewBusinessCounts(t, f); !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, after)) {
		t.Fatalf("preview browser unexpectedly persisted business rows: %v -> %v", before, after)
	}
}

// Build only synthetic test material; production always rereads the original
// archive rather than accepting this test helper's parsed objects.
func revisePreviewArchive(t *testing.T, archive []byte, change func(map[string]any, map[string]any, map[string][]byte)) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range reader.File {
		r, e := file.Open()
		if e != nil {
			t.Fatal(e)
		}
		files[file.Name], e = io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
	var manifest, body map[string]any
	json.Unmarshal(files["manifest.json"], &manifest)
	json.Unmarshal(files["definitions/coffee.json"], &body)
	change(manifest, body, files)
	files["definitions/coffee.json"] = mustPreviewJSON(t, body)
	hash := sha256.Sum256(files["definitions/coffee.json"])
	manifest["entries"].([]any)[0].(map[string]any)["sha256"] = hex.EncodeToString(hash[:])
	for _, value := range manifest["files"].([]any) {
		row := value.(map[string]any)
		bytes := files[row["path"].(string)]
		hash := sha256.Sum256(bytes)
		row["size_bytes"] = len(bytes)
		row["sha256"] = hex.EncodeToString(hash[:])
	}
	var typed wepkg.Manifest
	json.Unmarshal(mustPreviewJSON(t, manifest), &typed)
	digest, err := wepkg.ContentHash(typed)
	if err != nil {
		t.Fatal(err)
	}
	manifest["content_sha256"] = digest
	files["manifest.json"] = mustPreviewJSON(t, manifest)
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for name, raw := range files {
		file, e := writer.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = file.Write(raw); e != nil {
			t.Fatal(e)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if report := wepkg.Check(out.Bytes(), wepkg.DefaultLimits); report.Verdict != "PASS" {
		t.Fatalf("synthetic archive is invalid: %+v", report.Findings)
	}
	return out.Bytes()
}

func TestPackagePreviewHTTPAssetAndRouteGuards(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0006")
	fileBytes := []byte("private synthetic file")
	digest := sha256.Sum256(fileBytes)
	metadata := generated.AssetUploadMetadata{ProductID: "coffee", Name: "note.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "22", ExpectedSha256: hex.EncodeToString(digest[:]), AudiencePolicy: "PRODUCT_EDITORS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "P2 synthetic", License: "CC0-1.0", AllowedUses: []string{"EDIT", "COPY", "PUBLISH"}}}
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	writer.WriteField("metadata", string(mustPreviewJSON(t, metadata)))
	part, _ := writer.CreateFormFile("file", "note.txt")
	part.Write(fileBytes)
	writer.Close()
	request := httptest.NewRequest("POST", "/world/v1/admin/assets", &requestBody)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+f.token)
	request.Header.Set("Idempotency-Key", "p2-asset-upload-0001")
	reply := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(reply, request)
	if reply.Code != 201 && reply.Code != 200 {
		t.Fatalf("asset prerequisite: %d %s", reply.Code, reply.Body.String())
	}
	var file generated.FileRecord
	if err := json.Unmarshal(reply.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	archive := revisePreviewArchive(t, notePreviewArchive(t), func(manifest, body map[string]any, files map[string][]byte) {
		files["assets/note.txt"] = fileBytes
		rights := map[string]any{"license": "CC0-1.0", "source_reference": "fixture:p2-asset", "redistribution_allowed": true, "allowed_uses": []string{"WORLD_CONTENT"}}
		body["objects"] = append(body["objects"].([]any), map[string]any{"object_id": "media", "kind": "ASSET", "name": "Synthetic asset", "description": "P2", "tags": []string{}, "rights": rights, "audience_policy": "PRODUCT_EDITORS", "version": "1", "source_refs": []string{}, "dependency_refs": []any{}, "asset_sha256": file.Sha256, "media_type": file.MediaType, "size_bytes": file.SizeBytes, "client_capabilities": []string{}, "fallback": "TEXT", "file_path": "assets/note.txt"})
		manifest["files"] = append(manifest["files"].([]any), map[string]any{"path": "assets/note.txt", "sha256": file.Sha256, "size_bytes": 22, "media_type": "text/plain", "rights": rights})
	})
	options := resolveNoteOptions(t, f, archive)
	options["rights"] = append(options["rights"].([]any), map[string]any{"object_id": "media", "allowed_uses": []string{"EDIT", "COPY"}})
	options["asset_mappings"] = []any{map[string]any{"file_path": "assets/note.txt", "asset_id": file.AssetID}}
	before := previewBusinessCounts(t, f)
	original := mustPreviewJSON(t, file)
	for _, name := range []string{"ready", "state", "hash", "size", "MIME", "owner", "expiry", "copy"} {
		t.Run(name, func(t *testing.T) {
			var current map[string]any
			json.Unmarshal(original, &current)
			state := "READY"
			switch name {
			case "state":
				state = "FAILED"
			case "hash":
				current["sha256"] = strings.Repeat("f", 64)
			case "size":
				current["size_bytes"] = "23"
			case "MIME":
				current["media_type"] = "text/markdown"
			case "owner":
				current["owner_subject_id"] = "other-subject"
			case "expiry":
				current["rights"].(map[string]any)["expires_at"] = "2000-01-01T00:00:00Z"
			case "copy":
				current["rights"].(map[string]any)["allowed_uses"] = []string{"EDIT"}
			}
			if _, err := f.store.DB.Exec("UPDATE assets SET state=?,document=? WHERE asset_id=?", state, mustPreviewJSON(t, current), file.AssetID); err != nil {
				t.Fatal(err)
			}
			out := previewPost(t, f, archive, options, f.token)
			var report map[string]any
			json.Unmarshal(out.Body.Bytes(), &report)
			want := "BLOCKED"
			if name == "ready" {
				want = "RESOLVED"
			}
			if out.Code != 200 || report["status"] != want {
				t.Fatalf("%s: %d %s", name, out.Code, out.Body.String())
			}
		})
	}
	if _, err := f.store.DB.Exec("UPDATE assets SET state='READY',document=? WHERE asset_id=?", original, file.AssetID); err != nil {
		t.Fatal(err)
	}
	if after := previewBusinessCounts(t, f); !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, after)) {
		t.Fatal("asset preview persisted business rows")
	}
	for _, mode := range []string{"INHERIT", "REQUIREMENT"} {
		t.Run("route_"+mode, func(t *testing.T) {
			f := newPackageFixture(t, "private-synthetic-p2-route-operator-token-"+mode)
			archive := revisePreviewArchive(t, validPackage(t, portablePackageWorldBody(t)), func(manifest, body map[string]any, _ map[string][]byte) {
				for _, value := range body["objects"].([]any) {
					object := value.(map[string]any)
					if object["kind"] == "WORLD" {
						g := map[string]any{"mode": mode}
						if mode == "REQUIREMENT" {
							g["capability_id"] = "text"
						}
						object["generation"] = g
					}
				}
				manifest["requirements"].(map[string]any)["model_capabilities"] = []any{map[string]any{"capability_id": "text", "required": true}}
			})
			discover := previewPost(t, f, archive, map[string]any{"stage": "DISCOVER"}, f.token)
			var report map[string]any
			json.Unmarshal(discover.Body.Bytes(), &report)
			if report["status"] != "NEEDS_MAPPING" {
				t.Fatalf("route discovery: %s", discover.Body.String())
			}
			rights := []any{}
			for _, value := range report["requirements"].(map[string]any)["objects"].([]any) {
				rights = append(rights, map[string]any{"object_id": value.(map[string]any)["object_id"], "allowed_uses": []string{"EDIT", "COPY"}})
			}
			opts := map[string]any{"stage": "RESOLVE", "entrypoint": report["entrypoint"], "namespace": report["namespace"], "dependency_mappings": []any{}, "additional_locks": []any{}, "model_mappings": []any{}, "local_capabilities": map[string]string{"text": "TEXT"}, "asset_mappings": []any{}, "rights": rights, "rights_attested": true}
			if mode == "REQUIREMENT" {
				opts["model_mappings"] = []any{map[string]any{"capability_id": "text", "settings": map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}}}}
			}
			{
				out := previewPost(t, f, archive, opts, f.token)
				json.Unmarshal(out.Body.Bytes(), &report)
				if out.Code != 200 || report["status"] != "RESOLVED" {
					t.Fatalf("inherited ready: %d %s", out.Code, out.Body.String())
				}
				previewAdminJSON(t, f, "/world/v1/admin/settings/commands", map[string]any{"scope": map[string]any{"scope_type": "TENANT", "tenant_id": "demo-local"}, "kind": "DISABLE_CONNECTION", "expected_revision": "1", "connection_id": "mock-local"}, "p2-disable-mock")
			}
			out := previewPost(t, f, archive, opts, f.token)
			json.Unmarshal(out.Body.Bytes(), &report)
			if out.Code != 200 || report["status"] != "BLOCKED" {
				t.Fatalf("disabled route: %d %s", out.Code, out.Body.String())
			}
			findings := report["findings"].([]any)
			if len(findings) != 1 || findings[0].(map[string]any)["code"] != "LOCAL_DEPENDENCY_UNAVAILABLE" {
				t.Fatalf("route refusal occurred before current installation admission: %s", out.Body.String())
			}
		})
	}
}

func TestPackagePreviewHTTPTransitiveSourceRights(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-p2-preview-operator-token-0007")
	archive := bindingPreviewArchive(t)
	options := resolveNoteOptions(t, f, archive)
	for _, copyAllowed := range []bool{true, false} {
		key := "p2-child-copy"
		if !copyAllowed {
			key = "p2-child-no-copy"
		}
		target := publishPreviewResource(t, f, "ROLE", []string{"EDIT", "COPY", "PUBLISH"}, key, func(input *generated.DraftInput) {
			var root map[string]any
			json.Unmarshal(input.Objects[0], &root)
			root["knowledge_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "nested-note"}}
			input.Objects[0] = mustPreviewJSON(t, root)
			uses := []string{"EDIT", "PUBLISH"}
			if copyAllowed {
				uses = append(uses, "COPY")
			}
			input.Objects = append(input.Objects, mustPreviewJSON(t, map[string]any{"object_id": "nested-note", "kind": "KNOWLEDGE", "name": "Nested source", "description": "Synthetic", "tags": []string{}, "rights": map[string]any{"origin_kind": "SYNTHETIC", "source_label": "P2 nested fixture", "license": "CC0-1.0", "allowed_uses": uses}, "audience_policy": "PRODUCT_READERS", "version": "1", "source_refs": []string{}, "dependency_refs": []any{}, "knowledge_type": "NOTE", "text": "Nested source content", "binding_refs": []any{}}))
		})
		before := previewBusinessCounts(t, f)
		options["dependency_mappings"] = []any{map[string]any{"pointer": "/module_agent_bindings/0/role_ref", "target": target}}
		out := previewPost(t, f, archive, options, f.token)
		var report map[string]any
		json.Unmarshal(out.Body.Bytes(), &report)
		want := "BLOCKED"
		if copyAllowed {
			want = "RESOLVED"
		}
		if out.Code != 200 || report["status"] != want {
			t.Fatalf("nested source copy=%v: %d %s", copyAllowed, out.Code, out.Body.String())
		}
		if after := previewBusinessCounts(t, f); !bytes.Equal(mustPreviewJSON(t, before), mustPreviewJSON(t, after)) {
			t.Fatal("source preview wrote rows")
		}
	}
}
