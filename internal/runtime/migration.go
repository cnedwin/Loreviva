package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"strconv"
	"time"
)

func (r *Runtime) PreviewInstanceMigration(ctx context.Context, s domain.Scope, q generated.MigrationPreviewRequest, key string) (generated.MigrationPlan, error) {
	var out generated.MigrationPlan
	if string(q.ProductID) != s.ProductID {
		return out, content.ErrNotFound
	}
	_, e := r.GetInstance(ctx, s)
	if e != nil {
		return out, e
	}
	v, e := versions(q.ExpectedVersions)
	if e != nil {
		return out, e
	}
	from, e := r.Content.GetRelease(ctx, s.ProductScope, v.ReleaseID)
	if e != nil {
		return out, e
	}
	to, e := r.Content.GetRelease(ctx, s.ProductScope, string(q.TargetReleaseID))
	if e != nil {
		return out, e
	}
	rows, e := r.migrationRows(ctx, s)
	if e != nil {
		return out, e
	}
	id := content.Identity(s.TenantID, s.ProductID, s.SubjectID, s.WorldInstanceID, "migration-preview", key)
	keys := append(releaseLocks(s.ProductScope, from), releaseLocks(s.ProductScope, to)...)
	keys = append(keys, migrationLocks(s, rows)...)
	keys = append(keys, content.Key(s.ProductScope, "content_check", id))
	binding := store.CommandBarrierRequest{WorldScope: &s, Versions: &v, Purpose: store.MigrationPurpose, ExpectedWorldStatus: "PAUSED", Runtime: &store.RuntimeBinding{Action: "MIGRATE", PendingTasks: rows.Tasks}}
	raw, e := content.BoundCommand(ctx, r.Store, s.ProductScope, "previewInstanceMigration", key, s.WorldInstanceID, "MigrationPlan", q, keys, binding, func(tx store.Tx) (any, error) {
		if _, e := verifyRelease(ctx, tx, s.ProductScope, from); e != nil {
			return nil, e
		}
		if _, e := verifyRelease(ctx, tx, s.ProductScope, to); e != nil {
			return nil, e
		}
		differences, e := compatible(from, to)
		if e != nil {
			return nil, e
		}
		stateHash, e := preserveHash(ctx, tx, s, rows, "")
		if e != nil {
			return nil, e
		}
		plan := generated.MigrationPlan{Scope: generated.WorldScope{TenantID: s.TenantID, ProductID: s.ProductID, WorldInstanceID: s.WorldInstanceID}, PlanID: generated.Identity(id), FromReleaseID: from.ReleaseID, TargetReleaseID: to.ReleaseID, FromContentHash: from.ContentHash, TargetContentHash: to.ContentHash, ExpectedVersions: q.ExpectedVersions, Differences: differences, Unresolved: []generated.Identity{}, ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Format(time.RFC3339Nano), NoAutomaticStart: true, NoHistoryRegeneration: true, RestoreInstructions: "Keep the paused world and original data. No automatic resume.", FailureInstructions: "Query the original task and receipt with the original key; no reinitialization."}
		plan.ResolutionMap = []struct {
			LogicalID  generated.Identity `json:"logical_id"`
			Resolution string             `json:"resolution"`
		}{}
		for _, d := range differences {
			plan.ResolutionMap = append(plan.ResolutionMap, struct {
				LogicalID  generated.Identity `json:"logical_id"`
				Resolution string             `json:"resolution"`
			}{d.LogicalID, "KEEP_STATE"})
		}
		g, e := governance(ctx, tx, s)
		if e != nil {
			return nil, e
		}
		plan.Governance.TenantVersion = generated.ExactUnsigned(strconv.FormatUint(g.Tenant, 10))
		plan.Governance.ProductVersion = generated.ExactUnsigned(strconv.FormatUint(g.Product, 10))
		plan.Governance.SubjectVersion = generated.ExactUnsigned(strconv.FormatUint(g.Subject, 10))
		plan.Governance.PrivacyEpoch = generated.ExactUnsigned(strconv.FormatUint(g.Privacy, 10))
		plan.Governance.ViewEpoch = generated.ExactUnsigned(strconv.FormatUint(g.View, 10))
		hash, e := content.Hash(plan)
		if e != nil {
			return nil, e
		}
		b, e := json.Marshal(plan)
		if e != nil {
			return nil, e
		}
		if e = tx.RequireLock(content.Key(s.ProductScope, "content_check", id)); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO migration_plans(tenant_id,product_id,plan_id,owner_subject_id,world_instance_id,state_hash,plan_hash,document) VALUES(?,?,?,?,?,?,?,?)", s.TenantID, s.ProductID, id, s.SubjectID, s.WorldInstanceID, stateHash, hash, b)
		return plan, e
	})
	if e == nil {
		e = generated.Decode("MigrationPlan", raw, &out)
	}
	return out, e
}
func governance(ctx context.Context, tx store.Tx, s domain.Scope) (store.GateVersions, error) {
	var g store.GateVersions
	e := tx.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch FROM subject_governance s JOIN product_gates p ON p.tenant_id=s.tenant_id AND p.product_id=s.product_id JOIN tenant_gates t ON t.tenant_id=s.tenant_id WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", s.TenantID, s.ProductID, s.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View)
	return g, e
}
func (r *Runtime) loadPlan(ctx context.Context, s domain.Scope, id string) (generated.MigrationPlan, string, string, error) {
	var plan generated.MigrationPlan
	var state, hash string
	var raw []byte
	e := r.Store.DB.QueryRowContext(ctx, "SELECT document,state_hash,plan_hash FROM migration_plans WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=? AND plan_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID, id).Scan(&raw, &state, &hash)
	if e != nil {
		return plan, "", "", content.ErrNotFound
	}
	e = generated.Decode("MigrationPlan", raw, &plan)
	return plan, state, hash, e
}
func planCurrent(ctx context.Context, tx store.Tx, s domain.Scope, plan generated.MigrationPlan) error {
	expiry, e := time.Parse(time.RFC3339Nano, plan.ExpiresAt)
	if e != nil || !expiry.After(time.Now()) {
		return content.ErrBlocked
	}
	g, e := governance(ctx, tx, s)
	if e != nil {
		return e
	}
	want := []generated.ExactUnsigned{plan.Governance.TenantVersion, plan.Governance.ProductVersion, plan.Governance.SubjectVersion, plan.Governance.PrivacyEpoch, plan.Governance.ViewEpoch}
	got := []uint64{g.Tenant, g.Product, g.Subject, g.Privacy, g.View}
	for n, x := range got {
		if string(want[n]) != strconv.FormatUint(x, 10) {
			return store.ErrDenied
		}
	}
	return nil
}
func (r *Runtime) ExecuteInstanceMigration(ctx context.Context, s domain.Scope, q generated.MigrationExecuteRequest, key string) (generated.AdminTaskReceipt, error) {
	var out generated.AdminTaskReceipt
	if string(q.ProductID) != s.ProductID {
		return out, content.ErrNotFound
	}
	if _, e := r.GetInstance(ctx, s); e != nil {
		return out, e
	}
	plan, stateHash, planHash, e := r.loadPlan(ctx, s, string(q.PlanID))
	if e != nil {
		return out, e
	}
	from, e := r.Content.GetRelease(ctx, s.ProductScope, string(plan.FromReleaseID))
	if e != nil {
		return out, e
	}
	to, e := r.Content.GetRelease(ctx, s.ProductScope, string(plan.TargetReleaseID))
	if e != nil {
		return out, e
	}
	v, e := versions(plan.ExpectedVersions)
	if e != nil {
		return out, e
	}
	rows, e := r.migrationRows(ctx, s)
	if e != nil {
		return out, e
	}
	id := content.Identity(s.TenantID, s.ProductID, s.SubjectID, s.WorldInstanceID, "migration", key)
	keys := migrationLocks(s, rows)
	keys = append(keys, releaseLocks(s.ProductScope, from)...)
	keys = append(keys, releaseLocks(s.ProductScope, to)...)
	keys = append(keys, store.LockKey{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}}, content.Key(s.ProductScope, "content_check", id), content.Key(s.ProductScope, "content_check", string(q.PlanID)))
	raw, e := content.BoundCommand(ctx, r.Store, s.ProductScope, "executeInstanceMigration", key, s.WorldInstanceID, "AdminTaskReceipt", q, keys, store.CommandBarrierRequest{WorldScope: &s, Versions: &v, Purpose: store.MigrationPurpose, ExpectedWorldStatus: "PAUSED", Runtime: &store.RuntimeBinding{Action: "MIGRATE", PendingTasks: rows.Tasks}}, func(tx store.Tx) (any, error) {
		if q.PlanHash != planHash || q.ExpectedVersions != plan.ExpectedVersions || !reflect.DeepEqual(q.ResolutionMap, plan.ResolutionMap) {
			return nil, store.ErrConflict
		}
		if _, e := verifyRelease(ctx, tx, s.ProductScope, from); e != nil {
			return nil, e
		}
		if _, e := verifyRelease(ctx, tx, s.ProductScope, to); e != nil {
			return nil, e
		}
		if e := planCurrent(ctx, tx, s, plan); e != nil {
			return nil, e
		}
		h, e := preserveHash(ctx, tx, s, rows, "")
		if e != nil {
			return nil, e
		}
		if h != stateHash {
			return nil, store.ErrConflict
		}
		if e = tx.SealWorldAccounting(ctx, s); e != nil {
			return nil, e
		}
		if e = queueTask(ctx, tx, s, id, "MIGRATION", "INSTANCE_MIGRATION", persistedTask{Scope: s, Versions: v, Status: "PAUSED", Action: "MIGRATE", PlanID: string(q.PlanID)}); e != nil {
			return nil, e
		}
		if e = tx.SetMigration(ctx, id, false); e != nil {
			return nil, e
		}
		return taskReceipt(s.ProductScope, id, "INSTANCE_MIGRATION"), nil
	})
	if e == nil {
		e = generated.Decode("AdminTaskReceipt", raw, &out)
	}
	return out, e
}
func (r *Runtime) processMigration(ctx context.Context, l store.RegisteredLease, worker string, p persistedTask, raw []byte) error {
	plan, stateHash, _, e := r.loadPlan(ctx, p.Scope, p.PlanID)
	if e != nil {
		return e
	}
	from, e := r.Content.GetRelease(ctx, p.Scope.ProductScope, string(plan.FromReleaseID))
	if e != nil {
		return e
	}
	to, e := r.Content.GetRelease(ctx, p.Scope.ProductScope, string(plan.TargetReleaseID))
	if e != nil {
		return e
	}
	rows, e := r.migrationRows(ctx, p.Scope)
	if e != nil {
		return e
	}
	keys := append(releaseLocks(p.Scope.ProductScope, from), releaseLocks(p.Scope.ProductScope, to)...)
	keys = append(keys, migrationLocks(p.Scope, rows)...)
	id := content.Identity(l.TaskID, "result")
	keys = append(keys, eventLocks(p.Scope.ProductScope, id)...)
	keys = append(keys, content.Key(p.Scope.ProductScope, "content_check", p.PlanID))
	g, e := content.Gate(ctx, r.Store, p.Scope.ProductScope)
	if e != nil {
		return e
	}
	_, e = r.Store.WithBarrier(ctx, store.BarrierRequest{ProductScope: p.Scope.ProductScope, WorldScope: &p.Scope, Versions: &p.Versions, OwnerSubjectID: l.OwnerSubjectID, TaskID: l.TaskID, WorkerID: worker, Fence: l.Fence, Purpose: store.MigrationPurpose, ExpectedWorldStatus: "PAUSED", Runtime: &store.RuntimeBinding{Action: "MIGRATE", PendingTasks: rows.Tasks}, Operation: "INSTANCE_MIGRATION", CommandKey: l.TaskID, CommitKey: id, GateVersions: g, PayloadHash: sha256.Sum256(raw), Locks: keys}, func(tx store.Tx) error {
		if e := planCurrent(ctx, tx, p.Scope, plan); e != nil {
			return e
		}
		if _, e := verifyRelease(ctx, tx, p.Scope.ProductScope, from); e != nil {
			return e
		}
		if _, e := verifyRelease(ctx, tx, p.Scope.ProductScope, to); e != nil {
			return e
		}
		if _, e := compatible(from, to); e != nil {
			return e
		}
		h, e := preserveHash(ctx, tx, p.Scope, rows, l.TaskID)
		if e != nil {
			return e
		}
		if h != stateHash {
			return store.ErrConflict
		}
		if e = tx.AdvanceWorld(ctx, "MIGRATE", string(plan.TargetReleaseID)); e != nil {
			return e
		}
		if e = recordResult(ctx, tx, p.Scope, l.TaskID, id, p.Versions.WorldRevision+1, struct{ Kind, PlanID, FromRelease, TargetRelease, Policy string }{"CONTENT_MIGRATION", p.PlanID, string(plan.FromReleaseID), string(plan.TargetReleaseID), migrationPolicy}); e != nil {
			return e
		}
		if e = tx.SetTaskState(ctx, "COMMITTED"); e != nil {
			return e
		}
		return tx.SetMigration(ctx, l.TaskID, true)
	})
	return e
}
