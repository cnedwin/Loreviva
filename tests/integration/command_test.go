package integration

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func command(r store.BarrierRequest) store.CommandBarrierRequest {
	return store.CommandBarrierRequest{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, Operation: "save", CommandKey: "save", CommitKey: "save", GateVersions: r.GateVersions}
}

func TestProductCommandBarrierWithoutInventedTask(t *testing.T) {
	s, db, r := fixture(t, false)
	ctx := context.Background()
	c := command(r)
	receipt, e := s.WithCommandBarrier(ctx, c, func(store.Tx) error { return nil })
	if e != nil || !receipt.Committed || receipt.TaskID != "" {
		t.Fatal("synchronous product command needs no worker lease", receipt, e)
	}
	var task sql.NullString
	if e = db.QueryRow("SELECT task_id FROM commit_receipts WHERE commit_key='save'").Scan(&task); e != nil {
		t.Fatal(e)
	}
	if task.Valid {
		t.Fatal("command-only receipt invented task")
	}
	if _, e = s.QueryCommitScoped(ctx, r.ProductScope, nil, r.OwnerSubjectID, "save"); e != nil {
		t.Fatal(e)
	}
	c.OwnerSubjectID = "wrong"
	if _, e = s.WithCommandBarrier(ctx, c, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("command bypassed owner")
	}
	r.TaskID = ""
	r.WorkerID = ""
	r.Fence = 0
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("lease barrier accepted command-only")
	}
	if _, e = s.AuthorizeDispatch(ctx, dispatch(r)); e == nil {
		t.Fatal("dispatch accepted command-only")
	}
}

func TestCommandTaskIntakeAtomicRollbackAndFrozenPlan(t *testing.T) {
	s, db, r := fixture(t, false)
	ctx := context.Background()
	c := command(r)
	c.Locks = []store.LockKey{{Rank: 50, IDs: []string{"t", "p", "created"}}}
	input := domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: r.ProductScope, OwnerSubjectID: "owner"}, TaskID: "created", NextRunAt: time.Now().UTC()}
	_, e := s.WithCommandBarrier(ctx, c, func(tx store.Tx) error {
		if e := tx.CreateTask(ctx, input); e != nil {
			return e
		}
		return errors.New("abort intake")
	})
	if e == nil {
		t.Fatal("aborted intake committed")
	}
	var n int
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_id='created'"), &n)
	if n != 0 {
		t.Fatal("aborted intake left task")
	}
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM commit_receipts"), &n)
	if n != 0 {
		t.Fatal("aborted intake left receipt")
	}
	_, e = s.WithCommandBarrier(ctx, c, func(tx store.Tx) error { return tx.CreateTask(ctx, input) })
	if e != nil {
		t.Fatal(e)
	}
	var state string
	var fence uint64
	var worker sql.NullString
	scanAssertion(t, db.QueryRow("SELECT state,fence,lease_owner FROM tasks WHERE task_id='created'"), &state, &fence, &worker)
	if state != "ACCEPTED" || fence != 0 || worker.Valid {
		t.Fatal("intake manufactured scheduling authority", state, fence, worker)
	}
	_, e = s.WithCommandBarrier(ctx, c, func(store.Tx) error { t.Fatal("command replay recreated task"); return nil })
	if e != nil {
		t.Fatal(e)
	}
	c.CommitKey = "unplanned"
	c.CommandKey = "unplanned"
	input.TaskID = "unplanned"
	_, e = s.WithCommandBarrier(ctx, c, func(tx store.Tx) error { _ = tx.CreateTask(ctx, input); return nil })
	if !errors.Is(e, store.ErrReplan) {
		t.Fatal("intake acquired late rank50", e)
	}
}

func TestWorldTaskIntakeRequiresWorldControlInPlan(t *testing.T) {
	s, _, r := fixture(t, true)
	ctx := context.Background()
	c := command(r)
	c.Locks = []store.LockKey{{Rank: 50, IDs: []string{"t", "p", "world-new"}}}
	input := domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: r.ProductScope, WorldScope: r.WorldScope, OwnerSubjectID: "owner"}, TaskID: "world-new", NextRunAt: time.Now().UTC()}
	if _, e := s.WithCommandBarrier(ctx, c, func(tx store.Tx) error { return tx.CreateTask(ctx, input) }); e == nil {
		t.Fatal("WORLD intake acquired late control FK lock without world plan")
	}
	c.WorldScope = r.WorldScope
	c.Versions = r.Versions
	if _, e := s.WithCommandBarrier(ctx, c, func(tx store.Tx) error { return tx.CreateTask(ctx, input) }); e != nil {
		t.Fatal(e)
	}
}
