package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

// Baselines are historical facts, never fresh grants. DML and the applied marker
// commit together after independently retryable DDL has been verified.
func finalizeStudioMigration(ctx context.Context, c *sql.Conn, sum [32]byte) error {
	tx, e := c.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = studioConsentBaseline(ctx, tx); e != nil {
		return e
	}
	if e = studioIssuerBaseline(ctx, tx); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,sha256) VALUES(7,?)", sum[:]); e != nil {
		return e
	}
	return tx.Commit()
}

// Backfill only jobs whose issuer fields are both absent, defaulting to UNKNOWN_LEGACY. Assign an owner or program provenance only from the matching historical intake; this baseline creates no current grant.
func studioIssuerBaseline(ctx context.Context, tx *sql.Tx) error {
	rows, e := tx.QueryContext(ctx, `SELECT t.tenant_id,t.product_id,t.owner_subject_id,t.task_id,t.task_kind,j.operation,COALESCE(t.world_instance_id,'') FROM registered_jobs j JOIN tasks t USING(tenant_id,product_id,task_id) WHERE j.issuer_provenance IS NULL AND j.issuer_credential_id IS NULL`)
	if e != nil {
		return e
	}
	type job struct {
		scope                      domain.ProductScope
		id, kind, operation, world string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if e = rows.Scan(&j.scope.TenantID, &j.scope.ProductID, &j.scope.SubjectID, &j.id, &j.kind, &j.operation, &j.world); e != nil {
			rows.Close()
			return e
		}
		jobs = append(jobs, j)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, j := range jobs {
		provenance := "UNKNOWN_LEGACY"
		var credential any
		switch {
		case j.kind == "ADMIN" && j.world == "" && (j.operation == "CANDIDATE" || j.operation == "REHEARSAL"):
			valid, e := studioLegacyOwnerIntake(ctx, tx, j.scope, j.id, j.operation)
			if e != nil {
				return e
			}
			if valid {
				provenance = "LEGACY_ORIGINAL_OWNER"
				credential = "owner"
			}
		case j.kind == "ADMIN" && j.world == "" && j.operation == "FILE_CLEANUP":
			e := inspectLegacyAdminIntake(ctx, tx, j.scope, AdminTaskMetadata{ID: j.id, Kind: j.operation})
			if e == nil {
				provenance = "PROGRAM_OWNED"
			} else if !errors.Is(e, ErrConflict) && !errors.Is(e, ErrDenied) && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		case j.kind == "GOVERNANCE" && j.world != "" && j.operation == "MEMORY_GOVERNANCE":
			// This is the program's obligation to finish an already accepted privacy
			// intent. Credential revocation does not erase that intent or its cleanup.
			var hash []byte
			var world, owner string
			e := tx.QueryRowContext(ctx, "SELECT world_instance_id,owner_subject_id,request_hash FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?", j.scope.TenantID, j.scope.ProductID, j.id).Scan(&world, &owner, &hash)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if e == nil && world == j.world && owner == j.scope.SubjectID && len(hash) == 32 {
				r, e := queryReceipt(ctx, tx, turnIdentity(j.id, "accept"))
				if e != nil && !errors.Is(e, sql.ErrNoRows) {
					return e
				}
				if e == nil && r.tenant == j.scope.TenantID && r.product == j.scope.ProductID && r.owner == owner && r.kind == "WORLD" && r.world.String == world && r.phase == "COMMIT" && r.operation == "MEMORY_GOVERNANCE_ACCEPT" && bytes.Equal(r.hash, hash) && j.id == turnIdentity(r.tenant, r.product, owner, world, "MEMORY_GOVERNANCE", r.command) {
					provenance = "PROGRAM_OWNED"
				}
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE registered_jobs SET issuer_provenance=?,issuer_credential_id=? WHERE tenant_id=? AND product_id=? AND task_id=? AND issuer_provenance IS NULL AND issuer_credential_id IS NULL", provenance, credential, j.scope.TenantID, j.scope.ProductID, j.id); e != nil {
			return e
		}
	}
	return nil
}

func studioLegacyOwnerIntake(ctx context.Context, tx *sql.Tx, s domain.ProductScope, id, family string) (bool, error) {
	valid, e := studioExactDraftIntake(ctx, tx, s, id, family)
	if e != nil || !valid {
		return valid, e
	}
	var exact bool
	e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_credentials WHERE credential_id='owner' AND tenant_id=? AND product_id=? AND subject_id=?)", s.TenantID, s.ProductID, s.SubjectID).Scan(&exact)
	return exact, e
}

// Recognize only CANDIDATE or REHEARSAL with the exact original task, accepted key, owner and payload hash. The matching COMMIT receipt must agree on scope and operation; missing history is not reconstructed proof.
func studioExactDraftIntake(ctx context.Context, tx rowQuerier, s domain.ProductScope, id, family string) (bool, error) {
	table := "harness_admin_candidates"
	if family == "REHEARSAL" {
		table = "harness_admin_rehearsals"
	} else if family != "CANDIDATE" {
		return false, nil
	}
	var key, original, owner string
	var hash []byte
	e := tx.QueryRowContext(ctx, "SELECT accepted_commit_key,original_key,owner_subject_id,command_hash FROM "+table+" WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, id).Scan(&key, &original, &owner, &hash)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if owner != s.SubjectID || id != turnIdentity(s.TenantID, s.ProductID, s.SubjectID, family, original) || key != turnIdentity(id, "accepted") || len(hash) != 32 {
		return false, nil
	}
	r, e := queryReceipt(ctx, tx, key)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if r.tenant != s.TenantID || r.product != s.ProductID || r.owner != owner || r.kind != "PRODUCT" || r.world.Valid || r.command != original || r.operation != family+"_INTAKE" || r.phase != "COMMIT" || !bytes.Equal(hash, r.hash) {
		return false, nil
	}
	return true, nil
}

func studioConsentBaseline(ctx context.Context, tx *sql.Tx) error {
	// The false bootstrap's non-null accepted_at records installation, not consent.
	// Expired and revoked-credential positive history stays historical unchanged.
	_, e := tx.ExecContext(ctx, `INSERT INTO studio_self_consent_current(tenant_id,product_id,subject_id,credential_id,revision,last_action,accepted_at,expires_at,legacy_baseline)
 SELECT c.tenant_id,c.product_id,c.subject_id,c.credential_id,1,IF(c.opted_in,'LEGACY_TRUE','LEGACY_FALSE'),IF(c.opted_in,c.accepted_at,NULL),IF(c.opted_in,c.expires_at,NULL),1
 FROM r0_generation_consent c WHERE NOT EXISTS(SELECT 1 FROM studio_self_consent_current s WHERE s.tenant_id=c.tenant_id AND s.product_id=c.product_id AND s.subject_id=c.subject_id)`)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO studio_self_consent_facts(tenant_id,product_id,subject_id,revision,credential_id,command_key,action,accepted_at,expires_at,recorded_at)
 SELECT c.tenant_id,c.product_id,c.subject_id,c.revision,c.credential_id,NULL,c.last_action,c.accepted_at,c.expires_at,UTC_TIMESTAMP(6)
 FROM studio_self_consent_current c WHERE c.revision=1 AND c.legacy_baseline=1 AND c.last_action IN ('LEGACY_TRUE','LEGACY_FALSE') AND NOT EXISTS(SELECT 1 FROM studio_self_consent_facts f WHERE f.tenant_id=c.tenant_id AND f.product_id=c.product_id AND f.subject_id=c.subject_id AND f.revision=c.revision)`)
	return e
}
