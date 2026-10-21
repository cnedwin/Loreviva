package mysql

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"strings"
)

type operatorContextKey struct{}
type currentOperator struct {
	store                            *Store
	principal                        domain.OperatorPrincipal
	bearerHash                       [32]byte
	job, family, worker, world, kind string
	fence                            uint64
}

// AuthenticateOperator is the sole direct-principal mint. No subject, credential
// id, caller role or configured token can substitute for the unique bearer row.
func (s *Store) AuthenticateOperator(ctx context.Context, authorization string) (context.Context, domain.OperatorPrincipal, error) {
	var p domain.OperatorPrincipal
	if !strings.HasPrefix(authorization, "Bearer ") {
		return ctx, p, ErrDenied
	}
	secret := strings.TrimPrefix(authorization, "Bearer ")
	if len(secret) < 32 || len(secret) > 512 {
		return ctx, p, ErrDenied
	}
	h := sha256.Sum256([]byte(secret))
	e := s.DB.QueryRowContext(ctx, `SELECT c.credential_id,c.tenant_id,c.product_id,c.subject_id FROM operator_credentials c JOIN integration_credential_hashes h ON h.token_hash=c.token_sha256 AND h.kind='OPERATOR' AND h.principal_id=c.credential_id JOIN memory_installation i ON i.singleton=1 AND i.installation_id=h.installation_id LEFT JOIN studio_credential_state v USING(credential_id) JOIN tenant_gates t ON t.tenant_id=c.tenant_id WHERE c.token_sha256=? AND c.enabled=1 AND t.enabled=1 AND (v.expires_at IS NULL OR v.expires_at>UTC_TIMESTAMP(6))`, h[:]).Scan(&p.CredentialID, &p.TenantID, &p.ProductID, &p.SubjectID)
	if e != nil {
		return ctx, domain.OperatorPrincipal{}, ErrDenied
	}
	return context.WithValue(ctx, operatorContextKey{}, currentOperator{store: s, principal: p, bearerHash: h}), p, nil
}

func (s *Store) CurrentOperator(ctx context.Context) (domain.OperatorPrincipal, error) {
	p, ok := ctx.Value(operatorContextKey{}).(currentOperator)
	if !ok || p.store != s {
		return domain.OperatorPrincipal{}, ErrDenied
	}
	return p.principal, nil
}

// A store-bound context is only the starting identity: the transaction rechecks the current credential row and the applicable draft issuer proof.
func currentOperatorTx(ctx context.Context, q rowQuerier, s *Store, scope domain.ProductScope) (currentOperator, error) {
	p, ok := ctx.Value(operatorContextKey{}).(currentOperator)
	if !ok || p.store != s || p.principal.ProductScope != scope {
		return p, ErrDenied
	}
	var h []byte
	var enabled bool
	e := q.QueryRowContext(ctx, `SELECT c.token_sha256,c.enabled AND t.enabled AND (v.expires_at IS NULL OR v.expires_at>UTC_TIMESTAMP(6)) FROM operator_credentials c JOIN integration_credential_hashes h ON h.token_hash=c.token_sha256 AND h.kind='OPERATOR' AND h.principal_id=c.credential_id JOIN memory_installation i ON i.singleton=1 AND i.installation_id=h.installation_id JOIN tenant_gates t ON t.tenant_id=c.tenant_id LEFT JOIN studio_credential_state v USING(credential_id) WHERE c.credential_id=? AND c.tenant_id=? AND c.product_id=? AND c.subject_id=?`, p.principal.CredentialID, scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&h, &enabled)
	if e != nil || !enabled || subtle.ConstantTimeCompare(h, p.bearerHash[:]) != 1 {
		return p, ErrDenied
	}
	if e = checkDraftIssuerProof(ctx, q, p); e != nil {
		return p, e
	}
	return p, nil
}

// An explicit grant row overrides legacy registry actions even when disabled,
// expired or DENY. Legacy actions keep only their original product and subject.
func operatorActionTx(ctx context.Context, q rowQuerier, s *Store, scope domain.ProductScope, action string) error {
	p, e := currentOperatorTx(ctx, q, s, scope)
	if e != nil {
		return e
	}
	if e = operatorScopeTx(ctx, q, scope); e != nil {
		return e
	}
	if domain.ProductAdminAction(action) {
		var allowed bool
		e = q.QueryRowContext(ctx, "SELECT effect='ALLOW' AND enabled=1 AND (expires_at IS NULL OR expires_at>UTC_TIMESTAMP(6)) FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type='PRODUCT' AND scope_address=? AND product_id=? AND action=? AND allowed_use='PRODUCT_ADMIN'", scope.TenantID, p.principal.CredentialID, "P:"+scope.ProductID, scope.ProductID, action).Scan(&allowed)
		if e == nil {
			if allowed {
				return nil
			}
			return ErrDenied
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	// Existing self/memory actions are not delegated through StudioGrant. Their
	// separate owning memory-use and data-subject checks remain mandatory.
	var allowed bool
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_actions WHERE credential_id=? AND action=?)", p.principal.CredentialID, action).Scan(&allowed); e != nil {
		return e
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

func operatorScopeTx(ctx context.Context, q rowQuerier, scope domain.ProductScope) error {
	var gates bool
	if e := q.QueryRowContext(ctx, "SELECT p.enabled AND s.enabled FROM product_gates p JOIN subject_governance s USING(tenant_id,product_id) WHERE p.tenant_id=? AND p.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates); e != nil || !gates {
		return ErrDenied
	}
	return nil
}

func (s *Store) CheckOperator(ctx context.Context, scope domain.ProductScope, actions ...string) error {
	if len(actions) == 0 {
		return ErrDenied
	}
	for _, a := range actions {
		if e := operatorActionTx(ctx, s.DB, s, scope, a); e != nil {
			return e
		}
	}
	return nil
}

func CheckOperatorTx(ctx context.Context, tx Tx, scope domain.ProductScope, actions ...string) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore == nil || len(actions) == 0 {
		return ErrDenied
	}
	p, e := u.operatorStore.CurrentOperator(ctx)
	if e != nil {
		return e
	}
	for _, k := range operatorKeys(p) {
		if !u.plan[string(lockBytes(k))] {
			return fmt.Errorf("operator rank %d missing: %w", k.Rank, ErrReplan)
		}
	}
	for _, a := range actions {
		if e := operatorActionTx(ctx, u.tx, u.operatorStore, scope, a); e != nil {
			return e
		}
	}
	return nil
}
