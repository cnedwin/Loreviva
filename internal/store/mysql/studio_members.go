package mysql

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

type studioGrant struct {
	Scope   domain.SettingsScope `json:"scope"`
	Action  string               `json:"action"`
	Use     string               `json:"use"`
	Effect  string               `json:"effect"`
	Enabled bool                 `json:"enabled"`
	Expires *time.Time           `json:"expires_at"`
}

func sameStudioScope(a, b domain.SettingsScope) bool {
	return a.Kind == b.Kind && a.TenantID == b.TenantID && a.Address() == b.Address()
}
func studioTarget(ctx context.Context, q rowQuerier, sc domain.SettingsScope, id, subject string) (domain.OperatorPrincipal, error) {
	p := domain.OperatorPrincipal{CredentialID: id}
	e := q.QueryRowContext(ctx, "SELECT tenant_id,product_id,subject_id FROM operator_credentials WHERE tenant_id=? AND credential_id=?", sc.TenantID, id).Scan(&p.TenantID, &p.ProductID, &p.SubjectID)
	if e != nil || p.SubjectID != subject || sc.Kind == "PRODUCT" && p.ProductID != *sc.ProductID {
		return p, ErrDenied
	}
	return p, nil
}
func studioStableMaintainer(ctx context.Context, q rowQuerier, tenant, id string) (bool, error) {
	var valid bool
	e := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_credentials c JOIN studio_credential_state v USING(credential_id) JOIN studio_operator_grants g USING(credential_id) JOIN tenant_gates t ON t.tenant_id=c.tenant_id WHERE c.tenant_id=? AND c.credential_id=? AND c.enabled=1 AND t.enabled=1 AND v.expires_at IS NULL AND g.tenant_id=c.tenant_id AND g.scope_type='TENANT' AND g.scope_address='T' AND g.action='maintenance.manage' AND g.allowed_use='MAINTENANCE_COMMAND' AND g.effect='ALLOW' AND g.enabled=1 AND g.expires_at IS NULL)", tenant, id).Scan(&valid)
	return valid, e
}

// Returns the actual current issuer ceiling, including legacy PRODUCT scope.
func (s *Store) delegableExpiry(ctx context.Context, tx *sql.Tx, sc domain.SettingsScope, action, use string) (*time.Time, error) {
	p, e := s.CurrentOperator(ctx)
	if e != nil {
		return nil, e
	}
	if use == "PRODUCT_ADMIN" && sc.Kind == "PRODUCT" && domain.ProductAdminAction(action) {
		if *sc.ProductID != p.ProductID {
			return nil, ErrDenied
		}
		e = operatorActionTx(ctx, tx, s, p.ProductScope, action)
	} else {
		e = studioActionTx(ctx, tx, s, sc, action, use)
	}
	if e != nil {
		return nil, e
	}
	var credential, grant sql.NullTime
	e = tx.QueryRowContext(ctx, "SELECT expires_at FROM studio_credential_state WHERE credential_id=?", p.CredentialID).Scan(&credential)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	e = tx.QueryRowContext(ctx, "SELECT expires_at FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type=? AND scope_address=? AND action=? AND allowed_use=?", sc.TenantID, p.CredentialID, sc.Kind, sc.Address(), action, use).Scan(&grant)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	var ceiling *time.Time
	if credential.Valid {
		v := credential.Time
		ceiling = &v
	}
	if grant.Valid && (ceiling == nil || grant.Time.Before(*ceiling)) {
		v := grant.Time
		ceiling = &v
	}
	return ceiling, nil
}
func (s *Store) ApplyMember(ctx context.Context, key string, request generated.StudioMemberCommandRequest) (generated.StudioLocalCommandReceipt, error) {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	r, e := decodeStudio("StudioMemberCommandRequest", request)
	if e != nil {
		return nil, e
	}
	p, e := s.CurrentOperator(ctx)
	if e != nil {
		return nil, e
	}
	target, e := studioTarget(ctx, s.DB, r.Scope, r.CredentialID, r.SubjectID)
	if e != nil {
		return nil, e
	}
	tenant := domain.SettingsScope{Kind: "TENANT", TenantID: p.TenantID}
	section := "MEMBERS:" + target.CredentialID
	keys := []LockKey{{Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{target.TenantID, target.CredentialID}}, studioGrantKey(target, r.Scope), studioGrantKey(p, tenant), studioGrantKey(target, tenant), studioScopeKey(tenant), {Rank: 30, IDs: []string{target.TenantID, target.ProductID, target.SubjectID}}, studioSectionKey(r.Scope, section)}
	c, original, e := s.beginStudioCommand(ctx, r, "MEMBER", key, request, keys...)
	if e != nil || original != nil {
		return original, e
	}
	defer c.tx.Rollback()
	current, e := studioTarget(ctx, c.tx, r.Scope, r.CredentialID, r.SubjectID)
	if e != nil {
		return nil, e
	}
	if current != target {
		return nil, ErrReplan
	}
	stable, e := studioStableMaintainer(ctx, c.tx, p.TenantID, target.CredentialID)
	if e != nil {
		return nil, e
	}
	// Check the target revision in this locked transaction; if the target loses stable-maintainer status, the caller must still be a stable maintainer at completion.
	revision, _, e := studioRevision(ctx, c.tx, r.Scope, section)
	if e != nil {
		return nil, e
	}
	if r.Kind == "SET_CREDENTIAL_STATE" {
		e = c.tx.QueryRowContext(ctx, "SELECT revision FROM studio_credential_state WHERE credential_id=?", target.CredentialID).Scan(&revision)
		if errors.Is(e, sql.ErrNoRows) {
			revision = 0
		} else if e != nil {
			return nil, e
		}
	}
	if revision != c.expected {
		section := "MEMBERS"
		if r.Kind == "SET_CREDENTIAL_STATE" {
			section = "CREDENTIAL"
		}
		return nil, &StudioConflict{map[string]string{"section": section, "credential_id": target.CredentialID}, c.expected, revision}
	}
	revision++
	var delegated *studioGrant
	if r.Kind == "SET_SCOPE_GRANT" {
		var g studioGrant
		if e = json.Unmarshal(r.Grant, &g); e != nil || !sameStudioScope(g.Scope, r.Scope) {
			return nil, ErrDenied
		}
		ceiling, e := s.delegableExpiry(ctx, c.tx, r.Scope, g.Action, g.Use)
		if e != nil {
			return nil, e
		}
		if g.Expires != nil && !g.Expires.After(c.now) || ceiling != nil && (g.Expires == nil || g.Expires.After(*ceiling)) {
			return nil, ErrDenied
		}
		if g.Action == "maintenance.manage" {
			if e = studioActionTx(ctx, c.tx, s, tenant, "maintenance.manage", "MAINTENANCE_COMMAND"); e != nil {
				return nil, e
			}
		}
		var n int
		if e = c.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type=? AND scope_address=?", r.Scope.TenantID, target.CredentialID, r.Scope.Kind, r.Scope.Address()).Scan(&n); e != nil {
			return nil, e
		}
		var exists bool
		if e = c.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type=? AND scope_address=? AND action=? AND allowed_use=?)", r.Scope.TenantID, target.CredentialID, r.Scope.Kind, r.Scope.Address(), g.Action, g.Use).Scan(&exists); e != nil {
			return nil, e
		}
		if n >= 128 && !exists {
			return nil, ErrDenied
		}
		_, e = c.tx.ExecContext(ctx, "INSERT INTO studio_operator_grants(tenant_id,credential_id,scope_type,product_id,scope_address,action,allowed_use,effect,enabled,expires_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,1) ON DUPLICATE KEY UPDATE effect=VALUES(effect),enabled=VALUES(enabled),expires_at=VALUES(expires_at),revision=revision+1", r.Scope.TenantID, target.CredentialID, r.Scope.Kind, r.Scope.ProductValue(), r.Scope.Address(), g.Action, g.Use, g.Effect, g.Enabled, g.Expires)
		if e != nil {
			return nil, e
		}
		if e = writeStudioSection(ctx, c.tx, r.Scope, section, revision, []byte(`{}`)); e != nil {
			return nil, e
		}
		delegated = &g
	} else {
		if r.ExpiresAt != nil && !r.ExpiresAt.After(c.now) {
			return nil, ErrDenied
		}
		ceiling, err := s.delegableExpiry(ctx, c.tx, tenant, "maintenance.manage", "MAINTENANCE_COMMAND")
		if err != nil {
			return nil, err
		}
		if r.Enabled && ceiling != nil && (r.ExpiresAt == nil || r.ExpiresAt.After(*ceiling)) {
			return nil, ErrDenied
		}
		_, e = c.tx.ExecContext(ctx, "UPDATE operator_credentials SET enabled=? WHERE credential_id=? AND tenant_id=?", r.Enabled, target.CredentialID, p.TenantID)
		if e != nil {
			return nil, e
		}
		_, e = c.tx.ExecContext(ctx, "INSERT INTO studio_credential_state(credential_id,revision,expires_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE revision=VALUES(revision),expires_at=VALUES(expires_at)", target.CredentialID, revision, r.ExpiresAt)
		if e != nil {
			return nil, e
		}
	}
	after, e := studioStableMaintainer(ctx, c.tx, p.TenantID, target.CredentialID)
	if e != nil {
		return nil, e
	}
	if stable && !after {
		caller, e := studioStableMaintainer(ctx, c.tx, p.TenantID, p.CredentialID)
		if e != nil || !caller {
			return nil, ErrDenied
		}
	}
	// A single command must not revoke the permission it relied on before the
	// actual commit. Expiry ceilings are checked again after the test seam too.
	c.memberGrant = delegated
	c.removedStable = stable && !after
	result, e := s.studioMemberTx(ctx, c.tx, r.Scope, target)
	if e != nil {
		return nil, e
	}
	targetSection := "MEMBERS"
	if r.Kind == "SET_CREDENTIAL_STATE" {
		targetSection = "CREDENTIAL"
	}
	return c.complete(ctx, map[string]string{"section": targetSection, "credential_id": target.CredentialID}, revision, result)
}
func (s *Store) studioMemberTx(ctx context.Context, tx *sql.Tx, sc domain.SettingsScope, p domain.OperatorPrincipal) (generated.StudioMemberMetadata, error) {
	out := generated.StudioMemberMetadata{CredentialID: generated.Identity(p.CredentialID), HomeProductID: generated.Identity(p.ProductID), SubjectID: generated.Identity(p.SubjectID), CredentialRevision: "0", GrantSetRevision: "0", ExpiresAt: json.RawMessage("null"), Grants: []generated.StudioGrant{}}
	var revision uint64
	var expires sql.NullTime
	e := tx.QueryRowContext(ctx, "SELECT c.enabled,COALESCE(v.revision,0),v.expires_at FROM operator_credentials c LEFT JOIN studio_credential_state v USING(credential_id) WHERE c.credential_id=? AND c.tenant_id=?", p.CredentialID, p.TenantID).Scan(&out.Enabled, &revision, &expires)
	if e != nil {
		return out, e
	}
	out.CredentialRevision = generated.ExactUnsigned(strconv.FormatUint(revision, 10))
	if expires.Valid {
		out.ExpiresAt, _ = json.Marshal(expires.Time.UTC().Format(time.RFC3339Nano))
	}
	revision, _, e = studioRevision(ctx, tx, sc, "MEMBERS:"+p.CredentialID)
	if e != nil {
		return out, e
	}
	out.GrantSetRevision = generated.ExactUnsigned(strconv.FormatUint(revision, 10))
	rows, e := tx.QueryContext(ctx, "SELECT action,allowed_use,effect,enabled,expires_at,revision FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type=? AND scope_address=? ORDER BY action,allowed_use LIMIT 129", sc.TenantID, p.CredentialID, sc.Kind, sc.Address())
	if e != nil {
		return out, e
	}
	seen := map[string]bool{}
	for rows.Next() {
		var action, use, effect string
		var enabled bool
		var expiry sql.NullTime
		var rev uint64
		if e = rows.Scan(&action, &use, &effect, &enabled, &expiry, &rev); e != nil {
			rows.Close()
			return out, e
		}
		var when any
		if expiry.Valid {
			when = expiry.Time.UTC().Format(time.RFC3339Nano)
		}
		raw, _ := json.Marshal(map[string]any{"scope": sc, "action": action, "use": use, "effect": effect, "enabled": enabled, "expires_at": when, "revision": strconv.FormatUint(rev, 10)})
		out.Grants = append(out.Grants, raw)
		seen[action+"/"+use] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if sc.Kind == "PRODUCT" && p.ProductID == *sc.ProductID {
		rows, e = tx.QueryContext(ctx, "SELECT action FROM operator_actions WHERE credential_id=? ORDER BY action", p.CredentialID)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var action string
			if e = rows.Scan(&action); e != nil {
				rows.Close()
				return out, e
			}
			if !domain.ProductAdminAction(action) || seen[action+"/PRODUCT_ADMIN"] {
				continue
			}
			raw, _ := json.Marshal(map[string]any{"scope": sc, "action": action, "use": "PRODUCT_ADMIN", "effect": "ALLOW", "enabled": true, "expires_at": nil, "revision": "0"})
			out.Grants = append(out.Grants, raw)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
	}
	if len(out.Grants) > 128 {
		return out, ErrDenied
	}
	return out, typedJSON("StudioMemberMetadata", out)
}

type studioMemberCursor struct {
	Scope                    domain.SettingsScope
	Credential, After, Stamp string
	Limit                    int
}

func studioCursorCipher(ctx context.Context) (cipher.AEAD, error) {
	p, ok := ctx.Value(operatorContextKey{}).(currentOperator)
	if !ok {
		return nil, ErrDenied
	}
	key := sha256.Sum256(append([]byte("studio-members-cursor/v1/"), p.bearerHash[:]...))
	block, e := aes.NewCipher(key[:])
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(block)
}
func sealStudioCursor(ctx context.Context, c studioMemberCursor) (string, error) {
	a, e := studioCursorCipher(ctx)
	if e != nil {
		return "", e
	}
	raw, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	nonce := make([]byte, a.NonceSize())
	if _, e = io.ReadFull(rand.Reader, nonce); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(a.Seal(nonce, nonce, raw, nil)), nil
}
func openStudioCursor(ctx context.Context, token string) (studioMemberCursor, error) {
	var c studioMemberCursor
	a, e := studioCursorCipher(ctx)
	if e != nil {
		return c, e
	}
	raw, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(raw) < a.NonceSize() {
		return c, ErrDenied
	}
	plain, e := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], nil)
	if e != nil {
		return c, ErrDenied
	}
	e = json.Unmarshal(plain, &c)
	return c, e
}
func (s *Store) ListMembers(ctx context.Context, sc domain.SettingsScope, limit int, token string) (generated.StudioMemberPage, error) {
	var out generated.StudioMemberPage
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	p, e := s.CurrentOperator(ctx)
	if e != nil || sc.Validate() != nil || limit < 1 || limit > 100 {
		return out, ErrDenied
	}
	// Bind continuation to this credential, scope, limit and authority stamp; recheck members.manage before returning the page.
	cursor := studioMemberCursor{Scope: sc, Credential: p.CredentialID, Limit: limit}
	if token != "" {
		cursor, e = openStudioCursor(ctx, token)
		if e != nil || !sameStudioScope(cursor.Scope, sc) || cursor.Credential != p.CredentialID || cursor.Limit != limit || domain.ValidateID(cursor.After) != nil {
			return out, ErrDenied
		}
	}
	// Precollect page row identities before holding the transaction. Repeat the
	// bounded selection under the scope lock; a change requires whole replanning.
	selectMembers := func(q interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}) ([]domain.OperatorPrincipal, error) {
		query := "SELECT credential_id,tenant_id,product_id,subject_id FROM operator_credentials WHERE tenant_id=? AND credential_id>?"
		args := []any{sc.TenantID, cursor.After}
		if sc.Kind == "PRODUCT" {
			query += " AND product_id=?"
			args = append(args, *sc.ProductID)
		}
		query += " ORDER BY credential_id LIMIT ?"
		args = append(args, limit+1)
		rows, e := q.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		var list []domain.OperatorPrincipal
		for rows.Next() {
			var v domain.OperatorPrincipal
			if e = rows.Scan(&v.CredentialID, &v.TenantID, &v.ProductID, &v.SubjectID); e != nil {
				return nil, e
			}
			list = append(list, v)
		}
		return list, rows.Err()
	}
	planned, e := selectMembers(s.DB)
	if e != nil {
		return out, e
	}
	keys := studioBaseKeys(p, sc)
	for _, v := range planned {
		keys = append(keys, LockKey{Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{v.TenantID, v.CredentialID}}, studioGrantKey(v, sc), studioSectionKey(sc, "MEMBERS:"+v.CredentialID))
	}
	tx, e := s.studioLocalTx(ctx, keys)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if e = studioActionTx(ctx, tx, s, sc, "members.manage", sc.SettingsUse()); e != nil {
		return out, e
	}
	current, e := selectMembers(tx)
	if e != nil {
		return out, e
	}
	if len(current) != len(planned) {
		return out, ErrReplan
	}
	for i := range current {
		if current[i] != planned[i] {
			return out, ErrReplan
		}
	}
	out.Basis, e = studioBasis(ctx, tx, s, sc, p)
	if e != nil {
		return out, e
	}
	out.Items = []generated.StudioMemberMetadata{}
	out.DelegablePairs = []generated.StudioDelegableGrant{}
	own, e := s.studioMemberTx(ctx, tx, sc, p)
	if e != nil {
		return out, e
	}
	for _, raw := range own.Grants {
		var g studioGrant
		if json.Unmarshal(raw, &g) != nil {
			return out, ErrDenied
		}
		expiry, e := s.delegableExpiry(ctx, tx, sc, g.Action, g.Use)
		if e != nil {
			continue
		}
		var when any
		if expiry != nil {
			when = expiry.UTC().Format(time.RFC3339Nano)
		}
		pair, _ := json.Marshal(map[string]any{"scope": sc, "action": g.Action, "use": g.Use, "max_expires_at": when})
		out.DelegablePairs = append(out.DelegablePairs, pair)
	}
	var tenantVersion, productVersion, subjectVersion uint64
	if e = tx.QueryRowContext(ctx, "SELECT permission_version FROM tenant_gates WHERE tenant_id=?", sc.TenantID).Scan(&tenantVersion); e != nil {
		return out, e
	}
	if sc.Kind == "PRODUCT" {
		if e = tx.QueryRowContext(ctx, "SELECT p.permission_version,g.permission_version FROM product_gates p JOIN subject_governance g USING(tenant_id,product_id) WHERE p.tenant_id=? AND p.product_id=? AND g.subject_id=?", sc.TenantID, *sc.ProductID, p.SubjectID).Scan(&productVersion, &subjectVersion); e != nil {
			return out, e
		}
	}
	_, stamp, e := harnessPayload([]any{own, out.DelegablePairs, tenantVersion, productVersion, subjectVersion})
	if e != nil {
		return out, e
	}
	stampText := hex.EncodeToString(stamp[:])
	if token != "" && cursor.Stamp != stampText {
		return out, ErrConflict
	}
	for i, v := range current {
		if i == limit {
			break
		}
		item, e := s.studioMemberTx(ctx, tx, sc, v)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, item)
	}
	if len(current) > limit {
		cursor.After = current[limit-1].CredentialID
		cursor.Stamp = stampText
		next, e := sealStudioCursor(ctx, cursor)
		if e != nil {
			return out, e
		}
		out.NextCursor = &next
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = studioActionTx(ctx, tx, s, sc, "members.manage", sc.SettingsUse()); e != nil {
		return out, e
	}
	return out, typedJSON("StudioMemberPage", out)
}
