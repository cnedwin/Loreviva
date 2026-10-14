package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFix1CleanupProgressSurvivesRestartAndPreservesReferencedFiles(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	if e := maintenance.BootstrapSynthetic(ctx, s, scope, "owner", "synthetic-test-private-operator-secret"); e != nil {
		t.Fatal(e)
	}
	authenticated, _, authErr := s.AuthenticateOperator(ctx, "Bearer synthetic-test-private-operator-secret")
	if authErr != nil {
		t.Fatal(authErr)
	}
	ctx = authenticated
	root := t.TempDir()
	f, e := files.New(s, root)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { f.Close() }()
	body := []byte("synthetic")
	h := sha256.Sum256(body)
	m := generated.AssetUploadMetadata{ProductID: "coffee", Name: "note.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "9", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_READERS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "test", License: "Apache-2.0", AllowedUses: []string{"EDIT", "PUBLISH"}}}
	// Supported Stage writes simulate a crash before Upload intake. Retained young
	// stages precede the eligible orphan in the durable insertion sequence.
	for i := 0; i < 120; i++ {
		if _, e = f.Stage(ctx, scope, bytes.NewReader(body), m); e != nil {
			t.Fatal(e)
		}
	}
	ready, e := f.Upload(ctx, scope, bytes.NewReader(body), m, "ready")
	if e != nil {
		t.Fatal(e)
	}
	// Persist a PENDING registration for a real supported stage, representing a
	// process crash between database intake and filesystem finalization.
	stage, e := f.Stage(ctx, scope, bytes.NewReader(body), m)
	if e != nil {
		t.Fatal(e)
	}
	_ = stage
	entries, e := os.ReadDir(filepath.Join(root, "demo-local", "coffee"))
	if e != nil {
		t.Fatal(e)
	}
	beforeNames := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() != ".memory-locks" {
				t.Fatal("unexpected storage directory", entry.Name())
			}
			continue
		}
		beforeNames[entry.Name()] = true
	}
	var pendingKey string
	if e = s.DB.QueryRow("SELECT storage_key FROM file_staging ORDER BY sequence_id DESC LIMIT 1").Scan(&pendingKey); e != nil {
		t.Fatal(e)
	}
	pending := generated.FileRecord{Scope: generated.ProductScope{TenantID: "demo-local", ProductID: "coffee"}, AssetID: "pending", State: "PENDING", Name: m.Name, MediaType: m.DeclaredType, SizeBytes: m.ExpectedSize, Sha256: m.ExpectedSha256, Purpose: m.Purpose, Rights: m.Rights, AudiencePolicy: m.AudiencePolicy, OwnerSubjectID: "creator", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	pendingRaw, _ := json.Marshal(pending)
	if _, e = s.DB.Exec("INSERT INTO assets(tenant_id,product_id,asset_id,owner_subject_id,state,storage_key,document) VALUES('demo-local','coffee','pending','creator','PENDING',?,?)", pendingKey, pendingRaw); e != nil {
		t.Fatal(e)
	}
	if _, e = f.Stage(ctx, scope, bytes.NewReader(body), m); e != nil {
		t.Fatal(e)
	}
	entries, e = os.ReadDir(filepath.Join(root, "demo-local", "coffee"))
	if e != nil {
		t.Fatal(e)
	}
	orphan := ""
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !beforeNames[entry.Name()] {
			orphan = filepath.Join(root, "demo-local", "coffee", entry.Name())
		}
	}
	if orphan == "" {
		t.Fatal("supported Stage orphan absent")
	}
	old := time.Now().Add(-2 * time.Hour)
	if e = os.Chtimes(orphan, old, old); e != nil {
		t.Fatal(e)
	}
	if e = os.Chtimes(filepath.Join(root, "demo-local", "coffee", pendingKey+".part"), old, old); e != nil {
		t.Fatal(e)
	}
	// Move only synthetic index timestamps to simulate elapsed wall time.
	if _, e = s.DB.Exec("UPDATE file_staging SET created_at=?", old); e != nil {
		t.Fatal(e)
	}
	task, e := app.QueueCleanup(ctx, s, scope, "paged-cleanup")
	if e != nil {
		t.Fatal(e)
	}
	w := app.NewWorker(s, f)
	if e = w.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var state string
	if e = s.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", task.TaskID).Scan(&state); e != nil {
		t.Fatal(e)
	}
	if state == "COMMITTED" {
		t.Error("cleanup claimed completion after first bounded page")
	}
	var checkpoint uint64
	if e = s.DB.QueryRow("SELECT cursor_sequence FROM file_cleanup_tasks WHERE task_id=?", task.TaskID).Scan(&checkpoint); e != nil || checkpoint != 100 {
		t.Fatalf("durable first-page checkpoint=%d error=%v", checkpoint, e)
	}
	// Recreate both file store and worker: continuation must be durable.
	f.Close()
	for step := 0; step < 5 && state != "COMMITTED"; step++ {
		f, e = files.New(s, root)
		if e != nil {
			t.Fatal(e)
		}
		w = app.NewWorker(s, f)
		if e = w.Once(ctx); e != nil {
			t.Fatal(e)
		}
		f.Close()
		if e = s.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", task.TaskID).Scan(&state); e != nil {
			t.Fatal(e)
		}
	}
	if state != "COMMITTED" {
		t.Fatalf("cleanup never completed: %s", state)
	}
	if _, e = os.Stat(orphan); !os.IsNotExist(e) {
		t.Errorf("old indexed orphan unreachable: %v", e)
	}
	f, e = files.New(s, root)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r, e := f.OpenAuthorized(ctx, scope, string(ready.AssetID))
	if e != nil {
		t.Fatal("READY blob removed", e)
	}
	r.Close()
	entries, e = os.ReadDir(filepath.Join(root, "demo-local", "coffee"))
	if e != nil {
		t.Fatal(e)
	}
	retained := 0
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() != ".memory-locks" {
				t.Fatal("unexpected storage directory", entry.Name())
			}
			continue
		}
		if !beforeNames[entry.Name()] {
			t.Fatal("unexpected retained body", entry.Name())
		}
		actual, e := os.ReadFile(filepath.Join(root, "demo-local", "coffee", entry.Name()))
		if e != nil || !bytes.Equal(actual, body) {
			t.Fatal("retained body changed", entry.Name(), e)
		}
		retained++
	}
	if retained != len(beforeNames) {
		t.Fatalf("young/reference bodies changed: got=%d want=%d", retained, len(beforeNames))
	}
	t.Log(fmt.Sprintf("actual FILE_CLEANUP reached indexed EOF after restart; retained=%d old orphan removed", retained))
}
