// Package testprincipal registers synthetic test authority explicitly. Production
// services never import it or derive authority from a supplied scope.
package testprincipal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"path/filepath"
	"testing"
)

func Register(t *testing.T, s *store.Store, owner context.Context, scope domain.ProductScope) context.Context {
	return RegisterActions(t, s, owner, scope, maintenance.OperatorActions)
}

func RegisterActions(t *testing.T, s *store.Store, owner context.Context, scope domain.ProductScope, actions []string) context.Context {
	t.Helper()
	sum := sha256.Sum256([]byte(scope.TenantID + "\x00" + scope.ProductID + "\x00" + scope.SubjectID))
	id := "fixture-" + hex.EncodeToString(sum[:12])
	secret := "synthetic-registered-test-principal-" + hex.EncodeToString(sum[:])
	retainCredential(t, secret)
	// The real owner registers both the credential and its installation-bound hash.
	if e := maintenance.RegisterLocalCredential(owner, s, scope, id, secret); e != nil {
		t.Fatal(e)
	}
	// DATA_SETUP preserves only the legacy fixture action set, not a delegation API.
	for _, action := range actions {
		if _, e := s.DB.Exec("INSERT INTO operator_actions VALUES(?,?)", id, action); e != nil {
			t.Fatal(e)
		}
	}
	ctx, _, e := s.AuthenticateOperator(context.Background(), "Bearer "+secret)
	if e != nil {
		t.Fatal(e)
	}
	return ctx
}

// FreshSyntheticOwner supplies real installation/CONFIG authority to the two
// legacy file fixtures. It never reinitializes or bootstraps an existing store.
func FreshSyntheticOwner(t *testing.T) (*store.Store, domain.ProductScope, context.Context, string) {
	t.Helper()
	s := store.New(dbfixture.Open(t))
	ctx := context.Background()
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	independent, filesRoot := t.TempDir(), t.TempDir()
	if e := app.InitializeLocal(ctx, s, independent, filesRoot); e != nil {
		t.Fatal(e)
	}
	local, e := app.OpenLocal(ctx, s, independent, filesRoot)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := local.Participant.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	var random [32]byte
	if _, e = rand.Read(random[:]); e != nil {
		t.Fatal(e)
	}
	bearer := hex.EncodeToString(random[:])
	retainCredential(t, bearer)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	if e = maintenance.BootstrapSynthetic(ctx, s, scope, "owner", bearer); e != nil {
		t.Fatal(e)
	}
	owner, _, e := s.AuthenticateOperator(ctx, "Bearer "+bearer)
	if e != nil {
		t.Fatal(e)
	}
	return s, scope, owner, filesRoot
}

func retainCredential(t *testing.T, bearer string) {
	t.Helper()
	if channel := os.Getenv("LOREVIVA_TEST_CUSTODY_FILE"); channel != "" {
		if filepath.Dir(channel) != os.Getenv("TMP") {
			t.Fatal("private credential custody location")
		}
		f, e := os.OpenFile(channel, os.O_WRONLY|os.O_APPEND, 0600)
		if e != nil {
			t.Fatal("private credential custody unavailable")
		}
		e = json.NewEncoder(f).Encode([]string{bearer})
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			t.Fatal("private credential custody failed")
		}
	}
}
