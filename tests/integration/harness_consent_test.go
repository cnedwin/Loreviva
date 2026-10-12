package integration

import (
	"bytes"
	"context"
	"encoding/json"
	driver "github.com/go-sql-driver/mysql"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHarnessFreshBootstrapConsent(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-deny", true: "explicit-opt-in"}[optIn], func(t *testing.T) {
			ctx := context.Background()
			db := dbfixture.Open(t)
			s := store.New(db)
			if e := s.Migrate(ctx); e != nil {
				t.Fatal(e)
			}
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			if e := maintenance.BootstrapSyntheticWithConsent(ctx, s, scope, "owner", runtimeToken, optIn); e != nil {
				t.Fatal(e)
			}
			var grants int
			if e := db.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&grants); e != nil {
				t.Fatal(e)
			}
			if grants != map[bool]int{false: 0, true: 1}[optIn] {
				t.Fatal("explicit bootstrap consent not honored", grants)
			}
			if !optIn {
				if e := maintenance.BootstrapSyntheticWithConsent(ctx, s, scope, "owner", runtimeToken, true); e == nil {
					t.Fatal("repeat bootstrap retroactively granted generation")
				}
				if e := db.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&grants); e != nil || grants != 0 {
					t.Fatal("repeat flag seeded missing grant", grants, e)
				}
				return
			}
			var accepted, expiry time.Time
			var policy, issuer string
			if e := db.QueryRow("SELECT accepted_at,expires_at,policy_id,credential_id FROM r0_generation_consent WHERE subject_id='creator'").Scan(&accepted, &expiry, &policy, &issuer); e != nil {
				t.Fatal(e)
			}
			if expiry.Sub(accepted) != 30*24*time.Hour || policy != "R0_OWNER_PRIVATE_GENERATION_30D" || issuer != "owner" {
				t.Fatal("unbounded/unattributed consent", accepted, expiry, policy, issuer)
			}
			if e := maintenance.BootstrapSyntheticWithConsent(ctx, s, scope, "owner", runtimeToken, true); e != nil {
				t.Fatal(e)
			}
			var again time.Time
			if e := db.QueryRow("SELECT expires_at FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&again); e != nil || again != expiry {
				t.Fatal("bootstrap replay extended original grant", again, expiry, e)
			}
			if _, e := db.Exec("DELETE FROM memory_subject_permissions WHERE action='memory.generation.use'"); e != nil {
				t.Fatal(e)
			}
			_ = maintenance.BootstrapSyntheticWithConsent(ctx, s, scope, "owner", runtimeToken, true)
			if e := db.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&grants); e != nil || grants != 0 {
				t.Fatal("missing old grant reconstructed", grants, e)
			}
		})
	}
}

func TestHarnessActualBootstrapConsentCLI(t *testing.T) {
	goexe := os.Getenv("LOREVIVA_TEST_GO_EXE")
	if goexe == "" {
		goexe = "go"
	}
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	exe := filepath.Join(t.TempDir(), "loreviva.exe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, goexe, "build", "-o", exe, "./cmd/loreviva")
	build.Dir = repo
	if raw, e := build.CombinedOutput(); e != nil {
		t.Fatalf("local CLI build failed: %s", raw)
	}
	for _, optIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "explicit"}[optIn], func(t *testing.T) {
			db := dbfixture.Open(t)
			s := store.New(db)
			if e := s.Migrate(ctx); e != nil {
				t.Fatal(e)
			}
			var database string
			if e := db.QueryRow("SELECT DATABASE()").Scan(&database); e != nil {
				t.Fatal(e)
			}
			cfg, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
			if e != nil {
				t.Fatal("private fixture DSN parsing")
			}
			env := append(os.Environ(), "LOREVIVA_DB_ADDR="+cfg.Addr, "LOREVIVA_DB_NAME="+database, "LOREVIVA_DB_USER="+cfg.User, "LOREVIVA_DB_PASSWORD_REF=env:TASK6_CLI_PASSWORD", "TASK6_CLI_PASSWORD="+cfg.Passwd, "LOREVIVA_OPERATOR_SECRET_REF=env:TASK6_CLI_OPERATOR", "TASK6_CLI_OPERATOR="+runtimeToken, "LOREVIVA_FILES_ROOT="+t.TempDir(), "LOREVIVA_GOVERNANCE_ROOT="+t.TempDir())
			args := []string{"bootstrap", "--mock", "--tenant", "demo-local"}
			if optIn {
				args = append(args, "--owner-private-generation-consent")
			}
			call := func() map[string]any {
				cmd := exec.CommandContext(ctx, exe, args...)
				cmd.Dir = repo
				cmd.Env = env
				raw, e := cmd.CombinedOutput()
				if bytes.Contains(raw, []byte(runtimeToken)) || len(cfg.Passwd) > 0 && bytes.Contains(raw, []byte(cfg.Passwd)) {
					t.Fatal("CLI emitted secret")
				}
				if e != nil {
					t.Fatal("CLI bootstrap refused", e, string(raw))
				}
				var out map[string]any
				if e := json.Unmarshal(raw, &out); e != nil {
					t.Fatal(e)
				}
				return out
			}
			out := call()
			if out["owner_private_generation_consent_requested"] != optIn || out["owner_private_generation_grant_current"] != optIn {
				t.Fatal("CLI consent state misleading", out)
			}
			if optIn {
				if _, e := db.Exec("UPDATE memory_subject_permissions SET expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE action='memory.generation.use'"); e != nil {
					t.Fatal(e)
				}
				out = call()
				if out["owner_private_generation_consent_requested"] != true || out["owner_private_generation_grant_current"] != false {
					t.Fatal("replay renewed or misreported expired consent", out)
				}
			}
		})
	}
}
