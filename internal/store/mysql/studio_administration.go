package mysql

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"loreviva.local/loreviva/internal/domain"
)

func studioGrantKey(p domain.OperatorPrincipal, scope domain.SettingsScope) LockKey {
	return LockKey{Rank: 25, Kind: domain.StudioGrantsLock, IDs: []string{scope.TenantID, p.CredentialID, scope.Kind, scope.Address()}}
}
func studioScopeKey(scope domain.SettingsScope) LockKey {
	return LockKey{Rank: 35, Kind: domain.StudioScopeLock, IDs: []string{scope.TenantID, scope.Kind, scope.Address()}}
}

// Require the current operator and exact scope/action/use grant together. PRODUCT requests retain the operator's home product and live product/subject gates; TENANT maintenance uses its separate registered pair.
func studioActionTx(ctx context.Context, q rowQuerier, s *Store, scope domain.SettingsScope, action, use string) error {
	p, e := s.CurrentOperator(ctx)
	if e != nil || scope.Validate() != nil || scope.TenantID != p.TenantID {
		return ErrDenied
	}
	if _, e = currentOperatorTx(ctx, q, s, p.ProductScope); e != nil {
		return e
	}
	if scope.Kind == "PRODUCT" {
		// PRODUCT_ADMIN and ordinary content retain the registered home product.
		if *scope.ProductID != p.ProductID {
			return ErrDenied
		}
		var current bool
		if e = q.QueryRowContext(ctx, "SELECT p.enabled AND g.enabled FROM product_gates p JOIN subject_governance g USING(tenant_id,product_id) WHERE p.tenant_id=? AND p.product_id=? AND g.subject_id=?", scope.TenantID, *scope.ProductID, p.SubjectID).Scan(&current); e != nil || !current {
			return ErrDenied
		}
	}
	valid := domain.StudioSettingsAction(action) && use == scope.SettingsUse() || scope.Kind == "TENANT" && action == "maintenance.manage" && use == "MAINTENANCE_COMMAND"
	if !valid {
		return ErrDenied
	}
	var current bool
	e = q.QueryRowContext(ctx, "SELECT effect='ALLOW' AND enabled=1 AND (expires_at IS NULL OR expires_at>UTC_TIMESTAMP(6)) FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type=? AND scope_address=? AND action=? AND allowed_use=?", scope.TenantID, p.CredentialID, scope.Kind, scope.Address(), action, use).Scan(&current)
	if e != nil || !current {
		return ErrDenied
	}
	return nil
}

// SeedFreshAdministrationTx is used solely inside explicit empty-install
// bootstrap or the authenticated legacy initialization below. Reads/Open/restore
// never call it. All settings and maintenance grants are exact registered pairs.
func SeedFreshAdministrationTx(ctx context.Context, tx *sql.Tx, scope domain.ProductScope, credential string) error {
	if scope.TenantID != "demo-local" || scope.ProductID != "coffee" || scope.SubjectID != "creator" || credential != "owner" {
		return ErrDenied
	}
	if _, e := tx.ExecContext(ctx, "INSERT INTO studio_credential_state(credential_id,revision,expires_at) VALUES(?,1,NULL)", credential); e != nil {
		return e
	}
	for _, kind := range []string{"TENANT", "PRODUCT"} {
		sc := domain.SettingsScope{Kind: kind, TenantID: scope.TenantID}
		if kind == "PRODUCT" {
			sc.ProductID = &scope.ProductID
		}
		for _, a := range []string{"settings.read", "settings.defaults.write", "settings.quota.write", "settings.connection.write", "settings.notifications.write", "members.manage"} {
			if _, e := tx.ExecContext(ctx, "INSERT INTO studio_operator_grants(tenant_id,credential_id,scope_type,product_id,scope_address,action,allowed_use,effect,enabled,expires_at,revision) VALUES(?,?,?,?,?,?,?,'ALLOW',1,NULL,1)", sc.TenantID, credential, sc.Kind, sc.ProductValue(), sc.Address(), a, sc.SettingsUse()); e != nil {
				return e
			}
		}
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO studio_operator_grants(tenant_id,credential_id,scope_type,product_id,scope_address,action,allowed_use,effect,enabled,expires_at,revision) VALUES(?,?,'TENANT',NULL,'T','maintenance.manage','MAINTENANCE_COMMAND','ALLOW',1,NULL,1)", scope.TenantID, credential)
	if e != nil {
		return e
	}
	if e = initializeMockMetadataTx(ctx, tx, scope); e != nil {
		return e
	}
	return studioConsentBaseline(ctx, tx)
}

// Initialize only the uninitialized local administration for the original bootstrap identity. Existing administration rows are not repaired or replaced; the original capability must still be current.
func (s *Store) InitializeLocalAdministration(ctx context.Context) error {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer done()
	p, e := s.CurrentOperator(ctx)
	if e != nil || p.CredentialID != "owner" || p.TenantID != "demo-local" || p.ProductID != "coffee" || p.SubjectID != "creator" {
		return ErrDenied
	}
	tenant := domain.SettingsScope{Kind: "TENANT", TenantID: p.TenantID}
	product := domain.SettingsScope{Kind: "PRODUCT", TenantID: p.TenantID, ProductID: &p.ProductID}
	keys := []LockKey{{Rank: 10, IDs: []string{p.TenantID}}, {Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{p.TenantID, p.CredentialID}}, studioGrantKey(p, tenant), studioGrantKey(p, product), studioScopeKey(tenant), studioScopeKey(product)}
	keys = append(keys, NewRouteAdmissionLocks(p.ProductScope)...)
	tx, e := s.studioLocalTx(ctx, keys)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = currentOperatorTx(ctx, tx, s, p.ProductScope); e != nil {
		return e
	}
	var n int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM studio_operator_grants WHERE tenant_id=?", p.TenantID).Scan(&n); e != nil || n != 0 {
		return ErrDenied
	}
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM studio_credential_state WHERE credential_id=?", p.CredentialID).Scan(&n); e != nil || n != 0 {
		return ErrDenied
	}
	var original bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM r0_generation_consent WHERE tenant_id=? AND product_id=? AND subject_id=? AND credential_id=? AND policy_id='R0_OWNER_PRIVATE_GENERATION_30D')", p.TenantID, p.ProductID, p.SubjectID, p.CredentialID).Scan(&original); e != nil || !original {
		return ErrDenied
	}
	// Original local bootstrap capability must still be current; registry presence
	// alone is insufficient to acquire new administration privileges.
	if e = operatorActionTx(ctx, tx, s, p.ProductScope, "content.read"); e != nil {
		return e
	}
	if e = SeedFreshAdministrationTx(ctx, tx, p.ProductScope, p.CredentialID); e != nil {
		return e
	}
	if e = s.finishStudioLocal(ctx, tx, p); e != nil {
		return e
	}
	return tx.Commit()
}

// Register only under current tenant maintenance authority for an existing live subject. Reusing an identity requires the same secret hash; this operation does not grant actions, and authority is checked again before commit.
func (s *Store) RegisterLocalCredential(ctx context.Context, scope domain.ProductScope, id, secret string) error {
	ctx, installation, release, e := s.EnterConfiguration(ctx)
	if e != nil {
		return e
	}
	defer release()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return e
	}
	defer done()
	p, e := s.CurrentOperator(ctx)
	if e != nil || scope.Validate() != nil || scope.TenantID != p.TenantID || domain.ValidateID(id) != nil || len(secret) < 32 || len(secret) > 512 {
		return ErrDenied
	}
	tenant := domain.SettingsScope{Kind: "TENANT", TenantID: p.TenantID}
	h := sha256.Sum256([]byte(secret))
	keys := []LockKey{{Rank: 10, IDs: []string{p.TenantID}}, {Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{p.TenantID, p.CredentialID}}, {Rank: 15, Kind: domain.StudioCredentialLock, IDs: []string{p.TenantID, id}}, studioGrantKey(p, tenant), {Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}}
	keys = append(keys, credentialHashKey(h))
	tx, e := s.studioLocalTx(ctx, keys)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = studioActionTx(ctx, tx, s, tenant, "maintenance.manage", "MAINTENANCE_COMMAND"); e != nil {
		return e
	}
	var exists bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? AND enabled=1)", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&exists); e != nil || !exists {
		return ErrDenied
	}
	if e = checkConfigurationInstallationTx(ctx, tx, installation); e != nil {
		return e
	}
	if e = reserveCredentialHashTx(ctx, tx, h, "OPERATOR", id, installation.Installation, true); e != nil {
		return e
	}
	var old []byte
	e = tx.QueryRowContext(ctx, "SELECT token_sha256 FROM operator_credentials WHERE credential_id=? AND tenant_id=? AND product_id=? AND subject_id=?", id, scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&old)
	if e == nil {
		if subtle.ConstantTimeCompare(h[:], old) != 1 {
			return ErrConflict
		}
	} else if e == sql.ErrNoRows {
		if _, e = tx.ExecContext(ctx, "INSERT INTO operator_credentials(credential_id,tenant_id,product_id,subject_id,token_sha256,enabled) VALUES(?,?,?,?,?,1)", id, scope.TenantID, scope.ProductID, scope.SubjectID, h[:]); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO studio_credential_state(credential_id,revision,expires_at) VALUES(?,1,NULL)", id); e != nil {
			return e
		}
	} else {
		return e
	}
	if e = s.finishStudioLocal(ctx, tx, p); e != nil {
		return e
	}
	if e = studioActionTx(ctx, tx, s, tenant, "maintenance.manage", "MAINTENANCE_COMMAND"); e != nil {
		return e
	}
	return tx.Commit()
}

// Plan the locks before opening the short transaction, acquire each marker with its physical rows, then require a ready governance watermark. Any acquisition or readiness failure rolls the transaction back.
func (s *Store) studioLocalTx(ctx context.Context, keys []LockKey) (*sql.Tx, error) {
	keys, e := PlanLocks(keys)
	if e != nil {
		return nil, e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			tx.Rollback()
			return nil, e
		}
		if e = lockStudioRows(ctx, tx, k); e != nil {
			tx.Rollback()
			return nil, e
		}
	}
	var ready bool
	if e = tx.QueryRowContext(ctx, "SELECT applied_watermark>=required_watermark AND (fresh_synthetic=1 OR required_watermark>0) FROM governance_watermark WHERE singleton=1").Scan(&ready); e != nil || !ready {
		tx.Rollback()
		return nil, ErrDenied
	}
	return tx, nil
}

// Marker and physical row acquisition occur at the same registered rank.
// Missing rows remain protected by their exact marker/scope identity.
func lockStudioRows(ctx context.Context, tx *sql.Tx, k LockKey) error {
	var query string
	var args []any
	for _, id := range k.IDs {
		args = append(args, id)
	}
	switch k.Kind {
	case "integration_registrar":
		return studioLockRows(ctx, tx, "SELECT credential_id FROM integration_registrars WHERE tenant_id=? AND credential_id=? AND product_id=? FOR UPDATE", args...)
	case "integration_principal":
		return studioLockRows(ctx, tx, "SELECT principal_id FROM integration_principals WHERE principal_id=? FOR UPDATE", args...)
	case "integration_customer":
		return studioLockRows(ctx, tx, "SELECT customer_id FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=? FOR UPDATE", args...)
	case "integration_entitlement":
		return studioLockRows(ctx, tx, "SELECT entitlement_id FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=? FOR UPDATE", args...)
	case "integration_mapping":
		return studioLockRows(ctx, tx, "SELECT player_subject_id FROM integration_external_subjects WHERE tenant_id=? AND product_id=? AND customer_id=? AND SHA2(external_id,256)=? FOR UPDATE", args...)
	case "integration_command", "integration_receipt":
		return studioLockRows(ctx, tx, "SELECT original_key FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? AND scope_kind='REGISTRY' FOR UPDATE", args...)
	}
	switch k.Rank {
	case 5:
		return studioLockRows(ctx, tx, "SELECT token_hash FROM integration_credential_hashes WHERE token_hash=UNHEX(?) FOR UPDATE", k.IDs[0])
	case 10:
		query = "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE"
	case 15:
		if e := studioLockRows(ctx, tx, "SELECT credential_id FROM operator_credentials WHERE tenant_id=? AND credential_id=? FOR UPDATE", args...); e != nil {
			return e
		}
		return studioLockRows(ctx, tx, "SELECT credential_id FROM studio_credential_state WHERE credential_id=? FOR UPDATE", k.IDs[1])
	case 20:
		query = "SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE"
	case 25:
		query = "SELECT action FROM studio_operator_grants WHERE tenant_id=? AND credential_id=? AND scope_type=? AND scope_address=? ORDER BY action,allowed_use FOR UPDATE"
	case 30:
		if e := studioLockRows(ctx, tx, "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", args...); e != nil {
			return e
		}
		return studioLockRows(ctx, tx, "SELECT action FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND data_subject_id=? ORDER BY caller_subject_id,action,allowed_use FOR UPDATE", args...)
	case 45, 110:
		query = "SELECT command_key FROM studio_settings_commands WHERE tenant_id=? AND scope_type=? AND scope_address=? AND credential_id=? AND family=? AND command_key=? FOR UPDATE"
	case 70:
		return studioLockRows(ctx, tx, "SELECT account_id FROM budget_accounts WHERE kind=? AND scope_key=? AND account_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1])
	case 90:
		switch k.Kind {
		case "content_release":
			return studioLockRows(ctx, tx, "SELECT release_id FROM releases WHERE tenant_id=? AND product_id=? AND release_id=? FOR UPDATE", args...)
		case domain.StudioSettingsLock:
			query = "SELECT section FROM studio_scoped_settings WHERE tenant_id=? AND scope_type=? AND scope_address=? AND section=? FOR UPDATE"
		case domain.StudioConnectionLock:
			query = "SELECT connection_id FROM studio_connection_metadata WHERE tenant_id=? AND scope_type=? AND scope_address=? AND connection_id=? FOR UPDATE"
		case domain.StudioConsentLock:
			query = "SELECT subject_id FROM studio_self_consent_current WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE"
		}
	}
	if query == "" {
		return nil
	}
	return studioLockRows(ctx, tx, query, args...)
}
func studioLockRows(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	rows, e := tx.QueryContext(ctx, query, args...)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var value []byte
		if e = rows.Scan(&value); e != nil {
			return e
		}
	}
	return rows.Err()
}
func (s *Store) finishStudioLocal(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal) error {
	if s.BeforeCommit != nil {
		if e := s.BeforeCommit(ctx, tx); e != nil {
			return e
		}
	}
	_, e := currentOperatorTx(ctx, tx, s, p.ProductScope)
	return e
}
