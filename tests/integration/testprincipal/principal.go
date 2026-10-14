// Package testprincipal registers synthetic test authority explicitly. Production
// services never import it or derive authority from a supplied scope.
package testprincipal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func Register(t *testing.T, s *store.Store, scope domain.ProductScope) context.Context {
	return RegisterActions(t, s, scope, maintenance.OperatorActions)
}

func RegisterActions(t *testing.T, s *store.Store, scope domain.ProductScope, actions []string) context.Context {
	t.Helper()
	sum := sha256.Sum256([]byte(scope.TenantID + "\x00" + scope.ProductID + "\x00" + scope.SubjectID))
	id := "fixture-" + hex.EncodeToString(sum[:12])
	secret := "synthetic-registered-test-principal-" + hex.EncodeToString(sum[:])
	hash := sha256.Sum256([]byte(secret))
	if _, e := s.DB.Exec("INSERT INTO operator_credentials(credential_id,tenant_id,product_id,subject_id,token_sha256,enabled) VALUES(?,?,?,?,?,1)", id, scope.TenantID, scope.ProductID, scope.SubjectID, hash[:]); e != nil {
		t.Fatal(e)
	}
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
