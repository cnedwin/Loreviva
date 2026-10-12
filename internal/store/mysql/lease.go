package mysql

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

// ClaimTask is the strict task-only exception. It neither inspects gates nor
// reserves anything, and commits before returning scheduling eligibility.
func (s *Store) ClaimTask(ctx context.Context, workerID string, now time.Time, ttl time.Duration) (lease TaskLease, err error) {
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return lease, e
	}
	defer release()
	if e := domain.ValidateID(workerID); e != nil {
		return lease, e
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return lease, errors.New("lease TTL out of range")
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return lease, e
	}
	defer tx.Rollback()
	var world sql.NullString
	e = tx.QueryRowContext(ctx, "SELECT tenant_id,product_id,task_id,owner_subject_id,world_instance_id,fence FROM tasks WHERE NOT EXISTS(SELECT 1 FROM harness_rehearsal_worlds rw WHERE rw.tenant_id=tasks.tenant_id AND rw.product_id=tasks.product_id AND rw.world_instance_id=tasks.world_instance_id) AND NOT EXISTS(SELECT 1 FROM harness_envelopes h WHERE h.tenant_id=tasks.tenant_id AND h.product_id=tasks.product_id AND h.task_id=tasks.task_id AND h.state IN ('CUTOFF','ACCOUNTING_ONLY')) AND state IN ('ACCEPTED','RUNNING') AND cancelled=0 AND next_run_at<=? AND (lease_expires_at IS NULL OR lease_expires_at<=?) ORDER BY next_run_at,tenant_id,product_id,task_id LIMIT 1 FOR UPDATE SKIP LOCKED", now.UTC(), now.UTC()).Scan(&lease.TenantID, &lease.ProductID, &lease.TaskID, &lease.OwnerSubjectID, &world, &lease.Fence)
	if errors.Is(e, sql.ErrNoRows) {
		return lease, ErrNoTask
	}
	if e != nil {
		return lease, e
	}
	if lease.Fence == ^uint64(0) {
		return lease, errors.New("fence exhausted")
	}
	lease.Fence++
	lease.SubjectID = lease.OwnerSubjectID
	lease.ExpiresAt = now.UTC().Add(ttl)
	if world.Valid {
		w := world.String
		lease.WorldInstanceID = &w
	}
	res, e := tx.ExecContext(ctx, "UPDATE tasks SET state='RUNNING',fence=?,lease_owner=?,lease_expires_at=? WHERE tenant_id=? AND product_id=? AND task_id=? AND fence=?", lease.Fence, workerID, lease.ExpiresAt, lease.TenantID, lease.ProductID, lease.TaskID, lease.Fence-1)
	if e != nil {
		return TaskLease{}, e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return TaskLease{}, ErrDenied
	}
	if e = tx.Commit(); e != nil {
		return TaskLease{}, ErrCommitUnknown
	}
	return lease, nil
}
