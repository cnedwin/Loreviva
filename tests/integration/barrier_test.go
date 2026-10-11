package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
)

func fixture(t *testing.T, world bool) (*store.Store, *sql.DB, store.BarrierRequest) {
	t.Helper()
	db := dbfixture.Open(t)
	s := store.New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := db.QueryRow("SELECT VERSION()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "9.7.2" {
		t.Fatalf("actual target version=%s", version)
	}
	t.Log("actual version", version)
	statements := []string{
		"INSERT INTO tenant_gates(tenant_id,permission_version,enabled) VALUES('t',1,1)",
		"INSERT INTO product_gates(tenant_id,product_id,permission_version,enabled) VALUES('t','p',1,1)",
		"INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('t','p','owner',1,1,1,1)",
		"INSERT INTO releases(tenant_id,product_id,release_id,available) VALUES('t','p','r',1)",
		"INSERT INTO budget_accounts(tenant_id,product_id,account_id,kind,scope_key,balance,held) VALUES('t','p','account','customer','t.p',100,0)",
	}
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := store.BarrierRequest{ProductScope: domain.ProductScope{TenantID: "t", ProductID: "p", SubjectID: "owner"}, OwnerSubjectID: "owner", Operation: "generate", CommandKey: "command", CommitKey: "commit", TaskID: "task", WorkerID: "worker", Fence: 1, GateVersions: store.GateVersions{Tenant: 1, Product: 1, Subject: 1, Privacy: 1, View: 1}}
	var worldID any
	scopeKind := "PRODUCT"
	taskKind := "ADMIN"
	if world {
		scopeKind = "WORLD"
		taskKind = "TURN"
		worldID = "w"
		r.WorldScope = &domain.Scope{ProductScope: r.ProductScope, WorldInstanceID: "w"}
		r.Versions = &domain.Versions{ReleaseID: "r", WorldRevision: 1, ControlVersion: 1, PrivacyEpoch: 1, ViewEpoch: 1}
		if _, err := db.Exec("INSERT INTO world_controls(tenant_id,product_id,world_instance_id,release_id,world_revision,control_version,privacy_epoch,view_epoch,status) VALUES('t','p','w','r',1,1,1,1,'RUNNING')"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,world_instance_id,state,fence,lease_owner,lease_expires_at,next_run_at,task_kind) VALUES('t','p','task','owner',?,?, 'RUNNING',1,'worker',?,?,?)", scopeKind, worldID, time.Now().UTC().Add(time.Hour), time.Now().UTC().Add(-time.Hour), taskKind); err != nil {
		t.Fatal(err)
	}
	return s, db, r
}

func TestLockPlanCanonicalOrder(t *testing.T) {
	input := []store.LockKey{
		{Rank: 110, Kind: "commit_receipts", IDs: []string{"z"}}, {Rank: 100, IDs: []string{"x"}}, {Rank: 90, Kind: "memory", IDs: []string{"a"}},
		{Rank: 80, Kind: "world", IDs: []string{"a", "z"}}, {Rank: 70, Kind: "world", IDs: []string{"a"}}, {Rank: 60, IDs: []string{"x"}}, {Rank: 50, IDs: []string{"x"}}, {Rank: 45, IDs: []string{"x"}}, {Rank: 40, IDs: []string{"x"}},
		{Rank: 30, IDs: []string{"a"}}, {Rank: 30, IDs: []string{"Z"}}, {Rank: 20, IDs: []string{"x"}}, {Rank: 10, IDs: []string{"x"}},
	}
	p, err := store.PlanLocks(input)
	if err != nil {
		t.Fatal(err)
	}
	ranks := make([]int, len(p))
	for i, k := range p {
		ranks[i] = k.Rank
	}
	if !reflect.DeepEqual(ranks, []int{10, 20, 30, 30, 40, 45, 50, 60, 70, 80, 90, 100, 110}) {
		t.Fatalf("ranks=%v", ranks)
	}
	if p[2].IDs[0] != "Z" || p[3].IDs[0] != "a" {
		t.Fatal("subject IDs are not binary ordered")
	}
	for rank, kinds := range map[int][]string{70: {"supplier_global", "supplier_tenant", "platform_risk", "customer", "project", "world"}, 80: {"supplier_global", "supplier_tenant", "platform_risk", "customer", "project", "world"}, 90: {"item", "actor", "relationship", "memory"}, 110: {"events", "approved_results", "service_events", "postings", "outbox", "commit_receipts"}} {
		var unsorted []store.LockKey
		for i := len(kinds) - 1; i >= 0; i-- {
			unsorted = append(unsorted, store.LockKey{Rank: rank, Kind: kinds[i], IDs: []string{"a"}})
		}
		sorted, e := store.PlanLocks(unsorted)
		if e != nil {
			t.Fatal(e)
		}
		for i, k := range sorted {
			if k.Kind != kinds[i] {
				t.Fatalf("rank%d=%v", rank, sorted)
			}
		}
	}
	p, err = store.PlanLocks([]store.LockKey{{Rank: 90, Kind: "actor", IDs: []string{"a"}}, {Rank: 90, Kind: "actor", IDs: []string{"Z"}}})
	if err != nil || p[0].IDs[0] != "Z" {
		t.Fatal("domain byte order")
	}
	if _, err = store.PlanLocks([]store.LockKey{{Rank: 71, IDs: []string{"x"}}}); err == nil {
		t.Fatal("unregistered rank accepted")
	}
}

func TestBarrierActualIsolationAtomicRollbackAndOwnership(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	var isolation string
	if _, e := db.Exec("CREATE TABLE isolation_probe(id INT PRIMARY KEY,value INT NOT NULL) ENGINE=InnoDB"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("INSERT INTO isolation_probe VALUES(1,1)"); e != nil {
		t.Fatal(e)
	}
	readStarted := make(chan struct{})
	changed := make(chan error, 1)
	// This independent writer lives outside the callback and its connection.
	go func() { <-readStarted; _, e := db.Exec("UPDATE isolation_probe SET value=2 WHERE id=1"); changed <- e }()
	r.Locks = []store.LockKey{{Rank: 110, Kind: "events", IDs: []string{"t", "p", "event"}}, {Rank: 110, Kind: "events", IDs: []string{"t", "p", "rollback"}}}
	receipt, err := s.WithBarrier(ctx, r, func(tx store.Tx) error {
		if e := tx.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&isolation); e != nil {
			return e
		}
		var before, after int
		if e := tx.QueryRowContext(ctx, "SELECT value FROM isolation_probe WHERE id=1").Scan(&before); e != nil {
			return e
		}
		close(readStarted)
		if e := <-changed; e != nil {
			return e
		}
		if e := tx.QueryRowContext(ctx, "SELECT value FROM isolation_probe WHERE id=1").Scan(&after); e != nil {
			return e
		}
		if before != 1 || after != 2 {
			return fmt.Errorf("not READ COMMITTED: before=%d after=%d", before, after)
		}
		if e := tx.RequireLock(store.LockKey{Rank: 110, Kind: "events", IDs: []string{"t", "p", "event"}}); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','event','task','owner','COMMITTED',1)")
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	if isolation != "REPEATABLE-READ" || !receipt.Committed {
		t.Fatalf("isolation=%s receipt=%+v", isolation, receipt)
	}
	t.Log("session default REPEATABLE-READ; actual transaction rereads concurrent committed value 1 -> 2 (READ COMMITTED)")
	replay, e := s.WithBarrier(ctx, r, func(store.Tx) error { t.Fatal("idempotent replay ran body"); return nil })
	if e != nil || replay.Key != receipt.Key {
		t.Fatal(e, replay)
	}
	r.CommitKey = "rollback"
	r.CommandKey = "rollback"
	_, e = s.WithBarrier(ctx, r, func(tx store.Tx) error {
		if e := tx.RequireLock(store.LockKey{Rank: 110, Kind: "events", IDs: []string{"t", "p", "rollback"}}); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES('t','p','rollback','task','owner','COMMITTED',1)")
		if e != nil {
			return e
		}
		return errors.New("abort")
	})
	if e == nil {
		t.Fatal("abort accepted")
	}
	var count int
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM events"), &count)
	if count != 1 {
		t.Fatalf("partial writes count=%d", count)
	}
	r.OwnerSubjectID = "other"
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("wrong owner accepted")
	}
	r.OwnerSubjectID = "owner"
	r.WorldScope = nil
	r.Versions = nil
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("world task accepted as product task")
	}
	r.ProductScope.ProductID = "wrong"
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("wrong product accepted")
	}
}

func TestProductScopeAndMissingGatesFailClosed(t *testing.T) {
	s, db, r := fixture(t, false)
	ctx := context.Background()
	receipt, e := s.WithBarrier(ctx, r, func(store.Tx) error { return nil })
	if e != nil || !receipt.Committed {
		t.Fatal(e)
	}
	if _, e = s.QueryCommitScoped(ctx, r.ProductScope, nil, "other", receipt.Key); e == nil {
		t.Fatal("receipt leaked to nonowner")
	}
	if _, e = s.QueryCommitScoped(ctx, r.ProductScope, nil, "owner", receipt.Key); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("UPDATE subject_governance SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	r.CommitKey = "disabled"
	r.CommandKey = "disabled"
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("disabled owner authorized")
	}
	r.GateVersions.Subject = 0
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("missing version authorized")
	}
}
