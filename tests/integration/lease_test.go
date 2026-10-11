package integration

import (
	"context"
	"errors"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestClaimLeaseReleasesBeforeDispatch(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	if _, e := db.Exec("UPDATE tasks SET lease_expires_at=?,state='ACCEPTED'", time.Now().UTC().Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	lease, e := s.ClaimTask(ctx, "next", time.Now().UTC(), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	if lease.Fence != 2 {
		t.Fatalf("fence=%d", lease.Fence)
	}
	// A second connection obtains the task lock before any dispatch. Claim must commit already.
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec("SET innodb_lock_wait_timeout=1"); e != nil {
		t.Fatal(e)
	}
	var fence uint64
	if e = tx.QueryRow("SELECT fence FROM tasks WHERE task_id='task' FOR UPDATE NOWAIT").Scan(&fence); e != nil {
		t.Fatal("claim kept task lock", e)
	}
	tx.Rollback()
	var attempts, receipts, held int
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM attempts"), &attempts)
	scanAssertion(t, db.QueryRow("SELECT COUNT(*) FROM commit_receipts"), &receipts)
	scanAssertion(t, db.QueryRow("SELECT held FROM budget_accounts"), &held)
	if attempts != 0 || receipts != 0 || held != 0 {
		t.Fatal("claim touched dispatch or budget", attempts, receipts, held)
	}
	r.WorkerID = "next"
	r.Fence = lease.Fence
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e != nil {
		t.Fatal(e)
	}
}

func TestOldFenceRejectedAtDispatchAndCommit(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	if _, e := db.Exec("UPDATE tasks SET lease_expires_at=?", time.Now().UTC().Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	lease, e := s.ClaimTask(ctx, "new", time.Now().UTC(), time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	d := dispatch(r)
	if _, e = s.AuthorizeDispatch(ctx, d); e == nil {
		t.Fatal("old fence dispatch succeeded")
	}
	if _, e = s.StartSend(ctx, d, d.AttemptID); e == nil {
		t.Fatal("old fence send succeeded")
	}
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { t.Fatal("old fence reached commit body"); return nil }); e == nil {
		t.Fatal("old fence commit succeeded")
	}
	r.WorkerID = "new"
	r.Fence = lease.Fence
	if _, e = s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e != nil {
		t.Fatal(e)
	}
}

func TestLeaseHelperProcess(t *testing.T) {
	if os.Getenv("LOREVIVA_LEASE_HELPER") != "1" {
		t.Skip("child process entry")
	}
	db := dbfixture.ReopenHelper(t)
	s := store.New(db)
	lease, e := s.ClaimTask(context.Background(), os.Getenv("LOREVIVA_HELPER_WORKER"), time.Now().UTC(), time.Hour)
	if errors.Is(e, store.ErrNoTask) {
		t.Log("NO_TASK")
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("CLAIMED task=%s fence=%d pid=%d", lease.TaskID, lease.Fence, os.Getpid())
}

func TestLeaseTwoProcessCompetition(t *testing.T) {
	_, db, _ := fixture(t, false)
	if _, e := db.Exec("UPDATE tasks SET state='ACCEPTED',lease_expires_at=?", time.Now().UTC().Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	var name string
	scanAssertion(t, db.QueryRow("SELECT DATABASE()"), &name)
	commands := []*exec.Cmd{exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$", "-test.v"), exec.Command(os.Args[0], "-test.run=^TestLeaseHelperProcess$", "-test.v")}
	outputs := make(chan string, 2)
	failures := make(chan error, 2)
	for i, cmd := range commands {
		cmd.Env = append(os.Environ(), "LOREVIVA_LEASE_HELPER=1", "LOREVIVA_HELPER_DATABASE="+name, "LOREVIVA_HELPER_WORKER=process"+string(rune('A'+i)))
		go func(c *exec.Cmd) { out, e := c.CombinedOutput(); outputs <- string(out); failures <- e }(cmd)
	}
	claimed, none := 0, 0
	for range commands {
		out := <-outputs
		t.Log(out)
		if strings.Contains(out, "CLAIMED task=task fence=2") {
			claimed++
		}
		if strings.Contains(out, "NO_TASK") {
			none++
		}
		if e := <-failures; e != nil {
			t.Fatal("child process failed", e)
		}
	}
	if claimed != 1 || none != 1 {
		t.Fatalf("claims=%d noTask=%d", claimed, none)
	}
	var fence uint64
	scanAssertion(t, db.QueryRow("SELECT fence FROM tasks"), &fence)
	if fence != 2 {
		t.Fatalf("double claim fence=%d", fence)
	}
}
