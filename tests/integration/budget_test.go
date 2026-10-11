package integration

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestGlobalBudgetHelperProcess(t *testing.T) {
	if os.Getenv("LOREVIVA_BARRIER_HELPER") != "1" {
		t.Skip("child process entry")
	}
	db := dbfixture.ReopenHelper(t)
	s := store.New(db)
	var d store.DispatchRequest
	if e := json.Unmarshal([]byte(os.Getenv("LOREVIVA_HELPER_DISPATCH")), &d); e != nil {
		t.Fatal(e)
	}
	_, e := s.AuthorizeDispatch(context.Background(), d)
	if errors.Is(e, store.ErrDenied) {
		t.Logf("DENIED pid=%d", os.Getpid())
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("AUTHORIZED pid=%d", os.Getpid())
}

func TestGlobalBudgetTwoProcessDifferentTenantCompetition(t *testing.T) {
	_, db, r := fixture(t, false)
	for _, q := range []string{
		"INSERT INTO tenant_gates(tenant_id,permission_version,enabled) VALUES('u',1,1)",
		"INSERT INTO product_gates(tenant_id,product_id,permission_version,enabled) VALUES('u','q',1,1)",
		"INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('u','q','owner',1,1,1,1)",
		"INSERT INTO budget_accounts(kind,scope_key,account_id,balance,held) VALUES('supplier_global','GLOBAL','global',7,0)",
	} {
		if _, e := db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := db.Exec("INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,fence,lease_owner,lease_expires_at,next_run_at,task_kind) VALUES('u','q','task','owner','PRODUCT','RUNNING',1,'worker',?,?,'ADMIN')", time.Now().UTC().Add(time.Hour), time.Now().UTC()); e != nil {
		t.Fatal(e)
	}
	other := r
	other.ProductScope = domain.ProductScope{TenantID: "u", ProductID: "q", SubjectID: "owner"}
	other.CommandKey = "other-command"
	other.CommitKey = "other"
	requests := []store.DispatchRequest{dispatch(r), dispatch(other)}
	requests[1].CommitKey = "other-authorize"
	for i := range requests {
		requests[i].Reservations = []store.Reservation{{Kind: "supplier_global", ScopeKey: "GLOBAL", AccountID: "global", ReservationID: "r" + string(rune('A'+i)), Amount: 7}}
	}
	var name string
	if e := db.QueryRow("SELECT DATABASE()").Scan(&name); e != nil {
		t.Fatal(e)
	}
	outputs := make(chan string, 2)
	failures := make(chan error, 2)
	for _, d := range requests {
		encoded, e := json.Marshal(d)
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestGlobalBudgetHelperProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "LOREVIVA_BARRIER_HELPER=1", "LOREVIVA_HELPER_DATABASE="+name, "LOREVIVA_HELPER_DISPATCH="+string(encoded))
		go func(c *exec.Cmd) { out, e := c.CombinedOutput(); outputs <- string(out); failures <- e }(cmd)
	}
	approved, denied := 0, 0
	for range requests {
		out := <-outputs
		t.Log(out)
		if strings.Contains(out, "AUTHORIZED pid=") {
			approved++
		}
		if strings.Contains(out, "DENIED pid=") {
			denied++
		}
		if e := <-failures; e != nil {
			t.Fatal(e)
		}
	}
	if approved != 1 || denied != 1 {
		t.Fatal(approved, denied)
	}
	var held, calls, attempts int
	if e := db.QueryRow("SELECT held FROM budget_accounts WHERE kind='supplier_global'").Scan(&held); e != nil {
		t.Fatal(e)
	}
	if e := db.QueryRow("SELECT SUM(calls_used) FROM tasks").Scan(&calls); e != nil {
		t.Fatal(e)
	}
	if e := db.QueryRow("SELECT COUNT(*) FROM attempts").Scan(&attempts); e != nil {
		t.Fatal(e)
	}
	if held != 7 || calls != 1 || attempts != 1 {
		t.Fatal("global cap copied or exceeded", held, calls, attempts)
	}
}

func TestWorldBudgetCannotCrossWorldScope(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	if _, e := db.Exec("INSERT INTO world_controls(tenant_id,product_id,world_instance_id,release_id,world_revision,control_version,privacy_epoch,view_epoch,status) VALUES('t','p','other-world','r',1,1,1,1,'RUNNING')"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("INSERT INTO budget_accounts(tenant_id,product_id,account_id,kind,scope_key,balance,held,world_instance_id) VALUES('t','p','other-account','world','other-world',100,0,'other-world')"); e != nil {
		t.Fatal(e)
	}
	d := dispatch(r)
	d.Reservations = []store.Reservation{{Kind: "world", ScopeKey: "other-world", AccountID: "other-account", ReservationID: "other-reservation", Amount: 1}}
	if _, e := s.AuthorizeDispatch(ctx, d); e == nil {
		t.Fatal("world budget crossed scope")
	}
}
