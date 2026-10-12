package memory

import (
	"context"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

type MemoryUpdateRequest struct {
	Namespace Namespace
	TaskID    string
	Nodes     []MemoryRecord
}

// ApplyUpdates consumes only an established WORLD execution UoW. Its success is
// provisional until the shared outer commit succeeds.
func ApplyUpdates(tx store.Tx, r MemoryUpdateRequest) ([]MemoryRecord, error) {
	ctx := context.Background()
	a, e := validatedUpdatesTx(ctx, tx, r)
	if e != nil {
		return nil, e
	}
	if e = saveTx(ctx, tx, r.Namespace, a); e != nil {
		return nil, e
	}
	return r.Nodes, nil
}

// ValidateUpdatesTx checks exactly the same proposals as ApplyUpdates without
// saving the prospective graph. It grants no standalone commit permission.
func ValidateUpdatesTx(ctx context.Context, tx store.Tx, r MemoryUpdateRequest) error {
	_, e := validatedUpdatesTx(ctx, tx, r)
	return e
}
func validatedUpdatesTx(ctx context.Context, tx store.Tx, r MemoryUpdateRequest) (Aggregate, error) {
	zero := Aggregate{}
	if e := tx.AssertWorldTask(r.Namespace.Scope, r.TaskID); e != nil {
		return zero, e
	}
	if len(r.Nodes) == 0 || len(r.Nodes) > 32 {
		return zero, ErrInvalid
	}
	if e := authorizeTx(ctx, tx, r.Namespace, "memory.intake"); e != nil {
		return zero, e
	}
	a, e := loadTx(ctx, tx, r.Namespace)
	if e != nil {
		return zero, e
	}
	basis, e := tx.RehearsalBasis(r.Namespace.Scope)
	if e != nil {
		return zero, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return zero, e
	}
	for _, n := range r.Nodes {
		if domain.ValidateID(n.ActorID) != nil {
			return zero, ErrInvalid
		}
		if n.TargetKind == "PLAYER" {
			if n.TargetID != r.Namespace.DataSubjectID {
				return zero, ErrInvalid
			}
		} else if n.TargetKind == "ACTOR" {
			if e = actorExists(ctx, tx, r.Namespace, n.TargetID); e != nil {
				return zero, e
			}
		} else {
			return zero, ErrInvalid
		}
		if e = actorExists(ctx, tx, r.Namespace, n.ActorID); e != nil {
			return zero, e
		}
		if basis != nil {
			if n.ActorID != basis.ActorID || n.TargetKind != "PLAYER" || n.TargetID != r.Namespace.SubjectID {
				return zero, ErrInvalid
			}
			e = a.addRehearsalDerived(n, now)
		} else {
			e = a.AddDerived(n, now)
		}
		if e != nil {
			return zero, e
		}
	}
	return a, nil
}

const MaxDerivedAssets = 1000

func lineageHash(a Aggregate, n MemoryRecord) string {
	type ref struct {
		ID, Epoch string
		Revision  uint64
	}
	refs := []ref{}
	for _, id := range n.Sources {
		s := a.Sources[id]
		refs = append(refs, ref{id, s.Epoch, s.Revision})
	}
	return digest(struct {
		ID, Actor string
		Revision  uint64
		Sources   []ref
	}{n.ID, n.ActorID, n.Revision, refs})
}
func (s *MemoryService) AuthorizeMemoryAsset(ctx context.Context, scope domain.ProductScope, id string) error {
	var world, subject, actor, node, epoch, hash string
	var revision uint64
	var intent bool
	e := s.Store.DB.QueryRowContext(ctx, "SELECT world_instance_id,data_subject_id,actor_id,node_id,node_revision,source_epoch,lineage_hash,delete_intent FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, id).Scan(&world, &subject, &actor, &node, &revision, &epoch, &hash, &intent)
	if e != nil || intent {
		return store.ErrDenied
	}
	n := Namespace{Scope: domain.Scope{ProductScope: scope, WorldInstanceID: world}, DataSubjectID: subject}
	a, _, e := s.AdminRead(ctx, n, actor, "memory.private_body.read")
	if e != nil {
		return e
	}
	for _, v := range a.Allowed(actor, Private, PrivateResponse, time.Now()) {
		if v.ID == node && v.Revision == revision && a.Epoch == epoch && lineageHash(a, v) == hash {
			return nil
		}
	}
	return store.ErrDenied
}

// RenderPrivateText derives bytes from the current verified node itself. It
// cannot attach caller-supplied assets, paths or replacement body bytes.
func (s *MemoryService) RenderPrivateText(ctx context.Context, n Namespace, actor, node, key string) (string, error) {
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return "", e
	}
	defer release()
	if s.Files == nil {
		return "", ErrInvalid
	}
	a, _, e := s.AdminRead(ctx, n, actor, "memory.private_body.read")
	if e != nil {
		return "", e
	}
	var original MemoryRecord
	for _, v := range a.Allowed(actor, Private, PrivateResponse, time.Now()) {
		if v.ID == node {
			original = v
		}
	}
	if original.ID == "" || original.Body == "" {
		return "", store.ErrDenied
	}
	stage, e := s.Files.AllocateDerived(n.ProductScope, original.Body)
	if e != nil {
		return "", e
	}
	id := content.Identity(n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, node, key, "memory-asset")
	asset := content.Key(n.ProductScope, "asset", id)
	locks := []store.LockKey{asset, s.Files.DerivedStageLock(n.ProductScope, stage)}
	payload := []any{n, node, original.Revision, lineageHash(a, original), key}
	e = s.mutateLocks(ctx, n, key, "MEMORY_DERIVED_STAGE", payload, locks, func(tx store.Tx, current *Aggregate) error {
		if e := authorizeTx(ctx, tx, n, "memory.private_body.read"); e != nil {
			return e
		}
		eligible := false
		for _, v := range current.Allowed(actor, Private, PrivateResponse, time.Now()) {
			if v.ID == node && v.Revision == original.Revision && lineageHash(*current, v) == lineageHash(a, original) {
				eligible = true
			}
		}
		if !eligible {
			return store.ErrDenied
		}
		var count int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID).Scan(&count); e != nil {
			return e
		}
		if count >= MaxDerivedAssets {
			return ErrInvalid
		}
		if e := tx.RequireLock(s.Files.DerivedStageLock(n.ProductScope, stage)); e != nil {
			return e
		}
		storage := s.Files.DerivedStorage(stage)
		if _, e := tx.ExecContext(ctx, "INSERT INTO file_staging(tenant_id,product_id,storage_key,owner_subject_id) VALUES(?,?,?,?)", n.TenantID, n.ProductID, storage, n.SubjectID); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "INSERT INTO memory_asset_lineage(tenant_id,product_id,storage_key,render_id,world_instance_id,data_subject_id,actor_id,node_id,node_revision,source_epoch,lineage_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?)", n.TenantID, n.ProductID, storage, id, n.WorldInstanceID, n.DataSubjectID, actor, node, original.Revision, current.Epoch, lineageHash(*current, original))
		return e
	})
	if e != nil {
		return "", e
	}
	var storage string
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT storage_key FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND render_id=?", n.TenantID, n.ProductID, id).Scan(&storage); e != nil {
		return "", e
	}
	stage, e = s.Files.RecoverDerived(n.ProductScope, storage, original.Body)
	if e != nil {
		return "", e
	}
	locks = []store.LockKey{asset, s.Files.DerivedStageLock(n.ProductScope, stage)}
	if e = s.Files.WriteDerived(ctx, n.ProductScope, stage, original.Body); e != nil {
		return "", e
	}

	e = s.mutateLocks(ctx, n, key, "MEMORY_DERIVED_FILE", payload, locks, func(tx store.Tx, current *Aggregate) error {
		if e := authorizeTx(ctx, tx, n, "memory.private_body.read"); e != nil {
			return e
		}
		eligible := false
		for _, v := range current.Allowed(actor, Private, PrivateResponse, time.Now()) {
			if v.ID == node && v.Revision == original.Revision && lineageHash(*current, v) == lineageHash(a, original) {
				eligible = true
			}
		}
		if !eligible {
			return store.ErrDenied
		}
		var count int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID).Scan(&count); e != nil {
			return e
		}
		if count > MaxDerivedAssets || len(current.Nodes[node].MediaRefs) >= 64 {
			return ErrInvalid
		}
		if _, e := s.Files.BindDerived(ctx, tx, n.ProductScope, id, stage); e != nil {
			return e
		}
		if e := tx.RequireLock(asset); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET asset_id=? WHERE tenant_id=? AND product_id=? AND storage_key=? AND render_id=? AND delete_intent=0", id, n.TenantID, n.ProductID, storage, id); e != nil {
			return e
		}
		v := current.Nodes[node]
		v.MediaRefs = append(v.MediaRefs, id)
		current.Nodes[node] = v
		return nil
	})
	if e != nil {
		return "", e
	}
	var state string
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT state FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", n.TenantID, n.ProductID, id).Scan(&state); e != nil {
		return "", e
	}
	if state == "READY" {
		return id, nil
	}
	if e = s.Files.FinalizeDerived(ctx, n.ProductScope, id, stage); e != nil {
		return "", e
	}
	e = s.mutateLocks(ctx, n, key, "MEMORY_DERIVED_READY", payload, locks, func(tx store.Tx, current *Aggregate) error {
		if e := authorizeTx(ctx, tx, n, "memory.private_body.read"); e != nil {
			return e
		}
		eligible := false
		for _, v := range current.Allowed(actor, Private, PrivateResponse, time.Now()) {
			if v.ID == node && v.Revision == original.Revision {
				eligible = true
			}
		}
		if !eligible {
			return store.ErrDenied
		}
		if e := tx.RequireLock(asset); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "UPDATE assets a JOIN memory_asset_lineage m USING(tenant_id,product_id,asset_id) SET a.state='READY' WHERE a.tenant_id=? AND a.product_id=? AND a.asset_id=? AND a.state='PENDING' AND m.delete_intent=0", n.TenantID, n.ProductID, id)
		return e
	})
	return id, e
}

type assetRef struct{ ID, Storage string }

func (s *MemoryService) assetLocks(ctx context.Context, n Namespace) ([]store.LockKey, []assetRef, error) {
	rows, e := s.Store.DB.QueryContext(ctx, "SELECT COALESCE(asset_id,''),storage_key FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? ORDER BY storage_key LIMIT 1001", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID)
	if e != nil {
		return nil, nil, e
	}
	defer rows.Close()
	keys := []store.LockKey{}
	ids := []assetRef{}
	for rows.Next() {
		var ref assetRef
		if e = rows.Scan(&ref.ID, &ref.Storage); e != nil {
			return nil, nil, e
		}
		ids = append(ids, ref)
		keys = append(keys, content.Key(n.ProductScope, "asset", "staging."+ref.Storage))
		if ref.ID != "" {
			keys = append(keys, content.Key(n.ProductScope, "asset", ref.ID))
		}
	}
	if len(ids) > MaxDerivedAssets {
		return nil, nil, ErrInvalid
	}
	return keys, ids, rows.Err()
}
func (s *MemoryService) blockAssets(ctx context.Context, tx store.Tx, n Namespace, a Aggregate, ids []assetRef) error {
	var count int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID).Scan(&count); e != nil {
		return e
	}
	if count != len(ids) {
		return store.ErrReplan
	}
	for _, ref := range ids {
		var actor, node, id string
		if e := tx.QueryRowContext(ctx, "SELECT actor_id,node_id,COALESCE(asset_id,'') FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, ref.Storage, n.WorldInstanceID, n.DataSubjectID).Scan(&actor, &node, &id); e != nil {
			return e
		}
		if id != ref.ID {
			return store.ErrReplan
		}
		allowed := false
		for _, v := range a.Allowed(actor, Private, PrivateResponse, time.Now()) {
			if v.ID == node {
				allowed = true
			}
		}
		if result, eligible, e := resultLineageTx(ctx, tx, n, ref.Storage, actor, a); e != nil {
			return e
		} else if result {
			allowed = eligible
		}
		if allowed {
			continue
		}
		if e := tx.RequireLock(content.Key(n.ProductScope, "asset", "staging."+ref.Storage)); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET delete_intent=1 WHERE tenant_id=? AND product_id=? AND storage_key=?", n.TenantID, n.ProductID, ref.Storage); e != nil {
			return e
		}
		if id != "" {
			if e := tx.RequireLock(content.Key(n.ProductScope, "asset", id)); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, "UPDATE assets SET state='DELETED' WHERE tenant_id=? AND product_id=? AND asset_id=?", n.TenantID, n.ProductID, id); e != nil {
				return e
			}
		}
	}
	return nil
}

// Only these storage identities have durable prior confirmation or a successful
// actual purge in this pass. An asset skipped without intent is never included.
func (s *MemoryService) purgeAssets(ctx context.Context, n Namespace, ids []assetRef) (map[string]assetRef, error) {
	confirmed := make(map[string]assetRef)
	for _, ref := range ids {
		var intent, purged bool
		if e := s.Store.DB.QueryRowContext(ctx, "SELECT delete_intent,purged FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, ref.Storage, n.WorldInstanceID, n.DataSubjectID).Scan(&intent, &purged); e != nil {
			return nil, e
		}
		if intent && !purged {
			if s.Files == nil {
				return nil, ErrPreservation
			}
			if e := s.Files.PurgeDerived(ctx, n.ProductScope, ref.Storage); e != nil {
				return nil, e
			}
		}
		if intent {
			confirmed[ref.Storage] = ref
		}
	}
	return confirmed, nil
}

// The namespace lock prevents concurrent lineage intake or new governance while
// this current obligation check and acknowledgement run. File I/O already ended.
func acknowledgePurgedAssets(ctx context.Context, tx store.Tx, n Namespace, ids []assetRef, confirmed map[string]assetRef) error {
	if e := tx.RequireLock(n.Key()); e != nil {
		return e
	}
	var count int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID).Scan(&count); e != nil {
		return e
	}
	if count != len(ids) {
		return store.ErrReplan
	}
	for _, ref := range ids {
		if e := tx.RequireLock(content.Key(n.ProductScope, "asset", "staging."+ref.Storage)); e != nil {
			return e
		}
		var id string
		var intent, purged bool
		if e := tx.QueryRowContext(ctx, "SELECT COALESCE(asset_id,''),delete_intent,purged FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, ref.Storage, n.WorldInstanceID, n.DataSubjectID).Scan(&id, &intent, &purged); e != nil {
			return e
		}
		proof, ok := confirmed[ref.Storage]
		if id != ref.ID || ok && (proof != ref || !intent) || intent && !purged && !ok {
			return store.ErrReplan
		}
		if ok {
			if _, e := tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET purged=1 WHERE tenant_id=? AND product_id=? AND storage_key=? AND world_instance_id=? AND data_subject_id=? AND delete_intent=1", n.TenantID, n.ProductID, ref.Storage, n.WorldInstanceID, n.DataSubjectID); e != nil {
				return e
			}
		}
	}
	return nil
}
