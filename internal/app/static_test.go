package app

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

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
