package integration

import (
	"context"
	"database/sql"
	"loreviva.local/loreviva/internal/maintenance"
	"testing"
)

func TestStudioLocalAdministrationExplicitAndNoReadSeeding(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	var count int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM studio_operator_grants WHERE credential_id='owner'").Scan(&count); e != nil || count != 13 {
		t.Fatal("fresh transaction grants", count, e)
	}
	if e := maintenance.InitializeLocalAdministration(ctx, r.Store); e == nil {
		t.Fatal("repeat initialization replaced administration")
	}
	// Model an actual pre-Studio installation; no read/Open grants new authority.
	if _, e := r.Store.DB.Exec("DELETE FROM studio_operator_grants"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("DELETE FROM studio_credential_state"); e != nil {
		t.Fatal(e)
	}
	if e := r.Readiness(ctx); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM studio_operator_grants").Scan(&count); e != nil || count != 0 {
		t.Fatal("readiness seeded rights", count, e)
	}
	if e := maintenance.InitializeLocalAdministration(context.Background(), r.Store); e == nil {
		t.Fatal("missing real original owner initialized")
	}
	if e := maintenance.InitializeLocalAdministration(ctx, r.Store); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE product_gates SET enabled=0 WHERE product_id=?", scope.ProductID); e != nil {
		t.Fatal(e)
	}
	// TENANT registration depends on its exact grant, not home product readiness.
	secret := "synthetic-registered-member-token-00000000001"
	if e := maintenance.RegisterLocalCredential(ctx, r.Store, scope, "registered-member", secret); e != nil {
		t.Fatal("tenant authority used product gate", e)
	}
	next, _, e := r.Auth.Authenticate(context.Background(), "Bearer "+secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Auth.Authorize(next, "Bearer "+secret, scope.ProductID, "content.read"); e == nil {
		t.Fatal("registration granted content")
	}
	if e = maintenance.RegisterLocalCredential(ctx, r.Store, scope, "registered-member", secret); e != nil {
		t.Fatal("same registration was not stable", e)
	}
	if e = maintenance.RegisterLocalCredential(ctx, r.Store, scope, "registered-member", secret+"changed"); e == nil {
		t.Fatal("registration rotated existing bearer")
	}
	missing := scope
	missing.SubjectID = "not-registered"
	if e = maintenance.RegisterLocalCredential(ctx, r.Store, missing, "missing", secret+"other"); e == nil {
		t.Fatal("registration created subject")
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "DELETE FROM studio_operator_grants WHERE action='maintenance.manage'")
		return e
	}
	if e = maintenance.RegisterLocalCredential(ctx, r.Store, scope, "revoked-during-registration", secret+"third"); e == nil {
		t.Fatal("registration ignored current maintenance revoke")
	}
	r.Store.BeforeCommit = nil
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM operator_credentials WHERE credential_id='revoked-during-registration'").Scan(&count); e != nil || count != 0 {
		t.Fatal("registration failed to roll back", count, e)
	}
}

func TestStudioGrantOverrideAndExpiryDoNotBorrowLegacy(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	for _, mode := range []string{"DENY", "DISABLED", "EXPIRED", "ALLOW"} {
		effect := "ALLOW"
		enabled := true
		expiry := "NULL"
		if mode == "DENY" {
			effect = "DENY"
		}
		if mode == "DISABLED" {
			enabled = false
		}
		if mode == "EXPIRED" {
			expiry = "DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND)"
		}
		if _, e := r.Store.DB.Exec("DELETE FROM studio_operator_grants WHERE action='content.read'"); e != nil {
			t.Fatal(e)
		}
		q := "INSERT INTO studio_operator_grants VALUES('demo-local','owner','PRODUCT','coffee','P:coffee','content.read','PRODUCT_ADMIN',?,?," + expiry + ",1)"
		if _, e := r.Store.DB.Exec(q, effect, enabled); e != nil {
			t.Fatal(e)
		}
		e := r.Store.CheckOperator(ctx, scope, "content.read")
		if (e == nil) != (mode == "ALLOW") {
			t.Fatal(mode, e)
		}
	}
	if _, e := r.Store.DB.Exec("UPDATE studio_credential_state SET expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.CheckOperator(ctx, scope, "content.read"); e == nil {
		t.Fatal("cached identity survived current expiry")
	}
}
