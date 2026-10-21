package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

type ControlCommand struct {
	Request generated.AdminControlRequest
	Key     string
}
type persistedTask struct {
	Scope                  domain.Scope
	Versions               domain.Versions
	Status, Action, PlanID string
}

func legalControl(status, action string) bool {
	switch action {
	case "START":
		return status == "CREATED"
	case "PAUSE":
		return status == "RUNNING"
	case "RESUME":
		return status == "PAUSED"
	case "END":
		return status == "CREATED" || status == "RUNNING" || status == "PAUSED"
	}
	return false
}
// The command accepts a control task; execution later rechecks its stored scope, versions, status and live issuer before advancing the world and committing the result together.
func (r *Runtime) Control(ctx context.Context, s domain.Scope, c ControlCommand) (generated.AdminTaskReceipt, error) {
	var out generated.AdminTaskReceipt
	if string(c.Request.ProductID) != s.ProductID {
		return out, content.ErrNotFound
	}
	i, e := r.GetInstance(ctx, s)
	if e != nil {
		return out, e
	}
	v, e := versions(c.Request.ExpectedVersions)
	if e != nil {
		return out, e
	}
	status := i.InstanceState
	switch c.Request.Command {
	case "START":
		status = "CREATED"
	case "PAUSE":
		status = "RUNNING"
	case "RESUME":
		status = "PAUSED"
	case "END":
		if v.ControlVersion == 0 {
			status = "CREATED"
		}
	default:
		return out, content.ErrBlocked
	}
	id := content.Identity(s.TenantID, s.ProductID, s.SubjectID, s.WorldInstanceID, "control", c.Key)
	binding := store.CommandBarrierRequest{WorldScope: &s, Versions: &v, Purpose: store.ControlPurpose, ExpectedWorldStatus: status, Runtime: &store.RuntimeBinding{Action: c.Request.Command}}
	keys := []store.LockKey{{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}}, content.Key(s.ProductScope, "content_check", id)}
	raw, e := content.BoundCommand(ctx, r.Store, s.ProductScope, "controlAdminInstance", c.Key, s.WorldInstanceID, "AdminTaskReceipt", c.Request, keys, binding, func(tx store.Tx) (any, error) {
		if !legalControl(status, c.Request.Command) {
			return nil, store.ErrConflict
		}
		if e := queueTask(ctx, tx, s, id, "CONTROL", "INSTANCE_CONTROL", persistedTask{Scope: s, Versions: v, Status: status, Action: c.Request.Command}); e != nil {
			return nil, e
		}
		return taskReceipt(s.ProductScope, id, "INSTANCE_CONTROL"), nil
	})
	if e == nil {
		e = generated.Decode("AdminTaskReceipt", raw, &out)
	}
	return out, e
}
func taskReceipt(s domain.ProductScope, id, kind string) generated.AdminTaskReceipt {
	return generated.AdminTaskReceipt{Scope: content.Scope(s), TaskID: generated.Identity(id), TaskKind: generated.AdminTaskKind(kind), TaskState: "ACCEPTED", CorrelationID: generated.Identity(id)}
}
func queueTask(ctx context.Context, tx store.Tx, s domain.Scope, id, kind, operation string, p persistedTask) error {
	e := tx.CreateRegisteredTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: s.ProductScope, WorldScope: &s, OwnerSubjectID: s.SubjectID}, TaskID: id, Kind: kind, NextRunAt: time.Now().UTC()}, operation)
	if e != nil {
		return e
	}
	if e = tx.RequireLock(content.Key(s.ProductScope, "content_check", id)); e != nil {
		return e
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO runtime_tasks(tenant_id,product_id,task_id,document) VALUES(?,?,?,?)", s.TenantID, s.ProductID, id, raw)
	return e
}
func eventLocks(s domain.ProductScope, id string) []store.LockKey {
	out := []store.LockKey{}
	for _, kind := range []string{"events", "approved_results", "outbox"} {
		out = append(out, store.LockKey{Rank: 110, Kind: kind, IDs: []string{s.TenantID, s.ProductID, id}})
	}
	return out
}

// Each successful result names a real committed event. Event sequence is not a
// wall clock; world revision increments only for narrative actions/migrations.
func recordResult(ctx context.Context, tx store.Tx, s domain.Scope, task, id string, revision uint64, evidence any) error {
	for _, kind := range []string{"events", "approved_results", "outbox"} {
		if e := tx.RequireLock(store.LockKey{Rank: 110, Kind: kind, IDs: []string{s.TenantID, s.ProductID, id}}); e != nil {
			return e
		}
		var e error
		switch kind {
		case "events":
			_, e = tx.ExecContext(ctx, "INSERT INTO events(tenant_id,product_id,record_id,task_id,owner_subject_id,world_instance_id,status,revision) VALUES(?,?,?,?,?,?,'COMMITTED',?)", s.TenantID, s.ProductID, id, task, s.SubjectID, s.WorldInstanceID, revision)
			if e == nil {
				var raw []byte
				raw, e = json.Marshal(evidence)
				if e == nil {
					_, e = tx.ExecContext(ctx, "INSERT INTO runtime_events(tenant_id,product_id,record_id,document) VALUES(?,?,?,?)", s.TenantID, s.ProductID, id, raw)
				}
			}
		case "approved_results":
			_, e = tx.ExecContext(ctx, "INSERT INTO approved_results(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES(?,?,?,?,?,'COMMITTED',?)", s.TenantID, s.ProductID, id, task, s.SubjectID, revision)
		case "outbox":
			_, e = tx.ExecContext(ctx, "INSERT INTO outbox(tenant_id,product_id,record_id,task_id,owner_subject_id,consumer_id,message_id,status,revision) VALUES(?,?,?,?,?,'runtime',?,'PENDING',?)", s.TenantID, s.ProductID, id, task, s.SubjectID, id, revision)
		}
		if e != nil {
			return e
		}
	}
	return tx.AppendIntegrationControlResult(ctx, s, task, id)
}
func (r *Runtime) Process(ctx context.Context, lease store.RegisteredLease, worker string) error {
	if lease.WorldInstanceID == nil || !((lease.Kind == "CONTROL" && lease.Operation == "INSTANCE_CONTROL") || (lease.Kind == "MIGRATION" && lease.Operation == "INSTANCE_MIGRATION")) {
		return store.ErrDenied
	}
	ctx, issuerError := r.Store.EnterControlTaskIssuer(ctx, lease, worker)
	if issuerError != nil {
		return issuerError
	}
	var raw []byte
	var p persistedTask
	if e := r.Store.DB.QueryRowContext(ctx, "SELECT document FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&raw); e != nil {
		return e
	}
	if e := json.Unmarshal(raw, &p); e != nil {
		return e
	}
	if p.Scope.ProductScope != lease.ProductScope || p.Scope.WorldInstanceID != *lease.WorldInstanceID {
		return store.ErrDenied
	}
	if lease.Kind == "MIGRATION" {
		return r.processMigration(ctx, lease, worker, p, raw)
	}
	g, e := content.Gate(ctx, r.Store, p.Scope.ProductScope)
	if e != nil {
		return e
	}
	id := content.Identity(lease.TaskID, "result")
	_, e = r.Store.WithBarrier(ctx, store.BarrierRequest{ProductScope: p.Scope.ProductScope, WorldScope: &p.Scope, Versions: &p.Versions, OwnerSubjectID: lease.OwnerSubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, Purpose: store.ControlPurpose, ExpectedWorldStatus: p.Status, Runtime: &store.RuntimeBinding{Action: p.Action}, Operation: lease.Operation, CommandKey: lease.TaskID, CommitKey: id, GateVersions: g, PayloadHash: sha256.Sum256(raw), Locks: eventLocks(p.Scope.ProductScope, id)}, func(tx store.Tx) error {
		if e := tx.AdvanceWorld(ctx, p.Action, ""); e != nil {
			return e
		}
		if e := recordResult(ctx, tx, p.Scope, lease.TaskID, id, p.Versions.WorldRevision, struct{ Kind, Action string }{"CONTROL", p.Action}); e != nil {
			return e
		}
		return tx.SetTaskState(ctx, "COMMITTED")
	})
	return e
}
