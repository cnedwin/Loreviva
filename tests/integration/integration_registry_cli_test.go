package integration

import (
	"bytes"
	"context"
	"encoding/json"
	driver "github.com/go-sql-driver/mysql"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestIntegrationRegistryActualCLI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	goexe := os.Getenv("LOREVIVA_TEST_GO_EXE")
	if goexe == "" {
		t.Fatal("pinned Go executable required")
	}
	exe := filepath.Join(t.TempDir(), "loreviva.exe")
	build := exec.CommandContext(ctx, goexe, "build", "-o", exe, "./cmd/loreviva")
	build.Dir = repo
	if raw, e := build.CombinedOutput(); e != nil {
		t.Fatalf("CLI build: %s", raw)
	}
	db := dbfixture.Open(t)
	s := store.New(db)
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	var database string
	if e = db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); e != nil {
		t.Fatal(e)
	}
	cfg, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("private fixture configuration")
	}
	secret := "synthetic-task8-service-key-01234567890123456789"
	root := t.TempDir()
	secretFile := filepath.Join(root, "service-key")
	if e = os.WriteFile(secretFile, []byte(secret), 0600); e != nil {
		t.Fatal(e)
	}
	env := append(os.Environ(), "LOREVIVA_DB_ADDR="+cfg.Addr, "LOREVIVA_DB_NAME="+database, "LOREVIVA_DB_USER="+cfg.User, "LOREVIVA_DB_PASSWORD_REF=env:TASK8_CLI_PASSWORD", "TASK8_CLI_PASSWORD="+cfg.Passwd, "LOREVIVA_OPERATOR_SECRET_REF=env:TASK8_CLI_OPERATOR", "TASK8_CLI_OPERATOR="+runtimeToken, "LOREVIVA_FILES_ROOT="+t.TempDir(), "LOREVIVA_GOVERNANCE_ROOT="+t.TempDir())
	call := func(wantOK bool, args ...string) []byte {
		t.Helper()
		c := exec.CommandContext(ctx, exe, args...)
		c.Dir = repo
		c.Env = env
		raw, e := c.CombinedOutput()
		for _, v := range []string{secret, runtimeToken, cfg.Passwd} {
			if v != "" && bytes.Contains(raw, []byte(v)) {
				t.Fatal("secret in CLI output")
			}
		}
		if (e == nil) != wantOK {
			t.Fatalf("CLI result mismatch: %v %s", e, raw)
		}
		return raw
	}
	call(true, "bootstrap", "--mock", "--tenant", "demo-local")
	var installation string
	if e = db.QueryRowContext(ctx, "SELECT installation_id FROM memory_installation").Scan(&installation); e != nil {
		t.Fatal(e)
	}
	call(true, "integration", "init-registrar", "--key", "cli-init", "--installation", installation)
	var now time.Time
	if e = db.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		t.Fatal(e)
	}
	c := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "cli-customer", Customer: &domain.RegisterCustomer{CustomerID: "cli-customer", ServiceSubjectID: "cli-service", ConsumerID: "cli-consumer", ExpiresAt: now.Add(time.Hour)}}
	raw, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	request := filepath.Join(root, "request.json")
	if e = os.WriteFile(request, raw, 0600); e != nil {
		t.Fatal(e)
	}
	decode := func(raw []byte) domain.RegistryReceipt {
		t.Helper()
		var r domain.RegistryReceipt
		if e := json.Unmarshal(raw, &r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	first := decode(call(true, "integration", "registry", "--key", "cli-register", "--request-file", request, "--secret-file", secretFile))
	original := decode(call(true, "integration", "receipt", "--key", "cli-register"))
	replay := decode(call(true, "integration", "registry", "--key", "cli-register", "--request-file", request, "--secret-file", secretFile))
	if !reflect.DeepEqual(first, original) || !reflect.DeepEqual(first, replay) || first.RegistrationExpiry == nil {
		t.Fatal("CLI lost original registration receipt")
	}
	// Closed input rejects an attempted authority/secret field before any write.
	raw = append(raw[:len(raw)-1], []byte(",\"principal\":\"owner\"}")...)
	if e = os.WriteFile(request, raw, 0600); e != nil {
		t.Fatal(e)
	}
	call(false, "integration", "registry", "--key", "cli-unknown-field", "--request-file", request, "--secret-file", secretFile)
	var customers, clients, uses, receipts int
	for q, dest := range map[string]*int{"SELECT COUNT(*) FROM integration_customers": &customers, "SELECT COUNT(*) FROM integration_principals WHERE kind='CLIENT'": &clients, "SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'": &uses, "SELECT COUNT(*) FROM integration_receipts": &receipts} {
		if e = db.QueryRowContext(ctx, q).Scan(dest); e != nil {
			t.Fatal(e)
		}
	}
	if customers != 1 || clients != 0 || uses != 0 || receipts != 2 {
		t.Fatal("CLI broadened registration", customers, clients, uses, receipts)
	}
}
