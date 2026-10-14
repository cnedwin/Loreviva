package integration

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/maintenance"
)

// Explicit opt-in: this fixture serves the real embedded production bundle and
// existing owning services, with a fresh synthetic database and real worker.
// It has no test-only HTTP endpoints or fabricated successful API responses.
func TestStudioBrowser(t *testing.T) {
	if os.Getenv("LOREVIVA_STUDIO_BROWSER") != "1" {
		t.Skip("explicit local browser fixture required")
	}
	r, draft, _ := candidateHTTPFixture(t)
	// Synthetic subject prerequisite only. Credential registration and grants
	// use the real owning entry points; no SQL grants or fabricated principal.
	secondScope := r.Auth.Scope()
	secondScope.SubjectID = "browser-second-creator"
	if _, e := r.Store.DB.Exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES(?,?,?,1,1,1,1)", secondScope.TenantID, secondScope.ProductID, secondScope.SubjectID); e != nil {
		t.Fatal(e)
	}
	const secondSecret = "synthetic-browser-second-credential-at-least32"
	if e := maintenance.RegisterLocalCredential(runtimeContext(t, r), r.Store, secondScope, "browser-second", secondSecret); e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := listener.Addr().String()
	listener.Close()
	base := "http://" + address
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	cfg := config.Config{Mode: "all", ListenAddr: address}
	go func() { done <- app.Run(ctx, cfg, r.Dependencies(cfg)) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error("all mode shutdown", e)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		response, e := client.Get(base + "/ready")
		if e == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("actual all mode never became ready")
	}
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	runctx, stop := context.WithTimeout(context.Background(), 4*time.Minute)
	defer stop()
	if os.Getenv("LOREVIVA_TEST_PNPM") == "" {
		t.Fatal("explicit verified pnpm path required")
	}
	cmd := exec.CommandContext(runctx, os.Getenv("LOREVIVA_TEST_NODE"), os.Getenv("LOREVIVA_TEST_PNPM"), "--config.verify-deps-before-run=error", "test:e2e", "--project=creator")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "LOREVIVA_STUDIO_URL="+base, "LOREVIVA_STUDIO_BEARER="+runtimeToken, "LOREVIVA_STUDIO_SECOND_BEARER="+secondSecret, "LOREVIVA_STUDIO_DRAFT="+draft)
	// Private bearer and database DSN are only inherited environment values.
	out, e := cmd.CombinedOutput()
	t.Log(string(out))
	if e != nil {
		t.Fatal("browser process", e)
	}
}
