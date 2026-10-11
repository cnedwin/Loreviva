package mysql

import (
	"context"
	"time"
)

// Cleanup progress is written under the same current task/fence barrier as its
// receipt. ACCEPTED schedules the next page; the current lease remains valid
// through the barrier's final checks, and ClaimRegistered advances its fence.
func (u *unit) CreateFileCleanup(ctx context.Context, task string, cutoff time.Time, upper uint64) error {
	if !u.commandOnly {
		u.err = ErrDenied
		return u.err
	}
	if e := u.RequireLock(LockKey{Rank: 50, IDs: []string{u.scope.TenantID, u.scope.ProductID, task}}); e != nil {
		return e
	}
	_, e := u.tx.ExecContext(ctx, "INSERT INTO file_cleanup_tasks(tenant_id,product_id,task_id,cutoff,upper_sequence) VALUES(?,?,?,?,?)", u.scope.TenantID, u.scope.ProductID, task, cutoff.UTC(), upper)
	if e != nil {
		u.err = e
	}
	return e
}
func (u *unit) AdvanceFileCleanup(ctx context.Context, cursor uint64, done bool) error {
	if u.commandOnly {
		u.err = ErrDenied
		return u.err
	}
	if e := u.RequireLock(LockKey{Rank: 50, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.taskID}}); e != nil {
		return e
	}
	var operation string
	if e := u.tx.QueryRowContext(ctx, "SELECT operation FROM registered_jobs WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&operation); e != nil {
		u.err = e
		return e
	}
	if operation != "FILE_CLEANUP" {
		u.err = ErrDenied
		return u.err
	}
	res, e := u.tx.ExecContext(ctx, "UPDATE file_cleanup_tasks SET cursor_sequence=? WHERE tenant_id=? AND product_id=? AND task_id=? AND cursor_sequence<=? AND upper_sequence>=?", cursor, u.scope.TenantID, u.scope.ProductID, u.taskID, cursor, cursor)
	if e != nil {
		u.err = e
		return e
	}
	// MySQL affected rows may be zero on an empty final page; existence and bounds
	// are verified independently rather than treating an unchanged value as loss.
	_ = res
	var upper, current uint64
	if e = u.tx.QueryRowContext(ctx, "SELECT upper_sequence,cursor_sequence FROM file_cleanup_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&upper, &current); e != nil {
		u.err = e
		return e
	}
	if current != cursor || cursor > upper {
		u.err = ErrDenied
		return u.err
	}
	if done {
		return u.SetTaskState(ctx, "COMMITTED")
	}
	res, e = u.tx.ExecContext(ctx, "UPDATE tasks SET state='ACCEPTED',next_run_at=UTC_TIMESTAMP(6) WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND fence=? AND state='RUNNING' AND cancelled=0 AND lease_expires_at>UTC_TIMESTAMP(6)", u.scope.TenantID, u.scope.ProductID, u.taskID, u.scope.SubjectID, u.fence)
	if e != nil {
		u.err = e
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		u.err = e
		return e
	}
	if n != 1 {
		u.err = ErrDenied
		return u.err
	}
	return nil
}
