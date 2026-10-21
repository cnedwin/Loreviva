package mysql

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

// Revocation mutates only the exact persisted CLIENT credential and never its
// permanent hash reservation or the narrow proof used for original accounting.
// Natural expiry is not revocation; no expiry or consent generation is edited.
func revokeClientTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, id WorldAccountingIdentity, plan registryPlan, r *domain.RegistryReceipt) error {
	var hash []byte
	var subject, customer string
	e := tx.QueryRowContext(ctx, "SELECT k.token_hash,k.subject_id,k.customer_id FROM integration_principals k JOIN integration_credential_hashes h ON h.token_hash=k.token_hash AND h.kind='CLIENT' AND h.principal_id=k.principal_id AND h.installation_id=? WHERE k.tenant_id=? AND k.product_id=? AND k.principal_id=? AND k.kind='CLIENT'", id.Installation, p.TenantID, p.ProductID, plan.command.TargetID).Scan(&hash, &subject, &customer)
	if e != nil {
		return ErrDenied
	}
	if subtle.ConstantTimeCompare(hash, plan.oldHash[:]) != 1 || subject != plan.oldSubject || customer != plan.oldCustomer {
		return ErrReplan
	}
	result, e := tx.ExecContext(ctx, "UPDATE integration_principals SET revoked=1,revision=revision+1 WHERE tenant_id=? AND product_id=? AND principal_id=? AND kind='CLIENT' AND customer_id=? AND subject_id=? AND token_hash=? AND revoked=0 AND revision=? AND revision<18446744073709551615", p.TenantID, p.ProductID, plan.command.TargetID, customer, subject, hash, plan.command.ExpectedRevision)
	if e != nil {
		return e
	}
	if e = registryAffected(result); e != nil {
		return e
	}
	r.Revision = plan.command.ExpectedRevision + 1
	return nil
}

func disableQualifiedTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, plan registryPlan, r *domain.RegistryReceipt) error {
	c := plan.command
	customer := c.QualifiedTarget.CustomerID
	var result sql.Result
	var e error
	if c.Kind == "DISABLE_PLAYER_MAPPING" {
		var external []byte
		var subject string
		e = tx.QueryRowContext(ctx, "SELECT external_id,player_subject_id FROM integration_external_subjects WHERE tenant_id=? AND product_id=? AND customer_id=? AND player_subject_id=?", p.TenantID, p.ProductID, customer, c.TargetID).Scan(&external, &subject)
		if e != nil {
			return ErrDenied
		}
		if !bytes.Equal(external, plan.oldExternal) || subject != plan.oldSubject {
			return ErrReplan
		}
		result, e = tx.ExecContext(ctx, "UPDATE integration_external_subjects SET enabled=0,revision=revision+1 WHERE tenant_id=? AND product_id=? AND customer_id=? AND external_id=? AND player_subject_id=? AND revision=? AND revision<18446744073709551615", p.TenantID, p.ProductID, customer, external, subject, c.ExpectedRevision)
	} else {
		var subject string
		e = tx.QueryRowContext(ctx, "SELECT player_subject_id FROM integration_entitlements WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=?", p.TenantID, p.ProductID, customer, c.TargetID).Scan(&subject)
		if e != nil {
			return ErrDenied
		}
		if subject != plan.oldSubject {
			return ErrReplan
		}
		result, e = tx.ExecContext(ctx, "UPDATE integration_entitlements SET enabled=0,revision=revision+1 WHERE tenant_id=? AND product_id=? AND customer_id=? AND entitlement_id=? AND player_subject_id=? AND revision=? AND revision<18446744073709551615", p.TenantID, p.ProductID, customer, c.TargetID, subject, c.ExpectedRevision)
	}
	if e != nil {
		return e
	}
	if e = registryAffected(result); e != nil {
		return e
	}
	r.CustomerID = customer
	r.Revision = c.ExpectedRevision + 1
	return nil
}

func registryAffected(r sql.Result) error {
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}
func rotateServiceTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, id WorldAccountingIdentity, plan registryPlan, now time.Time, r *domain.RegistryReceipt) error {
	v := plan.command.Rotation
	var hash []byte
	var subject string
	var expiry, customerExpiry time.Time
	var revoked, enabled bool
	var revision uint64
	e := tx.QueryRowContext(ctx, "SELECT k.token_hash,k.subject_id,k.expires_at,k.revoked,k.revision,c.enabled,c.expires_at FROM integration_principals k JOIN integration_customers c USING(tenant_id,product_id,customer_id) JOIN integration_credential_hashes h ON h.token_hash=k.token_hash AND h.kind=k.kind AND h.principal_id=k.principal_id WHERE k.tenant_id=? AND k.product_id=? AND k.customer_id=? AND k.principal_id=? AND k.kind='SERVICE' AND h.installation_id=? AND c.service_subject_id=k.subject_id", p.TenantID, p.ProductID, v.CustomerID, v.OldPrincipalID, id.Installation).Scan(&hash, &subject, &expiry, &revoked, &revision, &enabled, &customerExpiry)
	if e != nil || revoked || !enabled || !expiry.After(now) || !customerExpiry.After(now) || expiry.After(customerExpiry) {
		return ErrDenied
	}
	if subtle.ConstantTimeCompare(hash, plan.oldHash[:]) != 1 || subject != plan.oldSubject {
		return ErrReplan
	}
	if revision != plan.command.ExpectedRevision || revision == ^uint64(0) {
		return ErrConflict
	}
	if e = reserveCredentialHashTx(ctx, tx, plan.secretHash, "SERVICE", v.NewPrincipalID, id.Installation, false); e != nil {
		return e
	}
	result, e := tx.ExecContext(ctx, "UPDATE integration_principals SET revoked=1,revision=revision+1 WHERE principal_id=? AND revision=? AND revoked=0", v.OldPrincipalID, revision)
	if e != nil {
		return e
	}
	if e = registryAffected(result); e != nil {
		return e
	}
	if e = insertServiceTx(ctx, tx, p, v.NewPrincipalID, plan.secretHash, v.CustomerID, subject, expiry, now, r.ReceiptRef); e != nil {
		return e
	}
	r.Revision = revision + 1
	return nil
}
func disableCustomerTx(ctx context.Context, tx *sql.Tx, p domain.OperatorPrincipal, c domain.RegistryCommand, r *domain.RegistryReceipt) error {
	result, e := tx.ExecContext(ctx, "UPDATE integration_customers SET enabled=0,revision=revision+1 WHERE tenant_id=? AND product_id=? AND customer_id=? AND revision=? AND revision<18446744073709551615", p.TenantID, p.ProductID, c.TargetID, c.ExpectedRevision)
	if e != nil {
		return e
	}
	if e = registryAffected(result); e != nil {
		return e
	}
	r.Revision = c.ExpectedRevision + 1
	return nil
}
