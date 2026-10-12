package mysql

import (
	"context"
	"database/sql"
	"loreviva.local/loreviva/internal/domain"
	"strconv"
	"time"
)

// ClaimRehearsalChild is the only isolated-child scheduler. It derives the
// original next child from a current root lease; callers cannot pick a task,
// deadline, scope, TTL, actor or lease fence.
func (s *Store) ClaimRehearsalChild(ctx context.Context, root domain.TaskLease, worker string) (out RegisteredLease, err error) {
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer close()
	accepted, e := s.StartRehearsalChild(ctx, root, worker)
	if e != nil {
		return out, e
	}
	world := turnIdentity(root.TaskID, "rehearsal-world")
	child := domain.TaskLease{ProductScope: root.ProductScope, WorldInstanceID: &world, OwnerSubjectID: root.OwnerSubjectID, TaskID: accepted.TurnID}
	plan, e := s.rehearsalChildBarrier(ctx, root, child, worker)
	if e != nil {
		return out, e
	}
	p := plan.rehearsalExecution
	evidence, e := s.DraftRehearsalReader.ReadRehearsalSourceEvidence(ctx, root.ProductScope, p.initialization.binding, p.initialization.request)
	if e != nil {
		return out, e
	}
	defer evidence.Close()
	var fence uint64
	var state string
	var owner sql.NullString
	var expiry sql.NullTime
	var now time.Time
	e = s.DB.QueryRowContext(ctx, "SELECT fence,state,lease_owner,lease_expires_at,UTC_TIMESTAMP(6) FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", root.TenantID, root.ProductID, child.TaskID).Scan(&fence, &state, &owner, &expiry, &now)
	if e != nil {
		return out, e
	}
	if state != "ACCEPTED" && state != "RUNNING" {
		return out, ErrNoTask
	}
	if expiry.Valid && expiry.Time.After(now) {
		if owner.String != worker {
			return out, ErrNoTask
		}
	} else {
		if fence == ^uint64(0) {
			return out, ErrDenied
		}
		fence++
	}
	if fence == 0 {
		return out, ErrDenied
	}
	key := turnIdentity(child.TaskID, "rehearsal-claim", strconv.FormatUint(fence, 10))
	r := BarrierRequest{ProductScope: root.ProductScope, TaskID: root.TaskID, OwnerSubjectID: root.SubjectID, WorkerID: worker, Fence: root.Fence, GateVersions: plan.GateVersions, Operation: "REHEARSAL_CHILD_CLAIM", CommandKey: key, CommitKey: key, Locks: append(plan.Locks, harnessTaskKey(root.ProductScope, child.TaskID), harnessTaskKey(root.ProductScope, p.child.quote)), rehearsalInitialization: &p.initialization, rehearsalChildStart: &p.child}
	_, r.PayloadHash, e = harnessPayload(struct {
		Root, Child, Worker string
		Fence               uint64
	}{root.TaskID, child.TaskID, worker, fence})
	if e != nil {
		return out, e
	}
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		u := port.(*unit)
		if e := s.checkRehearsalClaim(ctx, u, r); e != nil {
			return e
		}
		var current uint64
		var currentOwner sql.NullString
		var currentExpiry sql.NullTime
		var status string
		var clock, end, rootExpiry time.Time
		e := u.tx.QueryRowContext(ctx, "SELECT t.fence,t.state,t.lease_owner,t.lease_expires_at,UTC_TIMESTAMP(6),c.execute_by,rt.lease_expires_at FROM tasks t JOIN harness_rehearsal_children c ON c.tenant_id=t.tenant_id AND c.product_id=t.product_id AND c.child_task_id=t.task_id JOIN tasks rt ON rt.tenant_id=c.tenant_id AND rt.product_id=c.product_id AND rt.task_id=c.root_task_id JOIN registered_jobs j ON j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.cancelled=0 AND t.task_kind='TURN' AND t.scope_kind='WORLD' AND t.world_instance_id=? AND c.root_task_id=? AND j.operation='REHEARSAL_TURN'", root.TenantID, root.ProductID, child.TaskID, root.SubjectID, world, root.TaskID).Scan(&current, &status, &currentOwner, &currentExpiry, &clock, &end, &rootExpiry)
		if e != nil {
			return e
		}
		if (status != "ACCEPTED" && status != "RUNNING") || !end.After(clock) || !rootExpiry.After(clock) {
			return ErrNoTask
		}
		if currentExpiry.Valid && currentExpiry.Time.After(clock) {
			if current != fence || currentOwner.String != worker {
				return ErrNoTask
			}
			return nil
		}
		if current == ^uint64(0) || current+1 != fence {
			return ErrReplan
		}
		expires := clock.Add(time.Minute)
		if end.Before(expires) {
			expires = end
		}
		if rootExpiry.Before(expires) {
			expires = rootExpiry
		}
		result, e := u.tx.ExecContext(ctx, "UPDATE tasks SET state='RUNNING',fence=?,lease_owner=?,lease_expires_at=? WHERE tenant_id=? AND product_id=? AND task_id=? AND fence=?", fence, worker, expires, root.TenantID, root.ProductID, child.TaskID, current)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrReplan
		}
		return nil
	})
	if e != nil {
		return out, e
	}
	out = RegisteredLease{TaskLease: child, Kind: "TURN", Operation: "REHEARSAL_TURN"}
	out.Fence = fence
	e = s.DB.QueryRowContext(ctx, "SELECT lease_expires_at FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? AND fence=? AND lease_owner=? AND cancelled=0 AND state='RUNNING'", root.TenantID, root.ProductID, child.TaskID, fence, worker).Scan(&out.ExpiresAt)
	if e != nil {
		return RegisteredLease{}, e
	}
	return out, nil
}

// A claim receipt is metadata, not a reusable execution capability. Recheck
// the original child binding and current root qualification under the same
// world/root/child/source locks even before returning an existing receipt.
func (s *Store) checkRehearsalClaim(ctx context.Context, u *unit, r BarrierRequest) error {
	if r.Operation != "REHEARSAL_CHILD_CLAIM" || r.rehearsalInitialization == nil || r.rehearsalChildStart == nil {
		return ErrDenied
	}
	if e := s.checkRehearsalChildStart(ctx, u, r); e != nil {
		return e
	}
	if e := checkCurrentLease(ctx, u.tx, r); e != nil {
		return e
	}
	root := domain.TaskLease{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, TaskID: r.TaskID, Fence: r.Fence}
	childRequest := r
	childRequest.TaskID = r.rehearsalChildStart.child
	childRequest.rehearsalExecution = &rehearsalExecution{root: root, worker: r.WorkerID, initialization: *r.rehearsalInitialization, child: *r.rehearsalChildStart}
	return checkRehearsalChildIdentity(ctx, u, childRequest)
}
