package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"loreviva.local/loreviva/internal/generated"
)

func TestStudioEmbeddedManifest(t *testing.T) {
	data, err := studioFiles.ReadFile("studio_dist/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SourceHash string `json:"source_hash"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SourceHash != generated.SourceHash {
		t.Fatalf("embedded Studio uses contract %s; current server uses %s", manifest.SourceHash, generated.SourceHash)
	}
	files, err := fs.Sub(studioFiles, "studio_dist")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStudioManifest(files, generated.SourceHash); err != nil {
		t.Fatal(err)
	}
}

func TestStudioManifestRejectsStaleOrDamagedAssets(t *testing.T) {
	fixture := func() fstest.MapFS {
		body := []byte("<script src=\"/studio/assets/app.js\"></script>")
		manifest := fmt.Sprintf(`{"format":"loreviva.studio-build.v1","source_hash":"current","files":[{"path":"index.html","size":%d,"sha256":"%x"}]}`, len(body), sha256.Sum256(body))
		return fstest.MapFS{"index.html": {Data: body}, "manifest.json": {Data: []byte(manifest)}}
	}
	if err := validateStudioManifest(fixture(), "current"); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"contract", "tampered", "missing", "extra", "legacy"} {
		t.Run(change, func(t *testing.T) {
			files := fixture()
			expected := "current"
			switch change {
			case "contract":
				expected = "different"
			case "tampered":
				files["index.html"].Data = []byte("tampered")
			case "missing":
				delete(files, "index.html")
			case "extra":
				files["assets/old.js"] = &fstest.MapFile{Data: []byte("old")}
			case "legacy":
				files["manifest.json"].Data = []byte(`{"source_hash":"current"}`)
			}
			if err := validateStudioManifest(files, expected); err == nil {
				t.Fatal("invalid artifact accepted")
			}
		})
	}
}

func TestStudioEmbeddedRoutesAndCSP(t *testing.T) {
	h := studioHandler()
	for _, route := range []string{"/studio/", "/studio/editor", "/studio/resources", "/studio/runtime", "/studio/settings"} {
		q := httptest.NewRequest("GET", route, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, q)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "/studio/assets/") || !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("route %s: %d", route, w.Code)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-") {
			t.Fatal("unsafe CSP", csp)
		}
	}
	for _, route := range []string{"/studio/missing", "/studio/assets/missing.js", "/studio/../world/v1/assets/private", "/studio/%2e%2e/private", "/world/v1/assets/private", "/studio/private.json"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", route, nil))
		if w.Code != 404 {
			t.Fatalf("must not SPA fallback %s: %d", route, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/studio/editor", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("HEAD", "/studio/editor", nil))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD")
	}
	// The actual top-level handler must serve the embedded UI even before local
	// database initialization. It must not enter a synthetic auth/session flow.
	w = httptest.NewRecorder()
	(&Runtime{}).Handler().ServeHTTP(w, httptest.NewRequest("GET", "/studio/editor", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	entries, err := studioFiles.ReadDir("studio_dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/studio/assets/"+e.Name(), nil))
		b, _ := io.ReadAll(w.Result().Body)
		if w.Code != 200 || len(b) == 0 || strings.Contains(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("asset %s", e.Name())
		}
	}
}
