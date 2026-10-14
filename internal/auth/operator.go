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
// Authorize resolves the request bearer through the current registry and checks the requested action; the configured-owner Check is not a substitute for this request identity.
func (o *Operator) Authorize(ctx context.Context, authorization, product, action string) (domain.ProductScope, error) {
	ctx, current, e := o.committedReadOperator(ctx, authorization, product)
	if e != nil {
		return domain.ProductScope{}, e
	}
	if _, e = content.Gate(ctx, current.store, current.scope); e != nil {
		return domain.ProductScope{}, ErrUnauthenticated
	}
	if e = current.store.CheckOperator(ctx, current.scope, action); e != nil {
		return domain.ProductScope{}, ErrForbidden
	}
	return current.scope, nil
}
func (o *Operator) Scope() domain.ProductScope { return o.scope }
