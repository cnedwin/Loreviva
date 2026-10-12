// Package runtime owns real persistent world instances. Authoring definitions
// and proposed actions never confer runtime authority.
package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
)

type Runtime struct {
	Store           *store.Store
	Content         *content.Service
	InitialMemory   InitialMemoryAdapter
	RehearsalMemory RehearsalMemoryInitializer
}

// InitialMemoryAdapter participates only in the original creation transaction.
// Production app construction always registers it; standalone Runtime fixtures
// may leave it nil only with no installed production coordinator.
type InitialMemoryAdapter interface {
	CreationLocks(domain.Scope) []store.LockKey
	CreateInitial(context.Context, store.Tx, domain.Scope, Initialization) error
}

func New(s *store.Store) *Runtime { return &Runtime{Store: s, Content: content.New(s)} }

type CreateInstanceCommand struct {
	Request generated.AdminCreateInstanceRequest
	Key     string
}
type ActorState struct {
	ID, Slot, Name, OriginalReleaseID, DefinitionID string
	OwnerReference                                  generated.DefinitionRef
	OriginalSetup                                   generated.InitialActorSetup
	OriginalCharacter                               generated.CharacterDefinition
}
type ItemState struct {
	OwnerReference                                 generated.DefinitionRef
	OriginalReleaseID                              string
	Note                                           string
	ID, DefinitionID, CustodianID, State, HolderID string
	Definition                                     generated.PropDefinition
	Rules                                          []generated.RuleDefinition
}
type Initialization struct {
	ReleaseID, ReleaseHash, ProjectID, DefinitionID, PrimaryActorID, CreationCommandID string
	Participants                                                                       []string
	Actors                                                                             []ActorState
	Items                                                                              []ItemState
}

// InitializationEvidence exposes original creation evidence only inside the
// existing locked world UoW. Current release knowledge is never substituted.
func InitializationEvidence(ctx context.Context, tx store.Tx, s domain.Scope) (Initialization, error) {
	var out Initialization
	if e := tx.RequireLock(worldKey(s)); e != nil {
		return out, e
	}
	var raw []byte
	e := tx.QueryRowContext(ctx, "SELECT i.initialization FROM runtime_instances i WHERE i.tenant_id=? AND i.product_id=? AND i.world_instance_id=? AND i.owner_subject_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID).Scan(&raw)
	if e != nil {
		return out, e
	}
	if e = json.Unmarshal(raw, &out); e != nil {
		return out, e
	}
	var committed bool
	if b, _, e := qualifiedRehearsalTx(ctx, tx, s); e != nil {
		return out, e
	} else if b != nil {
		if out.CreationCommandID != content.Identity(b.RootTaskID, "rehearsal-init") || out.ReleaseID != content.Identity(b.RootTaskID, "rehearsal-version") || out.ReleaseHash != b.Binding.DraftHash {
			return out, store.ErrConflict
		}
		e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_worlds w JOIN commit_receipts c ON c.commit_key=w.initialization_receipt AND c.task_id=w.root_task_id AND c.tenant_id=w.tenant_id AND c.product_id=w.product_id AND c.operation='REHEARSAL_INIT' AND c.phase='COMMIT' WHERE w.tenant_id=? AND w.product_id=? AND w.root_task_id=? AND w.world_instance_id=? AND w.release_id=? AND c.owner_subject_id=?)", s.TenantID, s.ProductID, b.RootTaskID, s.WorldInstanceID, out.ReleaseID, s.SubjectID).Scan(&committed)
		if e != nil {
			return out, e
		}
		if !committed {
			return out, store.ErrDenied
		}
		return out, nil
	}
	e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE commit_key=? AND tenant_id=? AND product_id=? AND owner_subject_id=? AND operation='createAdminInstance' AND task_id IS NULL)", out.CreationCommandID, s.TenantID, s.ProductID, s.SubjectID).Scan(&committed)
	if e != nil {
		return out, e
	}
	if !committed {
		return out, store.ErrDenied
	}
	return out, nil
}

func worldKey(s domain.Scope) store.LockKey {
	return store.LockKey{Rank: 40, IDs: []string{s.TenantID, s.ProductID, s.WorldInstanceID}}
}
func resourceKey(s domain.Scope, kind, id string) store.LockKey {
	return store.LockKey{Rank: 90, Kind: kind, IDs: []string{s.TenantID, s.ProductID, s.WorldInstanceID, id}}
}
func releaseLocks(s domain.ProductScope, r generated.Release) []store.LockKey {
	keys := []store.LockKey{content.Key(s, "content_release", string(r.ReleaseID))}
	for _, d := range r.DependencyLocks {
		keys = append(keys, content.Key(s, "content_release", string(d.ReleaseID)))
	}
	return keys
}
func verifyRelease(ctx context.Context, tx store.Tx, s domain.ProductScope, wanted generated.Release) (content.ResolvedDefinitions, error) {
	if e := tx.RequireLock(content.Key(s, "content_release", string(wanted.ReleaseID))); e != nil {
		return nil, e
	}
	var raw, hash []byte
	e := tx.QueryRowContext(ctx, "SELECT c.document,c.content_hash FROM content_releases c JOIN releases r ON r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.release_id=c.release_id WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", s.TenantID, s.ProductID, wanted.ReleaseID).Scan(&raw, &hash)
	if e != nil {
		return nil, content.ErrNotFound
	}
	var r generated.Release
	if e = generated.Decode("Release", raw, &r); e != nil {
		return nil, e
	}
	h, e := content.Hash(struct {
		Input        generated.DraftInput
		Dependencies []generated.DependencyLock
		Mappings     any
	}{r.Input, r.DependencyLocks, r.RouteMappings})
	if e != nil || h != r.ContentHash || h != wanted.ContentHash || h != hex.EncodeToString(hash) || r.Scope != content.Scope(s) {
		return nil, content.ErrBlocked
	}
	return content.ResolvePinned(ctx, tx, s, r.Input)
}
func assemble(s domain.Scope, r generated.Release, resolved content.ResolvedDefinitions, props []runtimeProp) (Initialization, error) {
	init := Initialization{ReleaseID: string(r.ReleaseID), ReleaseHash: r.ContentHash, DefinitionID: string(r.Input.RootObjectID), Actors: []ActorState{}, Items: []ItemState{}}
	if r.ProjectID == nil {
		return init, content.ErrBlocked
	}
	init.ProjectID = string(*r.ProjectID)
	var world generated.WorldDefinition
	for _, raw := range r.Input.Objects {
		var head struct {
			ObjectID string `json:"object_id"`
		}
		_ = json.Unmarshal(raw, &head)
		if head.ObjectID == init.DefinitionID {
			if e := generated.Decode("WorldDefinition", raw, &world); e != nil {
				return init, e
			}
		}
	}
	if world.Kind != "WORLD" {
		return init, content.ErrBlocked
	}
	for _, setup := range world.InitialActorSetups {
		raw, e := content.ResolveDefinition(setup.CharacterRef, r.Input, resolved)
		if e != nil {
			return init, e
		}
		var c generated.CharacterDefinition
		if e = generated.Decode("CharacterDefinition", raw, &c); e != nil {
			return init, e
		}
		a := ActorState{ID: content.Identity(s.TenantID, s.ProductID, s.WorldInstanceID, "actor", string(setup.SlotID)), Slot: string(setup.SlotID), Name: c.Name, OriginalReleaseID: string(r.ReleaseID), DefinitionID: string(c.ObjectID), OwnerReference: setup.CharacterRef, OriginalSetup: setup, OriginalCharacter: c}
		if setup.NameOverride != nil {
			a.Name = *setup.NameOverride
		}
		init.Actors = append(init.Actors, a)
		if setup.SlotID == world.PrimaryActorSlot {
			init.PrimaryActorID = a.ID
		}
	}
	if init.PrimaryActorID == "" {
		return init, content.ErrBlocked
	}
	for _, spec := range props {
		p := spec.Definition
		item := ItemState{ID: runtimeItemID(s.TenantID, s.ProductID, s.WorldInstanceID, spec), DefinitionID: string(p.ObjectID), Definition: p, State: p.State, Rules: spec.Rules, OwnerReference: spec.Ref, OriginalReleaseID: spec.SourceRelease}
		for _, a := range init.Actors {
			if a.Slot == string(p.CustodianSlot) {
				item.CustodianID = a.ID
			}
		}
		if item.CustodianID == "" {
			return init, content.ErrBlocked
		}
		init.Items = append(init.Items, item)
	}
	return init, nil
}
func (r *Runtime) CreateInstance(ctx context.Context, s domain.ProductScope, c CreateInstanceCommand) (generated.Instance, error) {
	var out generated.Instance
	if r.InitialMemory == nil && r.Store.Guard != nil {
		return out, store.ErrDenied
	}
	if c.Request.ProductID != s.ProductID {
		return out, content.ErrBlocked
	}
	// R0 has only the explicitly registered synthetic creator. Future customer
	// enrollment requires its own verified session adapter, never body authority.
	for _, p := range c.Request.InitialParticipants {
		if p != s.SubjectID || s.TenantID != "demo-local" || s.ProductID != "coffee" || p != "creator" {
			return out, content.ErrBlocked
		}
	}
	rel, e := r.Content.GetRelease(ctx, s, c.Request.ReleaseID)
	if e != nil {
		return out, e
	}
	id := content.Identity(s.TenantID, s.ProductID, s.SubjectID, "createAdminInstance", c.Key)
	scope := domain.Scope{ProductScope: s, WorldInstanceID: id}
	keys := releaseLocks(s, rel)
	if r.InitialMemory != nil {
		keys = append(keys, r.InitialMemory.CreationLocks(scope)...)
	}
	if len(rel.DependencyLocks) > 200 {
		return out, content.ErrBlocked
	}
	catalog := map[string]generated.Release{string(rel.ReleaseID): rel}
	for _, dep := range rel.DependencyLocks {
		if _, ok := catalog[string(dep.ReleaseID)]; ok {
			continue
		}
		source, x := r.Content.GetRelease(ctx, s, string(dep.ReleaseID))
		if x != nil {
			return out, x
		}
		catalog[string(dep.ReleaseID)] = source
	}
	props, e := collectRuntimeProps(rel, catalog)
	if e != nil {
		return out, e
	}
	for _, p := range props {
		keys = append(keys, resourceKey(scope, "item", runtimeItemID(s.TenantID, s.ProductID, id, p)))
	}
	// Precollect actor/item identity from immutable slot and object IDs; pinned
	// character versions remain qualified in the resulting actor document.
	for _, raw := range rel.Input.Objects {
		var h struct {
			Kind               string                        `json:"kind"`
			ObjectID           string                        `json:"object_id"`
			InitialActorSetups []generated.InitialActorSetup `json:"initial_actor_setups"`
		}
		_ = json.Unmarshal(raw, &h)
		if h.Kind == "WORLD" && h.ObjectID == string(rel.Input.RootObjectID) {
			for _, a := range h.InitialActorSetups {
				keys = append(keys, resourceKey(scope, "actor", content.Identity(s.TenantID, s.ProductID, id, "actor", string(a.SlotID))))
			}
		}
		if h.Kind == "PROP" {
			keys = append(keys, resourceKey(scope, "item", content.Identity(s.TenantID, s.ProductID, id, "item", h.ObjectID)))
		}
	}
	raw, e := content.BoundCommand(ctx, r.Store, s, "createAdminInstance", c.Key, id, "Instance", c.Request, keys, store.CommandBarrierRequest{Runtime: &store.RuntimeBinding{CreationID: id, Action: "CREATE"}}, func(tx store.Tx) (any, error) {
		if c.Request.Mode != "PRIVATE" {
			return nil, content.ErrBlocked
		}
		resolved, e := verifyRelease(ctx, tx, s, rel)
		if e != nil {
			return nil, e
		}
		// Revalidate every catalog snapshot under the precollected release locks.
		for _, source := range catalog {
			if _, e = verifyRelease(ctx, tx, s, source); e != nil {
				return nil, e
			}
		}
		props, e := collectRuntimeProps(rel, catalog)
		if e != nil {
			return nil, e
		}
		init, e := assemble(scope, rel, resolved, props)
		if e != nil {
			return nil, e
		}
		init.Participants = append([]string{}, c.Request.InitialParticipants...)
		init.CreationCommandID = id
		if e = tx.CreateWorld(ctx, c.Request.ReleaseID); e != nil {
			return nil, e
		}
		b, e := json.Marshal(init)
		if e != nil {
			return nil, e
		}
		if e = tx.RequireLock(worldKey(scope)); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO runtime_instances(tenant_id,product_id,world_instance_id,owner_subject_id,project_id,definition_id,mode,initial_release_id,initialization) VALUES(?,?,?,?,?,?,'PRIVATE',?,?)", s.TenantID, s.ProductID, id, s.SubjectID, init.ProjectID, init.DefinitionID, rel.ReleaseID, b)
		if e != nil {
			return nil, e
		}
		for _, a := range init.Actors {
			if e = tx.RequireLock(resourceKey(scope, "actor", a.ID)); e != nil {
				return nil, e
			}
			b, e = json.Marshal(a)
			if e != nil {
				return nil, e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO actors(tenant_id,product_id,world_instance_id,actor_id,slot_id,document) VALUES(?,?,?,?,?,?)", s.TenantID, s.ProductID, id, a.ID, a.Slot, b); e != nil {
				return nil, e
			}
		}
		for _, i := range init.Items {
			if e = tx.RequireLock(resourceKey(scope, "item", i.ID)); e != nil {
				return nil, e
			}
			b, e = json.Marshal(i)
			if e != nil {
				return nil, e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO items(tenant_id,product_id,world_instance_id,item_id,document) VALUES(?,?,?,?,?)", s.TenantID, s.ProductID, id, i.ID, b); e != nil {
				return nil, e
			}
		}
		if r.InitialMemory != nil {
			if e = r.InitialMemory.CreateInitial(ctx, tx, scope, init); e != nil {
				return nil, e
			}
		}
		return generated.Instance{Scope: generated.WorldScope{TenantID: s.TenantID, ProductID: s.ProductID, WorldInstanceID: id}, Mode: "PRIVATE", InstanceState: "CREATED", Versions: generated.Versions{ReleaseID: c.Request.ReleaseID, WorldRevision: "0", ControlVersion: "0", PrivacyEpoch: "1", ViewEpoch: "1"}}, nil
	})
	if e == nil {
		e = generated.Decode("Instance", raw, &out)
	}
	return out, e
}
func (r *Runtime) GetInstance(ctx context.Context, s domain.Scope) (generated.Instance, error) {
	var out generated.Instance
	if s.Validate() != nil {
		return out, content.ErrNotFound
	}
	if _, e := content.Gate(ctx, r.Store, s.ProductScope); e != nil {
		return out, e
	}
	var v domain.Versions
	e := r.Store.DB.QueryRowContext(ctx, "SELECT w.status,w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,i.mode FROM world_controls w JOIN runtime_instances i ON i.tenant_id=w.tenant_id AND i.product_id=w.product_id AND i.world_instance_id=w.world_instance_id JOIN releases r ON r.tenant_id=w.tenant_id AND r.product_id=w.product_id AND r.release_id=w.release_id WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=? AND i.owner_subject_id=? AND r.available=1", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID).Scan(&out.InstanceState, &v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &out.Mode)
	if e != nil {
		return out, content.ErrNotFound
	}
	out.Scope = generated.WorldScope{TenantID: s.TenantID, ProductID: s.ProductID, WorldInstanceID: s.WorldInstanceID}
	out.Versions = wireVersions(v)
	return out, nil
}
func wireVersions(v domain.Versions) generated.Versions {
	return generated.Versions{ReleaseID: v.ReleaseID, WorldRevision: strconv.FormatUint(v.WorldRevision, 10), ControlVersion: strconv.FormatUint(v.ControlVersion, 10), PrivacyEpoch: strconv.FormatUint(v.PrivacyEpoch, 10), ViewEpoch: strconv.FormatUint(v.ViewEpoch, 10)}
}
func versions(v generated.Versions) (domain.Versions, error) {
	out := domain.Versions{ReleaseID: v.ReleaseID}
	fields := []string{v.WorldRevision, v.ControlVersion, v.PrivacyEpoch, v.ViewEpoch}
	ptrs := []*uint64{&out.WorldRevision, &out.ControlVersion, &out.PrivacyEpoch, &out.ViewEpoch}
	for n, s := range fields {
		x, e := strconv.ParseUint(s, 10, 64)
		if e != nil {
			return out, e
		}
		*ptrs[n] = x
	}
	return out, out.Validate()
}
func (r *Runtime) ListInstances(ctx context.Context, s domain.ProductScope, limit int, after string) (generated.AdminInstancePage, error) {
	out := generated.AdminInstancePage{Items: []generated.Instance{}}
	if _, e := content.Gate(ctx, r.Store, s); e != nil {
		return out, e
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if after != "" && domain.ValidateID(after) != nil {
		return out, content.ErrBlocked
	}
	rows, e := r.Store.DB.QueryContext(ctx, "SELECT i.world_instance_id FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) JOIN releases r ON r.tenant_id=w.tenant_id AND r.product_id=w.product_id AND r.release_id=w.release_id WHERE i.tenant_id=? AND i.product_id=? AND i.owner_subject_id=? AND i.world_instance_id>? AND r.available=1 ORDER BY i.world_instance_id LIMIT ?", s.TenantID, s.ProductID, s.SubjectID, after, limit+1)
	if e != nil {
		return out, e
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return out, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return out, e
	}
	if len(ids) > limit {
		ids = ids[:limit]
		next := ids[len(ids)-1]
		out.NextCursor = &next
	}
	for _, id := range ids {
		i, e := r.GetInstance(ctx, domain.Scope{ProductScope: s, WorldInstanceID: id})
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, i)
	}
	return out, nil
}
func (r *Runtime) Actors(ctx context.Context, s domain.Scope) (generated.AdminActorPage, error) {
	out := generated.AdminActorPage{Items: []generated.AdminActorMetadata{}}
	i, e := r.GetInstance(ctx, s)
	if e != nil {
		return out, e
	}
	rows, e := r.Store.DB.QueryContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? ORDER BY actor_id LIMIT 1001", s.TenantID, s.ProductID, s.WorldInstanceID)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var a ActorState
		if e = rows.Scan(&b); e != nil {
			return out, e
		}
		if e = json.Unmarshal(b, &a); e != nil {
			return out, e
		}
		out.Items = append(out.Items, generated.AdminActorMetadata{Scope: i.Scope, ActorInstanceID: generated.Identity(a.ID), DefinitionObjectID: generated.Identity(a.DefinitionID), DisplayName: a.Name, InitialSlotID: generated.Identity(a.Slot), ReleaseID: generated.Identity(i.Versions.ReleaseID)})
		if len(out.Items) > 1000 {
			return out, content.ErrBlocked
		}
	}
	return out, rows.Err()
}
