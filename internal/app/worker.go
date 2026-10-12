package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sync"
	"time"
)

type Worker struct {
	Turns  *harness.Executor
	Memory *memory.MemoryService
	s      *store.Store
	files  *files.Store
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
}

func NewWorker(s *store.Store, f *files.Store) *Worker { return &Worker{s: s, files: f} }
func (w *Worker) Check(context.Context) error {
	if w == nil || w.s == nil || w.files == nil {
		return errors.New("registered task processor unavailable")
	}
	return nil
}
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = w.QueryOriginalOnce(ctx)
				_ = w.QueryNecessaryCostOnce(ctx)
				_, _, _ = w.s.RunHarnessCutoffs(ctx)
				_, _, _ = w.s.RunOriginalBills(ctx)
				_, _, _ = w.s.RunOriginalServiceMetering(ctx)
				_ = w.Once(ctx)
			}
		}
	}()
	return nil
}

func (w *Worker) QueryOriginalOnce(ctx context.Context) error {
	if w.Turns == nil {
		return store.ErrNoTask
	}
	return w.Turns.QueryOnce(ctx)
}
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	done := w.done
	w.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (w *Worker) Once(ctx context.Context) error {
	ctx, release, permitError := w.s.EnterOperation(ctx)
	if permitError != nil {
		return permitError
	}
	defer release()
	registered := []store.RegisteredOperation{{Kind: "ADMIN", Operation: "FILE_CLEANUP"}, {Kind: "CONTROL", Operation: "INSTANCE_CONTROL"}, {Kind: "MIGRATION", Operation: "INSTANCE_MIGRATION"}}
	if w.Memory != nil {
		registered = append(registered, store.RegisteredOperation{Kind: "GOVERNANCE", Operation: "MEMORY_GOVERNANCE"})
	}
	if w.Turns != nil {
		registered = append(registered, store.RegisteredOperation{Kind: "TURN", Operation: "TURN"})
	}
	if w.Turns.CandidateReady() {
		registered = append(registered, store.RegisteredOperation{Kind: "ADMIN", Operation: "CANDIDATE"})
	}
	if w.Turns.RehearsalReady() {
		registered = append(registered, store.RegisteredOperation{Kind: "ADMIN", Operation: "REHEARSAL"})
	}
	lease, e := w.s.ClaimRegistered(ctx, "m1-file-cleanup", time.Now(), time.Minute, registered)
	if e != nil {
		return e
	}
	if lease.Kind == "TURN" && lease.Operation == "TURN" {
		return w.Turns.Execute(ctx, lease.TaskLease, "m1-file-cleanup")
	}
	if lease.Kind == "ADMIN" && lease.Operation == "CANDIDATE" {
		return w.Turns.ExecuteDraftCandidate(ctx, lease.TaskLease, "m1-file-cleanup")
	}
	if lease.Kind == "ADMIN" && lease.Operation == "REHEARSAL" {
		return w.Turns.ExecuteRehearsal(ctx, lease.TaskLease, "m1-file-cleanup")
	}
	if lease.Operation == "MEMORY_GOVERNANCE" {
		return w.Memory.Process(ctx, lease, "m1-file-cleanup")
	}
	if lease.Operation == "INSTANCE_CONTROL" || lease.Operation == "INSTANCE_MIGRATION" {
		return world.New(w.s).Process(ctx, lease, "m1-file-cleanup")
	}
	if lease.Operation != "FILE_CLEANUP" || lease.Kind != "ADMIN" || lease.WorldInstanceID != nil {
		return store.ErrDenied
	}
	g, e := content.Gate(ctx, w.s, lease.ProductScope)
	if e != nil {
		return e
	}
	var cutoff time.Time
	var cursor, upper uint64
	if e = w.s.DB.QueryRowContext(ctx, "SELECT cutoff,cursor_sequence,upper_sequence FROM file_cleanup_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&cutoff, &cursor, &upper); e != nil {
		return e
	}
	page, e := w.files.CleanupPage(ctx, lease.ProductScope, cutoff, cursor, upper)
	if e != nil {
		return e
	}
	step := fmt.Sprintf("%s.page.%d", lease.TaskID, cursor)
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%v", step, page.Cursor, page.Done)))
	_, e = w.s.WithBarrier(ctx, store.BarrierRequest{ProductScope: lease.ProductScope, OwnerSubjectID: lease.OwnerSubjectID, TaskID: lease.TaskID, WorkerID: "m1-file-cleanup", Fence: lease.Fence, Operation: "FILE_CLEANUP", CommandKey: step, CommitKey: content.Identity(lease.TenantID, lease.ProductID, step), GateVersions: g, PayloadHash: hash}, func(tx store.Tx) error { return tx.AdvanceFileCleanup(ctx, page.Cursor, page.Done) })
	return e
}
func QueueCleanup(ctx context.Context, s *store.Store, scope domain.ProductScope, key string) (generated.AdminTaskReceipt, error) {
	id := content.Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "cleanup", key)
	raw, e := content.Command(ctx, s, scope, "queueFileCleanup", key, "", "AdminTaskReceipt", struct{ Kind string }{"FILE_CLEANUP"}, []store.LockKey{{Rank: 50, IDs: []string{scope.TenantID, scope.ProductID, id}}}, func(tx store.Tx) (any, error) {
		if e := tx.CreateRegisteredTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: scope, OwnerSubjectID: scope.SubjectID}, TaskID: id, NextRunAt: time.Now().UTC(), Kind: "ADMIN"}, "FILE_CLEANUP"); e != nil {
			return nil, e
		}
		var upper uint64
		if e := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence_id),0) FROM file_staging WHERE tenant_id=? AND product_id=?", scope.TenantID, scope.ProductID).Scan(&upper); e != nil {
			return nil, e
		}
		if e := tx.CreateFileCleanup(ctx, id, time.Now().UTC().Add(-time.Hour), upper); e != nil {
			return nil, e
		}
		return generated.AdminTaskReceipt{Scope: content.Scope(scope), TaskID: generated.Identity(id), TaskKind: "FILE_CLEANUP", TaskState: "ACCEPTED", CorrelationID: generated.Identity(id)}, nil
	})
	var out generated.AdminTaskReceipt
	if e == nil {
		e = generated.Decode("AdminTaskReceipt", raw, &out)
	}
	return out, e
}

func (w *Worker) QueryNecessaryCostOnce(ctx context.Context) error {
	if w.Turns == nil {
		return store.ErrNoTask
	}
	return w.Turns.QueryNecessaryCostOnce(ctx)
}
