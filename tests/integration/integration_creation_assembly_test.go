package integration

import (
	"context"
	"database/sql"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIntegrationCreateActualAssemblyOrder(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	independent, private := t.TempDir(), t.TempDir()
	if e := app.InitializeLocal(ctx, s, independent, private); e != nil {
		t.Fatal(e)
	}
	f, e := files.New(s, private)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	// Match cmd/loreviva runtime(): NewRuntime precedes OpenLocal, then BindLocal.
	r := app.NewRuntime(s, f, auth.New(s, scope, "owner", runtimeToken))
	r.BindLocal(nil)
	if e = r.World.BindIntegrationCreation(r.Memory); e == nil {
		t.Fatal("nil admission bound creation")
	}
	local, e := app.OpenLocal(ctx, s, independent, private)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := local.Participant.Close(ctx); e != nil {
			t.Error(e)
		}
	})
	if e = maintenance.BootstrapSynthetic(ctx, s, scope, "owner", runtimeToken); e != nil {
		t.Fatal(e)
	}
	if e = r.World.BindIntegrationCreation(memory.New(store.New(s.DB), nil)); e == nil {
		t.Fatal("foreign memory Store bound")
	}
	r.BindLocal(&app.Local{Participant: local.Participant, Coordinator: local.Coordinator, Ledger: local.Ledger, Mock: local.Mock})
	// Publicly copied Local fields do not carry its private Store binding.
	if e = s.BindIntegrationCreationOwners(nil, nil); e == nil {
		t.Fatal("nil owners accepted")
	}
	r.BindLocal(local)
	if e = r.World.BindIntegrationCreation(r.Memory); e == nil {
		t.Fatal("second app binding accepted")
	}
	if e = local.Coordinator.BeginClose(ctx, "s66-assembly-close"); e != nil {
		t.Fatal(e)
	}
	r.BindLocal(local)
	if e = s.BindIntegrationCreationOwners(r.Content, r.World); e == nil {
		t.Fatal("Close reenabled binding")
	}
	if e = local.Coordinator.Drain(ctx, "s66-assembly-close"); e != nil {
		t.Fatal(e)
	}
	t.Log("actual CLI construction order; nil/foreign/copied/repeated/Close binding rejected")
}
func TestIntegrationCreateOriginalReadCurrentLinearization(t *testing.T) {
	r, op, ctx, req, _ := integrationCreationFixture(t)
	done := make(chan error, 1)
	calls := 0
	r.Store.BeforeReadFinish = func(context.Context, *sql.Tx) error {
		calls++
		go func() {
			_, e := r.Store.ApplyIntegrationRegistry(op, "s66-read-disable", domain.RegistryCommand{Kind: "DISABLE_ENTITLEMENT", ProductID: "coffee", TargetID: "s66-entitlement-one", QualifiedTarget: &domain.RegistryQualifiedTarget{CustomerID: "s66-customer"}, ExpectedRevision: 2})
			done <- e
		}()
		select {
		case e := <-done:
			t.Errorf("registry crossed held owning read lock: %v", e)
			return store.ErrDenied
		case <-time.After(50 * time.Millisecond):
			return nil
		}
	}
	out, e := r.World.CreateIntegrationInstance(ctx, "s66-linear", req)
	r.Store.BeforeReadFinish = nil
	if e != nil || calls != 1 {
		t.Fatal("current read should precede waiting revocation", e, calls)
	}
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("real registry writer did not drain")
	}
	if _, e = r.World.CreateIntegrationInstance(ctx, "s66-linear", req); e == nil {
		t.Fatal("original replay ignored current revocation")
	}
	if integrationWorldRows(t, r.Store, out.Scope.WorldInstanceID) == 0 {
		t.Fatal("later revocation erased earlier committed create")
	}
}
func TestIntegrationCreatePrecollectServiceRotation(t *testing.T) {
	r, op, ctx, req, _ := integrationCreationFixture(t)
	p, e := r.Store.PrepareIntegrationCreation(ctx, "s66-rotate", req)
	if e != nil {
		t.Fatal(e)
	}
	var principal string
	if e = r.Store.DB.QueryRow("SELECT principal_id FROM integration_principals WHERE customer_id='s66-customer' AND kind='SERVICE'").Scan(&principal); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "rotated-key")
	if e = os.WriteFile(path, []byte("synthetic-s66-rotated-service-secret-32"), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := domain.RegistryCommand{Kind: "ROTATE_SERVICE_KEY", ProductID: "coffee", TargetID: principal, ExpectedRevision: 1, Rotation: &domain.RotateServiceKey{CustomerID: "s66-customer", OldPrincipalID: principal, NewPrincipalID: "s66-rotated-principal", SecretHandleRef: path}}
	if _, e = r.Store.ApplyIntegrationRegistry(op, "s66-rotate-real", cmd); e != nil {
		t.Fatal("real rotate", e)
	}
	if _, e = r.Store.RunIntegrationCreation(ctx, p); e == nil || integrationWorldRows(t, r.Store, p.WorldID()) != 0 {
		t.Fatal("rotated credential used stale proof", e)
	}
}
