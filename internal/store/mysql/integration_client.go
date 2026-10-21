package mysql

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

// A CLIENT proof stays private to its issuing Store. Every current use reads
// the original issuer and qualified registration; metadata never grants access.
func (s *Store) currentClient(ctx context.Context, q creationRows, v currentIntegration) (sessionRecord, sessionEligibility, error) {
	return s.currentClientForTask(ctx, q, v, false)
}

// Only the private accepted-task path may disregard natural Session expiry.
// Explicit revocation and every persisted issuance/registration field stay current.
func (s *Store) currentClientForTask(ctx context.Context, q creationRows, v currentIntegration, acceptedTask bool) (sessionRecord, sessionEligibility, error) {
	var r sessionRecord
	var eligibility sessionEligibility
	actual, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || v.store != s || actual.store != s || !reflect.DeepEqual(actual, v) || v.principal.Kind != domain.IntegrationClient || ctx.Value(operatorContextKey{}) != nil || s.integrationClosed.Load() || s.Guard == nil {
		return r, eligibility, ErrDenied
	}
	var issuer, key string
	e := q.QueryRowContext(ctx, `SELECT k.issuer_service_key,x.original_key FROM integration_principals k JOIN integration_receipts x ON x.receipt_ref=k.issuance_receipt AND x.tenant_id=k.tenant_id AND x.product_id=k.product_id AND x.owner_id=k.issuer_service_key AND x.operation='EXCHANGE_SESSION' AND x.scope_kind='SESSION' AND x.scope_address=k.product_id WHERE k.principal_id=? AND k.kind='CLIENT'`, v.principal.PrincipalID).Scan(&issuer, &key)
	if e != nil {
		return r, eligibility, ErrDenied
	}
	var service domain.IntegrationPrincipal
	var hash []byte
	e = q.QueryRowContext(ctx, `SELECT principal_id,kind,tenant_id,product_id,customer_id,subject_id,profile,expires_at,token_hash FROM integration_principals WHERE principal_id=? AND kind='SERVICE'`, issuer).Scan(&service.PrincipalID, &service.Kind, &service.Tenant, &service.Product, &service.Customer, &service.StableSubject, &service.Profile, &service.ExpiresAt, &hash)
	if e != nil || len(hash) != 32 {
		return r, eligibility, ErrDenied
	}
	serviceProof := currentIntegration{store: s, principal: service}
	copy(serviceProof.hash[:], hash)
	r, e = sessionRead(ctx, q, service, key)
	if e != nil {
		return r, eligibility, e
	}
	if r.ID != v.principal.PrincipalID || r.Tenant != v.principal.Tenant || r.Product != v.principal.Product || r.Customer != v.principal.Customer || r.Player != v.principal.StableSubject || r.World != v.principal.World || r.Profile != v.principal.Profile || !r.Expires.Equal(v.principal.ExpiresAt) {
		return r, eligibility, ErrDenied
	}
	if e = s.currentSessionRowForTask(ctx, q, r, v.hash, acceptedTask); e != nil {
		return r, eligibility, e
	}
	// This temporary private proof is derived exclusively from the original
	// CLIENT issuance row. It is never returned as a SERVICE context.
	serviceCtx := context.WithValue(ctx, integrationContextKey{}, serviceProof)
	eligibility, e = s.sessionEligibility(serviceCtx, q, serviceProof, sessionRequest(r))
	if e != nil {
		return r, eligibility, e
	}
	if r.EntitlementRevision != eligibility.entitlement.Revision || r.MappingRevision != eligibility.mapping.MappingRevision || r.CustomerRevision != eligibility.mapping.CustomerRevision || r.Installation != eligibility.identity.Installation {
		return r, eligibility, ErrDenied
	}
	var actions []string
	var raw []byte
	if e = q.QueryRowContext(ctx, "SELECT client_actions FROM integration_principals WHERE principal_id=?", r.ID).Scan(&raw); e != nil || json.Unmarshal(raw, &actions) != nil || !reflect.DeepEqual(actions, r.Actions) {
		return r, eligibility, ErrDenied
	}
	return r, eligibility, nil
}

func (s *Store) authenticateClient(original, ctx context.Context, token string, hash [32]byte) (context.Context, domain.IntegrationPrincipal, error) {
	var p domain.IntegrationPrincipal
	if s.integrationSessionRoot == nil || s.integrationClosed.Load() || s.Guard == nil {
		return original, p, ErrDenied
	}
	e := s.DB.QueryRowContext(ctx, `SELECT principal_id,kind,tenant_id,product_id,customer_id,subject_id,world_instance_id,profile,expires_at FROM integration_principals WHERE token_hash=? AND kind='CLIENT' AND revoked=0 AND expires_at>UTC_TIMESTAMP(6)`, hash[:]).Scan(&p.PrincipalID, &p.Kind, &p.Tenant, &p.Product, &p.Customer, &p.StableSubject, &p.World, &p.Profile, &p.ExpiresAt)
	if e != nil {
		return original, domain.IntegrationPrincipal{}, ErrDenied
	}
	v := currentIntegration{s, p, hash}
	proofCtx := context.WithValue(ctx, integrationContextKey{}, v)
	_, id, release, e := s.EnterConfiguration(proofCtx)
	if e != nil {
		return original, domain.IntegrationPrincipal{}, e
	}
	defer release()
	r, eligible, e := s.currentClient(proofCtx, s.DB, v)
	if e != nil || eligible.identity.Installation != id.Installation || eligible.identity.Generation != id.Generation {
		return original, domain.IntegrationPrincipal{}, ErrDenied
	}
	tx, e := s.integrationMemoryReadTx(proofCtx, consentKeys(r, eligible, ""))
	if e != nil {
		return original, domain.IntegrationPrincipal{}, e
	}
	defer tx.Rollback()
	locked, lockedEligibility, e := s.currentClient(proofCtx, tx, v)
	if e != nil || !reflect.DeepEqual(locked, r) || !reflect.DeepEqual(lockedEligibility, eligible) {
		return original, domain.IntegrationPrincipal{}, ErrDenied
	}
	material, e := s.integrationSessionRoot.SessionHMACKey(id.Installation, false)
	if e != nil {
		return original, domain.IntegrationPrincipal{}, ErrDenied
	}
	kh := sha256.Sum256(append([]byte(id.Installation+"/1/"), material[:]...))
	if r.KeyRef != hex.EncodeToString(kh[:]) || !hmac.Equal([]byte(token), []byte(sessionToken(material, r))) {
		return original, domain.IntegrationPrincipal{}, ErrDenied
	}
	return context.WithValue(original, integrationContextKey{}, v), p, nil
}
