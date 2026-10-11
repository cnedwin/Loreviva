package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	driver "github.com/go-sql-driver/mysql"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
func TestActualCLIAndGoRunAll(t *testing.T) {
	db := dbfixture.Open(t)
	var database string
	if e := db.QueryRow("SELECT DATABASE()").Scan(&database); e != nil {
		t.Fatal(e)
	}
	cfg, e := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("private DSN parsing")
	}
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	goexe := os.Getenv("LOREVIVA_TEST_GO_EXE")
	if goexe == "" {
		goexe = "go"
	}
	private := t.TempDir()
	var random [32]byte
	if _, e = rand.Read(random[:]); e != nil {
		t.Fatal(e)
	}
	token := hex.EncodeToString(random[:])
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := listener.Addr().String()
	listener.Close()
	env := append(os.Environ(), "LOREVIVA_DB_ADDR="+cfg.Addr, "LOREVIVA_DB_NAME="+database, "LOREVIVA_DB_USER="+cfg.User, "LOREVIVA_DB_PASSWORD_REF=env:CLI_TEST_DB_PASSWORD", "CLI_TEST_DB_PASSWORD="+cfg.Passwd, "LOREVIVA_OPERATOR_SECRET_REF=env:CLI_TEST_OPERATOR_SECRET", "CLI_TEST_OPERATOR_SECRET="+token, "LOREVIVA_FILES_ROOT="+private, "LOREVIVA_HTTP_ADDR="+addr, "LOREVIVA_PUBLIC_BASE_URL=http://"+addr)
	call := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, goexe, append([]string{"run", "./cmd/loreviva"}, args...)...)
		cmd.Dir = repo
		cmd.Env = env
		output, e := cmd.CombinedOutput()
		if bytes.Contains(output, []byte(token)) || bytes.Contains(output, []byte(cfg.Passwd)) {
			t.Fatal("CLI secret leakage")
		}
		if e != nil {
			t.Fatalf("CLI %s failed: %s", strings.Join(args, " "), output)
		}
		t.Logf("CLI %s -> %s", strings.Join(args, " "), output)
		return output
	}
	plan := call("migrate", "plan")
	var p struct {
		PlanID string `json:"plan_id"`
	}
	if e = json.Unmarshal(plan, &p); e != nil || p.PlanID == "" {
		t.Fatal("stable pinned plan", e)
	}
	call("migrate", "up", "--plan-id", p.PlanID)
	call("migrate", "status")
	call("bootstrap", "--mock", "--tenant", "demo-local")
	first := call("seed", "coffee", "--mock", "--tenant", "demo-local")
	second := call("seed", "coffee", "--mock", "--tenant", "demo-local")
	if !bytes.Equal(first, second) {
		t.Fatal("CLI seed idempotency")
	}
	// Use the supported indexed Stage path before queueing the frozen sweep.
	// The original manual unindexed file fixture is intentionally unsupported.
	f, e := files.New(store.New(db), private)
	if e != nil {
		t.Fatal(e)
	}
	staleBody := []byte("synthetic stale staging")
	h := sha256.Sum256(staleBody)
	m := generated.AssetUploadMetadata{ProductID: "coffee", Name: "stale.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "23", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_READERS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "CLI fixture", License: "Apache-2.0", AllowedUses: []string{"EDIT", "PUBLISH"}}}
	if _, e = f.Stage(context.Background(), domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, bytes.NewReader(staleBody), m); e != nil {
		f.Close()
		t.Fatal(e)
	}
	f.Close()
	var storageKey string
	if e = db.QueryRow("SELECT storage_key FROM file_staging ORDER BY sequence_id DESC LIMIT 1").Scan(&storageKey); e != nil {
		t.Fatal(e)
	}
	orphan := filepath.Join(private, "demo-local", "coffee", storageKey+".part")
	old := time.Now().Add(-2 * time.Hour)
	if e = os.Chtimes(orphan, old, old); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("UPDATE file_staging SET created_at=? WHERE storage_key=?", old, storageKey); e != nil {
		t.Fatal(e)
	}
	queued := call("maintenance", "cleanup-files", "--key", "actual-cleanup")
	var task struct {
		TaskID string `json:"task_id"`
	}
	if e = json.Unmarshal(queued, &task); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, goexe, "run", "./cmd/loreviva", "--mode", "all", "--run-for", "5s")
	cmd.Dir = repo
	cmd.Env = env
	var console safeBuffer
	cmd.Stdout = &console
	cmd.Stderr = &console
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cancel()
			_ = cmd.Wait()
		}
	}()
	client := http.Client{Timeout: time.Second}
	base := "http://" + addr
	ready := false
	for i := 0; i < 60; i++ {
		res, e := client.Get(base + "/ready")
		if e == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("actual Go run readiness unavailable", console.String())
	}
	for _, path := range []string{"/health", "/version", "/"} {
		res, e := client.Get(base + path)
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(res.Body)
		res.Body.Close()
		if e != nil || res.StatusCode != 200 {
			t.Fatal("actual startup endpoint", path, e)
		}
		t.Logf("HTTP %s ->%d %s", path, res.StatusCode, b)
	}
	request, e := http.NewRequest("GET", base+"/world/v1/admin/content-drafts?product_id=coffee", nil)
	if e != nil {
		t.Fatal(e)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	res, e := client.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || res.StatusCode != 200 {
		t.Fatal("seed query", res.StatusCode, e)
	}
	t.Logf("HTTP actual authenticated seed query ->%d; bytes=%d", res.StatusCode, len(body))
	state := ""
	for i := 0; i < 30; i++ {
		if e = db.QueryRow("SELECT state FROM tasks WHERE task_id=?", task.TaskID).Scan(&state); e != nil {
			t.Fatal(e)
		}
		if state == "COMMITTED" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "COMMITTED" {
		t.Fatal("actual registered cleanup worker", state)
	}
	if _, e = os.Stat(orphan); !os.IsNotExist(e) {
		t.Fatal("worker did not actually clean orphan", e)
	}
	t.Log("actual registered FILE_CLEANUP COMMITTED and stale private staging removed")
	if e = cmd.Wait(); e != nil {
		t.Fatal("bounded registered go run exit", console.String())
	}
	if strings.Contains(console.String(), token) || strings.Contains(console.String(), cfg.Passwd) {
		t.Fatal("startup secret leakage")
	}
	t.Log("actual go run --mode all exited cleanly", console.String())
}
