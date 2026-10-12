package files

import (
	"context"
	"crypto/sha256"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"strconv"
	"time"
)

type CleanupProgress struct {
	Cursor  uint64
	Done    bool
	Removed int
}

// CleanupPage examines at most 100 indexed stages, with a frozen upper sequence
// and cutoff. Manual/pre-index filesystem entries are conservatively retained.
// Checkpoint advancement and task continuation belong to the worker's barrier.
func (f *Store) CleanupPage(ctx context.Context, scope domain.ProductScope, cutoff time.Time, cursor, upper uint64) (out CleanupProgress, err error) {
	ctx, release, permitError := f.DB.EnterOperation(ctx)
	if permitError != nil {
		return out, permitError
	}
	defer release()
	out.Cursor = cursor
	rows, e := f.DB.DB.QueryContext(ctx, "SELECT sequence_id,storage_key,created_at FROM file_staging WHERE tenant_id=? AND product_id=? AND sequence_id>? AND sequence_id<=? ORDER BY sequence_id LIMIT 100", scope.TenantID, scope.ProductID, cursor, upper)
	if e != nil {
		return out, e
	}
	type entry struct {
		sequence uint64
		key      string
		created  time.Time
	}
	entries := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.sequence, &v.key, &v.created); e != nil {
			rows.Close()
			return out, e
		}
		entries = append(entries, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	for _, v := range entries {
		if e = ctx.Err(); e != nil {
			return out, e
		}
		if v.created.Before(cutoff) {
			removed, e := f.cleanupStage(ctx, scope, v.key, cutoff)
			if e != nil {
				return out, e
			}
			if removed {
				out.Removed++
			}
		}
		out.Cursor = v.sequence
	}
	out.Done = len(entries) < 100 || out.Cursor == upper
	return out, nil
}
func (f *Store) cleanupStage(ctx context.Context, scope domain.ProductScope, key string, cutoff time.Time) (bool, error) {
	part, e := f.path(scope, key, ".part")
	if e != nil {
		return false, e
	}
	info, e := f.root.Lstat(part)
	if e != nil && !os.IsNotExist(e) {
		return false, e
	}
	if e == nil && (info.ModTime().After(cutoff) || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return false, nil
	}
	g, e := content.Gate(ctx, f.DB, scope)
	if e != nil {
		return false, e
	}
	lock := content.Key(scope, "asset", "staging."+key)
	closure, e := f.DB.CandidateStageCleanupLocks(ctx, scope, key)
	if e != nil {
		return false, e
	}
	closure = append(closure, lock)
	rehearsalLocks, e := f.DB.RehearsalStageCleanupLocks(ctx, scope, key)
	if e != nil {
		return false, e
	}
	closure = append(closure, rehearsalLocks...)
	// A cutoff-qualified claim can be replayed after unknown COMMIT. Upload intake
	// uses this same key and rejects DELETE_INTENT, so a late writer cannot revive
	// deleted bytes as a new PENDING asset. Existing PENDING/READY references win.
	command := "cleanup." + key + "." + strconv.FormatInt(cutoff.UnixMicro(), 10)
	hash := sha256.Sum256([]byte(command))
	claim := store.CommandBarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "CLAIM_STAGE_DELETE", CommandKey: command, CommitKey: content.Identity(scope.TenantID, scope.ProductID, command), GateVersions: g, PayloadHash: hash, Locks: closure}
	_, e = f.DB.WithCommandBarrier(ctx, claim, func(tx store.Tx) error {
		if e := tx.RequireLock(lock); e != nil {
			return e
		}
		var refs int
		// Report stages are bound before IO and share this exact storage lock
		// with final COMMIT. Their fixed derivation cleanup owns retirement.
		if live, e := tx.KeepRehearsalStage(ctx, scope, key); e != nil {
			return e
		} else if live {
			return nil
		}
		// Product candidate bodies have their own exact source binding, never a
		// fabricated WORLD memory lineage. A retained source protects the stage;
		// actual irreversible draft deletion retires its derived reference under
		// this same storage namespace lock before bytes are removed.
		if live, e := tx.KeepCandidateStage(ctx, scope, key); e != nil {
			return e
		} else if live {
			return nil
		}
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM (SELECT storage_key FROM assets WHERE tenant_id=? AND product_id=? AND storage_key=? AND state IN ('PENDING','READY') UNION ALL SELECT storage_key FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=? AND delete_intent=0) live_refs", scope.TenantID, scope.ProductID, key, scope.TenantID, scope.ProductID, key).Scan(&refs); e != nil {
			return e
		}
		if refs > 0 {
			return nil
		}
		_, e := tx.ExecContext(ctx, "UPDATE file_staging SET state='DELETE_INTENT' WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, key)
		return e
	})
	if e != nil {
		return false, e
	}
	var state string
	if e = f.DB.DB.QueryRowContext(ctx, "SELECT state FROM file_staging WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, key).Scan(&state); e != nil {
		return false, e
	}
	if state != "DELETE_INTENT" {
		return false, nil
	}
	// Result evidence may still be held through its local COMMIT attempt. Acquire
	// its storage lock only after the deletion-intent transaction has ended.
	done, e := f.lockDerived(ctx, scope, key)
	if e != nil {
		return false, e
	}
	defer done()
	if e = f.root.Remove(part); e != nil && !os.IsNotExist(e) {
		return false, e
	}
	if len(rehearsalLocks) > 0 {
		proof := &rehearsalPurgeEvidence{owner: f, scope: scope, key: key, claim: claim, live: true}
		defer func() { proof.live = false }()
		_, e = f.DB.FinishRehearsalStagePurge(ctx, scope, key, proof)
		if e != nil {
			return false, e
		}
	}
	// Keep the deletion intent permanently: a stalled pre-intake writer is denied,
	// and later sweeps can remove its eventual abandoned .part. No blob is removed.
	return info != nil, nil
}
