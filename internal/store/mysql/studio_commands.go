package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

// Only decoded, closed generated command families enter this private plan.
type studioRequest struct {
	Scope        domain.SettingsScope `json:"scope"`
	Kind         string               `json:"kind"`
	Expected     string               `json:"expected_revision"`
	Defaults     json.RawMessage      `json:"defaults"`
	Quota        string               `json:"quota_policy_ref"`
	Connection   json.RawMessage      `json:"connection"`
	ConnectionID string               `json:"connection_id"`
	Preference   json.RawMessage      `json:"preference"`
	CredentialID string               `json:"credential_id"`
	SubjectID    string               `json:"subject_id"`
	Grant        json.RawMessage      `json:"grant"`
	Enabled      bool                 `json:"enabled"`
	ExpiresAt    *time.Time           `json:"expires_at"`
}

type StudioConflict struct {
	Target            any
	Expected, Current uint64
}

func (e *StudioConflict) Error() string { return "Studio revision changed; fetch current snapshot" }
func (e *StudioConflict) Unwrap() error { return ErrConflict }

func decodeStudio(schema string, raw json.RawMessage) (studioRequest, error) {
	var r studioRequest
	if e := generated.Decode(schema, raw, &r); e != nil {
		return r, e
	}
	return r, r.Scope.Validate()
}
func studioCommandAction(kind string) (action, use string) {
	switch kind {
	case "SET_DEFAULTS":
		return "settings.defaults.write", ""
	case "SET_QUOTA_POLICY":
		return "settings.quota.write", ""
	case "UPSERT_CONNECTION_METADATA", "DISABLE_CONNECTION":
		return "settings.connection.write", ""
	case "SET_LOCAL_NOTIFICATIONS":
		return "settings.notifications.write", ""
	case "SET_SCOPE_GRANT":
		return "members.manage", ""
	case "SET_CREDENTIAL_STATE":
		return "maintenance.manage", "MAINTENANCE_COMMAND"
	case "INITIAL", "RENEW", "REVOKE":
		return "memory.generation.consent", "SELF"
	}
	return "", ""
}
func studioSectionKey(sc domain.SettingsScope, section string) LockKey {
	return LockKey{Rank: 90, Kind: domain.StudioSettingsLock, IDs: []string{sc.TenantID, sc.Kind, sc.Address(), section}}
}
func studioBaseKeys(p domain.OperatorPrincipal, sc domain.SettingsScope) []LockKey {
	keys := []LockKey{{Rank: 10, IDs: []string{sc.TenantID}}, {Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{sc.TenantID, p.CredentialID}}, studioGrantKey(p, sc), studioScopeKey(sc)}
	if sc.Kind == "PRODUCT" {
		keys = append(keys, LockKey{Rank: 20, IDs: []string{sc.TenantID, *sc.ProductID}}, LockKey{Rank: 30, IDs: []string{sc.TenantID, *sc.ProductID, p.SubjectID}})
	}
	return keys
}
// Map each supported command to its current action and use. SELF commands stay in the principal's product; other commands use the exact settings or tenant-maintenance grant, and unknown kinds are denied.
func (s *Store) studioCommandGate(ctx context.Context, q rowQuerier, sc domain.SettingsScope, kind string) error {
	a, use := studioCommandAction(kind)
	if a == "" {
		return ErrDenied
	}
	if use == "" {
		use = sc.SettingsUse()
	}
	if use == "SELF" {
		p, e := s.CurrentOperator(ctx)
		if e != nil || sc.Kind != "PRODUCT" || sc.TenantID != p.TenantID || *sc.ProductID != p.ProductID {
			return ErrDenied
		}
		return operatorActionTx(ctx, q, s, p.ProductScope, a)
	}
	return studioActionTx(ctx, q, s, sc, a, use)
}

type studioCommand struct {
	tx            *sql.Tx
	store         *Store
	principal     domain.OperatorPrincipal
	request       studioRequest
	family, key   string
	hash          [32]byte
	expected      uint64
	now           time.Time
	keys          []LockKey
	memberGrant   *studioGrant
	removedStable bool
}

// Check current command authority before looking up the original scope, credential, family and key. An existing key must match the original payload hash and returns its stored receipt without opening a new revision.
func (s *Store) beginStudioCommand(ctx context.Context, r studioRequest, family, key string, raw json.RawMessage, extra ...LockKey) (*studioCommand, generated.StudioLocalCommandReceipt, error) {
	p, e := s.CurrentOperator(ctx)
	if e != nil || r.Scope.Validate() != nil || p.TenantID != r.Scope.TenantID || domain.ValidateIdempotencyKey(key) != nil {
		return nil, nil, ErrDenied
	}
	expected, e := strconv.ParseUint(r.Expected, 10, 64)
	if e != nil || expected == ^uint64(0) {
		return nil, nil, ErrConflict
	}
	_, hash, e := harnessPayload(raw)
	if e != nil {
		return nil, nil, e
	}
	ids := []string{r.Scope.TenantID, r.Scope.Kind, r.Scope.Address(), p.CredentialID, family, key}
	keys := append(studioBaseKeys(p, r.Scope), extra...)
	keys = append(keys, LockKey{Rank: 45, Kind: "studio_command", IDs: ids}, LockKey{Rank: 110, Kind: "studio_receipt", IDs: ids})
	tx, e := s.studioLocalTx(ctx, keys)
	if e != nil {
		return nil, nil, e
	}
	good := false
	defer func() {
		if !good {
			tx.Rollback()
		}
	}()
	if e = s.studioCommandGate(ctx, tx, r.Scope, r.Kind); e != nil {
		return nil, nil, e
	}
	var oldHash, receipt []byte
	e = tx.QueryRowContext(ctx, "SELECT payload_hash,receipt FROM studio_settings_commands WHERE tenant_id=? AND scope_type=? AND scope_address=? AND credential_id=? AND family=? AND command_key=?", r.Scope.TenantID, r.Scope.Kind, r.Scope.Address(), p.CredentialID, family, key).Scan(&oldHash, &receipt)
	if e == nil {
		if !bytes.Equal(oldHash, hash[:]) {
			return nil, nil, ErrConflict
		}
		return nil, receipt, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, nil, e
	}
	c := &studioCommand{tx: tx, store: s, principal: p, request: r, family: family, key: key, hash: hash, expected: expected}
	c.keys = keys
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&c.now); e != nil {
		return nil, nil, e
	}
	good = true
	return c, nil, nil
}
// Keep the receipt in the mutation transaction and recheck current authority plus any applicable delegation or stable-maintainer constraint before commit. A commit error is an unknown outcome, not proof that the mutation rolled back.
func (c *studioCommand) complete(ctx context.Context, target any, revision uint64, result any) (generated.StudioLocalCommandReceipt, error) {
	sc := c.request.Scope
	receipt, e := json.Marshal(map[string]any{"scope": sc, "credential_id": c.principal.CredentialID, "family": c.family, "kind": c.request.Kind, "command_key": c.key, "target": target, "result_revision": strconv.FormatUint(revision, 10), "committed_at": c.now.UTC().Format(time.RFC3339Nano), "result": result})
	if e != nil {
		return nil, e
	}
	if e = typedJSON("StudioLocalCommandReceipt", json.RawMessage(receipt)); e != nil {
		return nil, e
	}
	_, e = c.tx.ExecContext(ctx, "INSERT INTO studio_settings_commands(tenant_id,scope_type,product_id,scope_address,credential_id,family,command_key,kind,payload_hash,receipt,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)", sc.TenantID, sc.Kind, sc.ProductValue(), sc.Address(), c.principal.CredentialID, c.family, c.key, c.request.Kind, c.hash[:], receipt, c.now)
	if e != nil {
		return nil, e
	}
	if e = c.store.finishStudioLocal(ctx, c.tx, c.principal); e != nil {
		return nil, e
	}
	if e = c.store.studioCommandGate(ctx, c.tx, sc, c.request.Kind); e != nil {
		return nil, e
	}
	if c.memberGrant != nil {
		ceiling, err := c.store.delegableExpiry(ctx, c.tx, sc, c.memberGrant.Action, c.memberGrant.Use)
		if err != nil {
			return nil, err
		}
		if ceiling != nil && (c.memberGrant.Expires == nil || c.memberGrant.Expires.After(*ceiling)) {
			return nil, ErrDenied
		}
	}
	if c.removedStable {
		valid, err := studioStableMaintainer(ctx, c.tx, c.principal.TenantID, c.principal.CredentialID)
		if err != nil || !valid {
			return nil, ErrDenied
		}
	}
	if c.request.Kind == "SET_CREDENTIAL_STATE" && c.request.Enabled {
		ceiling, err := c.store.delegableExpiry(ctx, c.tx, sc, "maintenance.manage", "MAINTENANCE_COMMAND")
		if err != nil {
			return nil, err
		}
		if ceiling != nil && (c.request.ExpiresAt == nil || c.request.ExpiresAt.After(*ceiling)) {
			return nil, ErrDenied
		}
	}
	// Read the stored JSON representation so first ACK and every replay have
	// identical safe bytes even when MySQL normalizes object formatting.
	if e = c.tx.QueryRowContext(ctx, "SELECT receipt FROM studio_settings_commands WHERE tenant_id=? AND scope_type=? AND scope_address=? AND credential_id=? AND family=? AND command_key=?", sc.TenantID, sc.Kind, sc.Address(), c.principal.CredentialID, c.family, c.key).Scan(&receipt); e != nil {
		return nil, e
	}
	if c.store.Commit != nil {
		e = c.store.Commit(ctx, c.tx)
	} else {
		e = c.tx.Commit()
	}
	if e != nil {
		return nil, ErrCommitUnknown
	}
	return receipt, nil
}
// Read only this credential's original scope, family and key, then authorize the recorded command kind under current authority. This path returns the stored receipt without resubmitting the mutation.
func (s *Store) ReadSettingsCommand(ctx context.Context, sc domain.SettingsScope, family, key string) (generated.StudioLocalCommandReceipt, error) {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	p, e := s.CurrentOperator(ctx)
	if e != nil || sc.Validate() != nil || p.TenantID != sc.TenantID || domain.ValidateIdempotencyKey(key) != nil {
		return nil, ErrDenied
	}
	if family != "SETTINGS" && family != "MEMBER" && family != "SELF_CONSENT" {
		return nil, ErrDenied
	}
	tx, e := s.studioLocalTx(ctx, studioBaseKeys(p, sc))
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var kind string
	var raw []byte
	e = tx.QueryRowContext(ctx, "SELECT kind,receipt FROM studio_settings_commands WHERE tenant_id=? AND scope_type=? AND scope_address=? AND credential_id=? AND family=? AND command_key=?", sc.TenantID, sc.Kind, sc.Address(), p.CredentialID, family, key).Scan(&kind, &raw)
	if e != nil {
		return nil, e
	}
	if e = s.studioCommandGate(ctx, tx, sc, kind); e != nil {
		return nil, e
	}
	return raw, nil
}
func studioRevision(ctx context.Context, q rowQuerier, sc domain.SettingsScope, section string) (uint64, []byte, error) {
	var revision uint64
	var raw []byte
	e := q.QueryRowContext(ctx, "SELECT revision,document FROM studio_scoped_settings WHERE tenant_id=? AND scope_type=? AND scope_address=? AND section=?", sc.TenantID, sc.Kind, sc.Address(), section).Scan(&revision, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return revision, raw, e
}
func writeStudioSection(ctx context.Context, tx *sql.Tx, sc domain.SettingsScope, section string, revision uint64, raw []byte) error {
	_, e := tx.ExecContext(ctx, "INSERT INTO studio_scoped_settings(tenant_id,scope_type,product_id,scope_address,section,revision,document) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE revision=VALUES(revision),document=VALUES(document)", sc.TenantID, sc.Kind, sc.ProductValue(), sc.Address(), section, revision, raw)
	return e
}
