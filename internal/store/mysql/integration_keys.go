package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
)

func credentialHashKey(h [32]byte) LockKey {
	return LockKey{Rank: 5, Kind: "credential_hash", IDs: []string{hex.EncodeToString(h[:])}}
}

// The exact marker is acquired before every credential writer's higher ranks.
// An existing reservation is only compatible with the same original identity;
// disabling or expiring a credential never releases its hash for another mint.
func reserveCredentialHashTx(ctx context.Context, tx *sql.Tx, h [32]byte, kind, id, installation string, allowOriginal bool) error {
	if len(installation) == 0 {
		return ErrDenied
	}
	var oldKind, oldID, oldInstallation string
	e := tx.QueryRowContext(ctx, "SELECT kind,principal_id,installation_id FROM integration_credential_hashes WHERE token_hash=?", h[:]).Scan(&oldKind, &oldID, &oldInstallation)
	if e == nil {
		if allowOriginal && kind == oldKind && id == oldID && installation == oldInstallation {
			// Replay requires the original owning row, not just its tombstone.
			var original bool
			if kind == "OPERATOR" {
				if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_credentials WHERE credential_id=? AND token_sha256=?)", id, h[:]).Scan(&original); err != nil {
					return err
				}
			}
			if original {
				return nil
			}
		}
		return ErrConflict
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO integration_credential_hashes(token_hash,kind,principal_id,installation_id,created_at) VALUES(?,?,?,?,UTC_TIMESTAMP(6))", h[:], kind, id, installation)
	return e
}

// ReserveBootstrapOperatorHashTx is only called by explicit fresh bootstrap.
// It adds no authority: the caller still proves the original installation and
// empty/current bootstrap tuple in this same transaction before committing.
func ReserveBootstrapOperatorHashTx(ctx context.Context, tx *sql.Tx, identity WorldAccountingIdentity, id, secret string) error {
	h := sha256.Sum256([]byte(secret))
	k := credentialHashKey(h)
	if e := marker(ctx, tx, k); e != nil {
		return e
	}
	if e := lockStudioRows(ctx, tx, k); e != nil {
		return e
	}
	if e := checkConfigurationInstallationTx(ctx, tx, identity); e != nil {
		return e
	}
	return reserveCredentialHashTx(ctx, tx, h, "OPERATOR", id, identity.Installation, true)
}

func checkConfigurationInstallationTx(ctx context.Context, q rowQuerier, id WorldAccountingIdentity) error {
	var current bool
	e := q.QueryRowContext(ctx, "SELECT installation_id=? AND generation=? FROM memory_installation WHERE singleton=1", id.Installation, id.Generation).Scan(&current)
	if e != nil || !current {
		return ErrDenied
	}
	return nil
}
