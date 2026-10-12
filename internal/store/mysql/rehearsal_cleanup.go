package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"

	"loreviva.local/loreviva/internal/domain"
)

// The original root lock prevents new children and the child locks prevent new
// result bindings. Every existing derived storage lock also serializes Memory
// lineage deletion. No world fact/aggregate, receipt or accounting row is edited.
type rehearsalStageClosure struct {
	root, key, world, asset string
	binding                 domain.RehearsalBinding
	stages                  []string
	keys                    []LockKey
}

func rehearsalStageSet(ctx context.Context, q rowQuerier, scope domain.ProductScope, key string) (*rehearsalStageClosure, error) {
	var root string
	e := q.QueryRowContext(ctx, `SELECT root_id FROM (
SELECT task_id root_id FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND storage_key=?
UNION SELECT c.root_task_id FROM harness_result_sources r JOIN harness_rehearsal_children c ON c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.child_task_id=r.task_id WHERE r.tenant_id=? AND r.product_id=? AND r.storage_key=?) original_root`, scope.TenantID, scope.ProductID, key, scope.TenantID, scope.ProductID, key).Scan(&root)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	p := &rehearsalStageClosure{root: root, key: key}
	var raw []byte
	e = q.QueryRowContext(ctx, `SELECT a.binding,w.world_instance_id FROM harness_admin_rehearsals a JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_rehearsal_worlds w ON w.tenant_id=a.tenant_id AND w.product_id=a.product_id AND w.root_task_id=a.task_id JOIN commit_receipts r ON r.commit_key=a.accepted_commit_key AND r.tenant_id=a.tenant_id AND r.product_id=a.product_id AND r.owner_subject_id=a.owner_subject_id AND r.operation='REHEARSAL_INTAKE' AND r.scope_kind='PRODUCT' AND r.world_instance_id IS NULL AND r.command_key=a.original_key AND r.payload_hash=a.command_hash AND r.phase='COMMIT' WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=? AND t.owner_subject_id=a.owner_subject_id AND t.world_instance_id IS NULL AND t.task_kind='ADMIN' AND j.operation='REHEARSAL'`, scope.TenantID, scope.ProductID, root, scope.SubjectID).Scan(&raw, &p.world)
	if e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &p.binding) != nil || len(p.binding.Inputs) < 1 || len(p.binding.Inputs) > 20 || len(p.binding.Sources) == 0 || p.world != turnIdentity(root, "rehearsal-world") {
		return nil, ErrDenied
	}
	_, hash, e := harnessPayload(p.binding)
	if e != nil {
		return nil, e
	}
	initKey := turnIdentity(root, "rehearsal-init")
	init, e := queryReceipt(ctx, q, initKey)
	if e != nil {
		return nil, e
	}
	if !init.matches(BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: root, Operation: "REHEARSAL_INIT", CommandKey: initKey, CommitKey: initKey, PayloadHash: hash}) {
		return nil, ErrDenied
	}
	keyFor := func(rank int, kind, id string) LockKey {
		return LockKey{Rank: rank, Kind: kind, IDs: []string{scope.TenantID, scope.ProductID, id}}
	}
	p.keys = append(p.keys, keyFor(50, "", root))
	var count int
	if e = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_rehearsal_children WHERE tenant_id=? AND product_id=? AND root_task_id=?", scope.TenantID, scope.ProductID, root).Scan(&count); e != nil {
		return nil, e
	}
	if count > len(p.binding.Inputs) {
		return nil, ErrDenied
	}
	for i := 0; i < count; i++ {
		child := turnIdentity(root, "rehearsal-child", strconv.Itoa(i))
		var found string
		e = q.QueryRowContext(ctx, `SELECT c.child_task_id FROM harness_rehearsal_children c JOIN tasks t ON t.tenant_id=c.tenant_id AND t.product_id=c.product_id AND t.task_id=c.child_task_id JOIN registered_jobs j ON j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id JOIN commit_receipts r ON r.commit_key=c.accepted_receipt AND r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.owner_subject_id=t.owner_subject_id AND r.operation='REHEARSAL_CHILD_START' AND r.phase='COMMIT' WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=? AND c.input_index=? AND c.world_instance_id=? AND t.owner_subject_id=? AND j.operation='REHEARSAL_TURN'`, scope.TenantID, scope.ProductID, root, i, p.world, scope.SubjectID).Scan(&found)
		if e != nil {
			return nil, e
		}
		if found != child {
			return nil, ErrDenied
		}
		var valid bool
		e = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_rehearsal_children c JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id AND h.world_instance_id=c.world_instance_id AND h.actor_id=c.actor_id AND h.input_source_id=c.input_source_id AND h.input_hash=c.input_hash AND h.accepted_commit_key=c.accepted_receipt JOIN commit_receipts r ON r.commit_key=c.accepted_receipt AND r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.owner_subject_id=h.owner_subject_id AND r.task_id=c.root_task_id AND r.scope_kind='PRODUCT' AND r.world_instance_id IS NULL AND r.command_key=c.accepted_receipt AND r.payload_hash=h.command_hash AND r.operation='REHEARSAL_CHILD_START' AND r.phase='COMMIT' WHERE c.tenant_id=? AND c.product_id=? AND c.child_task_id=? AND c.accepted_receipt=?)`, scope.TenantID, scope.ProductID, child, turnIdentity(child, "accepted")).Scan(&valid)
		if e != nil {
			return nil, e
		}
		if !valid {
			return nil, ErrDenied
		}
		p.keys = append(p.keys, keyFor(50, "", child))
		for slot := 0; slot < 2; slot++ {
			var storage string
			e = q.QueryRowContext(ctx, "SELECT storage_key FROM harness_result_sources WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", scope.TenantID, scope.ProductID, child, turnIdentity(child, "attempt", strconv.Itoa(slot))).Scan(&storage)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return nil, e
			}
			p.stages = append(p.stages, storage)
			p.keys = append(p.keys, keyFor(90, "asset", "staging."+storage))
		}
	}
	var sources int
	if e = q.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_result_sources r JOIN harness_rehearsal_children c ON c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.child_task_id=r.task_id WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=?", scope.TenantID, scope.ProductID, root).Scan(&sources); e != nil {
		return nil, e
	}
	if sources != len(p.stages) {
		return nil, ErrReplan
	}
	var storage string
	e = q.QueryRowContext(ctx, "SELECT storage_key,COALESCE(asset_id,'') FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, root).Scan(&storage, &p.asset)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if e == nil {
		p.stages = append(p.stages, storage)
		p.keys = append(p.keys, keyFor(90, "asset", "staging."+storage))
	}
	if p.asset != "" {
		p.keys = append(p.keys, keyFor(90, "asset", p.asset))
	}
	found := false
	for _, storage := range p.stages {
		if storage == key {
			found = true
		}
	}
	if !found {
		return nil, ErrDenied
	}
	return p, nil
}

func (s *Store) RehearsalStageCleanupLocks(ctx context.Context, scope domain.ProductScope, key string) ([]LockKey, error) {
	p, e := rehearsalStageSet(ctx, s.DB, scope, key)
	if e != nil || p == nil {
		return nil, e
	}
	if s.DraftRehearsalOwner == nil {
		return nil, ErrDenied
	}
	keys, e := s.DraftRehearsalOwner.PlanRehearsalHistoryLocks(ctx, scope, p.binding)
	return append(p.keys, keys...), e
}

func (u *unit) KeepRehearsalStage(ctx context.Context, scope domain.ProductScope, key string) (bool, error) {
	if !u.commandOnly || u.scope != scope {
		return false, ErrDenied
	}
	p, e := rehearsalStageSet(ctx, u.tx, scope, key)
	if e != nil || p == nil {
		return false, e
	}
	for _, k := range p.keys {
		if e = u.RequireLock(k); e != nil {
			return false, e
		}
	}
	if u.rehearsalSourcesOwner == nil {
		return false, ErrDenied
	}
	var retired bool
	if e = u.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=? AND delete_intent=1) OR EXISTS(SELECT 1 FROM memory_asset_lineage m JOIN harness_result_sources r ON r.tenant_id=m.tenant_id AND r.product_id=m.product_id AND r.storage_key=m.storage_key JOIN harness_rehearsal_children c ON c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.child_task_id=r.task_id WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=? AND m.delete_intent=1)`, scope.TenantID, scope.ProductID, p.root, scope.TenantID, scope.ProductID, p.root).Scan(&retired); e != nil {
		return false, e
	}
	live, e := u.rehearsalSourcesOwner.RehearsalSourcesLiveTx(ctx, u, scope, p.binding)
	if e != nil {
		return false, e
	}
	if live && !retired {
		return true, nil
	}
	for _, storage := range p.stages {
		if _, e = u.tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET delete_intent=1 WHERE tenant_id=? AND product_id=? AND storage_key=? AND world_instance_id=? AND data_subject_id=?", scope.TenantID, scope.ProductID, storage, p.world, scope.SubjectID); e != nil {
			return false, e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE file_staging SET state='DELETE_INTENT' WHERE tenant_id=? AND product_id=? AND storage_key=?", scope.TenantID, scope.ProductID, storage); e != nil {
			return false, e
		}
	}
	if _, e = u.tx.ExecContext(ctx, "UPDATE harness_rehearsal_reports SET delete_intent=1 WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, p.root); e != nil {
		return false, e
	}
	if p.asset != "" {
		if _, e = u.tx.ExecContext(ctx, "UPDATE assets SET state='DELETED' WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, p.asset); e != nil {
			return false, e
		}
	}
	u.rehearsalCleanup = p
	return false, nil
}

func (u *unit) checkRehearsalCleanup(ctx context.Context) error {
	p := u.rehearsalCleanup
	current, e := rehearsalStageSet(ctx, u.tx, u.scope, p.key)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(p, current) {
		return ErrReplan
	}
	for _, storage := range p.stages {
		var ok bool
		e = u.tx.QueryRowContext(ctx, `SELECT s.state='DELETE_INTENT' AND NOT EXISTS(SELECT 1 FROM memory_asset_lineage m WHERE m.tenant_id=s.tenant_id AND m.product_id=s.product_id AND m.storage_key=s.storage_key AND m.delete_intent=0) FROM file_staging s WHERE s.tenant_id=? AND s.product_id=? AND s.storage_key=?`, u.scope.TenantID, u.scope.ProductID, storage).Scan(&ok)
		if e != nil {
			return e
		}
		if !ok {
			return ErrDenied
		}
	}
	var ok bool
	e = u.tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM harness_rehearsal_reports r WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND (r.delete_intent=0 OR EXISTS(SELECT 1 FROM assets a WHERE a.tenant_id=r.tenant_id AND a.product_id=r.product_id AND a.asset_id=r.asset_id AND a.state<>'DELETED')))`, u.scope.TenantID, u.scope.ProductID, p.root).Scan(&ok)
	if e != nil {
		return e
	}
	if !ok {
		return ErrDenied
	}
	if u.rehearsalPurged != "" {
		var pending bool
		if e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=? AND (delete_intent=0 OR purged=0))", u.scope.TenantID, u.scope.ProductID, u.rehearsalPurged).Scan(&pending); e != nil {
			return e
		}
		if pending {
			return ErrDenied
		}
	}
	return nil
}

// Called by Files only after successful bounded removal under the original
// storage mutex. It acknowledges the exact existing deletion obligation.
func (u *unit) AcknowledgeRehearsalStagePurge(ctx context.Context, scope domain.ProductScope, key string) error {
	if !u.commandOnly || u.scope != scope {
		return ErrDenied
	}
	p, e := rehearsalStageSet(ctx, u.tx, scope, key)
	if e != nil || p == nil {
		return e
	}
	for _, k := range p.keys {
		if e = u.RequireLock(k); e != nil {
			return e
		}
	}
	u.rehearsalCleanup = p
	if e = u.checkRehearsalCleanup(ctx); e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET purged=1 WHERE tenant_id=? AND product_id=? AND storage_key=? AND world_instance_id=? AND data_subject_id=? AND delete_intent=1", scope.TenantID, scope.ProductID, key, p.world, scope.SubjectID)
	if e == nil {
		u.rehearsalPurged = key
	}
	return e
}
