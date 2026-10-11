// Package auth keeps OperatorBearer distinct from customer service/session tokens.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
)

var ErrUnauthenticated = errors.New("operator authentication required")
var ErrNotFound = errors.New("requested product missing or not visible")
var ErrForbidden = errors.New("operator action not granted")

type Operator struct {
	store        *store.Store
	scope        domain.ProductScope
	credentialID string
	configured   [32]byte
	resolved     bool
}

func New(s *store.Store, scope domain.ProductScope, id, secret string) *Operator {
	o := &Operator{store: s, scope: scope, credentialID: id}
	if len(secret) >= 32 && len(secret) <= 512 {
		o.configured = sha256.Sum256([]byte(secret))
		o.resolved = true
	}
	return o
}
func (o *Operator) Check(ctx context.Context) error {
	if o == nil || !o.resolved || o.scope.Validate() != nil || domain.ValidateID(o.credentialID) != nil {
		return ErrUnauthenticated
	}
	var h []byte
	var enabled bool
	e := o.store.DB.QueryRowContext(ctx, "SELECT token_sha256,enabled FROM operator_credentials WHERE credential_id=? AND tenant_id=? AND product_id=? AND subject_id=?", o.credentialID, o.scope.TenantID, o.scope.ProductID, o.scope.SubjectID).Scan(&h, &enabled)
	if e != nil || !enabled || subtle.ConstantTimeCompare(h, o.configured[:]) != 1 {
		return ErrUnauthenticated
	}
	if _, e = content.Gate(ctx, o.store, o.scope); e != nil {
		return ErrUnauthenticated
	}
	var n int
	if e = o.store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM operator_actions WHERE credential_id=? AND action='content.read'", o.credentialID).Scan(&n); e != nil || n != 1 {
		return ErrUnauthenticated
	}
	return nil
}
func (o *Operator) Authorize(ctx context.Context, authorization, product, action string) (domain.ProductScope, error) {
	if o == nil || !o.resolved || !strings.HasPrefix(authorization, "Bearer ") {
		return domain.ProductScope{}, ErrUnauthenticated
	}
	provided := strings.TrimPrefix(authorization, "Bearer ")
	h := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(h[:], o.configured[:]) != 1 {
		return domain.ProductScope{}, ErrUnauthenticated
	}
	if e := o.Check(ctx); e != nil {
		return domain.ProductScope{}, ErrUnauthenticated
	}
	if product != o.scope.ProductID {
		return domain.ProductScope{}, ErrNotFound
	}
	var n int
	if e := o.store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM operator_actions WHERE credential_id=? AND action=?", o.credentialID, action).Scan(&n); e != nil || n != 1 {
		return domain.ProductScope{}, ErrForbidden
	}
	return o.scope, nil
}
func (o *Operator) Scope() domain.ProductScope { return o.scope }
