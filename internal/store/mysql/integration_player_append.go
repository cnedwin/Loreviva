package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
	"time"
)

// The original mapping is immutable authority evidence, not a request to mint
// another subject or renew any self permission. Snapshot changes cause replan.
type registeredPlayerSnapshot struct {
	External                                                     string
	Player                                                       string
	MappingExpiry, CustomerExpiry                                time.Time
	MappingRevision, CustomerRevision, Permission, Privacy, View uint64
	MappingReceipt, CustomerReceipt                              string
	MappingOwner, MappingKey, CustomerOwner, CustomerKey         string
	Permissions                                                  map[string]time.Time
	OriginalCustomer                                             domain.RegisterCustomer
	OriginalPlayer                                               domain.RegisterPlayerAccess
	OriginalPrincipal                                            string
	OriginalSecretHash                                           [32]byte
}

func (v *registeredPlayerSnapshot) receiptKeys(p domain.OperatorPrincipal) []LockKey {
	return []LockKey{{Rank: 15, Kind: "integration_principal", IDs: []string{v.OriginalPrincipal}}, {Rank: 25, Kind: "integration_entitlement", IDs: []string{p.TenantID, p.ProductID, v.OriginalPlayer.CustomerID, v.OriginalPlayer.EntitlementID}}, {Rank: 110, Kind: "integration_receipt", IDs: []string{p.TenantID, p.ProductID, v.MappingOwner, v.MappingKey}}, {Rank: 110, Kind: "integration_receipt", IDs: []string{p.TenantID, p.ProductID, v.CustomerOwner, v.CustomerKey}}}
}

// Historical issuance proves the immutable registration tuple, not current
// eligibility. Consuming/disabling its grant or rotating its key must not erase
// that evidence. Current customer/mapping/subject/new-grant gates remain separate.
func (a *registeredPlayerSnapshot) originalRegistration(ctx context.Context, q rowQuerier, p domain.OperatorPrincipal, customer string, mr, cr domain.RegistryReceipt) error {
	var actions string
	var count int
	v := domain.RegisterPlayerAccess{CustomerID: customer, ExternalIDBytes: []byte(a.External)}
	e := q.QueryRowContext(ctx, `SELECT entitlement_id,player_subject_id,release_id,CAST(allowed_actions AS CHAR),expires_at,(SELECT COUNT(*) FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND registry_receipt=?) FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND registry_receipt=?`, p.TenantID, p.ProductID, customer, a.MappingReceipt, p.TenantID, p.ProductID, customer, a.MappingReceipt).Scan(&v.EntitlementID, &v.PlayerSubjectID, &v.ReleaseID, &actions, &v.ExpiresAt, &count)
	if e != nil || count != 1 || json.Unmarshal([]byte(actions), &v.Actions) != nil || v.PlayerSubjectID != a.Player || mr.Revision != 1 || mr.RegistrationExpiry.EntitlementExpiresAt == nil || !v.ExpiresAt.Equal(a.MappingExpiry) || !v.ExpiresAt.Equal(*mr.RegistrationExpiry.EntitlementExpiresAt) || !mr.RegistrationExpiry.CustomerExpiresAt.Equal(a.CustomerExpiry) {
		return ErrDenied
	}
	c := domain.RegistryCommand{Kind: "REGISTER_PLAYER_ACCESS", ProductID: p.ProductID, TargetID: a.Player, Player: &v}
	if !registryHistoricalRegistrationMatches(c, [32]byte{}, mr.Hash) {
		return ErrDenied
	}
	a.OriginalPlayer = v
	vCustomer := domain.RegisterCustomer{CustomerID: customer}
	var account string
	e = q.QueryRowContext(ctx, "SELECT service_subject_id,consumer_id,account_id,expires_at FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=?", p.TenantID, p.ProductID, customer).Scan(&vCustomer.ServiceSubjectID, &vCustomer.ConsumerID, &account, &vCustomer.ExpiresAt)
	if e != nil || cr.Revision != 1 || account != integrationID(p.TenantID, p.ProductID, customer, "account") || !vCustomer.ExpiresAt.Equal(a.CustomerExpiry) {
		return ErrDenied
	}
	a.OriginalPrincipal = integrationID(p.TenantID, p.ProductID, customer, "service", cr.Key)
	var hash []byte
	var exact bool
	e = q.QueryRowContext(ctx, `SELECT token_hash,(kind='SERVICE' AND tenant_id=? AND product_id=? AND customer_id=? AND subject_id=? AND issuance_receipt=? AND expires_at=?) FROM integration_principals WHERE principal_id=?`, p.TenantID, p.ProductID, customer, vCustomer.ServiceSubjectID, a.CustomerReceipt, vCustomer.ExpiresAt, a.OriginalPrincipal).Scan(&hash, &exact)
	if e != nil || !exact || len(hash) != 32 {
		return ErrDenied
	}
	copy(a.OriginalSecretHash[:], hash)
	c = domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: p.ProductID, TargetID: customer, Customer: &vCustomer}
	if !registryHistoricalRegistrationMatches(c, a.OriginalSecretHash, cr.Hash) {
		return ErrDenied
	}
	a.OriginalCustomer = vCustomer
	return nil
}
func registeredReceipt(ctx context.Context, q rowQuerier, p domain.OperatorPrincipal, ref, kind, target string) (domain.RegistryReceipt, string, string, error) {
	var owner, key string
	if e := q.QueryRowContext(ctx, "SELECT owner_id,original_key FROM integration_receipts WHERE tenant_id=? AND product_id=? AND scope_kind='REGISTRY' AND scope_address=? AND receipt_ref=?", p.TenantID, p.ProductID, p.ProductID, ref).Scan(&owner, &key); e != nil {
		return domain.RegistryReceipt{}, "", "", ErrDenied
	}
	original := p
	original.CredentialID = owner
	r, e := readRegistryReceipt(ctx, q, original, key)
	if e != nil || r.ReceiptRef != ref || r.Operation != kind || r.Target != target {
		return r, "", "", ErrDenied
	}
	return r, owner, key, nil
}
func readRegisteredPlayer(ctx context.Context, q rowQuerier, p domain.OperatorPrincipal, v domain.RegisterPlayerAccess) (*registeredPlayerSnapshot, error) {
	a := &registeredPlayerSnapshot{Permissions: map[string]time.Time{}}
	var live bool
	e := q.QueryRowContext(ctx, "SELECT external_id,player_subject_id,expires_at,revision,registry_receipt,enabled=1 AND expires_at>UTC_TIMESTAMP(6) FROM integration_external_subjects WHERE tenant_id=? AND product_id=? AND customer_id=? AND external_id=?", p.TenantID, p.ProductID, v.CustomerID, v.ExternalIDBytes).Scan(&a.External, &a.Player, &a.MappingExpiry, &a.MappingRevision, &a.MappingReceipt, &live)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if !live || a.Player != v.PlayerSubjectID || a.External != string(v.ExternalIDBytes) || v.ExpiresAt.After(a.MappingExpiry) {
		return nil, ErrDenied
	}
	e = q.QueryRowContext(ctx, "SELECT expires_at,revision,registry_receipt,enabled=1 AND expires_at>UTC_TIMESTAMP(6) FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=?", p.TenantID, p.ProductID, v.CustomerID).Scan(&a.CustomerExpiry, &a.CustomerRevision, &a.CustomerReceipt, &live)
	if e != nil || !live || a.MappingExpiry.After(a.CustomerExpiry) || v.ExpiresAt.After(a.CustomerExpiry) {
		return nil, ErrDenied
	}
	e = q.QueryRowContext(ctx, "SELECT permission_version,privacy_epoch,view_epoch,enabled FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?", p.TenantID, p.ProductID, v.PlayerSubjectID).Scan(&a.Permission, &a.Privacy, &a.View, &live)
	if e != nil || !live {
		return nil, ErrDenied
	}
	mr, mo, mk, e := registeredReceipt(ctx, q, p, a.MappingReceipt, "REGISTER_PLAYER_ACCESS", v.PlayerSubjectID)
	if e != nil || mr.RegistrationExpiry == nil || !mr.RegistrationExpiry.MappingExpiresAt.Equal(a.MappingExpiry) {
		return nil, ErrDenied
	}
	cr, co, ck, e := registeredReceipt(ctx, q, p, a.CustomerReceipt, "REGISTER_CUSTOMER", v.CustomerID)
	if e != nil || cr.RegistrationExpiry == nil || !cr.RegistrationExpiry.CustomerExpiresAt.Equal(a.CustomerExpiry) {
		return nil, ErrDenied
	}
	a.MappingOwner, a.MappingKey, a.CustomerOwner, a.CustomerKey = mo, mk, co, ck
	if e = a.originalRegistration(ctx, q, p, v.CustomerID, mr, cr); e != nil {
		return nil, e
	}
	for _, action := range v.Actions {
		var permissions []string
		if action == "MEMORY_VIEW" {
			permissions = []string{"memory.metadata.read", "memory.private_body.read"}
		}
		if action == "MEMORY_GOVERN" {
			permissions = []string{"memory.govern", "memory.correct"}
		}
		for _, permission := range permissions {
			var expiry time.Time
			e = q.QueryRowContext(ctx, "SELECT expires_at,enabled=1 AND expires_at>UTC_TIMESTAMP(6) FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action=? AND allowed_use='PRIVATE_RESPONSE'", p.TenantID, p.ProductID, v.PlayerSubjectID, v.PlayerSubjectID, permission).Scan(&expiry, &live)
			if e != nil || !live || v.ExpiresAt.After(expiry) {
				return nil, ErrDenied
			}
			a.Permissions[permission] = expiry
		}
	}
	return a, nil
}
func checkRegisteredPlayer(ctx context.Context, q rowQuerier, p domain.OperatorPrincipal, plan registryPlan) error {
	if plan.playerAppend == nil || plan.command.Player == nil {
		return ErrDenied
	}
	a, e := readRegisteredPlayer(ctx, q, p, *plan.command.Player)
	if e != nil {
		return e
	}
	if a == nil || !reflect.DeepEqual(a, plan.playerAppend) {
		return ErrReplan
	}
	var current bool
	e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM releases WHERE tenant_id=? AND product_id=? AND release_id=? AND available=1) AND ? > UTC_TIMESTAMP(6)", p.TenantID, p.ProductID, plan.command.Player.ReleaseID, plan.command.Player.ExpiresAt).Scan(&current)
	if e != nil {
		return e
	}
	if !current {
		return ErrDenied
	}
	return nil
}
