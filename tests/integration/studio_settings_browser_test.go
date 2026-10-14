package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
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
func TestStudioSettingsEntryBrowser(t *testing.T) {
	for _, mode := range []string{"PRODUCT", "TENANT", "TENANT_DISABLED_HOME"} {
		t.Run(mode, func(t *testing.T) {
			if os.Getenv("LOREVIVA_STUDIO_BROWSER") != "1" {
				t.Skip("explicit local browser fixture required")
			}
			r, draft, _ := candidateHTTPFixture(t)
			ctxOwner := runtimeContext(t, r)
			scope := r.Auth.Scope()
			const secret = "synthetic-settings-only-credential-at-least32"
			if e := maintenance.RegisterLocalCredential(ctxOwner, r.Store, scope, "settings-only", secret); e != nil {
				t.Fatal(e)
			}
			kind := "TENANT"
			use := "TENANT_SETTINGS"
			sc := map[string]any{"scope_type": "TENANT", "tenant_id": scope.TenantID}
			if mode == "PRODUCT" {
				kind = "PRODUCT"
				use = "PRODUCT_SETTINGS"
				sc["scope_type"] = kind
				sc["product_id"] = scope.ProductID
			}
			raw, _ := json.Marshal(map[string]any{"kind": "SET_SCOPE_GRANT", "scope": sc, "credential_id": "settings-only", "subject_id": scope.SubjectID, "expected_revision": "0", "grant": map[string]any{"scope": sc, "action": "settings.read", "use": use, "effect": "ALLOW", "enabled": true, "expires_at": nil}})
			if _, e := r.Store.ApplyMember(ctxOwner, "settings-entry-grant", generated.StudioMemberCommandRequest(raw)); e != nil {
				t.Fatal(e)
			}
			if mode == "TENANT_DISABLED_HOME" {
				if _, e := r.Store.DB.Exec("UPDATE product_gates SET enabled=0 WHERE tenant_id=? AND product_id=?", scope.TenantID, scope.ProductID); e != nil {
					t.Fatal(e)
				}
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
			cmd := exec.CommandContext(runctx, os.Getenv("LOREVIVA_TEST_NODE"), os.Getenv("LOREVIVA_TEST_PNPM"), "--config.verify-deps-before-run=error", "test:e2e", "--project=fix1-settings")
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "LOREVIVA_STUDIO_URL="+base, "LOREVIVA_STUDIO_BEARER="+runtimeToken, "LOREVIVA_STUDIO_SETTINGS_BEARER="+secret, "LOREVIVA_STUDIO_SETTINGS_KIND="+kind, "LOREVIVA_STUDIO_SETTINGS_CASE="+mode, "LOREVIVA_STUDIO_EVIDENCE="+filepath.Join(os.Getenv("LOREVIVA_STUDIO_EVIDENCE"), mode), "LOREVIVA_STUDIO_DRAFT="+draft)
			// Private bearer and database DSN are only inherited environment values.
			out, e := cmd.CombinedOutput()
			t.Log(string(out))
			if e != nil {
				t.Fatal("browser process", e)
			}
		})
	}
}
