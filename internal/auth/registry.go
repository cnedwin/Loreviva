package auth

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
)

// RegistryReady checks usable registered authority rather than a configured
// bootstrap identity. Individual requests still require their exact action.
func (o *Operator) RegistryReady(ctx context.Context) error {
	if o == nil {
		return ErrUnauthenticated
	}
	var ready bool
	e := o.store.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operator_credentials c JOIN integration_credential_hashes h ON h.token_hash=c.token_sha256 AND h.kind='OPERATOR' AND h.principal_id=c.credential_id JOIN memory_installation i ON i.singleton=1 AND i.installation_id=h.installation_id JOIN tenant_gates t ON t.tenant_id=c.tenant_id LEFT JOIN studio_credential_state v USING(credential_id) WHERE c.enabled=1 AND t.enabled=1 AND (v.expires_at IS NULL OR v.expires_at>UTC_TIMESTAMP(6)))`).Scan(&ready)
	if e != nil || !ready {
		return ErrUnauthenticated
	}
	return nil
}

func (o *Operator) Authenticate(ctx context.Context, bearer string) (context.Context, domain.OperatorPrincipal, error) {
	if o == nil {
		return ctx, domain.OperatorPrincipal{}, ErrUnauthenticated
	}
	next, p, e := o.store.AuthenticateOperator(ctx, bearer)
	if e != nil {
		return ctx, domain.OperatorPrincipal{}, ErrUnauthenticated
	}
	return next, p, nil
}

// Request-local copy prevents a second credential from changing shared runtime
// auth configuration. The owning context keeps its private verified principal.
func (o *Operator) committedReadOperator(ctx context.Context, bearer, product string) (context.Context, *Operator, error) {
	next, p, e := o.Authenticate(ctx, bearer)
	if e != nil {
		return ctx, nil, e
	}
	if product != p.ProductID {
		return ctx, nil, ErrNotFound
	}
	var gates bool
	e = o.store.DB.QueryRowContext(next, "SELECT p.enabled AND s.enabled FROM product_gates p JOIN subject_governance s USING(tenant_id,product_id) WHERE p.tenant_id=? AND p.product_id=? AND s.subject_id=?", p.TenantID, p.ProductID, p.SubjectID).Scan(&gates)
	if e != nil || !gates {
		return ctx, nil, ErrUnauthenticated
	}
	copy := *o
	copy.scope = p.ProductScope
	copy.credentialID = p.CredentialID
	return next, &copy, nil
}
