package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageTransferBrowser(t *testing.T) {
	if os.Getenv("LOREVIVA_PACKAGE_TRANSFER_BROWSER") != "1" {
		t.Skip("explicit owned transfer browser fixture required")
	}
	original, release, _, _ := transferOriginal(t)
	second, _, _ := runtimeFixture(t)
	a, b := transferFixture(t, original), transferFixture(t, second)
	first := true
	baseA := a.server.Config.Handler
	a.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "POST" && q.URL.Path == "/world/v1/admin/content-packages/exports" && first {
			first = false
			recorded := httptest.NewRecorder()
			baseA.ServeHTTP(recorded, q)
			if recorded.Code == 201 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				w.Write([]byte("{"))
				return
			}
			for k, values := range recorded.Header() {
				for _, v := range values {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(recorded.Code)
			w.Write(recorded.Body.Bytes())
			return
		}
		baseA.ServeHTTP(w, q)
	})
	baseB := b.server.Config.Handler
	b.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/rehearsals") {
			recorded := httptest.NewRecorder()
			baseB.ServeHTTP(recorded, q)
			if recorded.Code == 202 {
				if e := second.Worker.Once(runtimeContext(t, second)); e != nil {
					t.Error("actual browser Mock worker", e)
				}
			}
			for k, values := range recorded.Header() {
				for _, v := range values {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(recorded.Code)
			w.Write(recorded.Body.Bytes())
			return
		}
		baseB.ServeHTTP(w, q)
	})
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("LOREVIVA_TEST_NODE"), "scripts/package-transfer-browser.mjs")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "LOREVIVA_TRANSFER_ORIGINAL="+a.server.URL, "LOREVIVA_TRANSFER_SECOND="+b.server.URL, "LOREVIVA_TRANSFER_BEARER="+runtimeToken, "LOREVIVA_TRANSFER_RELEASE="+string(release.ReleaseID))
	raw, e := cmd.CombinedOutput()
	t.Log(string(raw))
	if e != nil {
		t.Fatal("actual two-installation creator transfer browser", e)
	}
	if transferCount(t, original, "content_package_exports") != 2 || transferCount(t, second, "content_package_imports") != 1 || transferCount(t, second, "content_package_adoptions") != 1 || transferCount(t, second, "content_releases") != 2 {
		t.Fatal("browser effects differ from original recovery and new installation")
	}
}
