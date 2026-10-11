package mysql

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"strings"
	"time"
)

type RegisteredOperation struct{ Kind, Operation, TaskID string }
type RegisteredLease struct {
	TaskLease
	Kind, Operation string
}

func (u *unit) CreateRegisteredTask(ctx context.Context, in domain.TaskIntake, operation string) error {
	if e := domain.ValidateID(operation); e != nil {
		u.err = e
		return e
	}
	if in.Kind != "ADMIN" && !(in.Kind == "CONTROL" && operation == "INSTANCE_CONTROL") && !(in.Kind == "MIGRATION" && operation == "INSTANCE_MIGRATION") && !(in.Kind == "GOVERNANCE" && operation == "MEMORY_GOVERNANCE") {
		u.err = ErrDenied
		return ErrDenied
	}
	if e := u.CreateTask(ctx, in); e != nil {
		return e
	}
	_, e := u.tx.ExecContext(ctx, "INSERT INTO registered_jobs(tenant_id,product_id,task_id,operation) VALUES(?,?,?,?)", in.TenantID, in.ProductID, in.TaskID, operation)
	if e != nil {
		u.err = e
	}
	return e
}
func (u *unit) CancelRegisteredTask(ctx context.Context, id string) error {
	if !u.commandOnly {
		u.err = ErrDenied
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 50, IDs: []string{u.scope.TenantID, u.scope.ProductID, id}}); e != nil {
		return e
	}
	var state, kind string
	e := u.tx.QueryRowContext(ctx, "SELECT t.state,t.task_kind FROM tasks t JOIN registered_jobs j ON j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=?", u.scope.TenantID, u.scope.ProductID, id, u.scope.SubjectID).Scan(&state, &kind)
	if e != nil {
		u.err = e
		return e
	}
	if state == "COMMITTED" || kind == "MIGRATION" || kind == "GOVERNANCE" {
		u.err = ErrConflict
		return ErrConflict
	}
	_, e = u.tx.ExecContext(ctx, "UPDATE tasks SET cancelled=1,state='CANCELLED' WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", u.scope.TenantID, u.scope.ProductID, id, u.scope.SubjectID)
	if e != nil {
		u.err = e
	}
	return e
}

// ClaimRegistered preserves ClaimTask's strict task-only lease exception and
// filters by explicit registered kind+operation before acquiring any lease.
func (s *Store) ClaimRegistered(ctx context.Context, worker string, now time.Time, ttl time.Duration, registered []RegisteredOperation) (lease RegisteredLease, err error) {
	ctx, release, permitError := s.EnterOperation(ctx)
	if permitError != nil {
		return lease, permitError
	}
	defer release()
	if domain.ValidateID(worker) != nil || ttl <= 0 || ttl > 24*time.Hour || len(registered) == 0 || len(registered) > 32 {
		return lease, ErrDenied
	}
	filters := []string{}
	args := []any{now.UTC(), now.UTC()}
	for _, op := range registered {
		if domain.ValidateID(op.Operation) != nil {
			return lease, ErrDenied
		}
		switch op.Kind {
		case "ADMIN", "TURN", "CONTROL", "GOVERNANCE", "MIGRATION":
		default:
			return lease, ErrDenied
		}
		if op.TaskID != "" {
			if domain.ValidateID(op.TaskID) != nil {
				return lease, ErrDenied
			}
			filters = append(filters, "(t.task_kind=? AND j.operation=? AND t.task_id=?)")
			args = append(args, op.Kind, op.Operation, op.TaskID)
		} else {
			filters = append(filters, "(t.task_kind=? AND j.operation=?)")
			args = append(args, op.Kind, op.Operation)
		}
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return lease, e
	}
	defer tx.Rollback()
	var world sql.NullString
	e = tx.QueryRowContext(ctx, "SELECT t.tenant_id,t.product_id,t.task_id,t.owner_subject_id,t.world_instance_id,t.fence,t.task_kind,j.operation FROM tasks t JOIN registered_jobs j ON j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id WHERE t.state IN ('ACCEPTED','RUNNING') AND t.cancelled=0 AND t.next_run_at<=? AND (t.state='ACCEPTED' OR t.lease_expires_at IS NULL OR t.lease_expires_at<=?) AND ("+strings.Join(filters, " OR ")+") ORDER BY t.next_run_at,t.tenant_id,t.product_id,t.task_id LIMIT 1 FOR UPDATE OF t SKIP LOCKED", args...).Scan(&lease.TenantID, &lease.ProductID, &lease.TaskID, &lease.OwnerSubjectID, &world, &lease.Fence, &lease.Kind, &lease.Operation)
	if errors.Is(e, sql.ErrNoRows) {
		return lease, ErrNoTask
	}
	if e != nil {
		return lease, e
	}
	if lease.Fence == ^uint64(0) {
		return lease, ErrDenied
	}
	lease.Fence++
	lease.SubjectID = lease.OwnerSubjectID
	lease.ExpiresAt = now.UTC().Add(ttl)
	if world.Valid {
		w := world.String
		lease.WorldInstanceID = &w
	}
	res, e := tx.ExecContext(ctx, "UPDATE tasks SET state='RUNNING',fence=?,lease_owner=?,lease_expires_at=? WHERE tenant_id=? AND product_id=? AND task_id=? AND fence=?", lease.Fence, worker, lease.ExpiresAt, lease.TenantID, lease.ProductID, lease.TaskID, lease.Fence-1)
	if e != nil {
		return lease, e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return lease, ErrDenied
	}
	if e = tx.Commit(); e != nil {
		return RegisteredLease{}, ErrCommitUnknown
	}
	return lease, nil
}
