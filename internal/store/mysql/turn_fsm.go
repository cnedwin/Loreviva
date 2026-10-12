package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// advanceTurn is reachable only from fixed Store lifecycle implementations, in
// the same WORLD UoW as its effect. No arbitrary transition or budget reset port
// is exported. The original task50 lock serializes state and step allocation.
func advanceTurn(ctx context.Context, u *unit, next string) error {
	var state string
	admin := false
	e := u.tx.QueryRowContext(ctx, "SELECT fsm_state FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&state)
	if errors.Is(e, sql.ErrNoRows) {
		admin = true
		e = u.tx.QueryRowContext(ctx, "SELECT fsm_state FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=?", u.scope.TenantID, u.scope.ProductID, u.taskID).Scan(&state)
	}
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	previous := map[string]string{"REJECTED": "VALIDATED", "CONTEXT_BOUND": "ACCEPTED", "AUTHORIZED": "CONTEXT_BOUND", "SEND_MARKED": "AUTHORIZED", "RESULT_KNOWN": "SEND_MARKED", "VALIDATED": "RESULT_KNOWN", "LOCAL_APPROVAL_READY": "VALIDATED", "APPROVED": "LOCAL_APPROVAL_READY", "LOCAL_COMMIT_READY": "APPROVED", "COMMITTED": "LOCAL_COMMIT_READY"}[next]
	if next == "VALIDATED" && state == "LOCAL_APPROVAL_READY" || next == "APPROVED" && state == "LOCAL_COMMIT_READY" {
		previous = state // A proven rollback consumes a step and restores its candidate phase.
	}
	if next == "EXTRA_APPROVED" && (state == "VALIDATED" || state == "APPROVED") || next == "AUTHORIZED" && state == "EXTRA_APPROVED" {
		previous = state
	}
	if previous == "" || state != previous {
		return ErrDenied
	}
	if e = u.RequireLock(harnessTaskKey(u.scope, u.taskID)); e != nil {
		return e
	}
	if p := u.rehearsalExecution; p != nil {
		if e = u.RequireLock(harnessTaskKey(u.scope, p.root.TaskID)); e != nil {
			return e
		}
		res, e := u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET steps_used=steps_used+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE' AND steps_used<max_steps AND execute_by>UTC_TIMESTAMP(6)", u.scope.TenantID, u.scope.ProductID, p.root.TaskID)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrDenied
		}
	}
	var now time.Time
	if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	result, e := u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET steps_used=steps_used+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE' AND steps_used<max_steps AND execute_by>?", u.scope.TenantID, u.scope.ProductID, u.taskID, now)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrDenied
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_turn_steps(tenant_id,product_id,task_id,step_number,from_state,to_state,fence,occurred_at) SELECT tenant_id,product_id,task_id,steps_used,?,?,?,? FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", state, next, u.fence, now, u.scope.TenantID, u.scope.ProductID, u.taskID)
	if e != nil {
		return e
	}
	if admin {
		_, e = u.tx.ExecContext(ctx, "UPDATE harness_admin_candidates SET fsm_state=? WHERE tenant_id=? AND product_id=? AND task_id=?", next, u.scope.TenantID, u.scope.ProductID, u.taskID)
	} else {
		_, e = u.tx.ExecContext(ctx, "UPDATE harness_turns SET fsm_state=? WHERE tenant_id=? AND product_id=? AND task_id=?", next, u.scope.TenantID, u.scope.ProductID, u.taskID)
	}
	return e
}
