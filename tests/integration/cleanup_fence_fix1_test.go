package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/testprincipal"
	"testing"
	"time"
)

func TestFix1CleanupAcceptedContinuationRejectsOldFence(t *testing.T) {
	s, scope, owner, filesRoot := testprincipal.FreshSyntheticOwner(t)
	ctx := testprincipal.RegisterActions(t, s, owner, scope, []string{"asset.upload", "asset.read"})
	f, e := files.New(s, filesRoot)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	body := []byte("synthetic")
	h := sha256.Sum256(body)
	m := generated.AssetUploadMetadata{ProductID: generated.Identity(scope.ProductID), Name: "young.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "9", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_READERS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "test", License: "Apache-2.0", AllowedUses: []string{"EDIT", "PUBLISH"}}}
	for i := 0; i < 101; i++ {
		if _, e = f.Stage(ctx, scope, bytes.NewReader(body), m); e != nil {
			t.Fatal(e)
		}
	}
	task, e := app.QueueCleanup(ctx, s, scope, "fence-cleanup")
	if e != nil {
		t.Fatal(e)
	}
	if e = app.NewWorker(s, f).Once(ctx); e != nil {
		t.Fatal(e)
	}
	var oldFence uint64
	if e = s.DB.QueryRow("SELECT fence FROM tasks WHERE task_id=?", task.TaskID).Scan(&oldFence); e != nil {
		t.Fatal(e)
	}
	// No expiry override: the ACCEPTED continuation must be immediately claimable.
	lease, e := s.ClaimRegistered(ctx, "takeover-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "FILE_CLEANUP"}})
	if e != nil || lease.Fence != oldFence+1 {
		t.Fatalf("continuation did not advance fence: %v %v", lease, e)
	}
	g, e := content.Gate(ctx, s, scope)
	if e != nil {
		t.Fatal(e)
	}
	r := store.BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: string(task.TaskID), WorkerID: "m1-file-cleanup", Fence: oldFence, Operation: "FILE_CLEANUP", CommandKey: "stale-cleanup-page", CommitKey: "stale-cleanup-commit", GateVersions: g, PayloadHash: sha256.Sum256([]byte("stale"))}
	ran := false
	_, e = s.WithBarrier(ctx, r, func(tx store.Tx) error { ran = true; return tx.AdvanceFileCleanup(ctx, 101, true) })
	if !errors.Is(e, store.ErrDenied) || ran {
		t.Fatalf("old worker checkpoint admitted after takeover: body=%v error=%v", ran, e)
	}
	var cursor, upper uint64
	var cutoff time.Time
	if e = s.DB.QueryRow("SELECT cursor_sequence,upper_sequence,cutoff FROM file_cleanup_tasks WHERE task_id=?", task.TaskID).Scan(&cursor, &upper, &cutoff); e != nil || cursor != 100 {
		t.Fatalf("old fence mutated checkpoint: cursor=%d error=%v", cursor, e)
	}
	page, e := f.CleanupPage(ctx, scope, cutoff, cursor, upper)
	if e != nil || !page.Done {
		t.Fatal(page, e)
	}
	r.WorkerID = "takeover-worker"
	r.Fence = lease.Fence
	r.CommandKey = "current-cleanup-page"
	r.CommitKey = "current-cleanup-commit"
	r.PayloadHash = sha256.Sum256([]byte("current"))
	if _, e = s.WithBarrier(ctx, r, func(tx store.Tx) error { return tx.AdvanceFileCleanup(ctx, page.Cursor, page.Done) }); e != nil {
		t.Fatal("current final-lease validation failed", e)
	}
	t.Log("ACCEPTED continuation advances fence immediately; old worker denied before callback, current checkpoint commits")
}
