package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

type StudioConsentOwner interface {
	WriteSelfConsentTx(context.Context, Tx, domain.ProductScope, bool, time.Time) error
}

func studioConsentKey(p domain.OperatorPrincipal) LockKey {
	return LockKey{Rank: 90, Kind: domain.StudioConsentLock, IDs: []string{p.TenantID, p.ProductID, p.SubjectID}}
}
// Consent state and timestamps describe the stored consent record; CurrentUseEnabled is a separate live permission projection.
// An ACTIVE record does not replace current action, source and use checks.
func (s *Store) readSelfConsentTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal) (generated.StudioSelfConsentSnapshot, error) {
	var out generated.StudioSelfConsentSnapshot
	out.Scope.ScopeType = "PRODUCT"
	out.Scope.TenantID = generated.Identity(p.TenantID)
	out.Scope.ProductID = generated.Identity(p.ProductID)
	out.SubjectID = generated.Identity(p.SubjectID)
	out.CredentialID = generated.Identity(p.CredentialID)
	out.PolicyID = "R0_OWNER_PRIVATE_GENERATION_30D"
	out.ConsentRevision = "0"
	out.State = "NOT_GRANTED"
	out.AcceptedAt = json.RawMessage("null")
	out.ExpiresAt = json.RawMessage("null")
	var now time.Time
	if e := tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return out, e
	}
	out.ServerNow = now.UTC().Format(time.RFC3339Nano)
	var revision uint64
	var action string
	var accepted, expires sql.NullTime
	e := tx.QueryRowContext(ctx, "SELECT revision,last_action,accepted_at,expires_at FROM studio_self_consent_current WHERE tenant_id=? AND product_id=? AND subject_id=?", p.TenantID, p.ProductID, p.SubjectID).Scan(&revision, &action, &accepted, &expires)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
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
	case action == "LEGACY_FALSE":
		out.State = "NOT_GRANTED"
	case expires.Valid && expires.Time.After(now):
		out.State = "ACTIVE"
	default:
		out.State = "EXPIRED"
	}
	e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.generation.use' AND allowed_use='PRIVATE_RESPONSE' AND enabled=1 AND expires_at>UTC_TIMESTAMP(6))", p.TenantID, p.ProductID, p.SubjectID, p.SubjectID).Scan(&out.CurrentUseEnabled)
	return out, e
}
func (s *Store) ReadSelfConsent(ctx context.Context, sc domain.SettingsScope) (generated.StudioSelfConsentSnapshot, error) {
	var out generated.StudioSelfConsentSnapshot
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	p, e := s.CurrentOperator(ctx)
	if e != nil || sc.Validate() != nil {
		return out, ErrDenied
	}
	tx, e := s.studioLocalTx(ctx, append(studioBaseKeys(p, sc), studioConsentKey(p)))
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if e = s.studioCommandGate(ctx, tx, sc, "INITIAL"); e != nil {
		return out, e
	}
	out, e = s.readSelfConsentTx(ctx, tx, p)
	if e != nil {
		return out, e
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = s.studioCommandGate(ctx, tx, sc, "INITIAL"); e != nil {
		return out, e
	}
	return out, typedJSON("StudioSelfConsentSnapshot", out)
}
func (s *Store) ApplySelfConsent(ctx context.Context, key string, request generated.StudioSelfConsentCommandRequest) (generated.StudioLocalCommandReceipt, error) {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	r, e := decodeStudio("StudioSelfConsentCommandRequest", request)
	if e != nil {
		return nil, e
	}
	p, e := s.CurrentOperator(ctx)
	if e != nil || s.StudioConsentOwner == nil {
		return nil, ErrDenied
	}
	// An exact original receipt returns before the revision check; new self-permission, consent-fact and legacy-projection writes share this revision-checked transaction.
	c, original, e := s.beginStudioCommand(ctx, r, "SELF_CONSENT", key, request, studioConsentKey(p))
	if e != nil || original != nil {
		return original, e
	}
	defer c.tx.Rollback()
	before, e := s.readSelfConsentTx(ctx, c.tx, p)
	if e != nil {
		return nil, e
	}
	revision, e := strconv.ParseUint(string(before.ConsentRevision), 10, 64)
	if e != nil || revision != c.expected {
		return nil, &StudioConflict{map[string]string{"section": "SELF_CONSENT", "subject_id": p.SubjectID}, c.expected, revision}
	}
	if r.Kind == "INITIAL" && before.State != "NOT_GRANTED" {
		return nil, ErrConflict
	}
	if r.Kind == "RENEW" && before.State == "NOT_GRANTED" {
		return nil, ErrConflict
	}
	enabled := r.Kind != "REVOKE"
	var accepted, expiry any
	useExpiry := c.now
	if enabled {
		accepted = c.now
		useExpiry = c.now.AddDate(0, 0, 30)
		expiry = useExpiry
	} else {
		if string(before.AcceptedAt) != "null" {
			var value string
			_ = json.Unmarshal(before.AcceptedAt, &value)
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				accepted = parsed
			}
		}
	}
	u := &unit{tx: c.tx, plan: map[string]bool{}, operatorStore: s, operation: "STUDIO_SELF_CONSENT", scope: p.ProductScope}
	for _, k := range c.keys {
		u.plan[string(lockBytes(k))] = true
	}
	if e = s.StudioConsentOwner.WriteSelfConsentTx(ctx, u, p.ProductScope, enabled, useExpiry); e != nil {
		return nil, e
	}
	if u.err != nil {
		return nil, u.err
	}
	revision++
	_, e = c.tx.ExecContext(ctx, "INSERT INTO studio_self_consent_current(tenant_id,product_id,subject_id,credential_id,revision,last_action,accepted_at,expires_at,legacy_baseline) VALUES(?,?,?,?,?,?,?,?,0) ON DUPLICATE KEY UPDATE credential_id=VALUES(credential_id),revision=VALUES(revision),last_action=VALUES(last_action),accepted_at=VALUES(accepted_at),expires_at=VALUES(expires_at),legacy_baseline=0", p.TenantID, p.ProductID, p.SubjectID, p.CredentialID, revision, r.Kind, accepted, expiry)
	if e != nil {
		return nil, e
	}
	_, e = c.tx.ExecContext(ctx, "INSERT INTO studio_self_consent_facts VALUES(?,?,?,?,?,?,?,?,?,?)", p.TenantID, p.ProductID, p.SubjectID, revision, p.CredentialID, key, r.Kind, accepted, expiry, c.now)
	if e != nil {
		return nil, e
	}
	// Keep the existing online R0 consent gate synchronized. Its NOT NULL
	// accepted_at is historical storage; false projections remain null above.
	_, e = c.tx.ExecContext(ctx, "INSERT INTO r0_generation_consent(tenant_id,product_id,subject_id,credential_id,policy_id,opted_in,accepted_at,expires_at) VALUES(?,?,?,?,'R0_OWNER_PRIVATE_GENERATION_30D',?,?,?) ON DUPLICATE KEY UPDATE credential_id=VALUES(credential_id),opted_in=VALUES(opted_in),accepted_at=VALUES(accepted_at),expires_at=VALUES(expires_at)", p.TenantID, p.ProductID, p.SubjectID, p.CredentialID, enabled, c.now, expiry)
	if e != nil {
		return nil, e
	}
	result, e := s.readSelfConsentTx(ctx, c.tx, p)
	if e != nil {
		return nil, e
	}
	return c.complete(ctx, map[string]string{"section": "SELF_CONSENT", "subject_id": p.SubjectID}, revision, result)
}
