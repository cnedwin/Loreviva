package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sync"
	"testing"
	"time"
)

func TestHarnessPreservesOriginalLegacyDispatchFingerprint(t *testing.T) {
	s, db, base := fixture(t, true)
	d := dispatch(base)
	original, _ := json.Marshal(struct {
		Attempt, Authorization, Route, Request string
		Hash                                   [32]byte
		Slot                                   uint8
		Reservations                           []store.Reservation
	}{d.AttemptID, d.AuthorizationID, d.Route, d.RequestKey, d.RequestHash, d.Slot, d.Reservations})
	expected := sha256.Sum256(original)
	if _, e := s.AuthorizeDispatch(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	var actual []byte
	if e := db.QueryRow("SELECT payload_hash FROM commit_receipts WHERE commit_key=?", d.CommitKey).Scan(&actual); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(actual, expected[:]) {
		t.Fatal("Task6 changed an original legacy request fingerprint")
	}
}

func harnessBudgetFixture(t *testing.T) (*store.Store, *sql.DB, store.BarrierRequest, store.CommandBarrierRequest, store.HarnessQuote, store.HarnessIntake) {
	t.Helper()
	s, db, base := fixture(t, true)
	for _, q := range []string{
		"INSERT INTO budget_accounts(kind,scope_key,account_id,balance) VALUES('supplier_global','GLOBAL','sup',100),('platform_risk','GLOBAL','risk',100)",
		"INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,balance) VALUES('supplier_tenant','t','sup','t',100)",
		"INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,project_id,balance) VALUES('project','t.p','project','t','p','proj',100)",
		"INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,world_instance_id,balance) VALUES('world','t.p.w','world','t','p','w',100)",
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	c := command(base)
	c.WorldScope = base.WorldScope
	c.Versions = base.Versions
	c.Operation = "HARNESS_QUOTE"
	c.CommandKey = "quote"
	c.CommitKey = "quote"
	q := store.HarnessQuote{ID: "quote", ProjectID: "proj", Kind: "TURN", RequestHash: [32]byte{1}, RouteHash: [32]byte{2}, ServiceAtoms: 60, SupplierAtoms: 60}
	in := store.HarnessIntake{TaskID: "budget-turn", QuoteID: q.ID, RequestHash: q.RequestHash, RouteHash: q.RouteHash, ParentHolds: []store.Reservation{
		{Kind: "platform_risk", ScopeKey: "GLOBAL", AccountID: "risk", ReservationID: "risk-hold", Amount: 60},
		{Kind: "customer", ScopeKey: "t.p", AccountID: "account", ReservationID: "customer-hold", Amount: 60},
		{Kind: "project", ScopeKey: "t.p", AccountID: "project", ReservationID: "project-hold", Amount: 60},
		{Kind: "world", ScopeKey: "t.p.w", AccountID: "world", ReservationID: "world-hold", Amount: 60},
	}}
	return s, db, base, c, q, in
}

func TestHarnessParentBudgetConcurrentAndRollback(t *testing.T) {
	s, db, _, c, q, in := harnessBudgetFixture(t)
	ctx := context.Background()
	q2 := q
	q2.ID = "quote-two"
	c2 := c
	c2.CommandKey = q2.ID
	c2.CommitKey = q2.ID
	if _, e := s.QuoteHarness(ctx, c, q); e != nil {
		t.Fatal(e)
	}
	if _, e := s.QuoteHarness(ctx, c2, q2); e != nil {
		t.Fatal(e)
	}
	c.Operation = "HARNESS_INTAKE"
	c.CommandKey = "accept-one"
	c.CommitKey = c.CommandKey
	c2.Operation = c.Operation
	c2.CommandKey = "accept-two"
	c2.CommitKey = c2.CommandKey
	in2 := in
	in2.TaskID = "budget-turn-two"
	in2.QuoteID = q2.ID
	in2.ParentHolds = append([]store.Reservation{}, in.ParentHolds...)
	for i := range in2.ParentHolds {
		in2.ParentHolds[i].ReservationID += "-two"
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	var wg sync.WaitGroup
	for _, v := range []struct {
		c  store.CommandBarrierRequest
		in store.HarnessIntake
	}{{c, in}, {c2, in2}} {
		wg.Add(1)
		go func(c store.CommandBarrierRequest, in store.HarnessIntake) {
			defer wg.Done()
			<-start
			_, e := s.AcceptHarness(ctx, c, in)
			done <- e
		}(v.c, v.in)
	}
	close(start)
	wg.Wait()
	close(done)
	wins := 0
	for e := range done {
		if e == nil {
			wins++
		} else if !errors.Is(e, store.ErrDenied) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatal("two parents exceeded one real global capacity", wins)
	}
	var count int
	var held int64
	if e := db.QueryRow("SELECT COUNT(*) FROM harness_envelopes").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	if e := db.QueryRow("SELECT held FROM budget_accounts WHERE kind='platform_risk'").Scan(&held); e != nil || held != 60 {
		t.Fatal("risk double hold", held, e)
	}
	if e := db.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&count); e != nil || count != 4 {
		t.Fatal("loser retained partial reservation", count, e)
	}
	if e := db.QueryRow("SELECT COUNT(*) FROM tasks WHERE task_id IN ('budget-turn','budget-turn-two')").Scan(&count); e != nil || count != 1 {
		t.Fatal("loser retained task", count, e)
	}
}

func TestHarnessExpiredQuoteAndAttemptCapRollback(t *testing.T) {
	s, db, base, c, q, in := harnessBudgetFixture(t)
	// Bare budget fixture exercises PRODUCT CANDIDATE; WORLD dispatch requires the real context owner.
	base.WorldScope = nil
	base.Versions = nil
	c.WorldScope = nil
	c.Versions = nil
	q.Kind = "CANDIDATE"
	in.ParentHolds = in.ParentHolds[:3]
	ctx := context.Background()
	if _, e := s.QuoteHarness(ctx, c, q); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("UPDATE harness_quotes SET created_at=DATE_SUB(created_at,INTERVAL 61 SECOND),expires_at=DATE_SUB(expires_at,INTERVAL 61 SECOND)"); e != nil {
		t.Fatal(e)
	}
	c.Operation = "HARNESS_INTAKE"
	c.CommandKey = "accept"
	c.CommitKey = c.CommandKey
	if _, e := s.AcceptHarness(ctx, c, in); !errors.Is(e, store.ErrDenied) {
		t.Fatal("expired quote admitted", e)
	}
	var count int
	if e := db.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	if _, e := db.Exec("UPDATE harness_quotes SET created_at=UTC_TIMESTAMP(6),expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 60 SECOND)"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.AcceptHarness(ctx, c, in); e != nil {
		t.Fatal(e)
	}
	lease, e := s.ClaimTask(ctx, "budget-worker", time.Now(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	d := dispatch(base)
	d.TaskID = in.TaskID
	d.WorkerID = "budget-worker"
	d.Fence = lease.Fence
	d.HarnessQuoteID = q.ID
	d.Reservations = []store.Reservation{{Kind: "supplier_global", ScopeKey: "GLOBAL", AccountID: "sup", ReservationID: "global-hold", Amount: 61}, {Kind: "supplier_tenant", ScopeKey: "t", AccountID: "sup", ReservationID: "tenant-hold", Amount: 61}}
	if _, e = s.AuthorizeDispatch(ctx, d); !errors.Is(e, store.ErrDenied) {
		t.Fatal("attempt exceeded parent cap", e)
	}
	if e = db.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=?", in.TaskID).Scan(&count); e != nil || count != 0 {
		t.Fatal("failed allocation left attempt", count, e)
	}
	for i := range d.Reservations {
		d.Reservations[i].Amount = 60
	}
	if _, e = s.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	// A legacy dispatch with a managed task cannot evade the envelope checks.
	legacy := d
	legacy.HarnessQuoteID = ""
	legacy.CommandKey = "legacy"
	legacy.CommitKey = "legacy"
	if _, e = s.AuthorizeDispatch(ctx, legacy); e == nil {
		t.Fatal("managed attempt accepted legacy bypass")
	}
	if _, e = db.Exec("UPDATE harness_envelopes SET execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND)"); e != nil {
		t.Fatal(e)
	}
	d.CommitKey = "start-send"
	if _, e = s.StartSend(ctx, d, d.AttemptID); !errors.Is(e, store.ErrDenied) {
		t.Fatal("expired execution sent", e)
	}
	if e = db.QueryRow("SELECT COUNT(*) FROM attempts WHERE send_started_at IS NOT NULL").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
}

func TestHarnessDurableParentAndAttemptHolds(t *testing.T) {
	for _, amount := range []int64{0, 7} {
		t.Run(time.Duration(amount).String(), func(t *testing.T) {
			s, db, base := fixture(t, false)
			ctx := context.Background()
			for _, q := range []string{
				"INSERT INTO budget_accounts(kind,scope_key,account_id,balance) VALUES('supplier_global','GLOBAL','sup',100),('platform_risk','GLOBAL','risk',100)",
				"INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,balance) VALUES('supplier_tenant','t','sup','t',100)",
				"INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,project_id,balance) VALUES('project','t.p','project','t','p','proj',100)",
			} {
				if _, e := db.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
			c := command(base)
			c.WorldScope = base.WorldScope
			c.Versions = base.Versions
			c.Operation = "HARNESS_QUOTE"
			c.CommandKey = "quote-command"
			c.CommitKey = "quote-commit"
			quote := store.HarnessQuote{ID: "quote", ProjectID: "proj", Kind: "CANDIDATE", RequestHash: [32]byte{1}, RouteHash: [32]byte{2}, ServiceAtoms: amount, SupplierAtoms: 2 * amount}
			if _, e := s.QuoteHarness(ctx, c, quote); e != nil {
				t.Fatal("durable quote unavailable", e)
			}
			in := store.HarnessIntake{TaskID: "budget-turn", QuoteID: "quote", RequestHash: quote.RequestHash, RouteHash: quote.RouteHash, ParentHolds: []store.Reservation{
				{Kind: "platform_risk", ScopeKey: "GLOBAL", AccountID: "risk", ReservationID: "risk-hold", Amount: 2 * amount},
				{Kind: "customer", ScopeKey: "t.p", AccountID: "account", ReservationID: "customer-hold", Amount: amount},
				{Kind: "project", ScopeKey: "t.p", AccountID: "project", ReservationID: "project-hold", Amount: amount},
			}}
			c.Operation = "HARNESS_INTAKE"
			c.CommandKey = "intake-command"
			c.CommitKey = "intake-commit"
			if _, e := s.AcceptHarness(ctx, c, in); e != nil {
				t.Fatal("durable intake unavailable", e)
			}
			if _, e := s.AcceptHarness(ctx, c, in); e != nil {
				t.Fatal("exact acceptance replay", e)
			}
			var accepted, expires, settle time.Time
			var calls, steps, count int
			if e := db.QueryRow("SELECT accepted_at,execute_by,settle_by,max_calls,max_steps FROM harness_envelopes WHERE task_id='budget-turn'").Scan(&accepted, &expires, &settle, &calls, &steps); e != nil {
				t.Fatal(e)
			}
			if expires.Sub(accepted) != 120*time.Second || settle.Sub(accepted) != 180*time.Second || calls != 2 || steps != 20 {
				t.Fatal("deadlines/limits not frozen", accepted, expires, settle, calls, steps)
			}
			if e := db.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id='budget-turn'").Scan(&count); e != nil || count != 3 {
				t.Fatal("parent holds missing or repeated", count, e)
			}
			lease, e := s.ClaimTask(ctx, "budget-worker", time.Now(), time.Minute)
			if e != nil || lease.TaskID != "budget-turn" {
				t.Fatal(lease, e)
			}
			d := dispatch(base)
			d.TaskID = in.TaskID
			d.WorkerID = "budget-worker"
			d.Fence = lease.Fence
			d.CommandKey = "budget-authorize"
			d.CommitKey = "budget-auth-receipt"
			d.HarnessQuoteID = quote.ID
			d.Reservations = []store.Reservation{{Kind: "supplier_global", ScopeKey: "GLOBAL", AccountID: "sup", ReservationID: "supplier-global-hold", Amount: amount}, {Kind: "supplier_tenant", ScopeKey: "t", AccountID: "sup", ReservationID: "supplier-tenant-hold", Amount: amount}}
			if _, e = s.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal("supplier attempt allocation", e)
			}
			if _, e = s.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal("attempt allocation replay", e)
			}
			if e = db.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id='budget-turn'").Scan(&count); e != nil || count != 5 {
				t.Fatal("exact parent+supplier holds", count, e)
			}
			var allocated int64
			if e = db.QueryRow("SELECT supplier_allocated FROM harness_envelopes WHERE task_id='budget-turn'").Scan(&allocated); e != nil || allocated != amount {
				t.Fatal("dual safety holds double-counted actual cost", allocated, e)
			}
			var used int
			if e = db.QueryRow("SELECT calls_used FROM tasks WHERE task_id='budget-turn'").Scan(&used); e != nil || used != 1 {
				t.Fatal(used, e)
			}
		})
	}
}
