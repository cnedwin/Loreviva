package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

const registrarOperation = "INSTALL_INTEGRATION_REGISTRAR_V1"

// A durable task issuer proves only its original obligation. Administrative
// registration requires a separately authenticated direct Operator context.
func (s *Store) registryDirectOperator(ctx context.Context) (domain.OperatorPrincipal, error) {
	v, ok := ctx.Value(operatorContextKey{}).(currentOperator)
	if !ok || v.store != s || v.job != "" {
		return domain.OperatorPrincipal{}, ErrDenied
	}
	return v.principal, nil
}

func registrarKey(p domain.OperatorPrincipal) LockKey {
	return LockKey{Rank: 25, Kind: "integration_registrar", IDs: []string{p.TenantID, p.CredentialID, p.ProductID}}
}
func registryKeys(p domain.OperatorPrincipal, key string) []LockKey {
	k := []LockKey{{Rank: 10, IDs: []string{p.TenantID}}, {Rank: 20, IDs: []string{p.TenantID, p.ProductID}}, {Rank: 30, IDs: []string{p.TenantID, p.ProductID, p.SubjectID}}, registrarKey(p), {Rank: 45, Kind: "integration_command", IDs: []string{p.TenantID, p.ProductID, p.CredentialID, key}}, {Rank: 110, Kind: "integration_receipt", IDs: []string{p.TenantID, p.ProductID, p.CredentialID, key}}}
	return append(k, operatorKeys(p)...)
}

// One original-key marker covers every registry operation for this owner.
// Operation remains part of the persisted receipt and canonical hash.
func readRegistryReceipt(ctx context.Context, q rowQuerier, p domain.OperatorPrincipal, key string) (domain.RegistryReceipt, error) {
	var raw []byte
	var operation, ref, state string
	var hash []byte
	var revision uint64
	var out domain.RegistryReceipt
	e := q.QueryRowContext(ctx, "SELECT result_metadata,operation,receipt_ref,payload_hash,target_revision,state FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND scope_kind='REGISTRY' AND scope_address=? AND original_key=?", p.TenantID, p.ProductID, p.CredentialID, p.ProductID, key).Scan(&raw, &operation, &ref, &hash, &revision, &state)
	if e != nil {
		return out, e
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var trailing any
	if decoder.Decode(&out) != nil || decoder.Decode(&trailing) != io.EOF || out.Key != key || state != "COMMITTED" || out.State != state || out.ReceiptRef != ref || ref == "" || out.Operation != operation || len(hash) != 32 || out.Hash != hex.EncodeToString(hash) || out.Revision != revision || revision == 0 || !validRegistryIDs(out.Target) || !registryReceiptShape(out) {
		return out, ErrDenied
	}
	return out, nil
}

// Original expiry is receipt data, never recomputed from current registration.
func registryReceiptShape(r domain.RegistryReceipt) bool {
	x := r.RegistrationExpiry
	validTime := func(v time.Time) bool { return !v.IsZero() && v.Nanosecond()%1000 == 0 }
	switch r.Operation {
	case "REGISTER_CUSTOMER":
		return x != nil && validTime(x.CustomerExpiresAt) && x.ServiceKeyExpiresAt != nil && x.ServiceKeyExpiresAt.Equal(x.CustomerExpiresAt) && x.MappingExpiresAt == nil && x.EntitlementExpiresAt == nil && r.CustomerID == ""
	case "REGISTER_PLAYER_ACCESS":
		return x != nil && validTime(x.CustomerExpiresAt) && x.ServiceKeyExpiresAt == nil && x.MappingExpiresAt != nil && x.EntitlementExpiresAt != nil && validTime(*x.MappingExpiresAt) && validTime(*x.EntitlementExpiresAt) && !x.EntitlementExpiresAt.After(*x.MappingExpiresAt) && !x.MappingExpiresAt.After(x.CustomerExpiresAt) && r.CustomerID == ""
	case "DISABLE_ENTITLEMENT", "DISABLE_PLAYER_MAPPING":
		return x == nil && validRegistryIDs(r.CustomerID)
	case registrarOperation, "ROTATE_SERVICE_KEY", "DISABLE_CUSTOMER", "REVOKE_CLIENT_SESSION", "DISABLE_REGISTRAR":
		return x == nil && r.CustomerID == ""
	default:
		return false
	}
}
func registryHash(value any) (string, error) {
	b, e := json.Marshal(value)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func writeRegistryReceipt(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, r domain.RegistryReceipt) error {
	if !registryReceiptShape(r) {
		return ErrDenied
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return e
	}
	h, e := hex.DecodeString(r.Hash)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO integration_receipts(tenant_id,product_id,owner_id,operation,scope_kind,scope_address,original_key,receipt_ref,payload_hash,result_metadata,state,target_revision,committed_at) VALUES(?,?,?,?,'REGISTRY',?,?,?,?,?,'COMMITTED',?,UTC_TIMESTAMP(6))", p.TenantID, p.ProductID, p.CredentialID, r.Operation, p.ProductID, r.Key, r.ReceiptRef, h, raw, r.Revision)
	return e
}
func (s *Store) commitRegistry(ctx context.Context, tx *sql.Tx) error {
	var e error
	if s.Commit != nil {
		e = s.Commit(ctx, tx)
	} else {
		e = tx.Commit()
	}
	if e != nil {
		return ErrCommitUnknown
	}
	return nil
}
func registrarCurrent(ctx context.Context, q rowQuerier, s *Store, p domain.OperatorPrincipal, id WorldAccountingIdentity) error {
	if _, e := s.registryDirectOperator(ctx); e != nil {
		return e
	}
	if _, e := currentOperatorTx(ctx, q, s, p.ProductScope); e != nil {
		return e
	}
	if e := operatorScopeTx(ctx, q, p.ProductScope); e != nil {
		return e
	}
	if e := checkConfigurationInstallationTx(ctx, q, id); e != nil {
		return e
	}
	var valid bool
	if e := q.QueryRowContext(ctx, "SELECT enabled=1 AND action='customer.register' AND allowed_use='TENANT_CUSTOMER_REGISTRATION' AND installation_id=? AND (expires_at IS NULL OR expires_at>UTC_TIMESTAMP(6)) FROM integration_registrars WHERE tenant_id=? AND credential_id=? AND product_id=?", id.Installation, p.TenantID, p.CredentialID, p.ProductID).Scan(&valid); e != nil || !valid {
		return ErrDenied
	}
	return nil
}
func (s *Store) InitializeIntegrationRegistrar(ctx context.Context, key, expectedInstallation string) (domain.RegistryReceipt, error) {
	var zero domain.RegistryReceipt
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if domain.ValidateIdempotencyKey(key) != nil || domain.ValidateID(expectedInstallation) != nil {
		return zero, ErrDenied
	}
	ctx, id, release, e := s.EnterConfiguration(ctx)
	if e != nil {
		return zero, e
	}
	defer release()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return zero, e
	}
	defer done()
	p, e := s.registryDirectOperator(ctx)
	if e != nil || p.CredentialID != "owner" || p.TenantID != "demo-local" || p.ProductID != "coffee" || p.SubjectID != "creator" || id.Installation != expectedInstallation {
		return zero, ErrDenied
	}
	tenant := domain.SettingsScope{Kind: "TENANT", TenantID: p.TenantID}
	keys := append(registryKeys(p, key), studioGrantKey(p, tenant))
	tx, e := s.studioLocalTx(ctx, keys)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback()
	authorize := func() error {
		if e := studioActionTx(ctx, tx, s, tenant, "maintenance.manage", "MAINTENANCE_COMMAND"); e != nil {
			return e
		}
		if e := operatorScopeTx(ctx, tx, p.ProductScope); e != nil {
			return e
		}
		if e := checkConfigurationInstallationTx(ctx, tx, id); e != nil {
			return e
		}
		var original bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM r0_generation_consent WHERE tenant_id=? AND product_id=? AND subject_id=? AND credential_id=? AND policy_id='R0_OWNER_PRIVATE_GENERATION_30D')", p.TenantID, p.ProductID, p.SubjectID, p.CredentialID).Scan(&original); e != nil || !original {
			return ErrDenied
		}
		return nil
	}
	if e = authorize(); e != nil {
		return zero, e
	}
	hash, _ := registryHash(struct{ Operation, Installation string }{registrarOperation, expectedInstallation})
	old, e := readRegistryReceipt(ctx, tx, p, key)
	if e == nil {
		if old.Hash != hash || old.Operation != registrarOperation {
			return zero, ErrConflict
		}
		return old, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return zero, e
	}
	var exists bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM integration_registrars WHERE tenant_id=? AND credential_id=? AND product_id=?)", p.TenantID, p.CredentialID, p.ProductID).Scan(&exists); e != nil {
		return zero, e
	}
	if exists {
		return zero, ErrConflict
	}
	r := domain.RegistryReceipt{Operation: registrarOperation, Key: key, Hash: hash, Target: p.CredentialID, Revision: 1, ReceiptRef: integrationID(p.TenantID, p.ProductID, p.CredentialID, "integration", key), State: "COMMITTED"}
	_, e = tx.ExecContext(ctx, "INSERT INTO integration_registrars(tenant_id,credential_id,product_id,action,allowed_use,expires_at,enabled,revision,bootstrap_receipt,installation_id) VALUES(?,?,?,'customer.register','TENANT_CUSTOMER_REGISTRATION',NULL,1,1,?,?)", p.TenantID, p.CredentialID, p.ProductID, r.ReceiptRef, id.Installation)
	if e != nil {
		return zero, e
	}
	if e = writeRegistryReceipt(ctx, tx, p, r); e != nil {
		return zero, e
	}
	if e = s.finishStudioLocal(ctx, tx, p); e != nil {
		return zero, e
	}
	if e = authorize(); e != nil {
		return zero, e
	}
	if e = s.commitRegistry(ctx, tx); e != nil {
		return zero, e
	}
	return r, nil
}

func integrationID(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return "integration-" + hex.EncodeToString(h[:16])
}
