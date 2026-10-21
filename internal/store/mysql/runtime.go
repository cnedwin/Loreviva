package mysql

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"strings"
)

// RuntimeBinding is limited to registered instance commands. Creation remains
// PRODUCT scoped: its server-derived target is reserved before command rank45.
type RuntimeBinding struct {
	CreationID, Action string
	TargetTaskID       string
	PendingTasks       []string
}

func (u *unit) AssertWorldTask(s domain.Scope, id string) error {
	if u.commandOnly || u.scope != s.ProductScope || u.world != s.WorldInstanceID || u.taskID != id || u.purpose != ExecutionPurpose {
		u.err = ErrDenied
		return u.err
	}
	return nil
}

func validateRuntime(r BarrierRequest) error {
	if r.Runtime == nil {
		return nil
	}
	b := r.Runtime
	if b.CreationID != "" {
		if a := r.integrationCreation; a != nil {
			if a.store == nil || !r.commandOnly || r.WorldScope != nil || r.Operation != "createInstance" || b.Action != "CREATE" || b.CreationID != a.id || r.ProductScope != (&IntegrationCreationPlan{a}).Scope().ProductScope || a.request.Mode != "PRIVATE" || len(a.request.InitialParticipants) != 1 {
				return ErrDenied
			}
			return nil
		}
		if !r.commandOnly || r.WorldScope != nil || r.Operation != "createAdminInstance" || b.Action != "CREATE" || domain.ValidateID(b.CreationID) != nil {
			return ErrDenied
		}
		return nil
	}
	if r.WorldScope == nil {
		return ErrDenied
	}
	switch r.Operation {
	case "cancelInstanceMigration":
		if !r.commandOnly || r.Purpose != MigrationPurpose || b.Action != "CANCEL_MIGRATION" || domain.ValidateID(b.TargetTaskID) != nil {
			return ErrDenied
		}
	case "controlIntegrationInstance":
		if r.integrationControl == nil || !r.commandOnly || r.Purpose != ControlPurpose || b.Action != r.integrationControl.document.Action {
			return ErrDenied
		}
	case "controlAdminInstance", "INSTANCE_CONTROL":
		if r.Purpose != ControlPurpose {
			return ErrDenied
		}
		switch b.Action {
		case "START", "PAUSE", "RESUME", "END":
		default:
			return ErrDenied
		}
	case "previewInstanceMigration", "executeInstanceMigration", "INSTANCE_MIGRATION":
		if r.Purpose != MigrationPurpose || b.Action != "MIGRATE" {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	return nil
}
func acquireRuntimeWorld(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	if e := marker(ctx, tx, k); e != nil {
		return e
	}
	t, p := r.ProductScope.TenantID, r.ProductScope.ProductID
	id := ""
	if r.Runtime.CreationID != "" {
		var owner string
		e := tx.QueryRowContext(ctx, "SELECT owner_subject_id FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", t, p, r.Runtime.CreationID).Scan(&owner)
		if errors.Is(e, sql.ErrNoRows) {
			var exists bool
			if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?)", t, p, r.Runtime.CreationID).Scan(&exists); e != nil {
				return e
			}
			if exists {
				return ErrDenied
			}
			return nil // Only a genuinely absent target gets the creation exception.
		}
		if e != nil {
			return e
		}
		if owner != r.OwnerSubjectID {
			return ErrDenied
		}
		id = r.Runtime.CreationID
	} else {
		id = r.WorldScope.WorldInstanceID
	}
	var owner, release string
	var available bool
	e := tx.QueryRowContext(ctx, "SELECT i.owner_subject_id,w.release_id FROM runtime_instances i JOIN world_controls w ON w.tenant_id=i.tenant_id AND w.product_id=i.product_id AND w.world_instance_id=i.world_instance_id WHERE i.tenant_id=? AND i.product_id=? AND i.world_instance_id=? FOR UPDATE", t, p, id).Scan(&owner, &release)
	if e != nil {
		return e
	}
	wantOwner := r.OwnerSubjectID
	if a := r.integrationControl; a != nil {
		if a.store == nil || a.document.Scope.WorldInstanceID != id {
			return ErrDenied
		}
		wantOwner = a.service.principal.StableSubject
	}
	if owner != wantOwner {
		return ErrDenied
	}
	if e = tx.QueryRowContext(ctx, "SELECT available FROM releases WHERE tenant_id=? AND product_id=? AND release_id=?", t, p, release).Scan(&available); e != nil {
		return e
	}
	if !available {
		return ErrDenied
	}
	return nil
}
func checkRuntimeVersions(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	var v domain.Versions
	var status string
	var active, last sql.NullString
	e := tx.QueryRowContext(ctx, "SELECT w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status,i.active_migration_task,i.last_migration_task FROM world_controls w JOIN runtime_instances i ON i.tenant_id=w.tenant_id AND i.product_id=w.product_id AND i.world_instance_id=w.world_instance_id WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.WorldScope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status, &active, &last)
	if e != nil {
		return e
	}
	if v != *r.Versions || status != r.ExpectedWorldStatus {
		return ErrConflict
	}
	if active.Valid && !(r.Operation == "INSTANCE_MIGRATION" && active.String == r.TaskID) && !(r.Operation == "cancelInstanceMigration" && active.String == r.Runtime.TargetTaskID) {
		return ErrConflict
	}
	if last.Valid && !active.Valid {
		var proof bool
		e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks t JOIN commit_receipts c ON c.tenant_id=t.tenant_id AND c.product_id=t.product_id AND c.task_id=t.task_id JOIN approved_results a ON a.tenant_id=t.tenant_id AND a.product_id=t.product_id AND a.task_id=t.task_id JOIN events e ON e.tenant_id=t.tenant_id AND e.product_id=t.product_id AND e.task_id=t.task_id AND e.record_id=a.record_id AND c.commit_key=e.record_id JOIN outbox o ON o.tenant_id=t.tenant_id AND o.product_id=t.product_id AND o.task_id=t.task_id AND o.record_id=e.record_id WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.world_instance_id=? AND t.state='COMMITTED' AND c.operation='INSTANCE_MIGRATION' AND a.status='COMMITTED' AND e.status='COMMITTED')", r.ProductScope.TenantID, r.ProductScope.ProductID, last.String, r.WorldScope.WorldInstanceID).Scan(&proof)
		if e != nil {
			return e
		}
		if !proof {
			return ErrDenied
		}
	}
	return nil
}

// Cancellation revokes the task at its current fence under world40/task50 and
// clears only its owned marker. No failed-commit or accounting inference.
func (u *unit) CancelRuntimeMigration(ctx context.Context, id string, fence uint64) error {
	if !u.commandOnly || u.runtime == nil || u.runtime.Action != "CANCEL_MIGRATION" || u.runtime.TargetTaskID != id || u.purpose != MigrationPurpose {
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 50, IDs: []string{u.scope.TenantID, u.scope.ProductID, id}}); e != nil {
		return e
	}
	res, e := u.tx.ExecContext(ctx, "UPDATE tasks SET cancelled=1,state='CANCELLED',fence=fence+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND world_instance_id=? AND task_kind='MIGRATION' AND fence=? AND state IN ('ACCEPTED','RUNNING')", u.scope.TenantID, u.scope.ProductID, id, u.scope.SubjectID, u.world, fence)
	if e == nil {
		var n int64
		n, e = res.RowsAffected()
		if n != 1 {
			e = ErrConflict
		}
	}
	if e != nil {
		u.err = e
		return e
	}
	if e = u.RequireLock(LockKey{Rank: 40, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.world}}); e != nil {
		return e
	}
	res, e = u.tx.ExecContext(ctx, "UPDATE runtime_instances SET active_migration_task=NULL WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND active_migration_task=?", u.scope.TenantID, u.scope.ProductID, u.world, id)
	if e == nil {
		var n int64
		n, e = res.RowsAffected()
		if n != 1 {
			e = ErrConflict
		}
	}
	if e != nil {
		u.err = e
	}
	return e
}
func (u *unit) CreateWorld(ctx context.Context, release string) error {
	if u.runtime == nil || u.runtime.CreationID == "" || !u.commandOnly {
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 40, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.runtime.CreationID}}); e != nil {
		return e
	}
	_, e := u.tx.ExecContext(ctx, "INSERT INTO world_controls(tenant_id,product_id,world_instance_id,release_id,world_revision,control_version,privacy_epoch,view_epoch,status) VALUES(?,?,?,?,0,0,1,1,'CREATED')", u.scope.TenantID, u.scope.ProductID, u.runtime.CreationID, release)
	if e != nil {
		u.err = e
	}
	return e
}

// AdvanceWorld owns the limited lifecycle/CAS; no caller can supply increments.
func (u *unit) AdvanceWorld(ctx context.Context, action, targetRelease string) error {
	if u.commandOnly || u.world == "" {
		u.err = ErrDenied
		return u.err
	}
	if e := u.RequireLock(LockKey{Rank: 40, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.world}}); e != nil {
		return e
	}
	var state string
	e := u.tx.QueryRowContext(ctx, "SELECT status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", u.scope.TenantID, u.scope.ProductID, u.world).Scan(&state)
	if e != nil {
		return e
	}
	if action == "ACTIONS" && u.purpose == ExecutionPurpose && state == "RUNNING" {
		_, e = u.tx.ExecContext(ctx, "UPDATE world_controls SET world_revision=world_revision+1 WHERE tenant_id=? AND product_id=? AND world_instance_id=?", u.scope.TenantID, u.scope.ProductID, u.world)
	} else if action == "MIGRATE" && u.purpose == MigrationPurpose && state == "PAUSED" && domain.ValidateID(targetRelease) == nil {
		_, e = u.tx.ExecContext(ctx, "UPDATE world_controls SET release_id=?,world_revision=world_revision+1,control_version=control_version+1,view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND world_instance_id=?", targetRelease, u.scope.TenantID, u.scope.ProductID, u.world)
	} else if u.purpose == ControlPurpose && u.runtime != nil && action == u.runtime.Action {
		next := ""
		switch action {
		case "START":
			if state == "CREATED" {
				next = "RUNNING"
			}
		case "PAUSE":
			if state == "RUNNING" {
				next = "PAUSED"
			}
		case "RESUME":
			if state == "PAUSED" {
				next = "RUNNING"
			}
		case "END":
			if state == "CREATED" || state == "RUNNING" || state == "PAUSED" {
				next = "ENDED"
			}
		}
		if next == "" {
			e = ErrConflict
		} else {
			e = u.SetControlStatus(ctx, next)
		}
	} else {
		e = ErrDenied
	}
	if e != nil {
		u.err = e
	}
	return e
}
func (u *unit) SetMigration(ctx context.Context, id string, clear bool) error {
	if u.runtime == nil || u.purpose != MigrationPurpose || u.world == "" || domain.ValidateID(id) != nil {
		return ErrDenied
	}
	if clear && (u.commandOnly || u.taskID != id) {
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 40, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.world}}); e != nil {
		return e
	}
	q := "UPDATE runtime_instances SET active_migration_task=? WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND active_migration_task IS NULL"
	if clear {
		q = "UPDATE runtime_instances SET last_migration_task=?,active_migration_task=NULL WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND active_migration_task=?"
	}
	args := []any{id, u.scope.TenantID, u.scope.ProductID, u.world}
	if clear {
		args = append(args, id)
	}
	res, e := u.tx.ExecContext(ctx, q, args...)
	if e == nil {
		n, x := res.RowsAffected()
		e = x
		if n != 1 {
			e = ErrConflict
		}
	}
	if e != nil {
		u.err = e
	}
	return e
}

// This is the one pinned expand migration that replaces an autogenerated check.
// MySQL atomic DDL either retains the old check or installs the named replacement.
func migrateInitialControl(ctx context.Context, c *sql.Conn, statement string) error {
	var clause string
	read := func(name string) error {
		var schema, table, kind, enforced string
		e := c.QueryRowContext(ctx, "SELECT tc.TABLE_SCHEMA,tc.TABLE_NAME,tc.CONSTRAINT_TYPE,tc.ENFORCED,cc.CHECK_CLAUSE FROM information_schema.TABLE_CONSTRAINTS tc JOIN information_schema.CHECK_CONSTRAINTS cc ON cc.CONSTRAINT_SCHEMA=tc.CONSTRAINT_SCHEMA AND cc.CONSTRAINT_NAME=tc.CONSTRAINT_NAME WHERE tc.CONSTRAINT_SCHEMA=DATABASE() AND tc.CONSTRAINT_NAME=?", name).Scan(&schema, &table, &kind, &enforced, &clause)
		if e != nil {
			return e
		}
		var current string
		if e = c.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); e != nil {
			return e
		}
		if schema != current || table != "world_controls" || kind != "CHECK" || enforced != "YES" {
			return errors.New("unexpected initial control constraint ownership or enforcement")
		}
		return nil
	}
	e := read("runtime_initial_control")
	if e == nil {
		normal := strings.NewReplacer("`", "", " ", "", "_utf8mb4", "", "\\", "").Replace(clause)
		if normal != "(((control_version>0)or((control_version=0)and(status='CREATED')))and(privacy_epoch>0)and(view_epoch>0))" {
			return errors.New("unexpected initial control constraint")
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	e = read("world_controls_chk_3")
	normalize := strings.NewReplacer("`", "", "(", "", ")", "", " ", "")
	if e != nil || normalize.Replace(clause) != "control_version>0andprivacy_epoch>0andview_epoch>0" {
		return errors.New("original initial control constraint differs")
	}
	_, e = c.ExecContext(ctx, statement)
	return e
}
