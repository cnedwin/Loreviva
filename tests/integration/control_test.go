package integration

import (
	"context"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestProgramControlCanStartCreatedButCannotGenerate(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	if _, e := db.Exec("UPDATE world_controls SET status='CREATED'"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("UPDATE tasks SET task_kind='CONTROL'"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("UPDATE subject_governance SET can_control=1"); e != nil {
		t.Fatal(e)
	}
	r.Purpose = store.ControlPurpose
	r.ExpectedWorldStatus = "CREATED"
	if _, e := s.AuthorizeDispatch(ctx, dispatch(r)); e == nil {
		t.Fatal("CONTROL acquired generation")
	}
	receipt, e := s.WithBarrier(ctx, r, func(tx store.Tx) error {
		if e := tx.SetControlStatus(ctx, "RUNNING"); e != nil {
			return e
		}
		return tx.SetTaskState(ctx, "COMMITTED")
	})
	if e != nil || !receipt.Committed {
		t.Fatal("CREATED control blocked", e)
	}
	var status string
	var version uint64
	if e := db.QueryRow("SELECT status,control_version FROM world_controls").Scan(&status, &version); e != nil {
		t.Fatal(e)
	}
	if status != "RUNNING" || version != 2 {
		t.Fatal(status, version)
	}
}

func TestTurnCannotImpersonateProgramControl(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	if _, e := db.Exec("UPDATE subject_governance SET can_control=1"); e != nil {
		t.Fatal(e)
	}
	r.Purpose = store.ControlPurpose
	r.ExpectedWorldStatus = "RUNNING"
	if _, e := s.WithBarrier(ctx, r, func(tx store.Tx) error { return tx.SetControlStatus(ctx, "PAUSED") }); e == nil {
		t.Fatal("TURN impersonated stored CONTROL task")
	}
}

func TestMigrationPurposeRequiresPausedWorld(t *testing.T) {
	s, db, r := fixture(t, true)
	ctx := context.Background()
	if _, e := db.Exec("UPDATE tasks SET task_kind='MIGRATION'"); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("UPDATE subject_governance SET can_migrate=1"); e != nil {
		t.Fatal(e)
	}
	r.Purpose = store.MigrationPurpose
	r.ExpectedWorldStatus = "RUNNING"
	if _, e := s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e == nil {
		t.Fatal("migration purpose admitted RUNNING world")
	}
	if _, e := db.Exec("UPDATE world_controls SET status='PAUSED'"); e != nil {
		t.Fatal(e)
	}
	r.ExpectedWorldStatus = "PAUSED"
	if _, e := s.WithBarrier(ctx, r, func(store.Tx) error { return nil }); e != nil {
		t.Fatal(e)
	}
}
