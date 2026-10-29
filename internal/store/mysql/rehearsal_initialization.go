package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

type RehearsalInitializer interface {
	PackageRehearsalRequiresSignTx(context.Context, Tx, domain.Scope) (bool, error)
	PlanRehearsalHistoryRuntimeLocks(context.Context, domain.ProductScope, string) ([]LockKey, error)
	EvaluateRehearsalAssertionsTx(context.Context, Tx, domain.Scope) ([]domain.RehearsalAssertion, error)
	RehearsalRouteHashTx(context.Context, Tx, domain.Scope, string) (string, error)
	PlanRehearsalInitialization(context.Context, domain.ProductScope, string, domain.RehearsalBinding, generated.RehearsalRequest) ([]LockKey, error)
	InitializeRehearsalTx(context.Context, Tx, domain.ProductScope, string, domain.RehearsalBinding, generated.RehearsalRequest) error
}
type rehearsalInitialization struct {
	root, world, release string
	binding              domain.RehearsalBinding
	request              generated.RehearsalRequest
}

func (s *Store) InitializeDraftRehearsal(ctx context.Context, lease domain.TaskLease, worker string) (CommitReceipt, error) {
	ctx, close, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer close()
	if lease.WorldInstanceID != nil || lease.SubjectID != lease.OwnerSubjectID || s.RehearsalInitializer == nil || s.DraftRehearsalOwner == nil || s.DraftRehearsalReader == nil {
		return CommitReceipt{}, ErrDenied
	}
	g, e := s.rehearsalGates(ctx, lease.ProductScope)
	if e != nil {
		return CommitReceipt{}, e
	}
	var bound, body []byte
	e = s.DB.QueryRowContext(ctx, "SELECT r.binding,i.document FROM harness_admin_rehearsals r JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) JOIN commit_receipts c ON c.commit_key=r.accepted_commit_key AND c.operation='REHEARSAL_INTAKE' AND c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.owner_subject_id=r.owner_subject_id AND c.payload_hash=r.command_hash AND c.command_key=r.original_key WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.owner_subject_id=?", lease.TenantID, lease.ProductID, lease.TaskID, lease.SubjectID).Scan(&bound, &body)
	if e != nil {
		return CommitReceipt{}, e
	}
	var m domain.RehearsalBinding
	var request generated.RehearsalRequest
	if json.Unmarshal(bound, &m) != nil || generated.Decode("RehearsalRequest", body, &request) != nil {
		return CommitReceipt{}, ErrDenied
	}
	evidence, e := s.DraftRehearsalReader.ReadRehearsalSourceEvidence(ctx, lease.ProductScope, m, request)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer evidence.Close()
	keys, e := s.DraftRehearsalOwner.PlanRehearsalLocks(ctx, lease.ProductScope, m.DraftID)
	if e != nil {
		return CommitReceipt{}, e
	}
	more, e := s.RehearsalInitializer.PlanRehearsalInitialization(ctx, lease.ProductScope, lease.TaskID, m, request)
	if e != nil {
		return CommitReceipt{}, e
	}
	keys = append(keys, more...)
	world := turnIdentity(lease.TaskID, "rehearsal-world")
	release := turnIdentity(lease.TaskID, "rehearsal-version")
	key := turnIdentity(lease.TaskID, "rehearsal-init")
	r := BarrierRequest{ProductScope: lease.ProductScope, OwnerSubjectID: lease.OwnerSubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, GateVersions: g, Operation: "REHEARSAL_INIT", CommandKey: key, CommitKey: key, Locks: keys, rehearsalInitialization: &rehearsalInitialization{lease.TaskID, world, release, m, request}}
	_, r.PayloadHash, e = harnessPayload(m)
	if e != nil {
		return CommitReceipt{}, e
	}
	receipt, _, e := s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := s.checkRehearsalInitialization(ctx, u, r); e != nil {
			return e
		}
		var state string
		if e := u.tx.QueryRowContext(ctx, "SELECT fsm_state FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&state); e != nil {
			return e
		}
		if state != "ACCEPTED" {
			return ErrDenied
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO releases(tenant_id,product_id,release_id,available) VALUES(?,?,?,0)", lease.TenantID, lease.ProductID, release); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO world_controls(tenant_id,product_id,world_instance_id,release_id,world_revision,control_version,privacy_epoch,view_epoch,status) VALUES(?,?,?,?,0,1,1,1,'RUNNING')", lease.TenantID, lease.ProductID, world, release); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO harness_rehearsal_worlds(tenant_id,product_id,root_task_id,world_instance_id,release_id,initialization_receipt) VALUES(?,?,?,?,?,?)", lease.TenantID, lease.ProductID, lease.TaskID, world, release, key); e != nil {
			return e
		}
		if e := s.RehearsalInitializer.InitializeRehearsalTx(ctx, u, lease.ProductScope, lease.TaskID, m, request); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET steps_used=steps_used+1 WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO harness_turn_steps(tenant_id,product_id,task_id,step_number,from_state,to_state,fence,occurred_at) SELECT tenant_id,product_id,task_id,steps_used,'ACCEPTED','ISOLATED',?,UTC_TIMESTAMP(6) FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", lease.Fence, lease.TenantID, lease.ProductID, lease.TaskID); e != nil {
			return e
		}
		_, e := u.tx.ExecContext(ctx, "UPDATE harness_admin_rehearsals SET fsm_state='ISOLATED',own_steps=1 WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID)
		return e
	})
	return receipt, e
}
func (s *Store) checkRehearsalInitialization(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.rehearsalInitialization
	if p == nil || p.root != r.TaskID || s.DraftRehearsalOwner == nil {
		return ErrDenied
	}
	var binding, body, commandHash []byte
	var key string
	if e := u.tx.QueryRowContext(ctx, "SELECT a.binding,i.document,a.command_hash,a.original_key FROM harness_admin_rehearsals a JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) JOIN commit_receipts c ON c.commit_key=a.accepted_commit_key AND c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.owner_subject_id=a.owner_subject_id AND c.operation='REHEARSAL_INTAKE' AND c.command_key=a.original_key AND c.payload_hash=a.command_hash AND c.scope_kind='PRODUCT' AND c.world_instance_id IS NULL AND c.phase='COMMIT' WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.OwnerSubjectID).Scan(&binding, &body, &commandHash, &key); e != nil {
		return e
	}
	var actual domain.RehearsalBinding
	var request generated.RehearsalRequest
	if json.Unmarshal(binding, &actual) != nil || generated.Decode("RehearsalRequest", body, &request) != nil || !reflect.DeepEqual(actual, p.binding) || !reflect.DeepEqual(request, p.request) || turnIdentity(r.ProductScope.TenantID, r.ProductScope.ProductID, r.OwnerSubjectID, "REHEARSAL", key) != r.TaskID {
		return ErrDenied
	}
	_, hash, e := harnessPayload(struct {
		Scope   domain.ProductScope
		Draft   string
		Request generated.RehearsalRequest
	}{r.ProductScope, p.binding.DraftID, request})
	if e != nil || !reflect.DeepEqual(commandHash, hash[:]) {
		return ErrDenied
	}
	m, e := s.DraftRehearsalOwner.BuildRehearsalTx(ctx, u, r.ProductScope, p.binding.DraftID, p.request)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(m, p.binding) {
		return ErrReplan
	}
	post := u.rehearsalReport != nil && u.rehearsalReport.applied != nil
	taskState, envelopeState, ownSteps := "RUNNING", "ACTIVE", 1
	if post {
		if e := checkRehearsalReportApplied(ctx, u); e != nil {
			return e
		}
		taskState, envelopeState, ownSteps = "COMMITTED", "COMMITTED", 2
	}
	var ok bool
	e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_admin_rehearsals a JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_envelopes h USING(tenant_id,product_id,task_id) JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=? AND t.task_kind='ADMIN' AND j.operation='REHEARSAL' AND t.state=? AND t.cancelled=0 AND h.state=? AND h.execute_by>UTC_TIMESTAMP(6) AND h.steps_used<h.max_steps+? AND a.own_steps<=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.OwnerSubjectID, taskState, envelopeState, post, ownSteps).Scan(&ok)
	if e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	return nil
}
func acquireRehearsalInitialization(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	p := r.rehearsalInitialization
	if p == nil || k.IDs[2] != p.world {
		return ErrDenied
	}
	if e := marker(ctx, tx, k); e != nil {
		return e
	}
	var root string
	e := tx.QueryRowContext(ctx, "SELECT root_task_id FROM harness_rehearsal_worlds WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", r.ProductScope.TenantID, r.ProductScope.ProductID, p.world).Scan(&root)
	if e == nil {
		if root != p.root {
			return ErrDenied
		}
		if r.rehearsalChildStart != nil || r.rehearsalReport != nil {
			var valid bool
			if e = tx.QueryRowContext(ctx, "SELECT w.status='RUNNING' AND w.release_id=? AND i.owner_subject_id=? AND i.active_migration_task IS NULL AND EXISTS(SELECT 1 FROM commit_receipts c WHERE c.commit_key=? AND c.task_id=? AND c.operation='REHEARSAL_INIT' AND c.phase='COMMIT' AND c.tenant_id=w.tenant_id AND c.product_id=w.product_id AND c.owner_subject_id=i.owner_subject_id AND c.scope_kind='PRODUCT') FROM world_controls w JOIN runtime_instances i USING(tenant_id,product_id,world_instance_id) WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=? FOR UPDATE", p.release, r.OwnerSubjectID, turnIdentity(p.root, "rehearsal-init"), p.root, r.ProductScope.TenantID, r.ProductScope.ProductID, p.world).Scan(&valid); e != nil {
				return e
			}
			if !valid {
				return ErrDenied
			}
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if r.rehearsalChildStart != nil {
		return ErrDenied
	}
	var exists bool
	if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, p.world).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return ErrDenied
	}
	return nil
}
func (u *unit) AssertRehearsalInitialization(scope domain.Scope, root string) error {
	p := u.rehearsalInitialization
	if p == nil || u.rehearsalChildStart != nil || u.commandOnly || u.scope != scope.ProductScope || p.world != scope.WorldInstanceID || p.root != root || u.taskID != root || u.purpose != ExecutionPurpose {
		return ErrDenied
	}
	return u.RequireLock(LockKey{Rank: 40, IDs: []string{scope.TenantID, scope.ProductID, scope.WorldInstanceID}})
}
