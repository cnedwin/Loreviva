package integration

import (
	"context"
	"testing"
)

func TestRuntimeInitialVersionDDLRejectsWrongOwnershipOrEnforcement(t *testing.T) {
	for _, mode := range []string{"wrong-table", "not-enforced", "original-not-enforced", "original-wrong-table"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _ := runtimeFixture(t)
			statements := []string{"DELETE FROM schema_migrations WHERE version=4"}
			switch mode {
			case "wrong-table":
				statements = append(statements, "ALTER TABLE world_controls DROP CHECK runtime_initial_control, ADD CONSTRAINT world_controls_chk_3 CHECK(control_version>0 AND privacy_epoch>0 AND view_epoch>0)", "CREATE TABLE decoy(control_version BIGINT,status VARCHAR(20),privacy_epoch BIGINT,view_epoch BIGINT, CONSTRAINT runtime_initial_control CHECK ((control_version>0 OR (control_version=0 AND status='CREATED')) AND privacy_epoch>0 AND view_epoch>0))")
			case "original-wrong-table":
				statements = append(statements, "ALTER TABLE world_controls DROP CHECK runtime_initial_control", "CREATE TABLE decoy(control_version BIGINT,privacy_epoch BIGINT,view_epoch BIGINT, CONSTRAINT world_controls_chk_3 CHECK(control_version>0 AND privacy_epoch>0 AND view_epoch>0))")
			case "not-enforced":
				statements = append(statements, "ALTER TABLE world_controls ALTER CHECK runtime_initial_control NOT ENFORCED")
			case "original-not-enforced":
				statements = append(statements, "ALTER TABLE world_controls DROP CHECK runtime_initial_control, ADD CONSTRAINT world_controls_chk_3 CHECK(control_version>0 AND privacy_epoch>0 AND view_epoch>0) NOT ENFORCED")
			}
			for _, q := range statements {
				if _, e := r.Store.DB.Exec(q); e != nil {
					t.Fatal(e)
				}
			}
			if e := r.Store.Migrate(context.Background()); e == nil {
				t.Error("invalid DDL metadata accepted")
			}
			var n int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version=4").Scan(&n); e != nil || n != 0 {
				t.Fatal("version 4 recorded for invalid constraint", n, e)
			}
		})
	}
}

func TestRuntimeInitialVersionDDLRecoveryVerifiesExactConstraint(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	var clause string
	if e := r.Store.DB.QueryRow("SELECT CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=DATABASE() AND CONSTRAINT_NAME='runtime_initial_control'").Scan(&clause); e != nil {
		t.Fatal(e)
	}
	t.Log("actual named initial control clause:", clause)
	if _, e := r.Store.DB.Exec("DELETE FROM schema_migrations WHERE version=4"); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.Migrate(context.Background()); e != nil {
		t.Fatal("atomic DDL completion recovery", e)
	}
	if _, e := r.Store.DB.Exec("DELETE FROM schema_migrations WHERE version=4"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("ALTER TABLE world_controls DROP CHECK runtime_initial_control, ADD CONSTRAINT runtime_initial_control CHECK(control_version>=0 AND (status='CREATED' OR status='RUNNING' OR status='PAUSED' OR status='ENDED') AND privacy_epoch>0 AND view_epoch>0)"); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.Migrate(context.Background()); e == nil {
		t.Fatal("unrecognized weaker named constraint accepted as successful DDL recovery")
	}
}
