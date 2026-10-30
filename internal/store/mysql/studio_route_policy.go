package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

func routeScopes(scope domain.ProductScope) []domain.SettingsScope {
	return []domain.SettingsScope{{Kind: "TENANT", TenantID: scope.TenantID}, {Kind: "PRODUCT", TenantID: scope.TenantID, ProductID: &scope.ProductID}}
}
func RoutePolicyLocks(scope domain.ProductScope) []LockKey {
	keys := NewRouteAdmissionLocks(scope)
	for _, sc := range routeScopes(scope) {
		keys = append(keys, studioSectionKey(sc, "DEFAULTS"))
	}
	return keys
}
func NewRouteAdmissionLocks(scope domain.ProductScope) []LockKey {
	var keys []LockKey
	for _, sc := range routeScopes(scope) {
		keys = append(keys, studioScopeKey(sc), LockKey{Rank: 90, Kind: domain.StudioConnectionLock, IDs: []string{sc.TenantID, sc.Kind, sc.Address(), domain.InstalledMockConnection}})
	}
	return keys
}

// Use the unit's frozen policy or the rehearsal's original binding when present. Otherwise read tenant and product defaults only under their planned scope and section locks; a missing lock requires replanning.
func ReadRoutePolicyTx(ctx context.Context, tx Tx, scope domain.ProductScope) (*domain.RoutePolicySnapshot, error) {
	u, ok := tx.(*unit)
	if !ok || u.scope != scope {
		return nil, ErrDenied
	}
	if u.routePolicyFrozen {
		return u.routePolicy, nil
	}
	if u.rehearsalExecution != nil {
		return u.rehearsalExecution.initialization.binding.RoutePolicy, nil
	}
	if u.rehearsalInitialization != nil {
		return u.rehearsalInitialization.binding.RoutePolicy, nil
	}
	if u.rehearsalHistory != nil {
		return u.rehearsalHistory.init.binding.RoutePolicy, nil
	}
	policy := &domain.RoutePolicySnapshot{DescriptorHash: domain.InstalledMockDescriptorHash(), TenantDefaults: []generated.StudioModuleDefault{}, ProductDefaults: []generated.StudioModuleDefault{}}
	for _, sc := range routeScopes(scope) {
		for _, k := range []LockKey{studioScopeKey(sc), studioSectionKey(sc, "DEFAULTS")} {
			if !u.plan[string(lockBytes(k))] {
				return nil, ErrReplan
			}
		}
		revision, raw, e := studioRevision(ctx, u.tx, sc, "DEFAULTS")
		if e != nil {
			return nil, e
		}
		var values struct {
			Defaults []generated.StudioModuleDefault `json:"module_defaults"`
		}
		if raw != nil && json.Unmarshal(raw, &values) != nil {
			return nil, ErrDenied
		}
		if sc.Kind == "TENANT" {
			policy.TenantRevision = revision
			policy.TenantDefaults = values.Defaults
		} else {
			policy.ProductRevision = revision
			policy.ProductDefaults = values.Defaults
		}
	}
	return policy, nil
}

// Only new acquisition/send branches call this. Original query, body evidence
// and accounting recovery never acquire a new purchase via this function.
func CheckNewRouteAdmissionTx(ctx context.Context, tx Tx, scope domain.ProductScope, connection, model string) error {
	u, ok := tx.(*unit)
	if !ok || u.scope != scope {
		return ErrDenied
	}
	for _, k := range NewRouteAdmissionLocks(scope) {
		if !u.plan[string(lockBytes(k))] {
			return ErrReplan
		}
	}
	if e := checkNewRouteAdmission(ctx, u.tx, scope, connection, model); e != nil {
		return e
	}
	u.newRouteAdmission = true
	return nil
}

// Only the installed Mock pair is admitted here. Absent metadata keeps the explicit legacy unmanaged branch; present metadata must pass its status, provider, model and enabled-descriptor checks.
func checkNewRouteAdmission(ctx context.Context, q rowQuerier, scope domain.ProductScope, connection, model string) error {
	if connection != domain.InstalledMockConnection || model != domain.InstalledMockModel {
		return ErrDenied
	}
	for _, sc := range routeScopes(scope) {
		_, raw, e := studioConnection(ctx, q, sc, connection)
		if e != nil {
			return e
		}
		if raw == nil {
			continue
		} // explicit legacy unmanaged installation
		if e = typedJSON("StudioConnectionMetadata", json.RawMessage(raw)); e != nil {
			return ErrDenied
		}
		var metadata map[string]any
		if json.Unmarshal(raw, &metadata) != nil || metadata["status"] == "DISABLED" || metadata["provider_ref"] != domain.InstalledMockProvider || metadata["model_id"] != model {
			return ErrDenied
		}
		var descriptor string
		var enabled bool
		e = q.QueryRowContext(ctx, "SELECT JSON_UNQUOTE(JSON_EXTRACT(document,'$.descriptor_hash')),enabled FROM studio_reference_catalog WHERE tenant_id=? AND scope_type=? AND scope_address=? AND ref_kind='PROVIDER' AND ref_id=?", sc.TenantID, sc.Kind, sc.Address(), domain.InstalledMockProvider).Scan(&descriptor, &enabled)
		if e != nil || !enabled || descriptor != domain.InstalledMockDescriptorHash() {
			return ErrDenied
		}
	}
	return nil
}

// Seed only missing installed-Mock metadata. A conflicting provider descriptor is rejected, existing connection revisions are retained, and a new connection remains UNVERIFIED.
func initializeMockMetadataTx(ctx context.Context, tx *sql.Tx, scope domain.ProductScope) error {
	for _, sc := range routeScopes(scope) {
		var descriptor string
		e := tx.QueryRowContext(ctx, "SELECT JSON_UNQUOTE(JSON_EXTRACT(document,'$.descriptor_hash')) FROM studio_reference_catalog WHERE tenant_id=? AND scope_type=? AND scope_address=? AND ref_kind='PROVIDER' AND ref_id=?", sc.TenantID, sc.Kind, sc.Address(), domain.InstalledMockProvider).Scan(&descriptor)
		if errors.Is(e, sql.ErrNoRows) {
			document, _ := json.Marshal(map[string]any{"label": "Installed synthetic Mock program", "descriptor_hash": domain.InstalledMockDescriptorHash()})
			_, e = tx.ExecContext(ctx, "INSERT INTO studio_reference_catalog(tenant_id,scope_type,product_id,scope_address,ref_kind,ref_id,revision,enabled,document) VALUES(?,?,?,?,'PROVIDER',?,1,1,?)", sc.TenantID, sc.Kind, sc.ProductValue(), sc.Address(), domain.InstalledMockProvider, document)
		} else if e == nil && descriptor != domain.InstalledMockDescriptorHash() {
			return ErrConflict
		}
		if e != nil {
			return e
		}
		revision, _, e := studioConnection(ctx, tx, sc, domain.InstalledMockConnection)
		if e != nil {
			return e
		}
		if revision != 0 {
			continue
		}
		raw, _ := json.Marshal(map[string]any{"connection_id": domain.InstalledMockConnection, "revision": "1", "name": "Local synthetic Mock", "provider_ref": domain.InstalledMockProvider, "model_id": domain.InstalledMockModel, "endpoint_alias_ref": nil, "secret_alias_ref": nil, "status": "UNVERIFIED"})
		_, e = tx.ExecContext(ctx, "INSERT INTO studio_connection_metadata(tenant_id,scope_type,product_id,scope_address,connection_id,revision,document) VALUES(?,?,?,?,?,1,?)", sc.TenantID, sc.Kind, sc.ProductValue(), sc.Address(), domain.InstalledMockConnection, raw)
		if e != nil {
			return e
		}
	}
	return nil
}

// A NONE entry needs no connection here, while INHERIT defers its connection choice. Explicit generation settings stay within the installed Mock pair, TEXT capabilities and NONE external use; this metadata check does not run a provider.
func validateStudioDefaults(raw []byte) error {
	var v struct {
		Defaults []generated.StudioModuleDefault `json:"module_defaults"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ErrDenied
	}
	for _, d := range v.Defaults {
		g := d.GenerationSettings
		if g.Mode == "NONE" {
			continue
		}
		if !domain.MockModule(d.ModuleID) {
			return ErrDenied
		}
		if g.Mode == "INHERIT" {
			continue
		}
		if g.ConnectionRef == nil || g.ModelID == nil || *g.ConnectionRef != domain.InstalledMockConnection || *g.ModelID != domain.InstalledMockModel {
			return ErrDenied
		}
		for _, cap := range g.RequiredCapabilities {
			if cap != "TEXT" {
				return ErrDenied
			}
		}
		for _, use := range g.AllowedExternalUses {
			if use != "NONE" {
				return ErrDenied
			}
		}
	}
	return nil
}
