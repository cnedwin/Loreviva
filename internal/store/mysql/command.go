package mysql

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
)

func (u *unit) SetControlStatus(ctx context.Context, state string) error {
	fail := func(e error) error { u.err = e; return e }
	if u.commandOnly || u.purpose != ControlPurpose || u.world == "" {
		return fail(ErrDenied)
	}
	switch state {
	case "RUNNING", "PAUSED", "ENDED":
	default:
		return fail(ErrDenied)
	}
	if e := u.RequireLock(LockKey{Rank: 40, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.world}}); e != nil {
		return e
	}
	res, e := u.tx.ExecContext(ctx, "UPDATE world_controls SET status=?,control_version=control_version+1 WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND control_version=?", state, u.scope.TenantID, u.scope.ProductID, u.world, u.controlVersion)
	if e != nil {
		return fail(e)
	}
	n, e := res.RowsAffected()
	if e != nil {
		return fail(e)
	}
	if n != 1 {
		return fail(ErrDenied)
	}
	u.controlVersion++
	return nil
}

// Task state changes require the sealed task lock and a live RUNNING row for this worker and fence; a zero-row update is rejected.
func (u *unit) SetTaskState(ctx context.Context, state string) error {
	fail := func(e error) error { u.err = e; return e }
	if u.commandOnly {
		return fail(ErrDenied)
	}
	switch state {
	case "WAITING_EXTERNAL", "UNKNOWN", "COMMITTED", "FAILED", "CANCELLED", "REVOKED":
	default:
		return fail(ErrDenied)
	}
	if e := u.RequireLock(LockKey{Rank: 50, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.taskID}}); e != nil {
		return e
	}
	res, e := u.tx.ExecContext(ctx, "UPDATE tasks SET state=? WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND fence=? AND state='RUNNING' AND cancelled=0 AND lease_expires_at>UTC_TIMESTAMP(6)", state, u.scope.TenantID, u.scope.ProductID, u.taskID, u.scope.SubjectID, u.fence)
	if e != nil {
		return fail(e)
	}
	n, e := res.RowsAffected()
	if e != nil {
		return fail(e)
	}
	if n != 1 {
		return fail(ErrDenied)
	}
	return nil
}

func (s *Store) WithCommandBarrier(ctx context.Context, c CommandBarrierRequest, body func(Tx) error) (CommitReceipt, error) {
	r := BarrierRequest{ProductScope: c.ProductScope, WorldScope: c.WorldScope, Versions: c.Versions, Purpose: c.Purpose, ExpectedWorldStatus: c.ExpectedWorldStatus, OwnerSubjectID: c.OwnerSubjectID, Operation: c.Operation, CommandKey: c.CommandKey, CommitKey: c.CommitKey, GateVersions: c.GateVersions, PayloadHash: c.PayloadHash, Locks: c.Locks, commandOnly: true}
	r.packageApproval = c.packageApproval
	r.Runtime = c.Runtime
	r.contentAction = c.ContentAction
	r.contentWithdrawal = c.contentWithdrawal
	receipt, _, e := s.barrier(ctx, r, body)
	return receipt, e
}

// CreateTask admits an ACCEPTED task under the command transaction and purpose-specific subject gate; it does not create an execution lease.
func (u *unit) CreateTask(ctx context.Context, input domain.TaskIntake) error {
	fail := func(e error) error { u.err = e; return e }
	if !u.commandOnly {
		return fail(ErrDenied)
	}
	if e := input.TaskScope.Validate(); e != nil {
		return fail(e)
	}
	if input.ProductScope != u.scope {
		return fail(ErrDenied)
	}
	if (input.WorldScope == nil && u.world != "") || (input.WorldScope != nil && input.WorldScope.WorldInstanceID != u.world) {
		return fail(ErrDenied)
	}
	if e := domain.ValidateID(input.TaskID); e != nil {
		return fail(e)
	}
	if input.NextRunAt.IsZero() {
		return fail(ErrDenied)
	}
	if input.ProjectID != "" {
		if e := domain.ValidateID(input.ProjectID); e != nil {
			return fail(e)
		}
	}
	taskKind := input.Kind
	if taskKind == "" {
		taskKind = "ADMIN"
		if input.WorldScope != nil {
			taskKind = "TURN"
		}
	}
	permissionColumn := ""
	switch taskKind {
	case "TURN":
		if input.WorldScope == nil {
			return fail(ErrDenied)
		}
		permissionColumn = "can_execute"
	case "ADMIN":
		if input.WorldScope != nil {
			return fail(ErrDenied)
		}
		permissionColumn = "can_execute"
	case "CONTROL":
		if input.WorldScope == nil {
			return fail(ErrDenied)
		}
		permissionColumn = "can_control"
	case "GOVERNANCE":
		permissionColumn = "can_govern"
	case "MIGRATION":
		if input.WorldScope == nil {
			return fail(ErrDenied)
		}
		permissionColumn = "can_migrate"
	default:
		return fail(ErrDenied)
	}
	var allowed bool
	if e := u.tx.QueryRowContext(ctx, "SELECT "+permissionColumn+" FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=?", u.scope.TenantID, u.scope.ProductID, u.scope.SubjectID).Scan(&allowed); e != nil {
		return fail(e)
	}
	if !allowed && !(taskKind == "CONTROL" && u.integrationControl.clientControl(u.scope, u.world)) {
		return fail(ErrDenied)
	}
	key := LockKey{Rank: 50, IDs: []string{input.TenantID, input.ProductID, input.TaskID}}
	if e := u.RequireLock(key); e != nil {
		return e
	}
	kind := "PRODUCT"
	var world any
	if input.WorldScope != nil {
		kind = "WORLD"
		world = input.WorldScope.WorldInstanceID
	}
	var project any
	if input.ProjectID != "" {
		project = input.ProjectID
	}
	_, e := u.tx.ExecContext(ctx, "INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,world_instance_id,state,fence,next_run_at,task_kind,project_id) VALUES(?,?,?,?,?,?,'ACCEPTED',0,?,?,?)", input.TenantID, input.ProductID, input.TaskID, input.OwnerSubjectID, kind, world, input.NextRunAt.UTC(), taskKind, project)
	if e != nil {
		return fail(e)
	}
	return nil
}
