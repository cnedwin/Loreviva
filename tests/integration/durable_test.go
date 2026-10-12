package integration

import (
	"context"
	"errors"
	"loreviva.local/loreviva/internal/content"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"testing"
)

func TestCoreDurableUnitOfWorkAndCanonicalTrace(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	r.Locks = []store.LockKey{
		{Rank: 110, Kind: "outbox", IDs: []string{"t", "p", "out"}}, {Rank: 110, Kind: "postings", IDs: []string{"t", "p", "posting"}},
		{Rank: 110, Kind: "service_events", IDs: []string{"t", "p", "service"}}, {Rank: 110, Kind: "approved_results", IDs: []string{"t", "p", "result"}}, {Rank: 110, Kind: "events", IDs: []string{"t", "p", "event"}},
		{Rank: 100, IDs: []string{"t", "p", "consumer", "message"}}, {Rank: 90, Kind: "memory", IDs: []string{"t", "p", "w", "m"}}, {Rank: 90, Kind: "relationship", IDs: []string{"t", "p", "w", "r"}},
		{Rank: 90, Kind: "actor", IDs: []string{"t", "p", "w", "a"}}, {Rank: 90, Kind: "item", IDs: []string{"t", "p", "w", "i"}},
		{Rank: 80, Kind: "customer", IDs: []string{"t.p", "account", "held"}}, {Rank: 70, Kind: "customer", IDs: []string{"t.p", "account"}},
		{Rank: 60, IDs: []string{"t", "p", "task", "a", "auth"}},
	}
	var trace []int
	var receiptLocks []string
	s.Trace = func(k store.LockKey) {
		trace = append(trace, k.Rank)
		if k.Rank == 110 && k.Kind == "commit_receipts" {
			receiptLocks = append(receiptLocks, k.IDs[0])
		}
		t.Logf("LOCK rank=%d kind=%s ids=%q", k.Rank, k.Kind, k.IDs)
	}
	writes := []struct{ kind, id, sql string }{
		{"events", "event", "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,world_instance_id,status,revision,sequence_no) VALUES('t','p','event','task','owner','w','COMMITTED',1,1)"},
		{"approved_results", "result", "INSERT INTO approved_results(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','result','task','owner','COMMITTED',1)"},
		{"service_events", "service", "INSERT INTO service_events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','service','task','owner','COMMITTED',1)"},
		{"postings", "posting", "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES('t','p','posting','task','owner','customer','t.p','account',0,'COMMITTED',1)"},
		{"outbox", "out", "INSERT INTO outbox(tenant_id,product_id,record_id,task_id,owner_subject_id,consumer_id,message_id,status,revision) VALUES('t','p','out','task','owner','consumer','message','PENDING',1)"},
	}
	write := func(tx store.Tx) error {
		for _, w := range writes {
			if e := tx.RequireLock(store.LockKey{Rank: 110, Kind: w.kind, IDs: []string{"t", "p", w.id}}); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, w.sql); e != nil {
				return e
			}
		}
		return tx.SetTaskState(ctx, "COMMITTED")
	}
	_, e := s.WithBarrier(ctx, r, func(tx store.Tx) error {
		if e := write(tx); e != nil {
			return e
		}
		return errors.New("abort all durable rows")
	})
	if e == nil {
		t.Fatal("abort committed")
	}
	for _, table := range []string{"events", "approved_results", "service_events", "postings", "outbox", "commit_receipts"} {
		var n int
		if e := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n != 0 {
			t.Fatal("atomic rollback left", table, n)
		}
	}
	trace = nil
	receiptLocks = nil
	receipt, e := s.WithBarrier(ctx, r, write)
	if e != nil || !receipt.Committed {
		t.Fatal(e)
	}
	// Execution preplans both possible Skill capabilities for both immutable
	// attempt slots, even without a binding. They are locks, not extra receipts.
	want := []int{10, 20, 30, 40, 45, 50, 60, 70, 80, 90, 90, 90, 90, 100, 110, 110, 110, 110, 110, 110, 110, 110, 110, 110}
	if !reflect.DeepEqual(trace, want) {
		t.Fatal("actual SQL lock trace order", trace)
	}
	wantReceipt := map[string]bool{r.CommitKey: true}
	for _, slot := range []string{"0", "1"} {
		for _, capability := range []string{"TEXT", "SIGN_CUSTODY"} {
			wantReceipt[content.Identity(r.TaskID, content.Identity(r.TaskID, "attempt", slot), "skill-adapter", capability)] = true
		}
	}
	if len(receiptLocks) != len(wantReceipt) {
		t.Fatal("exact receipt lock count", receiptLocks)
	}
	for _, key := range receiptLocks {
		if !wantReceipt[key] {
			t.Fatal("unexpected receipt lock", key)
		}
		delete(wantReceipt, key)
	}
	for _, table := range []string{"events", "approved_results", "service_events", "postings", "outbox", "commit_receipts"} {
		var n int
		if e := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n != 1 {
			t.Fatal("atomic commit missing", table, n)
		}
	}
	var state string
	scanAssertion(t, db.QueryRow("SELECT state FROM tasks"), &state)
	if state != "COMMITTED" {
		t.Fatal(state)
	}
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { t.Fatal("committed task replay reran body"); return nil }); e != nil {
		t.Fatal("committed task cannot replay original receipt", e)
	}
}

func TestZeroAmountReservationStillDurable(t *testing.T) {
	s, db, r := fixture(t, false)
	d := dispatch(r)
	d.Reservations[0].Amount = 0
	if _, e := s.AuthorizeDispatch(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	d.CommitKey = "send"
	if a, e := s.StartSend(context.Background(), d, d.AttemptID); e != nil || !a.CanSend {
		t.Fatal(a, e)
	}
	var held, used, reservations int
	scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
	scanAssertion(t, db.QueryRow("SELECT calls_used FROM tasks"), &used)
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM reservations WHERE amount=0"), &reservations)
	if held != 0 || used != 1 || reservations != 1 {
		t.Fatal(held, used, reservations)
	}
}
