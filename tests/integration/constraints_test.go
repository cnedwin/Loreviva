package integration

import (
	"context"
	"database/sql"
	"errors"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestUnplannedLockAndSwallowedDatabaseErrorRollback(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	planned := store.LockKey{Rank: 110, Kind: "events", IDs: []string{"t", "p", "planned"}}
	r.Locks = []store.LockKey{planned}
	_, e := s.WithBarrier(ctx, r, func(tx store.Tx) error {
		if e := tx.RequireLock(planned); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','planned','task','owner','COMMITTED',1)"); e != nil {
			return e
		}
		_ = tx.RequireLock(store.LockKey{Rank: 30, IDs: []string{"t", "p", "late-subject"}})
		return nil
	})
	if !errors.Is(e, store.ErrReplan) {
		t.Fatalf("missing lower lock did not fail whole transaction: %v", e)
	}
	var n int
	if e = db.QueryRow("SELECT COUNT(*) FROM events").Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatal("replan retained prior writes")
	}
	_, e = s.WithBarrier(ctx, r, func(tx store.Tx) error {
		tx.RequireLock(planned)
		tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','planned','task','owner','COMMITTED',1)")
		_, _ = tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','planned','task','owner','COMMITTED',1)")
		return nil
	})
	if e == nil {
		t.Fatal("swallowed duplicate committed")
	}
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM events"), &n)
	if n != 0 {
		t.Fatal("partial statement error committed")
	}
	_, e = s.WithBarrier(ctx, r, func(tx store.Tx) error {
		_, _ = tx.ExecContext(ctx, "UPDATE subject_governance SET enabled=1")
		return nil
	})
	if !errors.Is(e, store.ErrReplan) {
		t.Fatal("unplanned governance mutation accepted", e)
	}
}

func TestDDLRejectsCrossScopeEmptyIDsAndUniqueCollisions(t *testing.T) {
	_, db, _ := fixture(t, true)
	invalid := []string{
		"INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,next_run_at) VALUES('t','other','task2','owner','PRODUCT','ACCEPTED',UTC_TIMESTAMP(6))",
		"INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,world_instance_id,state,next_run_at) VALUES('t','p','task2','owner','WORLD',NULL,'ACCEPTED',UTC_TIMESTAMP(6))",
		"INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,world_instance_id,state,next_run_at) VALUES('t','p','task2','owner','PRODUCT','w','ACCEPTED',UTC_TIMESTAMP(6))",
		"INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,next_run_at) VALUES('t','p','task2','','PRODUCT','ACCEPTED',UTC_TIMESTAMP(6))",
		"INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,next_run_at) VALUES('t','p','','owner','PRODUCT','ACCEPTED',UTC_TIMESTAMP(6))",
		"INSERT INTO commands(tenant_id,product_id,subject_id,operation,scope_kind,idem_key,payload_hash) VALUES('t','p','owner','generate','PRODUCT','',UNHEX(REPEAT('00',32)))",
		"INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,balance,held) VALUES('supplier_global','GLOBAL','global','t','p',100,0)",
	}
	for _, q := range invalid {
		if _, e := db.Exec(q); e == nil {
			t.Fatal("invalid scope/key accepted", q)
		}
	}
	if _, e := db.Exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('t','p','Owner',1,1,1,1)"); e != nil {
		t.Fatal("binary IDs collapsed", e)
	}
	var n int
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM subject_governance"), &n)
	if n != 2 {
		t.Fatal("case identity collapsed")
	}
}

func TestDispatchFrozenBindingsBudgetAndSlots(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	d := dispatch(r)
	if _, e := s.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	for _, change := range []string{"route", "hash", "amount", "account", "authorization", "slot"} {
		d2 := dispatch(r)
		d2.CommitKey = "changed-" + change
		switch change {
		case "route":
			d2.Route = "changed"
		case "hash":
			d2.RequestHash[0] = 2
		case "amount":
			d2.Reservations[0].Amount = 8
		case "account":
			d2.Reservations[0].AccountID = "wrong"
		case "authorization":
			d2.AuthorizationID = "other"
		case "slot":
			d2.Slot = 1
		}
		if _, e := s.AuthorizeDispatch(ctx, d2); e == nil {
			t.Fatalf("frozen %s changed", change)
		}
	}
	d2 := dispatch(r)
	d2.CommitKey = "extra"
	d2.CommandKey = "extra"
	d2.AttemptID = "extra"
	d2.AuthorizationID = "extra"
	d2.RequestKey = "extra"
	d2.Reservations[0].ReservationID = "extra"
	d2.Slot = 1
	if _, e := s.AuthorizeDispatch(ctx, d2); !errors.Is(e, store.ErrDenied) {
		t.Fatal("unsupported extra slot admitted", e)
	}
	d3 := d2
	d3.Slot = 2
	if _, e := s.AuthorizeDispatch(ctx, d3); e == nil {
		t.Fatal("third slot accepted")
	}
	var held, used, n int
	scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
	scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &used)
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM attempts"), &n)
	if held != 7 || used != 1 || n != 1 {
		t.Fatal(held, used, n)
	}
}

func TestInsufficientBudgetRollsBackAttemptAndReservation(t *testing.T) {
	s, db, r := fixture(t, false)
	ctx := context.Background()
	d := dispatch(r)
	d.Reservations[0].Amount = 101
	if _, e := s.AuthorizeDispatch(ctx, d); e == nil {
		t.Fatal("budget overspend")
	}
	var n int
	for _, table := range []string{"attempts", "dispatch_authorizations", "reservations", "commit_receipts"} {
		if e := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n != 0 {
			t.Fatal("budget failure left rows", table, n)
		}
	}
	scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &n)
	if n != 0 {
		t.Fatal("budget failure consumed slot")
	}
}

func TestReadinessDoesNotInventPrerequisites(t *testing.T) {
	s, db, _ := fixture(t, false)
	ctx := context.Background()
	if e := s.Readiness(ctx); e == nil {
		t.Fatal("unregistered prerequisite ready")
	}
	fail := errors.New("governance not restored")
	if e := s.Readiness(ctx, func(context.Context) error { return fail }); !errors.Is(e, fail) {
		t.Fatal("governance ignored", e)
	}
	if e := s.Readiness(ctx, func(context.Context) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("DELETE FROM schema_migrations"); e != nil {
		t.Fatal(e)
	}
	if e := s.Readiness(ctx, func(context.Context) error { return nil }); e == nil {
		t.Fatal("missing migration ready")
	}
}

func TestDispatchStartRequiresDistinctMarkerCommitKey(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	d := dispatch(r)
	if _, e := s.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	a, e := s.StartSend(ctx, d, d.AttemptID)
	if e == nil || a.CanSend {
		t.Fatal("authorization commit key reused as send marker")
	}
	var marker sql.NullTime
	scanAssertion(t, db.QueryRow("SELECT send_started_at FROM attempts"), &marker)
	if marker.Valid {
		t.Fatal("reused key wrote marker")
	}
}
