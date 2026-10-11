package maintenance

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
)

var OperatorActions = []string{"content.read", "content.edit", "content.copy", "content.archive", "content.restore", "content.delete", "content.check", "content.publish", "content.rehearse", "content.assist", "task.read", "task.cancel", "asset.upload", "asset.read", "runtime.create", "runtime.read", "runtime.control", "runtime.migrate"}

// BootstrapSynthetic is an explicit fresh-install maintenance action. A restored
// installation cannot infer a zero ledger or replace an existing credential.
func BootstrapSynthetic(ctx context.Context, s *store.Store, scope domain.ProductScope, credentialID, secret string) error {
	if scope.TenantID != "demo-local" || scope.ProductID != "coffee" || scope.SubjectID != "creator" || credentialID != "owner" || len(secret) < 32 || len(secret) > 512 {
		return errors.New("explicit synthetic owner bootstrap required")
	}
	if e := Compatible(ctx, s); e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var gates int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM subject_governance").Scan(&gates); e != nil {
		return e
	}
	hash := sha256.Sum256([]byte(secret))
	if gates > 0 {
		var h []byte
		if e = tx.QueryRowContext(ctx, "SELECT token_sha256 FROM operator_credentials WHERE credential_id=? AND tenant_id=? AND product_id=? AND subject_id=? AND enabled=1", credentialID, scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&h); e != nil || subtle.ConstantTimeCompare(h, hash[:]) != 1 {
			return errors.New("existing installation cannot be rebootstraped or overwrite operator identity")
		}
		var n int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM governance_watermark WHERE singleton=1").Scan(&n); e != nil || n != 1 {
			return errors.New("restored installation missing governance watermark; zero bootstrap refused")
		}
		return tx.Commit()
	}
	var existing int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tenant_gates").Scan(&existing); e != nil || existing != 0 {
		return errors.New("bootstrap requires an empty synthetic installation")
	}
	for _, q := range []string{"INSERT INTO tenant_gates VALUES('demo-local',1,1)", "INSERT INTO product_gates VALUES('demo-local','coffee',1,1)", "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('demo-local','coffee','creator',1,1,1,1)", "INSERT INTO governance_watermark VALUES(1,0,0,1)"} {
		if _, e = tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO operator_credentials(credential_id,tenant_id,product_id,subject_id,token_sha256,enabled) VALUES(?,?,?,?,?,1)", credentialID, scope.TenantID, scope.ProductID, scope.SubjectID, hash[:]); e != nil {
		return e
	}
	for _, action := range OperatorActions {
		if _, e = tx.ExecContext(ctx, "INSERT INTO operator_actions VALUES(?,?)", credentialID, action); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, "UPDATE subject_governance SET can_control=1,can_migrate=1 WHERE tenant_id=? AND product_id=? AND subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID); e != nil {
		return e
	}
	return tx.Commit()
}
