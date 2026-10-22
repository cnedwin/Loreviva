package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wepkg"
	"loreviva.local/loreviva/tests/integration/dbfixture"
)

type packageFixture struct {
	server *httptest.Server
	token  string
	store  *store.Store
	scope  domain.ProductScope
}

func newPackageFixture(t *testing.T, token string) packageFixture {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	privateRoot := t.TempDir()
	initializeTestConfiguration(t, s, privateRoot)
	if e := maintenance.BootstrapSynthetic(ctx, s, scope, "owner", token); e != nil {
		t.Fatal(e)
	}
	f, e := files.New(s, privateRoot)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	r := app.NewRuntime(s, f, auth.New(s, scope, "owner", token))
	server := httptest.NewServer(r.Handler())
	t.Cleanup(server.Close)
	return packageFixture{server: server, token: token, store: s, scope: scope}
}

func (f packageFixture) post(t *testing.T, token string, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req, e := http.NewRequest("POST", f.server.URL+"/world/v1/admin/content-packages/preflight?product_id=coffee", bytes.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "preflight-key-0001")
	req.Header.Set("Content-Type", contentType)
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, req)
	return out
}

func (f packageFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req, _ := http.NewRequest("GET", f.server.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	out := httptest.NewRecorder()
	f.server.Config.Handler.ServeHTTP(out, req)
	return out
}

// validPackage builds a package the preflight must accept: one declared file,
// one entry pointing at it, and the content digest computed with the same rule
// the check applies.
func validPackage(t *testing.T, body []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	m := wepkg.Manifest{
		FormatVersion: "1.0", PackageID: "coffee.demo", Kind: "WORLD", PackageVersion: "1.0.0",
		Entrypoints: []string{"coffee_demo"},
		Entries:     []wepkg.Entry{{LogicalID: "coffee_demo", Kind: "WORLD", Path: "definitions/coffee.json", Version: "1.0.0", SHA256: digest}},
		Files:       []wepkg.FileRecord{{Path: "definitions/coffee.json", SHA256: digest, SizeBytes: int64(len(body)), MediaType: "application/json", Rights: wepkg.Rights{License: "CC0-1.0", SourceReference: "synthetic", RedistributionAllowed: true, AllowedUses: []string{"WORLD_BACKGROUND"}}}},
		Provenance:  wepkg.Provenance{AuthorDisplayName: "test", License: "CC0-1.0", SourceReferences: []string{"synthetic"}},
	}
	m.Requirements.Presentation = []wepkg.Capability{{CapabilityID: "text", Required: true}}
	content, e := wepkg.ContentHash(m)
	if e != nil {
		t.Fatal(e)
	}
	m.ContentSHA256 = content
	manifest, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, part := range []struct {
		name string
		body []byte
	}{{"manifest.json", manifest}, {"definitions/coffee.json", body}} {
		f, e := w.Create(part.name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(part.body); e != nil {
			t.Fatal(e)
		}
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	return buf.Bytes()
}

func multipartBody(t *testing.T, archive []byte, field string) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, e := mw.CreateFormFile(field, "package.wepkg")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = part.Write(archive); e != nil {
		t.Fatal(e)
	}
	if e = mw.Close(); e != nil {
		t.Fatal(e)
	}
	return mw.FormDataContentType(), body.Bytes()
}

func countRows(t *testing.T, s *store.Store) [4]int {
	t.Helper()
	var out [4]int
	if e := s.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM assets),(SELECT COUNT(*) FROM content_drafts),(SELECT COUNT(*) FROM harness_turns)").Scan(&out[0], &out[1], &out[2], &out[3]); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestContentPackagePreflightPassAndDeterminism(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-preflight-operator-token-0001")
	before := countRows(t, f.store)
	archive := validPackage(t, []byte(`{"name":"coffee"}`))
	contentType, body := multipartBody(t, archive, "archive")

	first := f.post(t, f.token, contentType, body)
	if first.Code != 200 {
		t.Fatalf("preflight status %d: %s", first.Code, first.Body.String())
	}
	var report wepkg.Report
	if e := json.Unmarshal(first.Body.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.Verdict != "PASS" {
		t.Fatalf("valid package rejected: %v", report.Findings)
	}
	second := f.post(t, f.token, contentType, body)
	var again wepkg.Report
	if e := json.Unmarshal(second.Body.Bytes(), &again); e != nil {
		t.Fatal(e)
	}
	if again.ReportSHA256 != report.ReportSHA256 {
		t.Fatalf("report digest is not deterministic: %s vs %s", report.ReportSHA256, again.ReportSHA256)
	}
	if after := countRows(t, f.store); after != before {
		t.Fatalf("preflight wrote business state: %v -> %v", before, after)
	}
}

func TestContentPackagePreflightRejectsWithCompleteReport(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-preflight-operator-token-0002")
	archive := validPackage(t, []byte(`{"name":"coffee"}`))
	// Add a member nobody declared, then rebuild the archive around it: the
	// report must say so rather than failing the request.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	reader, e := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if e != nil {
		t.Fatal(e)
	}
	for _, file := range reader.File {
		part, e := w.Create(file.Name)
		if e != nil {
			t.Fatal(e)
		}
		rc, e := file.Open()
		if e != nil {
			t.Fatal(e)
		}
		var raw bytes.Buffer
		if _, e = raw.ReadFrom(rc); e != nil {
			t.Fatal(e)
		}
		rc.Close()
		if _, e = part.Write(raw.Bytes()); e != nil {
			t.Fatal(e)
		}
	}
	extra, e := w.Create("definitions/undeclared.json")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = extra.Write([]byte(`{}`)); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}

	contentType, body := multipartBody(t, buf.Bytes(), "archive")
	out := f.post(t, f.token, contentType, body)
	if out.Code != 200 {
		t.Fatalf("a rejected package must still answer with a report, got %d: %s", out.Code, out.Body.String())
	}
	var report wepkg.Report
	if e := json.Unmarshal(out.Body.Bytes(), &report); e != nil {
		t.Fatal(e)
	}
	if report.Verdict != "REJECT" {
		t.Fatalf("undeclared member accepted")
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Code == wepkg.CodeFileUndeclared {
			found = true
		}
	}
	if !found {
		t.Fatalf("report does not name the undeclared member: %+v", report.Findings)
	}
}

func TestContentPackagePreflightRequestShape(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-preflight-operator-token-0003")
	archive := validPackage(t, []byte(`{"name":"coffee"}`))

	if out := f.post(t, f.token, "application/json", []byte(`{}`)); out.Code != 400 {
		t.Fatalf("non-multipart accepted: %d", out.Code)
	}
	contentType, body := multipartBody(t, archive, "file")
	if out := f.post(t, f.token, contentType, body); out.Code != 400 {
		t.Fatalf("wrong part name accepted: %d", out.Code)
	}
	contentType, body = multipartBody(t, archive, "archive")
	if out := f.post(t, "not-an-operator", contentType, body); out.Code != 401 && out.Code != 403 {
		t.Fatalf("missing operator authority accepted: %d", out.Code)
	}
}

func TestContentPackageCapabilitiesPublishesLimits(t *testing.T) {
	f := newPackageFixture(t, "private-synthetic-preflight-operator-token-0004")
	out := f.get(t, "/world/v1/admin/content-packages/capabilities?product_id=coffee")
	if out.Code != 200 {
		t.Fatalf("capabilities status %d: %s", out.Code, out.Body.String())
	}
	var caps struct {
		RegistryVersion string           `json:"registry_version"`
		Limits          map[string]int64 `json:"limits"`
		Capabilities    []string         `json:"capabilities"`
	}
	if e := json.Unmarshal(out.Body.Bytes(), &caps); e != nil {
		t.Fatal(e)
	}
	if caps.RegistryVersion != "1" || caps.Limits["file_bytes"] != wepkg.DefaultLimits.FileBytes || len(caps.Capabilities) == 0 {
		t.Fatalf("published limits are not the applied ones: %+v", caps)
	}
}
