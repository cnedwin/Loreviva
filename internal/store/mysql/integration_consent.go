package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"

	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
	"time"
)

const playerConsentPolicy = "R0_PLAYER_PRIVATE_GENERATION_30D"

type IntegrationConsentOwner interface {
	WritePlayerSelfConsentTx(context.Context, Tx, domain.ProductScope, bool, time.Time) error
}
type integrationConsentWrite struct {
	store     *Store
	principal currentIntegration
	scope     domain.ProductScope
	enabled   bool
	expires   time.Time
	active    bool
	written   bool
}

func (s *Store) BindIntegrationConsentOwner(ctx context.Context, owner IntegrationConsentOwner) error {
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if owner == nil || s.integrationConsentOwner != nil || s.integrationClosed.Load() || s.Guard == nil {
		return ErrDenied
	}
	_, done, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer done()
	s.integrationConsentOwner = owner
	return nil
}

// The unexported unit and one-use sealed choice bind the complete writer tuple.
// This is a typed persistence port, not a general SQL callback or metadata grant.
func (s *Store) WriteIntegrationConsentPermissionTx(ctx context.Context, tx Tx, scope domain.ProductScope, enabled bool, expiry time.Time) error {
	u, ok := tx.(*unit)
	if !ok || u.integrationConsent == nil {
		return ErrDenied
	}
	w := u.integrationConsent
	if !w.active || w.written || w.store != s || w.scope != scope || w.enabled != enabled || !w.expires.Equal(expiry) {
		return ErrDenied
	}
	if _, _, e := s.currentClient(ctx, u.tx, w.principal); e != nil {
		return e
	}
	if e := u.RequireLock(LockKey{Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}); e != nil {
		return e
	}
	_, e := u.tx.ExecContext(ctx, `INSERT INTO memory_subject_permissions(tenant_id,product_id,caller_subject_id,data_subject_id,action,allowed_use,expires_at,enabled) VALUES(?,?,?,?,'memory.generation.use','PRIVATE_RESPONSE',?,?) ON DUPLICATE KEY UPDATE expires_at=VALUES(expires_at),enabled=VALUES(enabled)`, scope.TenantID, scope.ProductID, scope.SubjectID, scope.SubjectID, expiry, enabled)
	if e != nil {
		return e
	}
	result, e := u.tx.ExecContext(ctx, `UPDATE subject_governance SET permission_version=permission_version+1,privacy_epoch=privacy_epoch+1,view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND subject_id=? AND permission_version<18446744073709551615 AND privacy_epoch<18446744073709551615 AND view_epoch<18446744073709551615`, scope.TenantID, scope.ProductID, scope.SubjectID)
	if e != nil {
		return e
	}
	if e = registryAffected(result); e != nil {
		return e
	}
	w.written = true
	return nil
}
func clientService(r sessionRecord) domain.IntegrationPrincipal {
	return domain.IntegrationPrincipal{Kind: domain.IntegrationService, PrincipalID: r.Service, Tenant: r.Tenant, Product: r.Product, Customer: r.Customer, StableSubject: r.ServiceSubject}
}
func consentKeys(r sessionRecord, eligibility sessionEligibility, key string) []LockKey {
	keys := sessionKeys(clientService(r), r, eligibility.keys)
	keys = append(keys, LockKey{Rank: 90, Kind: "integration_consent", IDs: []string{r.Tenant, r.Product, r.Player}})
	if key != "" {
		keys = append(keys, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{r.Tenant, r.Product, r.Player, key}})
	}
	return keys
}
func (s *Store) consentSnapshot(ctx context.Context, q rowQuerier, r sessionRecord, eligibility sessionEligibility) (generated.PlayerSelfConsentSnapshot, uint64, error) {
	var out generated.PlayerSelfConsentSnapshot
	var generation uint64
	var now time.Time
	out.Scope.TenantID = generated.Identity(r.Tenant)
	out.Scope.ProductID = generated.Identity(r.Product)
	out.SubjectID = generated.Identity(r.Player)
	out.PolicyID = playerConsentPolicy
	out.ConsentRevision = "0"
	out.State = "NOT_GRANTED"
	out.AcceptedAt = json.RawMessage("null")
	out.ExpiresAt = json.RawMessage("null")
	out.Eligible = eligibility.entitlement.Eligible && eligibility.entitlement.Policy == "SELF_PRIVATE_R0"
	if e := q.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return out, 0, e
	}
	out.ServerNow = now.UTC().Format(time.RFC3339Nano)
	var revision uint64
	var action, policy string
	var accepted, expires sql.NullTime
	e := q.QueryRowContext(ctx, `SELECT revision,action,accepted_at,expires_at,use_authority_generation,policy FROM integration_self_consent_current WHERE tenant_id=? AND product_id=? AND player_subject_id=?`, r.Tenant, r.Product, r.Player).Scan(&revision, &action, &accepted, &expires, &generation, &policy)
	if errors.Is(e, sql.ErrNoRows) {
		return out, 0, nil
	}
	if e != nil || policy != playerConsentPolicy {
		return out, 0, ErrDenied
	}
	out.ConsentRevision = generated.ExactUnsigned(strconv.FormatUint(revision, 10))
	if accepted.Valid {
		out.AcceptedAt, _ = json.Marshal(accepted.Time.UTC().Format(time.RFC3339Nano))
	}
	if expires.Valid {
		out.ExpiresAt, _ = json.Marshal(expires.Time.UTC().Format(time.RFC3339Nano))
	}
	switch {
	case action == "REVOKE":
		out.State = "REVOKED"
	case (action == "INITIAL" || action == "RENEW") && expires.Valid && expires.Time.After(now):
		out.State = "ACTIVE"
	case action == "INITIAL" || action == "RENEW":
		out.State = "EXPIRED"
	default:
		return out, 0, ErrDenied
	}
	if out.State == "ACTIVE" {
		e = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.generation.use' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6) AND expires_at=?)`, r.Tenant, r.Product, r.Player, r.Player, expires.Time).Scan(&out.CurrentUseEnabled)
	}
	return out, generation, e
}
func (s *Store) ReadPlayerSelfConsent(ctx context.Context) (generated.PlayerSelfConsentSnapshot, error) {
	var out generated.PlayerSelfConsentSnapshot
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok {
		return out, ErrDenied
	}
	r, eligible, e := s.currentClient(ctx, s.DB, v)
	if e != nil {
		return out, e
	}
	frozenEligibility := eligible
	tx, e := s.integrationMemoryReadTx(ctx, consentKeys(r, eligible, ""))
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	r, eligible, e = s.currentClient(ctx, tx, v)
	if e != nil || !reflect.DeepEqual(eligible, frozenEligibility) {
		return out, ErrDenied
	}
	out, _, e = s.consentSnapshot(ctx, tx, r, eligible)
	if e != nil {
		return out, e
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return out, e
		}
	}
	if _, finalEligibility, err := s.currentClient(ctx, tx, v); err != nil || !reflect.DeepEqual(finalEligibility, frozenEligibility) {
		return out, ErrDenied
	}
	return out, typedJSON("PlayerSelfConsentSnapshot", out)
}
func (s *Store) ApplyPlayerSelfConsent(ctx context.Context, key string, raw generated.PlayerSelfConsentCommandRequest) (generated.PlayerSelfConsentReceipt, error) {
	var out generated.PlayerSelfConsentReceipt
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	if s.integrationConsentOwner == nil || domain.ValidateIdempotencyKey(key) != nil || generated.Validate("PlayerSelfConsentCommandRequest", raw) != nil {
		return out, ErrDenied
	}
	var command struct {
		Command  string `json:"command"`
		Expected string `json:"expected_revision"`
		Policy   string `json:"policy_id"`
	}
	if json.Unmarshal(raw, &command) != nil {
		return out, ErrDenied
	}
	expected, e := strconv.ParseUint(command.Expected, 10, 64)
	if e != nil || expected == ^uint64(0) {
		return out, ErrConflict
	}
	frozen, _ := json.Marshal(command)
	hash := sha256.Sum256(frozen)
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok {
		return out, ErrDenied
	}
	r, eligible, e := s.currentClient(ctx, s.DB, v)
	if e != nil {
		return out, e
	}
	frozenEligibility := eligible
	original, originalError := consentOriginalSession(ctx, s.DB, r, key)
	if originalError != nil && !errors.Is(originalError, sql.ErrNoRows) {
		return out, originalError
	}
	keys := consentKeys(r, eligible, key)
	if originalError == nil {
		keys = append(keys, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{original.ID}}, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{original.Service}}, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{original.Tenant, original.Product, original.Service, original.Key}})
	}
	tx, e := s.integrationMemoryReadTx(ctx, keys)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	r, eligible, e = s.currentClient(ctx, tx, v)
	if e != nil || !reflect.DeepEqual(eligible, frozenEligibility) {
		return out, ErrDenied
	}
	var old, oldHash []byte
	e = tx.QueryRowContext(ctx, `SELECT result_metadata,payload_hash FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND operation='PLAYER_SELF_CONSENT' AND scope_kind='PRODUCT' AND scope_address=? AND original_key=? AND state='COMMITTED'`, r.Tenant, r.Product, r.Player, r.Product, key).Scan(&old, &oldHash)
	if e == nil {
		lockedOriginal, err := consentOriginalSession(ctx, tx, r, key)
		if originalError != nil || err != nil || !reflect.DeepEqual(lockedOriginal, original) {
			return out, ErrDenied
		}
		if !bytes.Equal(oldHash, hash[:]) || json.Unmarshal(old, &out) != nil || out.OriginalKey != key || out.Snapshot.SubjectID != generated.Identity(r.Player) || typedJSON("PlayerSelfConsentReceipt", out) != nil {
			return out, ErrConflict
		}
		if e = validatePlayerConsentReceipt(ctx, tx, r, key, hash, out); e != nil {
			return out, e
		}
		return out, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if originalError == nil {
		return out, ErrDenied
	}
	before, generation, e := s.consentSnapshot(ctx, tx, r, eligible)
	if e != nil {
		return out, e
	}
	revision, e := strconv.ParseUint(string(before.ConsentRevision), 10, 64)
	if e != nil || revision != expected {
		return out, ErrConflict
	}
	if command.Command == "INITIAL" && (revision != 0 || before.State != "NOT_GRANTED") {
		return out, ErrConflict
	}
	if command.Command == "RENEW" && revision == 0 {
		return out, ErrConflict
	}
	enabled := command.Command != "REVOKE"
	if enabled && !before.Eligible {
		return out, ErrDenied
	}
	if command.Command == "INITIAL" {
		generation = 1
	}
	if !enabled {
		if generation == ^uint64(0) {
			return out, ErrConflict
		}
		generation++
	}
	now, e := time.Parse(time.RFC3339Nano, before.ServerNow)
	if e != nil {
		return out, e
	}
	var accepted, expiry any
	useExpiry := now
	if enabled {
		accepted = now
		useExpiry = now.Add(30 * 24 * time.Hour)
		expiry = useExpiry
	} else if string(before.AcceptedAt) != "null" {
		var prior string
		if json.Unmarshal(before.AcceptedAt, &prior) != nil {
			return out, ErrDenied
		}
		a, e := time.Parse(time.RFC3339Nano, prior)
		if e != nil {
			return out, e
		}
		accepted = a
	}
	scope := domain.ProductScope{TenantID: r.Tenant, ProductID: r.Product, SubjectID: r.Player}
	seal := &integrationConsentWrite{store: s, principal: v, scope: scope, enabled: enabled, expires: useExpiry, active: true}
	var priorEpochs [3]uint64
	if e = tx.QueryRowContext(ctx, "SELECT permission_version,privacy_epoch,view_epoch FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?", r.Tenant, r.Product, r.Player).Scan(&priorEpochs[0], &priorEpochs[1], &priorEpochs[2]); e != nil {
		return out, e
	}
	u := &unit{tx: tx, integrationConsent: seal, plan: map[string]bool{}}
	for _, k := range consentKeys(r, eligible, key) {
		u.plan[string(lockBytes(k))] = true
	}
	e = s.integrationConsentOwner.WritePlayerSelfConsentTx(ctx, u, scope, enabled, useExpiry)
	seal.active = false
	if e != nil {
		return out, e
	}
	if !seal.written {
		return out, ErrDenied
	}
	revision++
	ref := creationID(r.Tenant, r.Product, r.Player, "self-consent", key)
	_, e = tx.ExecContext(ctx, `INSERT INTO integration_self_consent_current(tenant_id,product_id,player_subject_id,policy,revision,action,accepted_at,expires_at,use_authority_generation,eligibility_ref,receipt_ref) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE revision=VALUES(revision),action=VALUES(action),accepted_at=VALUES(accepted_at),expires_at=VALUES(expires_at),use_authority_generation=VALUES(use_authority_generation),eligibility_ref=VALUES(eligibility_ref),receipt_ref=VALUES(receipt_ref)`, r.Tenant, r.Product, r.Player, playerConsentPolicy, revision, command.Command, accepted, expiry, generation, r.Entitlement, ref)
	if e != nil {
		return out, e
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO integration_self_consent_facts(tenant_id,product_id,player_subject_id,policy,revision,action,accepted_at,expires_at,use_authority_generation,eligibility_ref,receipt_ref,principal_ref,original_key,payload_hash,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.Tenant, r.Product, r.Player, playerConsentPolicy, revision, command.Command, accepted, expiry, generation, r.Entitlement, ref, r.ID, key, hash[:], now)
	if e != nil {
		return out, e
	}
	if e = registryAffected(result); e != nil {
		return out, e
	}
	snapshot, _, e := s.consentSnapshot(ctx, tx, r, eligible)
	if e != nil {
		return out, e
	}
	out = generated.PlayerSelfConsentReceipt{OriginalKey: key, ReceiptRef: generated.Identity(ref), Snapshot: snapshot, State: "COMMITTED"}
	if e = typedJSON("PlayerSelfConsentReceipt", out); e != nil {
		return out, e
	}
	metadata, _ := json.Marshal(out)
	result, e = tx.ExecContext(ctx, `INSERT INTO integration_receipts(tenant_id,product_id,owner_id,operation,scope_kind,scope_address,original_key,receipt_ref,payload_hash,result_metadata,state,target_revision,committed_at) VALUES(?,?,?,'PLAYER_SELF_CONSENT','PRODUCT',?,?,?,?,?,'COMMITTED',?,?)`, r.Tenant, r.Product, r.Player, r.Product, key, ref, hash[:], metadata, revision, now)
	if e != nil {
		return out, e
	}
	if e = registryAffected(result); e != nil {
		return out, e
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return out, e
		}
	}
	expectedEligibility := frozenEligibility
	expectedMapping := *frozenEligibility.mapping
	expectedMapping.Permission, expectedMapping.Privacy, expectedMapping.View = priorEpochs[0]+1, priorEpochs[1]+1, priorEpochs[2]+1
	expectedEligibility.mapping = &expectedMapping
	if _, finalEligibility, err := s.currentClient(ctx, tx, v); err != nil || !reflect.DeepEqual(finalEligibility, expectedEligibility) {
		return out, fmt.Errorf("consent final eligibility: %w", ErrDenied)
	}
	// Compare all current/fact/receipt fields to this exact writer's expected
	// values after the fault hook. A structurally valid replacement is not ours.
	var exact bool
	e = tx.QueryRowContext(ctx, `SELECT (c.policy=? AND c.revision=? AND c.action=? AND c.accepted_at<=>? AND c.expires_at<=>? AND c.use_authority_generation=? AND c.eligibility_ref=? AND c.receipt_ref=? AND f.policy=c.policy AND f.action=c.action AND f.accepted_at<=>c.accepted_at AND f.expires_at<=>c.expires_at AND f.use_authority_generation=c.use_authority_generation AND f.eligibility_ref=c.eligibility_ref AND f.receipt_ref=c.receipt_ref AND f.principal_ref=? AND f.original_key=? AND f.payload_hash=? AND f.recorded_at=? AND x.result_metadata=CAST(? AS JSON) AND x.committed_at=f.recorded_at AND x.receipt_ref=f.receipt_ref AND x.payload_hash=f.payload_hash AND x.target_revision=f.revision AND x.state='COMMITTED' AND g.permission_version=? AND g.privacy_epoch=? AND g.view_epoch=? AND m.enabled=? AND m.expires_at=?) FROM integration_self_consent_current c JOIN integration_self_consent_facts f USING(tenant_id,product_id,player_subject_id,revision) JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.operation='PLAYER_SELF_CONSENT' AND x.scope_kind='PRODUCT' AND x.scope_address=f.product_id AND x.original_key=f.original_key JOIN subject_governance g ON g.tenant_id=c.tenant_id AND g.product_id=c.product_id AND g.subject_id=c.player_subject_id JOIN memory_subject_permissions m ON m.tenant_id=c.tenant_id AND m.product_id=c.product_id AND m.caller_subject_id=c.player_subject_id AND m.data_subject_id=c.player_subject_id AND m.action='memory.generation.use' AND m.allowed_use='PRIVATE_RESPONSE' WHERE c.tenant_id=? AND c.product_id=? AND c.player_subject_id=?`, playerConsentPolicy, revision, command.Command, accepted, expiry, generation, r.Entitlement, ref, r.ID, key, hash[:], now, metadata, priorEpochs[0]+1, priorEpochs[1]+1, priorEpochs[2]+1, enabled, useExpiry, r.Tenant, r.Product, r.Player).Scan(&exact)
	if e != nil || !exact {
		return out, fmt.Errorf("consent final tuple (query error=%t): %w", e != nil, ErrDenied)
	}
	final, fg, e := s.consentSnapshot(ctx, tx, r, eligible)
	if e != nil || fg != generation || final.ConsentRevision != snapshot.ConsentRevision || final.CurrentUseEnabled != enabled || final.State != snapshot.State {
		return out, ErrDenied
	}
	if e = validatePlayerConsentReceipt(ctx, tx, r, key, hash, out); e != nil {
		return out, fmt.Errorf("consent original proof: %w", e)
	}
	if e = s.commitRegistry(ctx, tx); e != nil {
		return out, e
	}
	return out, nil
}

// Replay retains the original fact and result, but never accepts a structurally
// valid receipt borrowed from another product, player, key or fact revision.
func validatePlayerConsentReceipt(ctx context.Context, q creationRows, r sessionRecord, key string, hash [32]byte, out generated.PlayerSelfConsentReceipt) error {
	ref := creationID(r.Tenant, r.Product, r.Player, "self-consent", key)
	if string(out.ReceiptRef) != ref || out.OriginalKey != key || out.State != "COMMITTED" || string(out.Snapshot.Scope.TenantID) != r.Tenant || string(out.Snapshot.Scope.ProductID) != r.Product || string(out.Snapshot.SubjectID) != r.Player || out.Snapshot.PolicyID != playerConsentPolicy {
		return ErrDenied
	}
	revision, e := strconv.ParseUint(string(out.Snapshot.ConsentRevision), 10, 64)
	if e != nil || revision == 0 {
		return ErrDenied
	}
	var action, policy, principal, entitlement string
	var accepted, expiry sql.NullTime
	var factHash []byte
	var generation uint64
	e = q.QueryRowContext(ctx, `SELECT f.action,f.policy,f.accepted_at,f.expires_at,f.payload_hash,f.use_authority_generation,f.principal_ref,f.eligibility_ref FROM integration_self_consent_facts f JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.receipt_ref=f.receipt_ref AND x.original_key=f.original_key AND x.target_revision=f.revision AND x.payload_hash=f.payload_hash AND x.operation='PLAYER_SELF_CONSENT' AND x.scope_kind='PRODUCT' AND x.scope_address=f.product_id AND x.state='COMMITTED' WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.revision=? AND f.original_key=? AND f.receipt_ref=?`, r.Tenant, r.Product, r.Player, revision, key, ref).Scan(&action, &policy, &accepted, &expiry, &factHash, &generation, &principal, &entitlement)
	if e != nil || policy != playerConsentPolicy || !bytes.Equal(factHash, hash[:]) || generation == 0 || principal == "" || entitlement == "" {
		return ErrDenied
	}
	original, e := consentOriginalSession(ctx, q, r, key)
	if e != nil || original.ID != principal || original.Entitlement != entitlement {
		return ErrDenied
	}
	// Facts are product/player history, not the current Session's history.
	// Reconstruct each original action hash and generation without extending its
	// expiry or treating the caller's newer Session as the original issuer.
	rows, e := q.QueryContext(ctx, `SELECT f.revision,f.action,f.policy,f.original_key,f.payload_hash,f.use_authority_generation FROM integration_self_consent_facts f JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.operation='PLAYER_SELF_CONSENT' AND x.scope_kind='PRODUCT' AND x.scope_address=f.product_id AND x.original_key=f.original_key AND x.receipt_ref=f.receipt_ref AND x.target_revision=f.revision AND x.payload_hash=f.payload_hash AND x.committed_at=f.recorded_at AND x.state='COMMITTED' WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.revision<=? ORDER BY f.revision`, r.Tenant, r.Product, r.Player, revision)
	if e != nil {
		return e
	}
	var count, derived uint64
	for rows.Next() {
		var rev, gen uint64
		var act, pol, k string
		var h []byte
		if e = rows.Scan(&rev, &act, &pol, &k, &h, &gen); e != nil {
			break
		}
		count++
		if rev != count || pol != playerConsentPolicy || domain.ValidateIdempotencyKey(k) != nil {
			e = ErrDenied
			break
		}
		switch act {
		case "INITIAL":
			if rev != 1 {
				e = ErrDenied
			}
			derived = 1
		case "REVOKE":
			if derived == ^uint64(0) {
				e = ErrDenied
			} else {
				derived++
			}
		case "RENEW":
			if rev == 1 {
				e = ErrDenied
			}
		default:
			e = ErrDenied
		}
		command := struct {
			Command  string `json:"command"`
			Expected string `json:"expected_revision"`
			Policy   string `json:"policy_id"`
		}{act, strconv.FormatUint(rev-1, 10), pol}
		body, _ := json.Marshal(command)
		expectedHash := sha256.Sum256(body)
		if e != nil || gen != derived || !bytes.Equal(h, expectedHash[:]) {
			e = ErrDenied
			break
		}
	}
	if e == nil {
		e = rows.Err()
	}
	closeErr := rows.Close()
	if e != nil || closeErr != nil || count != revision || derived != generation {
		return ErrDenied
	}
	encoded := func(v sql.NullTime) []byte {
		if !v.Valid {
			return []byte("null")
		}
		b, _ := json.Marshal(v.Time.UTC().Format(time.RFC3339Nano))
		return b
	}
	if !bytes.Equal(encoded(accepted), out.Snapshot.AcceptedAt) || !bytes.Equal(encoded(expiry), out.Snapshot.ExpiresAt) {
		return ErrDenied
	}
	if action == "REVOKE" {
		if expiry.Valid || out.Snapshot.State != "REVOKED" || out.Snapshot.CurrentUseEnabled {
			return ErrDenied
		}
	} else if action == "INITIAL" || action == "RENEW" {
		if !accepted.Valid || !expiry.Valid || !expiry.Time.Equal(accepted.Time.Add(30*24*time.Hour)) || out.Snapshot.State != "ACTIVE" || !out.Snapshot.CurrentUseEnabled {
			return ErrDenied
		}
	} else {
		return ErrDenied
	}
	return nil
}

// Original issuance is historical evidence, so expiry/revocation is not used
// to reject a legitimate new same-player Session replay. All immutable CLIENT
// issuance fields and its actual committed receipt must still match exactly.
func consentOriginalSession(ctx context.Context, q rowQuerier, current sessionRecord, key string) (sessionRecord, error) {
	var zero sessionRecord
	var principal, entitlement, issuer, originalKey string
	var recorded time.Time
	e := q.QueryRowContext(ctx, `SELECT f.principal_ref,f.eligibility_ref,k.issuer_service_key,x.original_key,f.recorded_at FROM integration_self_consent_facts f JOIN integration_principals k ON k.principal_id=f.principal_ref AND k.kind='CLIENT' AND k.tenant_id=f.tenant_id AND k.product_id=f.product_id AND k.subject_id=f.player_subject_id JOIN integration_receipts x ON x.tenant_id=k.tenant_id AND x.product_id=k.product_id AND x.owner_id=k.issuer_service_key AND x.operation='EXCHANGE_SESSION' AND x.scope_kind='SESSION' AND x.scope_address=k.product_id AND x.receipt_ref=k.issuance_receipt AND x.state='COMMITTED' WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.original_key=?`, current.Tenant, current.Product, current.Player, key).Scan(&principal, &entitlement, &issuer, &originalKey, &recorded)
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			var n int
			err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM integration_self_consent_facts WHERE tenant_id=? AND product_id=? AND player_subject_id=? AND original_key=?", current.Tenant, current.Product, current.Player, key).Scan(&n)
			if err != nil || n != 0 {
				return zero, ErrDenied
			}
		}
		return zero, e
	}
	p := clientService(current)
	p.PrincipalID = issuer
	r, e := sessionRead(ctx, q, p, originalKey)
	if e != nil || r.ID != principal || r.Player != current.Player || r.Entitlement != entitlement || r.Installation != current.Installation || r.ID != creationID(r.Tenant, r.Product, r.Service, "session", r.Key) || recorded.Before(r.Issued) || !recorded.Before(r.Expires) {
		return zero, ErrDenied
	}
	var good bool
	e = q.QueryRowContext(ctx, `SELECT (k.tenant_id=? AND k.product_id=? AND k.customer_id=? AND k.subject_id=? AND k.world_instance_id=? AND k.issuer_service_key=? AND k.profile=? AND k.entitlement_id=? AND k.issued_at=? AND k.expires_at=? AND k.nonce=? AND k.key_version=? AND k.key_ref=? AND k.issuance_receipt=? AND h.kind='CLIENT' AND h.principal_id=k.principal_id AND h.installation_id=?) FROM integration_principals k JOIN integration_credential_hashes h ON h.token_hash=k.token_hash WHERE k.principal_id=? AND k.kind='CLIENT'`, r.Tenant, r.Product, r.Customer, r.Player, r.World, r.Service, r.Profile, r.Entitlement, r.Issued, r.Expires, r.Nonce, r.KeyVersion, r.KeyRef, r.Receipt, r.Installation, r.ID).Scan(&good)
	if e != nil || !good {
		return zero, ErrDenied
	}
	var actions []string
	var raw []byte
	if e = q.QueryRowContext(ctx, "SELECT client_actions FROM integration_principals WHERE principal_id=?", r.ID).Scan(&raw); e != nil || json.Unmarshal(raw, &actions) != nil || !reflect.DeepEqual(actions, r.Actions) {
		return zero, ErrDenied
	}

	return r, nil
}
