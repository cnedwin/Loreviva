package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"os"
	"path/filepath"
	"time"
)

type registryPlan struct {
	playerAppend       *registeredPlayerSnapshot
	command            domain.RegistryCommand
	hash, legacyHash   string
	secretHash         [32]byte
	principal, account string
	keys               []LockKey
	oldHash            [32]byte
	oldSubject         string
	oldExternal        []byte
	oldCustomer        string
	replayOnly         bool
}

// This is a writer-local expectation, never reconstructed from SQL after a
// fault hook. Existing-subject append and original-key replay do not mint it.
type registrySubjectExpected struct {
	tenant, product, subject string
	canGovern bool
}

func checkRegistrySubjectWritten(ctx context.Context, tx *sql.Tx, expected *registrySubjectExpected) error {
	if expected == nil { return nil }
	var permission, privacy, view uint64
	var enabled, control, migrate, govern bool
	e := tx.QueryRowContext(ctx, "SELECT permission_version,privacy_epoch,view_epoch,enabled,can_control,can_migrate,can_govern FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?", expected.tenant, expected.product, expected.subject).Scan(&permission, &privacy, &view, &enabled, &control, &migrate, &govern)
	if e != nil || permission != 1 || privacy != 1 || view != 1 || !enabled || control || migrate || govern != expected.canGovern { return ErrDenied }
	return nil
}

func cloneRegistryCommand(c domain.RegistryCommand) domain.RegistryCommand {
	if c.Customer != nil {
		v := *c.Customer
		c.Customer = &v
	}
	if c.Rotation != nil {
		v := *c.Rotation
		c.Rotation = &v
	}
	if c.QualifiedTarget != nil {
		v := *c.QualifiedTarget
		c.QualifiedTarget = &v
	}
	if c.Player != nil {
		v := *c.Player
		v.ExternalIDBytes = append([]byte(nil), v.ExternalIDBytes...)
		v.Actions = append([]string(nil), v.Actions...)
		c.Player = &v
	}
	return c
}

func registryRegistration(kind string) bool {
	return kind == "REGISTER_CUSTOMER" || kind == "REGISTER_PLAYER_ACCESS"
}

func registryUTCCommand(raw domain.RegistryCommand) domain.RegistryCommand {
	c := cloneRegistryCommand(raw)
	if c.Kind == "REGISTER_CUSTOMER" && c.Customer != nil {
		c.Customer.ExpiresAt = c.Customer.ExpiresAt.UTC()
	}
	if c.Kind == "REGISTER_PLAYER_ACCESS" && c.Player != nil {
		c.Player.ExpiresAt = c.Player.ExpiresAt.UTC()
	}
	return c
}

// Replay retains the exact caller's V1 representation. The UTC candidate has
// a separate domain; it must never become a bare V1(UTC) replay fallback.
func registryCommandHashes(raw domain.RegistryCommand, secret [32]byte) (current, legacy string, err error) {
	legacy, err = registryHash(struct {
		Command    domain.RegistryCommand
		SecretHash [32]byte
	}{raw, secret})
	if err != nil || !registryRegistration(raw.Kind) {
		return legacy, legacy, err
	}
	current, err = registryHash(struct {
		Domain     string
		Command    domain.RegistryCommand
		SecretHash [32]byte
	}{
		"loreviva.registry.registration.utc.v2", registryUTCCommand(raw), secret,
	})
	return current, legacy, err
}

// Historical reconstruction knows only persisted UTC fields. This helper is
// not the replay matcher: unavailable original offsets are never enumerated.
func registryHistoricalRegistrationMatches(c domain.RegistryCommand, secret [32]byte, original string) bool {
	if !registryRegistration(c.Kind) {
		return false
	}
	current, legacy, err := registryCommandHashes(registryUTCCommand(c), secret)
	return err == nil && (original == current || original == legacy)
}

func validRegistryIDs(ids ...string) bool {
	for _, id := range ids {
		if domain.ValidateID(id) != nil {
			return false
		}
	}
	return true
}
func validateRegistryCommand(c domain.RegistryCommand) error {
	if !validRegistryIDs(c.ProductID, c.TargetID) {
		return ErrDenied
	}
	count := 0
	for _, present := range []bool{c.Customer != nil, c.Player != nil, c.Rotation != nil, c.QualifiedTarget != nil} {
		if present {
			count++
		}
	}
	switch c.Kind {
	case "REGISTER_CUSTOMER":
		if count != 1 || c.Customer == nil || c.ExpectedRevision != 0 {
			return ErrDenied
		}
		v := c.Customer
		if c.TargetID != v.CustomerID || !validRegistryIDs(v.CustomerID, v.ServiceSubjectID, v.ConsumerID) || v.SecretHandleRef == "" || v.ExpiresAt.IsZero() {
			return ErrDenied
		}
	case "REGISTER_PLAYER_ACCESS":
		if count != 1 || c.Player == nil || c.ExpectedRevision != 0 {
			return ErrDenied
		}
		v := c.Player
		if c.TargetID != v.PlayerSubjectID || !validRegistryIDs(v.CustomerID, v.PlayerSubjectID, v.EntitlementID, v.ReleaseID) || domain.ValidateExternalIdentity(string(v.ExternalIDBytes)) != nil || len(v.ExternalIDBytes) > 512 || v.ExpiresAt.IsZero() || len(v.Actions) < 1 || len(v.Actions) > 6 {
			return ErrDenied
		}
		seen := map[string]bool{}
		for _, a := range v.Actions {
			switch a {
			case "READ", "TURN", "CONTROL_PAUSE", "CONTROL_RESUME", "MEMORY_VIEW", "MEMORY_GOVERN":
			default:
				return ErrDenied
			}
			if seen[a] {
				return ErrDenied
			}
			seen[a] = true
		}
	case "ROTATE_SERVICE_KEY":
		if count != 1 || c.Rotation == nil || c.ExpectedRevision == 0 {
			return ErrDenied
		}
		v := c.Rotation
		if c.TargetID != v.OldPrincipalID || v.NewPrincipalID == v.OldPrincipalID || !validRegistryIDs(v.CustomerID, v.OldPrincipalID, v.NewPrincipalID) || v.SecretHandleRef == "" {
			return ErrDenied
		}
	case "DISABLE_ENTITLEMENT", "DISABLE_PLAYER_MAPPING":
		if count != 1 || c.QualifiedTarget == nil || c.ExpectedRevision == 0 || !validRegistryIDs(c.QualifiedTarget.CustomerID) {
			return ErrDenied
		}
	case "DISABLE_CUSTOMER", "DISABLE_REGISTRAR", "REVOKE_CLIENT_SESSION":
		if count != 0 || c.ExpectedRevision == 0 {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	return nil
}

// File I/O is completed under CONFIG admission, before any SQL transaction.
// The canonical request contains only its actual hash, never a path or secret.
func registrySecretHash(ref string) ([32]byte, error) {
	var zero [32]byte
	if !filepath.IsAbs(ref) {
		return zero, ErrDenied
	}
	info, e := os.Lstat(ref)
	if e != nil || !info.Mode().IsRegular() || info.Size() < 32 || info.Size() > 512 {
		return zero, ErrDenied
	}
	f, e := os.Open(ref)
	if e != nil {
		return zero, ErrDenied
	}
	opened, e := f.Stat()
	if e != nil || !os.SameFile(info, opened) {
		f.Close()
		return zero, ErrDenied
	}
	raw, e := io.ReadAll(io.LimitReader(f, 513))
	closed := f.Close()
	if e != nil || closed != nil || len(raw) < 32 || len(raw) > 512 {
		return zero, ErrDenied
	}
	h := sha256.Sum256(raw)
	clear(raw)
	return h, nil
}
func (s *Store) prepareRegistry(ctx context.Context, p domain.OperatorPrincipal, key string, c domain.RegistryCommand) (registryPlan, error) {
	out := registryPlan{command: c, keys: registryKeys(p, key)}
	if e := validateRegistryCommand(c); e != nil {
		return out, e
	}
	if c.ProductID != p.ProductID {
		return out, ErrDenied
	}
	var e error
	ref := ""
	if c.Customer != nil {
		ref = c.Customer.SecretHandleRef
	}
	if c.Rotation != nil {
		ref = c.Rotation.SecretHandleRef
	}
	if ref != "" {
		out.secretHash, e = registrySecretHash(ref)
		if e != nil {
			return out, e
		}
	}
	out.hash, out.legacyHash, e = registryCommandHashes(c, out.secretHash)
	if e != nil {
		return out, e
	}
	out.command = registryUTCCommand(c)
	c = out.command
	// A pre-read only chooses the receipt path. The owning transaction rechecks
	// authority, operation and canonical hash; this metadata grants no rights.
	if e = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND scope_kind='REGISTRY' AND scope_address=? AND original_key=?)", p.TenantID, p.ProductID, p.CredentialID, p.ProductID, key).Scan(&out.replayOnly); e != nil {
		return out, e
	}
	if out.replayOnly {
		return out, nil
	}
	if c.Customer != nil {
		v := c.Customer
		out.principal = integrationID(p.TenantID, p.ProductID, v.CustomerID, "service", key)
		out.account = integrationID(p.TenantID, p.ProductID, v.CustomerID, "account")
		out.keys = append(out.keys, credentialHashKey(out.secretHash), LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{out.principal}}, LockKey{Rank: 30, IDs: []string{p.TenantID, p.ProductID, v.ServiceSubjectID}}, LockKey{Rank: 35, Kind: "integration_customer", IDs: []string{p.TenantID, p.ProductID, v.CustomerID}}, LockKey{Rank: 70, Kind: "customer", IDs: []string{p.TenantID + "." + p.ProductID, out.account}})
	}
	if c.Player != nil {
		v := c.Player
		out.playerAppend, e = readRegisteredPlayer(ctx, s.DB, p, *v)
		if e != nil {
			return out, e
		}
		if out.playerAppend != nil {
			out.keys = append(out.keys, out.playerAppend.receiptKeys(p)...)
		}
		h := sha256.Sum256(v.ExternalIDBytes)
		out.keys = append(out.keys, LockKey{Rank: 25, Kind: "integration_entitlement", IDs: []string{p.TenantID, p.ProductID, v.CustomerID, v.EntitlementID}}, LockKey{Rank: 30, IDs: []string{p.TenantID, p.ProductID, v.PlayerSubjectID}}, LockKey{Rank: 35, Kind: "integration_customer", IDs: []string{p.TenantID, p.ProductID, v.CustomerID}}, LockKey{Rank: 35, Kind: "integration_mapping", IDs: []string{p.TenantID, p.ProductID, v.CustomerID, hex.EncodeToString(h[:])}}, LockKey{Rank: 90, Kind: "content_release", IDs: []string{p.TenantID, p.ProductID, v.ReleaseID}})
	}
	if c.Rotation != nil {
		v := c.Rotation
		var hash []byte
		e = s.DB.QueryRowContext(ctx, "SELECT token_hash,subject_id FROM integration_principals WHERE tenant_id=? AND product_id=? AND customer_id=? AND principal_id=? AND kind='SERVICE'", p.TenantID, p.ProductID, v.CustomerID, v.OldPrincipalID).Scan(&hash, &out.oldSubject)
		if e != nil || len(hash) != 32 {
			return out, ErrDenied
		}
		copy(out.oldHash[:], hash)
		out.keys = append(out.keys, credentialHashKey(out.oldHash), credentialHashKey(out.secretHash), LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{v.OldPrincipalID}}, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{v.NewPrincipalID}}, LockKey{Rank: 30, IDs: []string{p.TenantID, p.ProductID, out.oldSubject}}, LockKey{Rank: 35, Kind: "integration_customer", IDs: []string{p.TenantID, p.ProductID, v.CustomerID}})
	}
	if c.Kind == "REVOKE_CLIENT_SESSION" {
		var hash []byte
		e = s.DB.QueryRowContext(ctx, "SELECT token_hash,subject_id,customer_id FROM integration_principals WHERE tenant_id=? AND product_id=? AND principal_id=? AND kind='CLIENT'", p.TenantID, p.ProductID, c.TargetID).Scan(&hash, &out.oldSubject, &out.oldCustomer)
		if e != nil || len(hash) != 32 {
			return out, ErrDenied
		}
		copy(out.oldHash[:], hash)
		out.keys = append(out.keys, credentialHashKey(out.oldHash), LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{c.TargetID}}, LockKey{Rank: 30, IDs: []string{p.TenantID, p.ProductID, out.oldSubject}}, LockKey{Rank: 35, Kind: "integration_customer", IDs: []string{p.TenantID, p.ProductID, out.oldCustomer}})
	}
	if c.QualifiedTarget != nil {
		customer := c.QualifiedTarget.CustomerID
		if c.Kind == "DISABLE_PLAYER_MAPPING" {
			e = s.DB.QueryRowContext(ctx, "SELECT external_id,player_subject_id FROM integration_external_subjects WHERE tenant_id=? AND product_id=? AND customer_id=? AND player_subject_id=?", p.TenantID, p.ProductID, customer, c.TargetID).Scan(&out.oldExternal, &out.oldSubject)
			if e != nil {
				return out, ErrDenied
			}
			h := sha256.Sum256(out.oldExternal)
			out.keys = append(out.keys, LockKey{Rank: 35, Kind: "integration_mapping", IDs: []string{p.TenantID, p.ProductID, customer, hex.EncodeToString(h[:])}})
		} else {
			e = s.DB.QueryRowContext(ctx, "SELECT player_subject_id FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=?", p.TenantID, p.ProductID, customer, c.TargetID).Scan(&out.oldSubject)
			if e != nil {
				return out, ErrDenied
			}
			out.keys = append(out.keys, LockKey{Rank: 25, Kind: "integration_entitlement", IDs: []string{p.TenantID, p.ProductID, customer, c.TargetID}})
		}
		out.keys = append(out.keys, LockKey{Rank: 30, IDs: []string{p.TenantID, p.ProductID, out.oldSubject}}, LockKey{Rank: 35, Kind: "integration_customer", IDs: []string{p.TenantID, p.ProductID, customer}})
	}
	if c.Kind == "DISABLE_CUSTOMER" {
		out.keys = append(out.keys, LockKey{Rank: 35, Kind: "integration_customer", IDs: []string{p.TenantID, p.ProductID, c.TargetID}})
	}
	if c.Kind == "DISABLE_REGISTRAR" && c.TargetID != p.CredentialID {
		return out, ErrDenied
	}
	return out, nil
}
func registryExpiry(now, expiry time.Time) bool {
	return !expiry.IsZero() && expiry.Nanosecond()%1000 == 0 && expiry.After(now) && !expiry.After(now.Add(30*24*time.Hour))
}
func (s *Store) ApplyIntegrationRegistry(ctx context.Context, key string, c domain.RegistryCommand) (domain.RegistryReceipt, error) {
	var zero domain.RegistryReceipt
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if domain.ValidateIdempotencyKey(key) != nil {
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
	if e != nil {
		return zero, e
	}
	// Freeze raw caller identity before preparing a separate UTC command.
	c = cloneRegistryCommand(c)
	var originalHash, originalLegacyHash string
	for attempt := 0; attempt < 3; attempt++ {
		plan, err := s.prepareRegistry(ctx, p, key, c)
		if err != nil {
			return zero, err
		}
		if attempt == 0 {
			originalHash, originalLegacyHash = plan.hash, plan.legacyHash
		} else if plan.hash != originalHash || plan.legacyHash != originalLegacyHash {
			return zero, ErrConflict
		}
		result, err := s.applyIntegrationRegistryTx(ctx, key, p, id, plan)
		if !errors.Is(err, ErrReplan) {
			return result, err
		}
	}
	return zero, ErrReplan
}

func (s *Store) applyIntegrationRegistryTx(ctx context.Context, key string, p domain.OperatorPrincipal, id WorldAccountingIdentity, plan registryPlan) (domain.RegistryReceipt, error) {
	var zero domain.RegistryReceipt
	c := plan.command
	tx, e := s.studioLocalTx(ctx, plan.keys)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback()
	if e = registrarCurrent(ctx, tx, s, p, id); e != nil {
		return zero, e
	}
	old, e := readRegistryReceipt(ctx, tx, p, key)
	if e == nil {
		matches := old.Hash == plan.hash
		if registryRegistration(c.Kind) {
			matches = matches || old.Hash == plan.legacyHash
			if old.Target != c.TargetID || old.Revision != 1 {
				return zero, ErrConflict
			}
		}
		if !matches || old.Operation != c.Kind {
			return zero, ErrConflict
		}
		return old, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return zero, e
	}
	if plan.replayOnly {
		return zero, ErrReplan
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return zero, e
	}
	r := domain.RegistryReceipt{Operation: c.Kind, Key: key, Hash: plan.hash, Target: c.TargetID, Revision: 1, ReceiptRef: integrationID(p.TenantID, p.ProductID, p.CredentialID, "integration", key), State: "COMMITTED"}
	var insertedSubject *registrySubjectExpected
	switch c.Kind {
	case "REGISTER_CUSTOMER":
		e = registerCustomerTx(ctx, tx, p, id, plan, now, &r)
	case "REGISTER_PLAYER_ACCESS":
		e = registerPlayerTx(ctx, tx, p, plan, now, &r, &insertedSubject)
	case "ROTATE_SERVICE_KEY":
		e = rotateServiceTx(ctx, tx, p, id, plan, now, &r)
	case "DISABLE_ENTITLEMENT", "DISABLE_PLAYER_MAPPING":
		e = disableQualifiedTx(ctx, tx, p, plan, &r)
	case "DISABLE_CUSTOMER":
		e = disableCustomerTx(ctx, tx, p, c, &r)
	case "REVOKE_CLIENT_SESSION":
		e = revokeClientTx(ctx, tx, p, id, plan, &r)
	case "DISABLE_REGISTRAR":
		var result sql.Result
		result, e = tx.ExecContext(ctx, "UPDATE integration_registrars SET enabled=0,revision=revision+1 WHERE tenant_id=? AND product_id=? AND credential_id=? AND revision=? AND revision<18446744073709551615", p.TenantID, p.ProductID, p.CredentialID, c.ExpectedRevision)
		if e == nil {
			e = registryAffected(result)
		}
		r.Revision = c.ExpectedRevision + 1
	default:
		e = ErrDenied
	}
	if e != nil {
		return zero, e
	}
	if e = writeRegistryReceipt(ctx, tx, p, r); e != nil {
		return zero, e
	}
	if e = s.finishStudioLocal(ctx, tx, p); e != nil {
		return zero, e
	}
	if c.Kind == "DISABLE_REGISTRAR" {
		// This one explicit self-disable is authorized before its own mutation.
		// It does not waive the current credential, scope or installation gate.
		e = operatorScopeTx(ctx, tx, p.ProductScope)
		if e == nil {
			e = checkConfigurationInstallationTx(ctx, tx, id)
		}
	} else {
		e = registrarCurrent(ctx, tx, s, p, id)
	}
	if e != nil {
		return zero, e
	}
	if plan.playerAppend != nil {
		if e = checkRegisteredPlayer(ctx, tx, p, plan); e != nil {
			return zero, e
		}
	}
	// finishStudioLocal has already run BeforeCommit. Check the original
	// inserted tuple here, before the unchanged commit/UNKNOWN boundary.
	if e = checkRegistrySubjectWritten(ctx, tx, insertedSubject); e != nil { return zero, e }
	if e = s.commitRegistry(ctx, tx); e != nil {
		return zero, e
	}
	return r, nil
}
func registerCustomerTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, id WorldAccountingIdentity, plan registryPlan, now time.Time, r *domain.RegistryReceipt) error {
	v := plan.command.Customer
	if !registryExpiry(now, v.ExpiresAt) {
		return ErrDenied
	}
	var exists bool
	if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM integration_customers WHERE tenant_id=? AND product_id=? AND (customer_id=? OR service_subject_id=? OR consumer_id=?)) OR EXISTS(SELECT 1 FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?)", p.TenantID, p.ProductID, v.CustomerID, v.ServiceSubjectID, v.ConsumerID, p.TenantID, p.ProductID, v.ServiceSubjectID).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return ErrConflict
	}
	if e := reserveCredentialHashTx(ctx, tx, plan.secretHash, "SERVICE", plan.principal, id.Installation, false); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled,can_control,can_migrate) VALUES(?,?,?,1,1,1,1,1,0)", p.TenantID, p.ProductID, v.ServiceSubjectID); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "INSERT INTO integration_customers(tenant_id,product_id,customer_id,service_subject_id,consumer_id,account_id,expires_at,enabled,revision,registry_receipt) VALUES(?,?,?,?,?,?,?,1,1,?)", p.TenantID, p.ProductID, v.CustomerID, v.ServiceSubjectID, v.ConsumerID, plan.account, v.ExpiresAt, r.ReceiptRef); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,balance) VALUES('customer',?,?,?,?,0)", p.TenantID+"."+p.ProductID, plan.account, p.TenantID, p.ProductID); e != nil {
		return e
	}
	if e := insertServiceTx(ctx, tx, p, plan.principal, plan.secretHash, v.CustomerID, v.ServiceSubjectID, v.ExpiresAt, now, r.ReceiptRef); e != nil {
		return e
	}
	expiry := v.ExpiresAt.UTC()
	r.RegistrationExpiry = &domain.RegistrationExpiry{CustomerExpiresAt: expiry, ServiceKeyExpiresAt: &expiry}
	return nil
}
func insertServiceTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, principal string, h [32]byte, customer, subject string, expires, now time.Time, receipt string) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO integration_principals(principal_id,token_hash,kind,tenant_id,product_id,customer_id,subject_id,profile,client_actions,issued_at,expires_at,revoked,revision,issuance_receipt) VALUES(?,?,'SERVICE',?,?,?,?,'integration_r0_v1',JSON_ARRAY(),?,?,0,1,?)", principal, h[:], p.TenantID, p.ProductID, customer, subject, now, expires, receipt)
	return e
}
func registerPlayerTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, plan registryPlan, now time.Time, r *domain.RegistryReceipt, inserted **registrySubjectExpected) error {
	v := plan.command.Player
	if !registryExpiry(now, v.ExpiresAt) {
		return ErrDenied
	}
	var customerExpiry time.Time
	var enabled bool
	if e := tx.QueryRowContext(ctx, "SELECT enabled,expires_at FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=?", p.TenantID, p.ProductID, v.CustomerID).Scan(&enabled, &customerExpiry); e != nil || !enabled || !customerExpiry.After(now) || v.ExpiresAt.After(customerExpiry) {
		return ErrDenied
	}
	var available bool
	if e := tx.QueryRowContext(ctx, "SELECT r.available FROM releases r JOIN content_releases c USING(tenant_id,product_id,release_id) WHERE r.tenant_id=? AND r.product_id=? AND r.release_id=?", p.TenantID, p.ProductID, v.ReleaseID).Scan(&available); e != nil || !available {
		return ErrDenied
	}
	if plan.playerAppend != nil {
		if e := checkRegisteredPlayer(ctx, tx, p, plan); e != nil {
			return e
		}
	} else {
		var exists bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?) OR EXISTS(SELECT 1 FROM integration_external_subjects WHERE tenant_id=? AND product_id=? AND customer_id=? AND external_id=?) OR EXISTS(SELECT 1 FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=?)", p.TenantID, p.ProductID, v.PlayerSubjectID, p.TenantID, p.ProductID, v.CustomerID, v.ExternalIDBytes, p.TenantID, p.ProductID, v.CustomerID, v.EntitlementID).Scan(&exists); e != nil {
			return e
		}
		if exists {
			return ErrConflict
		}
		expected := registrySubjectExpected{tenant:p.TenantID, product:p.ProductID, subject:v.PlayerSubjectID}
		for _, action := range v.Actions { if action == "MEMORY_GOVERN" { expected.canGovern = true } }
		if _, e := tx.ExecContext(ctx, "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled,can_control,can_migrate,can_govern) VALUES(?,?,?,1,1,1,1,0,0,?)", expected.tenant, expected.product, expected.subject, expected.canGovern); e != nil {
			return e
		}
		*inserted = &expected
		if _, e := tx.ExecContext(ctx, "INSERT INTO integration_external_subjects(tenant_id,product_id,customer_id,external_id,player_subject_id,expires_at,enabled,revision,registry_receipt) VALUES(?,?,?,?,?,?,1,1,?)", p.TenantID, p.ProductID, v.CustomerID, v.ExternalIDBytes, v.PlayerSubjectID, v.ExpiresAt, r.ReceiptRef); e != nil {
			return e
		}

	}
	actions, e := json.Marshal(v.Actions)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO integration_entitlements(tenant_id,product_id,customer_id,entitlement_id,player_subject_id,release_id,allowed_actions,self_consent_eligible,eligibility_policy,expires_at,enabled,revision,registry_receipt) VALUES(?,?,?,?,?,?,?,1,'SELF_PRIVATE_R0',?,1,1,?)", p.TenantID, p.ProductID, v.CustomerID, v.EntitlementID, v.PlayerSubjectID, v.ReleaseID, actions, v.ExpiresAt, r.ReceiptRef); e != nil {
		return e
	}
	if plan.playerAppend == nil {
		for _, a := range v.Actions {
			var permissions []string
			if a == "MEMORY_VIEW" {
				permissions = []string{"memory.metadata.read", "memory.private_body.read"}
			}
			if a == "MEMORY_GOVERN" {
				permissions = []string{"memory.govern", "memory.correct"}
			}
			for _, action := range permissions {
				if _, e = tx.ExecContext(ctx, "INSERT INTO memory_subject_permissions(tenant_id,product_id,caller_subject_id,data_subject_id,action,allowed_use,expires_at,enabled) VALUES(?,?,?,?,?,'PRIVATE_RESPONSE',?,1)", p.TenantID, p.ProductID, v.PlayerSubjectID, v.PlayerSubjectID, action, v.ExpiresAt); e != nil {
					return e
				}
			}
		}
	}
	expiry := v.ExpiresAt.UTC()
	mappingExpiry := expiry
	if plan.playerAppend != nil {
		mappingExpiry = plan.playerAppend.MappingExpiry
	}
	r.RegistrationExpiry = &domain.RegistrationExpiry{CustomerExpiresAt: customerExpiry.UTC(), MappingExpiresAt: &mappingExpiry, EntitlementExpiresAt: &expiry}
	return nil
}
func (s *Store) ReadIntegrationRegistryReceipt(ctx context.Context, key string) (domain.RegistryReceipt, error) {
	var zero domain.RegistryReceipt
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if domain.ValidateIdempotencyKey(key) != nil {
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
	if e != nil {
		return zero, e
	}
	tx, e := s.studioLocalTx(ctx, registryKeys(p, key))
	if e != nil {
		return zero, e
	}
	defer tx.Rollback()
	if e = registrarCurrent(ctx, tx, s, p, id); e != nil {
		return zero, e
	}
	r, e := readRegistryReceipt(ctx, tx, p, key)
	if errors.Is(e, sql.ErrNoRows) {
		return zero, sql.ErrNoRows
	}
	return r, e
}
