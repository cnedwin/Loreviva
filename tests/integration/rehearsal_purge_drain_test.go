package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
)

type capturedPurgeOwner struct {
	owner    store.RehearsalPurgeOwner
	evidence store.RehearsalPurgeEvidence
}

func (p *capturedPurgeOwner) CheckRehearsalPurgeEvidence(e store.RehearsalPurgeEvidence, s domain.ProductScope, key string) (store.CommandBarrierRequest, error) {
	p.evidence = e
	return p.owner.CheckRehearsalPurgeEvidence(e, s, key)
}

func TestRehearsalPurgedAcknowledgmentOwnsDrainLifetime(t *testing.T) {
	for _, name := range []string{"VALID", "CLOSING", "FINISH_CLAIM", "FINISH_PRIVACY"} {
		closing := name == "CLOSING"
		denied := name == "FINISH_CLAIM" || name == "FINISH_PRIVACY"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			r, draft, assets, req := physicalRehearsalDraft(t)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "purge-lifetime", req)
			if e != nil {
				t.Fatal(e)
			}
			r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			captured := &capturedPurgeOwner{owner: r.Files}
			r.Store.RehearsalPurgeOwner = captured
			var key string
			var sequence uint64
			var cutoff time.Time
			if e = r.Store.DB.QueryRow("SELECT s.storage_key,s.sequence_id,DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging s JOIN harness_rehearsal_reports r ON r.tenant_id=s.tenant_id AND r.product_id=s.product_id AND r.storage_key=s.storage_key WHERE r.task_id=?", root.TaskID).Scan(&key, &sequence, &cutoff); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", assets[1]); e != nil {
				t.Fatal(e)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var count int
				if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_STAGE_PURGED'").Scan(&count); e != nil {
					return e
				}
				if count == 0 {
					return nil
				}
				close(entered)
				select {
				case <-release:
					if name == "FINISH_CLAIM" {
						_, e := tx.ExecContext(ctx, "UPDATE commit_receipts SET operation='WRONG_CLAIM' WHERE operation='CLAIM_STAGE_DELETE'")
						return e
					}
					if name == "FINISH_PRIVACY" {
						_, e := tx.ExecContext(ctx, "UPDATE subject_governance SET privacy_epoch=privacy_epoch+1 WHERE subject_id='creator'")
						return e
					}
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			done := make(chan error, 1)
			go func() { _, e := r.Files.CleanupPage(ctx, scope, cutoff, sequence-1, sequence); done <- e }()
			finished := false
			defer func() {
				unblock()
				if !finished {
					<-done
				}
				r.Store.BeforeCommit = nil
			}()
			select {
			case <-entered:
			case e := <-done:
				finished = true
				t.Fatal("did not reach physical purge ACK", e)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, e = os.Stat(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, key+".part")); !os.IsNotExist(e) {
				t.Fatal("bytes not physically purged before independent ACK", e)
			}
			wrong := scope
			wrong.SubjectID = "wrong-owner"
			if _, e = r.Files.CheckRehearsalPurgeEvidence(captured.evidence, wrong, key); !errors.Is(e, store.ErrDenied) {
				t.Fatal("wrong owner evidence", e)
			}
			if _, e = r.Files.CheckRehearsalPurgeEvidence(captured.evidence, scope, key+"x"); !errors.Is(e, store.ErrDenied) {
				t.Fatal("wrong storage evidence", e)
			}
			if _, e = r.Files.CheckRehearsalPurgeEvidence(nil, scope, key); !errors.Is(e, store.ErrDenied) {
				t.Fatal("missing evidence", e)
			}
			var count int
			if e = r.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_STAGE_PURGED'").Scan(&count); e != nil || count != 0 {
				t.Fatal("ACK visible before commit", count, e)
			}
			storage, e := localstate.Open(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, ".memory-locks", key), false)
			if e != nil {
				t.Fatal(e)
			}
			short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
			unlock, lockErr := storage.Lock(short, "operations.lock")
			stop()
			if lockErr == nil {
				unlock()
				t.Fatal("purge ACK released actual storage lifetime")
			}
			var drained chan error
			if closing {
				if e = r.Local.Coordinator.BeginClose(ctx, "purge-ack"); e != nil {
					t.Fatal(e)
				}
				if _, freshErr := r.Files.CleanupPage(ctx, scope, cutoff, sequence-1, sequence); !errors.Is(freshErr, maintenance.ErrMaintenance) {
					t.Fatal("new work entered after closing", freshErr)
				}
				drained = make(chan error, 1)
				go func() { drained <- r.Local.Coordinator.Drain(ctx, "purge-ack") }()
				select {
				case e := <-drained:
					t.Fatal("drain passed actual unfinished purge ACK", e)
				case <-time.After(50 * time.Millisecond):
				}
			}
			unblock()
			select {
			case e = <-done:
				finished = true
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if closing {
				if e != nil {
					t.Fatal("retired purge ACK wrong gate", e)
				}
				select {
				case e = <-drained:
					if e != nil {
						t.Fatal(e)
					}
				case <-ctx.Done():
					t.Fatal("drain did not finish", ctx.Err())
				}
			} else if denied {
				if !errors.Is(e, store.ErrDenied) {
					t.Fatal("finisher wrong gate", e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_STAGE_PURGED'").Scan(&count); e != nil {
				t.Fatal(e)
			}
			want := 1
			if denied {
				want = 0
			}
			if closing {
				want = 1
			}
			if count != want {
				t.Fatal("wrong durable physical ACK", count, want)
			}
			if _, e = r.Store.FinishRehearsalStagePurge(ctx, scope, key, captured.evidence); !errors.Is(e, store.ErrDenied) {
				t.Fatal("closed opaque IO evidence replayed", e)
			}
			unlock, e = storage.Lock(ctx, "operations.lock")
			if e != nil {
				t.Fatal("storage leaked after transaction", e)
			}
			unlock()
			var committed, services int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_REPORT_COMMIT'),(SELECT COUNT(*) FROM service_events WHERE task_id=?)", root.TaskID).Scan(&committed, &services); e != nil || committed != 1 || services != 1 {
				t.Fatal("purge altered business/accounting", committed, services, e)
			}
		})
	}
}

func TestRehearsalPurgeIOLifetimeFinishesOriginalACK(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	r, draft, assets, req := physicalRehearsalDraft(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "purge-io", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var key string
	var sequence uint64
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT s.storage_key,s.sequence_id,DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging s JOIN harness_rehearsal_reports r ON r.tenant_id=s.tenant_id AND r.product_id=s.product_id AND r.storage_key=s.storage_key WHERE r.task_id=?", root.TaskID).Scan(&key, &sequence, &cutoff); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", assets[1]); e != nil {
		t.Fatal(e)
	}
	storage, e := localstate.Open(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, ".memory-locks", key), false)
	if e != nil {
		t.Fatal(e)
	}
	unlock, e := storage.Lock(ctx, "operations.lock")
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	done := make(chan error, 1)
	go func() { _, e := r.Files.CleanupPage(ctx, scope, cutoff, sequence-1, sequence); done <- e }()
	finished := false
	defer func() {
		unlock()
		if !finished {
			<-done
		}
	}()
	for {
		var state string
		if e = r.Store.DB.QueryRowContext(ctx, "SELECT state FROM file_staging WHERE storage_key=?", key).Scan(&state); e != nil {
			t.Fatal(e)
		}
		if state == "DELETE_INTENT" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, e = os.Stat(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, key+".part")); e != nil {
		t.Fatal("physical IO bypassed held storage", e)
	}
	if e = r.Local.Coordinator.BeginClose(ctx, "purge-io"); e != nil {
		t.Fatal(e)
	}
	drained := make(chan error, 1)
	go func() { drained <- r.Local.Coordinator.Drain(ctx, "purge-io") }()
	select {
	case e := <-drained:
		t.Fatal("drain passed held physical IO", e)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case e = <-done:
		finished = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if e != nil {
		t.Fatal("original admitted purge did not complete its independent ACK", e)
	}
	select {
	case e = <-drained:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_STAGE_PURGED'").Scan(&count); e != nil || count != 1 {
		t.Fatal("original purge ACK missing", count, e)
	}
}
