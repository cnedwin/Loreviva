package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
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

type pausedStageReader struct {
	started, resume chan struct{}
	body            io.Reader
	once            bool
}

func (r *pausedStageReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		close(r.started)
		<-r.resume
	}
	return r.body.Read(p)
}

func TestFix1CleanupDeletionIntentWinsLateUploadIntake(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	if e := maintenance.BootstrapSynthetic(ctx, s, scope, "owner", "synthetic-test-private-operator-secret"); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	f, e := files.New(s, root)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	body := []byte("synthetic")
	h := sha256.Sum256(body)
	m := generated.AssetUploadMetadata{ProductID: "coffee", Name: "late.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "9", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_READERS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "test", License: "Apache-2.0", AllowedUses: []string{"EDIT", "PUBLISH"}}}
	r := &pausedStageReader{started: make(chan struct{}), resume: make(chan struct{}), body: bytes.NewReader(body)}
	result := make(chan error, 1)
	go func() { _, e := f.Upload(ctx, scope, r, m, "late-upload"); result <- e }()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stage registration stalled")
	}
	resumed := false
	defer func() {
		if !resumed {
			close(r.resume)
			<-result
		}
	}()
	var key string
	if e = s.DB.QueryRow("SELECT storage_key FROM file_staging").Scan(&key); e != nil {
		t.Fatal(e)
	}
	old := time.Now().Add(-2 * time.Hour)
	if _, e = s.DB.Exec("UPDATE file_staging SET created_at=?", old); e != nil {
		t.Fatal(e)
	}
	part := filepath.Join(root, "demo-local", "coffee", key+".part")
	if e = os.Chtimes(part, old, old); e != nil {
		t.Fatal(e)
	}
	task, e := app.QueueCleanup(ctx, s, scope, "late-cleanup")
	if e != nil {
		t.Fatal(e)
	}
	w := app.NewWorker(s, f)
	cleanupError := w.Once(ctx) // Windows may refuse removal while the writer is open.
	var intent string
	if e = s.DB.QueryRow("SELECT state FROM file_staging").Scan(&intent); e != nil || intent != "DELETE_INTENT" {
		t.Fatalf("no durable deletion intent: %s %v", intent, e)
	}
	close(r.resume)
	resumed = true
	select {
	case e = <-result:
		if e == nil {
			t.Fatal("late original Upload intake succeeded after deletion intent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late writer did not finish")
	}
	var n int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM assets").Scan(&n); e != nil || n != 0 {
		t.Fatalf("rejected intake mutated assets=%d error=%v", n, e)
	}
	if _, e = os.Stat(part); !os.IsNotExist(e) {
		t.Fatal("late abandoned bytes remain", e)
	}
	if cleanupError != nil {
		// Simulate expiry after a retriable filesystem failure; the original page
		// has no checkpoint or successful receipt and must be retried under a fence.
		if _, e = s.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id=?", task.TaskID); e != nil {
			t.Fatal(e)
		}
		if e = w.Once(ctx); e != nil {
			t.Fatal(e)
		}
	}
	var state string
	if e = s.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", task.TaskID).Scan(&state); e != nil || state != "COMMITTED" {
		t.Fatalf("cleanup recovery state=%s error=%v", state, e)
	}
	t.Log("actual worker deletion intent denied delayed original Upload intake; assets=0, late part absent")
}
