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

func settingsSection(r studioRequest, p domain.OperatorPrincipal) (section string, target map[string]string) {
	switch r.Kind {
	case "SET_DEFAULTS":
		return "DEFAULTS", map[string]string{"section": "DEFAULTS"}
	case "SET_QUOTA_POLICY":
		return "QUOTA", map[string]string{"section": "QUOTA"}
	case "SET_LOCAL_NOTIFICATIONS":
		return "NOTIFICATIONS:" + p.CredentialID, map[string]string{"section": "NOTIFICATIONS", "credential_id": p.CredentialID}
	case "UPSERT_CONNECTION_METADATA", "DISABLE_CONNECTION":
		id := r.ConnectionID
		if id == "" {
			var v struct {
				ID string `json:"connection_id"`
			}
			_ = json.Unmarshal(r.Connection, &v)
			id = v.ID
		}
		return "CONNECTION:" + id, map[string]string{"section": "CONNECTION", "connection_id": id}
	}
	return "", nil
}
// Replay returns before the target revision comparison; a new command compares the locked target's revision and advances only that section or connection. Persist the validated result through the same command transaction and receipt.
func (s *Store) ApplySettings(ctx context.Context, key string, request generated.StudioSettingsCommandRequest) (generated.StudioLocalCommandReceipt, error) {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	r, e := decodeStudio("StudioSettingsCommandRequest", request)
	if e != nil {
		return nil, e
	}
	p, e := s.CurrentOperator(ctx)
	if e != nil {
		return nil, e
	}
	section, target := settingsSection(r, p)
	keys := []LockKey{studioSectionKey(r.Scope, section)}
	if target["section"] == "CONNECTION" {
		keys = []LockKey{{Rank: 90, Kind: domain.StudioConnectionLock, IDs: []string{r.Scope.TenantID, r.Scope.Kind, r.Scope.Address(), target["connection_id"]}}}
	}
	c, original, e := s.beginStudioCommand(ctx, r, "SETTINGS", key, request, keys...)
	if e != nil || original != nil {
		return original, e
	}
	defer c.tx.Rollback()
	revision, _, e := studioRevision(ctx, c.tx, r.Scope, section)
	if target["section"] == "CONNECTION" {
		var raw []byte
		revision, raw, e = studioConnection(ctx, c.tx, r.Scope, target["connection_id"])
		_ = raw
	}
	if e != nil {
		return nil, e
	}
	if revision != c.expected {
		return nil, &StudioConflict{target, c.expected, revision}
	}
	revision++
	var result map[string]any
	switch r.Kind {
	case "SET_DEFAULTS":
		if e = validateStudioDefaults(r.Defaults); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(r.Defaults, &result); e != nil {
			return nil, e
		}
	case "SET_LOCAL_NOTIFICATIONS":
		if e = json.Unmarshal(r.Preference, &result); e != nil {
			return nil, e
		}
	case "SET_QUOTA_POLICY":
		if e = studioReference(ctx, c.tx, r.Scope, "QUOTA_POLICY", r.Quota); e != nil {
			return nil, e
		}
		result = map[string]any{"policy_ref": r.Quota, "hard_limit_source": "Registered policy metadata; existing account limits remain authoritative"}
	case "UPSERT_CONNECTION_METADATA":
		if e = json.Unmarshal(r.Connection, &result); e != nil {
			return nil, e
		}
		if result["revision"] != r.Expected {
			return nil, ErrConflict
		}
		if e = checkStudioConnectionRefs(ctx, c.tx, r.Scope, result); e != nil {
			return nil, e
		}
		if c.expected == 0 {
			var n int
			if e = c.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM studio_connection_metadata WHERE tenant_id=? AND scope_type=? AND scope_address=?", r.Scope.TenantID, r.Scope.Kind, r.Scope.Address()).Scan(&n); e != nil {
				return nil, e
			}
			if n >= 64 {
				return nil, ErrDenied
			}
		}
	case "DISABLE_CONNECTION":
		_, raw, readErr := studioConnection(ctx, c.tx, r.Scope, r.ConnectionID)
		if readErr != nil {
			return nil, readErr
		}
		if raw == nil {
			return nil, ErrConflict
		}
		if e = json.Unmarshal(raw, &result); e != nil {
			return nil, e
		}
		result["status"] = "DISABLED"
	default:
		return nil, ErrDenied
	}
	result["revision"] = strconv.FormatUint(revision, 10)
	raw, e := json.Marshal(result)
	if e != nil {
		return nil, e
	}
	if target["section"] == "CONNECTION" {
		_, e = c.tx.ExecContext(ctx, "INSERT INTO studio_connection_metadata(tenant_id,scope_type,product_id,scope_address,connection_id,revision,document) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE revision=VALUES(revision),document=VALUES(document)", r.Scope.TenantID, r.Scope.Kind, r.Scope.ProductValue(), r.Scope.Address(), target["connection_id"], revision, raw)
	} else {
		e = writeStudioSection(ctx, c.tx, r.Scope, section, revision, raw)
	}
	if e != nil {
		return nil, e
	}
	return c.complete(ctx, target, revision, json.RawMessage(raw))
}
func studioConnection(ctx context.Context, q rowQuerier, sc domain.SettingsScope, id string) (uint64, []byte, error) {
	var revision uint64
	var raw []byte
	e := q.QueryRowContext(ctx, "SELECT revision,document FROM studio_connection_metadata WHERE tenant_id=? AND scope_type=? AND scope_address=? AND connection_id=?", sc.TenantID, sc.Kind, sc.Address(), id).Scan(&revision, &raw)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return revision, raw, e
}
func studioReference(ctx context.Context, q rowQuerier, sc domain.SettingsScope, kind, id string) error {
	var valid bool
	e := q.QueryRowContext(ctx, "SELECT enabled=1 FROM studio_reference_catalog WHERE tenant_id=? AND scope_type=? AND scope_address=? AND ref_kind=? AND ref_id=?", sc.TenantID, sc.Kind, sc.Address(), kind, id).Scan(&valid)
	if e != nil || !valid {
		return ErrDenied
	}
	return nil
}
// Connection fields name registered metadata, not endpoint or secret values. The installed Mock has its fixed descriptor and no aliases; other connections require enabled provider, endpoint-alias and secret-alias references in this scope.
func checkStudioConnectionRefs(ctx context.Context, q rowQuerier, sc domain.SettingsScope, v map[string]any) error {
	pairs := map[string]string{"provider_ref": "PROVIDER"}
	if v["connection_id"] == domain.InstalledMockConnection {
		if v["provider_ref"] != domain.InstalledMockProvider || v["model_id"] != domain.InstalledMockModel || v["endpoint_alias_ref"] != nil || v["secret_alias_ref"] != nil {
			return ErrDenied
		}
		var descriptor string
		if e := q.QueryRowContext(ctx, "SELECT JSON_UNQUOTE(JSON_EXTRACT(document,'$.descriptor_hash')) FROM studio_reference_catalog WHERE tenant_id=? AND scope_type=? AND scope_address=? AND ref_kind='PROVIDER' AND ref_id=?", sc.TenantID, sc.Kind, sc.Address(), domain.InstalledMockProvider).Scan(&descriptor); e != nil || descriptor != domain.InstalledMockDescriptorHash() {
			return ErrDenied
		}
	} else {
		pairs["endpoint_alias_ref"] = "ENDPOINT_ALIAS"
		pairs["secret_alias_ref"] = "SECRET_ALIAS"
	}
	for field, kind := range pairs {
		id, ok := v[field].(string)
		if !ok {
			return ErrDenied
		}
		if e := studioReference(ctx, q, sc, kind, id); e != nil {
			return e
		}
	}
	return nil
}
func studioBasis(ctx context.Context, tx *sql.Tx, s *Store, sc domain.SettingsScope, p domain.OperatorPrincipal) (generated.StudioSettingsBasis, error) {
	var out generated.StudioSettingsBasis
	raw, _ := json.Marshal(sc)
	out.Scope = raw
	out.CredentialID = generated.Identity(p.CredentialID)
	out.AllowedCommands = []generated.StudioSettingsCommandKind{}
	var now time.Time
	if e := tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return out, e
	}
	out.ServerNow = now.UTC().Format(time.RFC3339Nano)
	for _, kind := range []string{"SET_DEFAULTS", "SET_QUOTA_POLICY", "UPSERT_CONNECTION_METADATA", "DISABLE_CONNECTION", "SET_LOCAL_NOTIFICATIONS", "SET_SCOPE_GRANT", "SET_CREDENTIAL_STATE"} {
		if s.studioCommandGate(ctx, tx, sc, kind) == nil {
			out.AllowedCommands = append(out.AllowedCommands, generated.StudioSettingsCommandKind(kind))
		}
	}
	return out, nil
}
// Read the snapshot under current settings authority and recheck it before returning. Command availability is a live projection, while the catalog exposes only reference metadata; storage paths and unmeasured capacity are not disclosed as values.
func (s *Store) ReadSettings(ctx context.Context, sc domain.SettingsScope) (generated.StudioSettingsSnapshot, error) {
	var out generated.StudioSettingsSnapshot
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	p, e := s.CurrentOperator(ctx)
	if e != nil || sc.Validate() != nil || p.TenantID != sc.TenantID {
		return out, ErrDenied
	}
	keys := studioBaseKeys(p, sc)
	for _, section := range []string{"DEFAULTS", "QUOTA", "NOTIFICATIONS:" + p.CredentialID} {
		keys = append(keys, studioSectionKey(sc, section))
	}
	tx, e := s.studioLocalTx(ctx, keys)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if e = studioActionTx(ctx, tx, s, sc, "settings.read", sc.SettingsUse()); e != nil {
		return out, e
	}
	out.Basis, e = studioBasis(ctx, tx, s, sc, p)
	if e != nil {
		return out, e
	}
	out.Defaults = generated.StudioDefaults{Revision: "0", ProductPackRef: json.RawMessage("null"), ModuleDefaults: json.RawMessage("[]")}
	out.QuotaPolicy = generated.StudioQuotaPolicyBinding{Revision: "0", PolicyRef: json.RawMessage("null"), HardLimitSource: "Existing account limits; no registered Studio quota policy selected"}
	out.LocalNotifications = generated.StudioLocalNotificationPreference{Revision: "0", Channel: "LOCAL_DISPLAY", Enabled: false, Events: []string{}, Frequency: "ON_OPEN"}
	for section, value := range map[string]any{"DEFAULTS": &out.Defaults, "QUOTA": &out.QuotaPolicy, "NOTIFICATIONS:" + p.CredentialID: &out.LocalNotifications} {
		_, raw, e := studioRevision(ctx, tx, sc, section)
		if e != nil {
			return out, e
		}
		if raw != nil {
			if e = json.Unmarshal(raw, value); e != nil {
				return out, e
			}
		}
	}
	out.Connections = []generated.StudioConnectionMetadata{}
	rows, e := tx.QueryContext(ctx, "SELECT document FROM studio_connection_metadata WHERE tenant_id=? AND scope_type=? AND scope_address=? ORDER BY connection_id LIMIT 65", sc.TenantID, sc.Kind, sc.Address())
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var raw []byte
		if e = rows.Scan(&raw); e != nil {
			rows.Close()
			return out, e
		}
		out.Connections = append(out.Connections, json.RawMessage(raw))
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(out.Connections) > 64 {
		return out, ErrDenied
	}
	catalog := map[string][]json.RawMessage{"providers": {}, "endpoint_aliases": {}, "secret_aliases": {}, "quota_policies": {}, "product_packs": {}}
	names := map[string]string{"PROVIDER": "providers", "ENDPOINT_ALIAS": "endpoint_aliases", "SECRET_ALIAS": "secret_aliases", "QUOTA_POLICY": "quota_policies", "PRODUCT_PACK": "product_packs"}
	rows, e = tx.QueryContext(ctx, "SELECT ref_kind,ref_id,revision,enabled,document FROM studio_reference_catalog WHERE tenant_id=? AND scope_type=? AND scope_address=? ORDER BY ref_kind,ref_id", sc.TenantID, sc.Kind, sc.Address())
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var kind, id string
		var revision uint64
		var enabled bool
		var raw []byte
		if e = rows.Scan(&kind, &id, &revision, &enabled, &raw); e != nil {
			rows.Close()
			return out, e
		}
		name, ok := names[kind]
		if !ok {
			continue
		}
		var metadata struct {
			Label string `json:"label"`
		}
		if json.Unmarshal(raw, &metadata) != nil || metadata.Label == "" {
			rows.Close()
			return out, ErrDenied
		}
		safe, _ := json.Marshal(map[string]any{"ref_id": id, "revision": strconv.FormatUint(revision, 10), "enabled": enabled, "label": metadata.Label})
		catalog[name] = append(catalog[name], safe)
		if len(catalog[name]) > 128 {
			rows.Close()
			return out, ErrDenied
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	raw, _ := json.Marshal(catalog)
	if e = json.Unmarshal(raw, &out.ReferenceCatalog); e != nil {
		return out, e
	}
	out.InstallationStorage = generated.StudioInstallationStorageMetadata{Adapter: "LOCAL_PRIVATE_FILES", Source: "Local installation configuration; paths are private", State: "UNKNOWN", RestartRequired: true, CapacityBytes: json.RawMessage("null"), CapacitySource: "Capacity is not measured by Studio"}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = studioActionTx(ctx, tx, s, sc, "settings.read", sc.SettingsUse()); e != nil {
		return out, e
	}
	return out, typedJSON("StudioSettingsSnapshot", out)
}
