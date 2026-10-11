package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

func scopeOf(r BarrierRequest) (string, any, string) {
	if r.WorldScope != nil {
		return "WORLD", r.WorldScope.WorldInstanceID, r.WorldScope.WorldInstanceID
	}
	return "PRODUCT", nil, "PRODUCT"
}
func receiptPhase(r BarrierRequest) string {
	if r.phase == "" {
		return "COMMIT"
	}
	return r.phase
}
func validateBarrier(r BarrierRequest) error {
	if e := validateRuntime(r); e != nil {
		return e
	}
	switch r.Purpose {
	case ExecutionPurpose, ControlPurpose, GovernancePurpose, MigrationPurpose:
	default:
		return ErrDenied
	}
	if r.Purpose == MigrationPurpose && r.ExpectedWorldStatus != "PAUSED" {
		return ErrDenied
	}
	if r.Purpose != ExecutionPurpose {
		if r.WorldScope == nil && r.Purpose != GovernancePurpose {
			return ErrDenied
		}
		if r.WorldScope != nil {
			switch r.ExpectedWorldStatus {
			case "CREATED", "RUNNING", "PAUSED", "ENDED":
			default:
				return ErrDenied
			}
		} else if r.ExpectedWorldStatus != "" {
			return ErrDenied
		}
	}
	if e := (domain.TaskScope{ProductScope: r.ProductScope, WorldScope: r.WorldScope, OwnerSubjectID: r.OwnerSubjectID}).Validate(); e != nil {
		return e
	}
	ids := []string{r.Operation}
	if !r.commandOnly {
		ids = append(ids, r.TaskID, r.WorkerID)
	}
	for _, id := range ids {
		if e := domain.ValidateID(id); e != nil {
			return e
		}
	}
	for _, key := range []string{r.CommandKey, r.CommitKey} {
		if e := domain.ValidateIdempotencyKey(key); e != nil {
			return e
		}
	}
	if (!r.commandOnly && r.Fence == 0) || r.GateVersions.Tenant == 0 || r.GateVersions.Product == 0 || r.GateVersions.Subject == 0 || r.GateVersions.Privacy == 0 || r.GateVersions.View == 0 {
		return ErrDenied
	}
	if r.WorldScope != nil {
		if r.Versions == nil {
			return ErrDenied
		}
		if e := r.Versions.Validate(); e != nil {
			return e
		}
		initialControl := r.Runtime != nil && r.Purpose == ControlPurpose && r.ExpectedWorldStatus == "CREATED" && (r.Runtime.Action == "START" || r.Runtime.Action == "END")
		if (r.Versions.ControlVersion == 0 && !initialControl) || r.Versions.PrivacyEpoch == 0 || r.Versions.ViewEpoch == 0 {
			return ErrDenied
		}
	} else if r.Versions != nil {
		return ErrDenied
	}
	return nil
}
func baseLocks(r BarrierRequest) []LockKey {
	t, p, sub := r.ProductScope.TenantID, r.ProductScope.ProductID, r.ProductScope.SubjectID
	kind, _, w := scopeOf(r)
	keys := []LockKey{{Rank: 10, IDs: []string{t}}, {Rank: 20, IDs: []string{t, p}}, {Rank: 30, IDs: []string{t, p, sub}}, {Rank: 45, IDs: []string{t, p, sub, r.Operation, kind, w, r.CommandKey}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{r.CommitKey}}}
	if !r.commandOnly {
		keys = append(keys, LockKey{Rank: 50, IDs: []string{t, p, r.TaskID}})
	}
	if r.WorldScope != nil {
		keys = append(keys, LockKey{Rank: 40, IDs: []string{t, p, w}})
	}
	if r.Runtime != nil && r.Runtime.CreationID != "" {
		keys = append(keys, LockKey{Rank: 40, IDs: []string{t, p, r.Runtime.CreationID}})
	}
	return keys
}
func (s *Store) WithBarrier(ctx context.Context, r BarrierRequest, body func(Tx) error) (CommitReceipt, error) {
	receipt, _, e := s.barrier(ctx, r, body)
	return receipt, e
}

// replay is deliberately not an authorization to send: even an exact successful
// replay of StartSend's receipt grants only QueryOriginal, never CanSend.
func (s *Store) barrier(ctx context.Context, r BarrierRequest, body func(Tx) error) (receipt CommitReceipt, replay bool, err error) {
	ctx, release, permitError := s.EnterOperation(ctx)
	if permitError != nil {
		return receipt, false, permitError
	}
	defer release()
	if r.WorldScope != nil {
		copy := *r.WorldScope
		r.WorldScope = &copy
	}
	if r.Versions != nil {
		copy := *r.Versions
		r.Versions = &copy
	}
	if e := validateBarrier(r); e != nil {
		return receipt, false, e
	}
	if body == nil {
		return receipt, false, errors.New("transaction body required")
	}
	plan, e := PlanLocks(append(baseLocks(r), r.Locks...))
	if e != nil {
		return receipt, false, e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return receipt, false, e
	}
	defer func() {
		rollbackError := tx.Rollback()
		if err != nil && rollbackError == nil && r.commandOnly && r.Purpose == GovernancePurpose && r.Operation == "MEMORY_GOVERNANCE_ACCEPT" && !errors.Is(err, ErrCommitUnknown) {
			err = &RejectedCommand{request: r, cause: err}
		}
	}()
	u := &unit{tx: tx, plan: map[string]bool{}, scope: r.ProductScope, taskID: r.TaskID, commandOnly: r.commandOnly, fence: r.Fence, purpose: r.Purpose, world: worldKey(r)}
	u.runtime = r.Runtime
	if r.Versions != nil {
		u.controlVersion = r.Versions.ControlVersion
	}
	for _, k := range plan {
		if e = s.acquire(ctx, tx, r, k); e != nil {
			return receipt, false, e
		}
		u.plan[string(lockBytes(k))] = true
		if s.Trace != nil {
			s.Trace(k)
		}
	}
	existing, e := queryReceipt(ctx, tx, r.CommitKey)
	if e == nil {
		if !existing.matches(r) {
			return receipt, false, ErrConflict
		}
		return existing.public(), true, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return receipt, false, e
	}
	if r.Runtime != nil && r.WorldScope != nil {
		if e = checkRuntimeVersions(ctx, tx, r); e != nil {
			return receipt, false, e
		}
	}
	// Command lock 45 protects this stable identity. A retry cannot silently
	// generate a new receipt key and rerun local effects in the same phase.
	var originalKey string
	kind, _, _ := scopeOf(r)
	e = tx.QueryRowContext(ctx, "SELECT commit_key FROM commit_receipts WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND operation=? AND scope_kind=? AND instance_scope=? AND command_key=? AND phase=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.OwnerSubjectID, r.Operation, kind, worldKey(r), r.CommandKey, receiptPhase(r)).Scan(&originalKey)
	if e == nil {
		return receipt, false, ErrConflict
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return receipt, false, e
	}
	if e = checkCurrentLease(ctx, tx, r); e != nil {
		return receipt, false, e
	}
	if !r.commandOnly {
		var running bool
		if e := tx.QueryRowContext(ctx, "SELECT state='RUNNING' FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&running); e != nil {
			return receipt, false, e
		}
		if !running {
			return receipt, false, ErrDenied
		}
	}
	if e = body(u); e != nil {
		return receipt, false, e
	}
	if u.err != nil {
		return receipt, false, u.err
	}
	if e = checkCurrentLease(ctx, tx, r); e != nil {
		return receipt, false, e
	}
	kind, w, _ := scopeOf(r)
	var task any
	if !r.commandOnly {
		task = r.TaskID
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,?,?,?,?,?,?,?)", r.CommitKey, r.ProductScope.TenantID, r.ProductScope.ProductID, r.OwnerSubjectID, kind, w, task, r.Operation, r.CommandKey, r.PayloadHash[:], receiptPhase(r))
	if e != nil {
		return receipt, false, e
	}
	if e = s.finish(ctx, tx, r); e != nil {
		return receipt, false, e
	}
	return CommitReceipt{Key: r.CommitKey, TaskID: r.TaskID, OwnerSubjectID: r.OwnerSubjectID, Committed: true}, false, nil
}
func checkCurrentLease(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	if r.commandOnly {
		return nil
	}
	var valid bool
	e := tx.QueryRowContext(ctx, "SELECT fence=? AND lease_owner=? AND cancelled=0 AND lease_expires_at>UTC_TIMESTAMP(6) FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", r.Fence, r.WorkerID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}
func (s *Store) acquire(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	t, p, sub := r.ProductScope.TenantID, r.ProductScope.ProductID, r.ProductScope.SubjectID
	// Caller cannot substitute a different tenant/product/control/task lock.
	base := baseLocks(r)
	if k.Rank <= 50 && k.Rank != 30 && !(r.commandOnly && k.Rank == 50) {
		found := false
		if k.Rank == 50 && r.Runtime != nil && r.Purpose == MigrationPurpose && len(k.IDs) == 3 && k.IDs[0] == t && k.IDs[1] == p {
			for _, id := range r.Runtime.PendingTasks {
				if k.IDs[2] == id {
					found = true
				}
			}
		}
		for _, b := range base {
			if reflect.DeepEqual(k, b) {
				found = true
				break
			}
		}
		if !found {
			return ErrDenied
		}
	}
	var enabled bool
	var version uint64
	switch k.Rank {
	case 10:
		if e := tx.QueryRowContext(ctx, "SELECT permission_version,enabled FROM tenant_gates WHERE tenant_id=? FOR UPDATE", t).Scan(&version, &enabled); e != nil {
			return e
		}
		if !enabled || version != r.GateVersions.Tenant {
			return ErrDenied
		}
	case 20:
		if e := tx.QueryRowContext(ctx, "SELECT permission_version,enabled FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE", t, p).Scan(&version, &enabled); e != nil {
			return e
		}
		if !enabled || version != r.GateVersions.Product {
			return ErrDenied
		}
	case 30:
		if len(k.IDs) != 3 || k.IDs[0] != t || k.IDs[1] != p {
			return ErrDenied
		}
		var privacy, view uint64
		var execute, control, govern, migrate bool
		if e := tx.QueryRowContext(ctx, "SELECT permission_version,privacy_epoch,view_epoch,enabled,can_execute,can_control,can_govern,can_migrate FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", t, p, k.IDs[2]).Scan(&version, &privacy, &view, &enabled, &execute, &control, &govern, &migrate); e != nil {
			return e
		}
		if !enabled {
			return ErrDenied
		}
		if k.IDs[2] == sub && (version != r.GateVersions.Subject || privacy != r.GateVersions.Privacy || view != r.GateVersions.View) {
			return ErrDenied
		}
		if k.IDs[2] == sub {
			switch r.Purpose {
			case ExecutionPurpose:
				if !execute {
					return ErrDenied
				}
			case ControlPurpose:
				if !control {
					return ErrDenied
				}
			case GovernancePurpose:
				if !govern {
					return ErrDenied
				}
			case MigrationPurpose:
				if !migrate {
					return ErrDenied
				}
			}
		}
	case 40:
		if r.Runtime != nil {
			return acquireRuntimeWorld(ctx, tx, r, k)
		}
		var release, status string
		var revision, control, privacy, view uint64
		if e := tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", t, p, r.WorldScope.WorldInstanceID).Scan(&release, &revision, &control, &privacy, &view, &status); e != nil {
			return e
		}
		v := r.Versions
		expectedStatus := "RUNNING"
		if r.Purpose != ExecutionPurpose {
			expectedStatus = r.ExpectedWorldStatus
		}
		if status != expectedStatus || release != v.ReleaseID || revision != v.WorldRevision || control != v.ControlVersion || privacy != v.PrivacyEpoch || view != v.ViewEpoch {
			return ErrDenied
		}
		if e := tx.QueryRowContext(ctx, "SELECT available FROM releases WHERE tenant_id=? AND product_id=? AND release_id=?", t, p, release).Scan(&enabled); e != nil {
			return e
		}
		if !enabled {
			return ErrDenied
		}
		var migration sql.NullString
		e := tx.QueryRowContext(ctx, "SELECT active_migration_task FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=?", t, p, r.WorldScope.WorldInstanceID).Scan(&migration)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if migration.Valid && r.Purpose != GovernancePurpose {
			return ErrDenied
		}
	case 45:
		if e := marker(ctx, tx, k); e != nil {
			return e
		}
		kind, w, _ := scopeOf(r)
		var hash []byte
		e := tx.QueryRowContext(ctx, "SELECT payload_hash FROM commands WHERE tenant_id=? AND product_id=? AND subject_id=? AND operation=? AND scope_kind=? AND instance_scope=? AND idem_key=? FOR UPDATE", t, p, sub, r.Operation, kind, worldKey(r), r.CommandKey).Scan(&hash)
		if e == nil {
			if !bytes.Equal(hash, r.PayloadHash[:]) {
				return ErrConflict
			}
		} else if errors.Is(e, sql.ErrNoRows) {
			_, e = tx.ExecContext(ctx, "INSERT INTO commands(tenant_id,product_id,subject_id,operation,scope_kind,world_instance_id,idem_key,payload_hash) VALUES(?,?,?,?,?,?,?,?)", t, p, sub, r.Operation, kind, w, r.CommandKey, r.PayloadHash[:])
			if e != nil {
				return e
			}
		} else {
			return e
		}
	case 50:
		if r.Runtime != nil && r.Purpose == MigrationPurpose && k.IDs[2] != r.TaskID {
			if e := marker(ctx, tx, k); e != nil {
				return e
			}
			var owner, world string
			e := tx.QueryRowContext(ctx, "SELECT owner_subject_id,world_instance_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", t, p, k.IDs[2]).Scan(&owner, &world)
			if errors.Is(e, sql.ErrNoRows) && r.commandOnly {
				return nil
			}
			if e != nil {
				return e
			}
			if world != r.WorldScope.WorldInstanceID || owner != r.OwnerSubjectID {
				return ErrDenied
			}
			return nil
		}
		if r.commandOnly {
			if len(k.IDs) != 3 || k.IDs[0] != t || k.IDs[1] != p {
				return ErrDenied
			}
			if e := marker(ctx, tx, k); e != nil {
				return e
			}
			var owner string
			e := tx.QueryRowContext(ctx, "SELECT owner_subject_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", t, p, k.IDs[2]).Scan(&owner)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if e == nil && owner != r.OwnerSubjectID {
				return ErrDenied
			}
			return nil
		}
		var owner, kind, state, taskKind string
		var world, worker sql.NullString
		var fence uint64
		var valid, cancel bool
		if e := tx.QueryRowContext(ctx, "SELECT owner_subject_id,scope_kind,world_instance_id,state,fence,lease_owner,(lease_expires_at>UTC_TIMESTAMP(6)),cancelled,task_kind FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", t, p, r.TaskID).Scan(&owner, &kind, &world, &state, &fence, &worker, &valid, &cancel, &taskKind); e != nil {
			return e
		}
		expected, _, _ := scopeOf(r)
		expectedTaskKind := string(r.Purpose)
		if r.Purpose == ExecutionPurpose {
			expectedTaskKind = "TURN"
			if r.WorldScope == nil {
				expectedTaskKind = "ADMIN"
			}
		}
		if taskKind != expectedTaskKind || owner != r.OwnerSubjectID || kind != expected || world.String != worldKey(r) || world.Valid != (r.WorldScope != nil) || fence != r.Fence || worker.String != r.WorkerID || !valid || cancel || (state != "RUNNING" && state != "COMMITTED") {
			return ErrDenied
		}
	default:
		if e := validateResourceLock(r, k); e != nil {
			return e
		}
		if e := marker(ctx, tx, k); e != nil {
			return e
		}
		// Existing rows are locked at this same rank. Missing rows stay protected by
		// the marker and may be inserted later by the planned local body.
		var id string
		var e error
		switch k.Rank {
		case 60:
			e = tx.QueryRowContext(ctx, "SELECT attempt_id FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? FOR UPDATE", t, p, r.TaskID, k.IDs[3]).Scan(&id)
			if e == nil {
				e = tx.QueryRowContext(ctx, "SELECT authorization_id FROM dispatch_authorizations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? FOR UPDATE", t, p, r.TaskID, k.IDs[3]).Scan(&id)
			}
		case 70:
			var tenant, product, world, project sql.NullString
			var balance, held int64
			e = tx.QueryRowContext(ctx, "SELECT tenant_id,product_id,balance,held,world_instance_id,project_id FROM budget_accounts WHERE kind=? AND scope_key=? AND account_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1]).Scan(&tenant, &product, &balance, &held, &world, &project)
			if e != nil {
				return e
			}
			if balance < held {
				return ErrDenied
			}
			if k.Kind == "supplier_global" || k.Kind == "platform_risk" {
				if tenant.Valid || product.Valid || k.IDs[0] != "GLOBAL" {
					return ErrDenied
				}
			} else {
				if tenant.String != t {
					return ErrDenied
				}
				if k.Kind != "supplier_tenant" && product.String != p {
					return ErrDenied
				}
				if k.Kind == "world" && (r.WorldScope == nil || world.String != r.WorldScope.WorldInstanceID) {
					return ErrDenied
				}
				// Project budget needs an authoritative task project binding.
				if k.Kind == "project" {
					var taskProject sql.NullString
					if e := tx.QueryRowContext(ctx, "SELECT project_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", t, p, r.TaskID).Scan(&taskProject); e != nil {
						return e
					}
					if !project.Valid || !taskProject.Valid || project.String != taskProject.String {
						return ErrDenied
					}
				}
			}
			return nil
		case 80:
			e = tx.QueryRowContext(ctx, "SELECT reservation_id FROM reservations WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=? FOR UPDATE", k.Kind, k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&id)
		case 100:
			e = tx.QueryRowContext(ctx, "SELECT message_id FROM inbox WHERE tenant_id=? AND product_id=? AND consumer_id=? AND message_id=? FOR UPDATE", t, p, k.IDs[2], k.IDs[3]).Scan(&id)
		case 110:
			if k.Kind == "commit_receipts" {
				e = tx.QueryRowContext(ctx, "SELECT commit_key FROM commit_receipts WHERE commit_key=? FOR UPDATE", k.IDs[0]).Scan(&id)
			} else {
				e = tx.QueryRowContext(ctx, fmt.Sprintf("SELECT record_id FROM %s WHERE tenant_id=? AND product_id=? AND record_id=? FOR UPDATE", k.Kind), t, p, k.IDs[2]).Scan(&id)
			}
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
	}
	return nil
}
func worldKey(r BarrierRequest) string {
	if r.WorldScope != nil {
		return r.WorldScope.WorldInstanceID
	}
	return ""
}
func validateResourceLock(r BarrierRequest, k LockKey) error {
	t, p := r.ProductScope.TenantID, r.ProductScope.ProductID
	switch k.Rank {
	case 60:
		if len(k.IDs) != 5 || k.IDs[0] != t || k.IDs[1] != p || k.IDs[2] != r.TaskID {
			return ErrDenied
		}
	case 70:
		if len(k.IDs) != 2 {
			return ErrDenied
		}
	case 80:
		if len(k.IDs) != 3 {
			return ErrDenied
		}
	case 90:
		if len(k.IDs) < 3 || k.IDs[0] != t || k.IDs[1] != p {
			return ErrDenied
		}
		if k.Kind == "asset" && len(k.IDs) == 3 && (r.Operation == "MEMORY_DERIVED_STAGE" || r.Operation == "MEMORY_DERIVED_FILE" || r.Operation == "MEMORY_DERIVED_READY" || r.Operation == "MEMORY_GOVERNANCE_ACCEPT" || r.Operation == "MEMORY_GOVERNANCE") {
			return nil
		}
		productKind := k.Kind == "content_release" || k.Kind == "content_check" || k.Kind == "content_project"
		if r.Runtime != nil && productKind && len(k.IDs) == 3 {
			return nil
		}
		if r.Runtime != nil && r.Runtime.CreationID != "" && len(k.IDs) == 4 && k.IDs[2] == r.Runtime.CreationID && (k.Kind == "actor" || k.Kind == "item" || k.Kind == "memory") {
			return nil
		}
		if (r.WorldScope == nil && len(k.IDs) != 3) || (r.WorldScope != nil && (len(k.IDs) != 4 || k.IDs[2] != r.WorldScope.WorldInstanceID)) {
			return ErrDenied
		}
	case 100:
		if len(k.IDs) != 4 || k.IDs[0] != t || k.IDs[1] != p {
			return ErrDenied
		}
	case 110:
		if k.Kind == "commit_receipts" {
			if len(k.IDs) != 1 {
				return ErrDenied
			}
		} else if len(k.IDs) != 3 || k.IDs[0] != t || k.IDs[1] != p {
			return ErrDenied
		}
	default:
		return ErrDenied
	}
	return nil
}
