package mysql

import (
	"context"
	"crypto/sha256"
	"loreviva.local/loreviva/internal/domain"
	"strings"
	"time"
)

type integrationContextKey struct{}
type currentIntegration struct {
	store     *Store
	principal domain.IntegrationPrincipal
	hash      [32]byte
}

// This mint accepts only actual current Service rows. Client minting additionally
// requires the Session/entitlement proof implemented by the later owning slice.
func (s *Store) AuthenticateIntegration(ctx context.Context, authorization string, kind domain.IntegrationKind) (context.Context, domain.IntegrationPrincipal, error) {
	var p domain.IntegrationPrincipal
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if (kind != domain.IntegrationService && kind != domain.IntegrationClient) || !strings.HasPrefix(authorization, "Bearer ") {
		return ctx, p, ErrDenied
	}
	secret := strings.TrimPrefix(authorization, "Bearer ")
	if len(secret) < 32 || len(secret) > 512 {
		return ctx, p, ErrDenied
	}
	h := sha256.Sum256([]byte(secret))
	if kind == domain.IntegrationClient {
		return s.authenticateClient(ctx, bounded, secret, h)
	}
	e := s.DB.QueryRowContext(bounded, `SELECT k.principal_id,k.kind,k.tenant_id,k.product_id,k.customer_id,k.subject_id,k.profile,k.expires_at FROM integration_principals k JOIN integration_credential_hashes h ON h.token_hash=k.token_hash AND h.principal_id=k.principal_id AND h.kind=k.kind JOIN memory_installation i ON i.singleton=1 AND i.installation_id=h.installation_id JOIN integration_customers c USING(tenant_id,product_id,customer_id) JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) JOIN subject_governance g ON g.tenant_id=k.tenant_id AND g.product_id=k.product_id AND g.subject_id=k.subject_id WHERE k.token_hash=? AND k.kind='SERVICE' AND k.revoked=0 AND k.expires_at>UTC_TIMESTAMP(6) AND c.enabled=1 AND c.expires_at>UTC_TIMESTAMP(6) AND c.service_subject_id=k.subject_id AND t.enabled=1 AND p.enabled=1 AND g.enabled=1`, h[:]).Scan(&p.PrincipalID, &p.Kind, &p.Tenant, &p.Product, &p.Customer, &p.StableSubject, &p.Profile, &p.ExpiresAt)
	if e != nil {
		return ctx, domain.IntegrationPrincipal{}, ErrDenied
	}
	return context.WithValue(ctx, integrationContextKey{}, currentIntegration{s, p, h}), p, nil
}
