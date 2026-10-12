package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
)

type RehearsalMemoryInitializer interface {
	CreationLocks(domain.Scope) []store.LockKey
	CreateRehearsalInitial(context.Context, store.Tx, domain.Scope, string, Initialization, map[string]generated.Release) error
}

// Only a transient projection of the current exact frozen authoring basis.
// It is never inserted in content_releases or returned by published APIs.
func rehearsalRelease(root string, m content.RehearsalDefinition) generated.Release {
	return generated.Release{ReleaseID: generated.Identity(content.Identity(root, "rehearsal-version")), ContentHash: m.Draft.ContentHash, ProjectID: m.Draft.ProjectID, Input: m.Draft.Input, DependencyLocks: m.Draft.Input.DependencyLocks}
}

func qualifiedRehearsalTx(ctx context.Context, tx store.Tx, scope domain.Scope) (*domain.RehearsalBasis, generated.Release, error) {
	if historical, e := tx.RehearsalReportHistoryBasis(scope); e != nil {
		return nil, generated.Release{}, e
	} else if historical != nil {
		m, e := content.New(nil).RehearsalHistoryDefinitionTx(ctx, tx, scope.ProductScope, historical.Binding)
		if e != nil {
			return nil, generated.Release{}, e
		}
		return historical, rehearsalRelease(historical.RootTaskID, m), nil
	}
	b, e := tx.RehearsalBasis(scope)
	if e != nil || b == nil {
		return b, generated.Release{}, e
	}
	m, e := content.New(nil).BuildRehearsalDefinitionTx(ctx, tx, scope.ProductScope, b.Binding.DraftID, b.Request)
	if e != nil {
		return nil, generated.Release{}, e
	}
	if m.Draft.ContentHash != b.Binding.DraftHash || m.SourceHash != b.Binding.SourceHash || m.RouteHash != b.Binding.RouteHash || !reflect.DeepEqual(m.Settings, b.Binding.Settings) {
		return nil, generated.Release{}, store.ErrConflict
	}
	return b, rehearsalRelease(b.RootTaskID, m), nil
}

func (r *Runtime) PlanRehearsalHistoryRuntimeLocks(ctx context.Context, s domain.ProductScope, root string) ([]store.LockKey, error) {
	world := domain.Scope{ProductScope: s, WorldInstanceID: content.Identity(root, "rehearsal-world")}
	var raw []byte
	if e := r.Store.DB.QueryRowContext(ctx, "SELECT initialization FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=?", s.TenantID, s.ProductID, world.WorldInstanceID, s.SubjectID).Scan(&raw); e != nil {
		return nil, e
	}
	var init Initialization
	if json.Unmarshal(raw, &init) != nil || len(init.Actors) > 32 || len(init.Items) > 32 || r.RehearsalMemory == nil {
		return nil, store.ErrDenied
	}
	keys := r.RehearsalMemory.CreationLocks(world)
	keys = append(keys, resourceKey(world, "content_release", init.ReleaseID))
	// The internal version key is Product-scoped, despite runtime rows being World-scoped.
	keys[len(keys)-1] = content.Key(s, "content_release", init.ReleaseID)
	for _, a := range init.Actors {
		keys = append(keys, resourceKey(world, "actor", a.ID))
	}
	for _, i := range init.Items {
		keys = append(keys, resourceKey(world, "item", i.ID))
	}
	return keys, nil
}

func (r *Runtime) RehearsalRouteHashTx(ctx context.Context, tx store.Tx, s domain.Scope, actor string) (string, error) {
	route, e := rehearsalGenerationRouteTx(ctx, tx, s, actor)
	if e != nil {
		return "", e
	}
	if route == nil {
		return "", store.ErrDenied
	}
	return content.Hash(*route)
}
func rehearsalGenerationRouteTx(ctx context.Context, tx store.Tx, s domain.Scope, actor string) (*GenerationRoute, error) {
	b, rel, e := qualifiedRehearsalTx(ctx, tx, s)
	if e != nil || b == nil {
		return nil, e
	}
	if b.ActorID != actor {
		return nil, store.ErrDenied
	}
	g := b.Binding.Settings
	if g.Mode != "OVERRIDE" || g.ConnectionRef == nil || g.ModelID == nil || string(*g.ConnectionRef) != content.MockConnection || string(*g.ModelID) != content.MockModel {
		return nil, store.ErrDenied
	}
	ref, _ := json.Marshal(map[string]string{"kind": "LOCAL_DRAFT", "object_id": string(rel.Input.RootObjectID)})
	settings, e := content.Hash(g)
	if e != nil {
		return nil, e
	}
	descriptor, _ := content.Hash(struct{ Connection, Model, Price string }{content.MockConnection, content.MockModel, "0"})
	return &GenerationRoute{ReleaseID: string(rel.ReleaseID), ActorID: actor, Module: "REHEARSAL", Connection: content.MockConnection, Model: content.MockModel, DescriptorHash: descriptor, SettingsHash: settings, OwnerReference: ref, Settings: g}, nil
}
func (r *Runtime) PlanRehearsalInitialization(ctx context.Context, scope domain.ProductScope, root string, b domain.RehearsalBinding, request generated.RehearsalRequest) ([]store.LockKey, error) {
	if r.RehearsalMemory == nil {
		return nil, store.ErrDenied
	}
	m, e := r.Content.InspectRehearsal(ctx, scope, b.DraftID, request)
	if e != nil {
		return nil, e
	}
	if m.Draft.ContentHash != b.DraftHash || m.SourceHash != b.SourceHash || m.RouteHash != b.RouteHash {
		return nil, store.ErrReplan
	}
	rel := rehearsalRelease(root, m)
	catalog := map[string]generated.Release{string(rel.ReleaseID): rel}
	for _, d := range rel.DependencyLocks {
		if _, ok := catalog[string(d.ReleaseID)]; ok {
			continue
		}
		source, e := r.Content.GetRelease(ctx, scope, string(d.ReleaseID))
		if e != nil {
			return nil, e
		}
		catalog[string(d.ReleaseID)] = source
	}
	props, e := collectRuntimeProps(rel, catalog)
	if e != nil {
		return nil, e
	}
	w := domain.Scope{ProductScope: scope, WorldInstanceID: content.Identity(root, "rehearsal-world")}
	init, e := assemble(w, rel, m.Resolved, props)
	if e != nil {
		return nil, e
	}
	keys := []store.LockKey{content.Key(scope, "content_release", string(rel.ReleaseID))}
	keys = append(keys, r.RehearsalMemory.CreationLocks(w)...)
	for _, a := range init.Actors {
		keys = append(keys, resourceKey(w, "actor", a.ID))
	}
	for _, i := range init.Items {
		keys = append(keys, resourceKey(w, "item", i.ID))
	}
	return keys, nil
}
func (r *Runtime) InitializeRehearsalTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, root string, b domain.RehearsalBinding, request generated.RehearsalRequest) error {
	w := domain.Scope{ProductScope: scope, WorldInstanceID: content.Identity(root, "rehearsal-world")}
	if e := tx.AssertRehearsalInitialization(w, root); e != nil {
		return e
	}
	if r.RehearsalMemory == nil {
		return store.ErrDenied
	}
	current, e := r.Content.BuildRehearsalTx(ctx, tx, scope, b.DraftID, request)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(b, current) {
		return store.ErrReplan
	}
	m, e := r.Content.BuildRehearsalDefinitionTx(ctx, tx, scope, b.DraftID, request)
	if e != nil {
		return e
	}
	rel := rehearsalRelease(root, m)
	catalog := map[string]generated.Release{string(rel.ReleaseID): rel}
	for _, d := range rel.DependencyLocks {
		id := string(d.ReleaseID)
		if _, ok := catalog[id]; ok {
			continue
		}
		if e = tx.RequireLock(content.Key(scope, "content_release", id)); e != nil {
			return e
		}
		var raw []byte
		if e = tx.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", scope.TenantID, scope.ProductID, id).Scan(&raw); e != nil {
			return e
		}
		var source generated.Release
		if e = generated.Decode("Release", raw, &source); e != nil {
			return e
		}
		catalog[id] = source
	}
	props, e := collectRuntimeProps(rel, catalog)
	if e != nil {
		return e
	}
	init, e := assemble(w, rel, m.Resolved, props)
	if e != nil {
		return e
	}
	init.CreationCommandID = content.Identity(root, "rehearsal-init")
	init.Participants = []string{scope.SubjectID}
	if e = tx.RequireLock(worldKey(w)); e != nil {
		return e
	}
	raw, e := json.Marshal(init)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO runtime_instances(tenant_id,product_id,world_instance_id,owner_subject_id,project_id,definition_id,mode,initial_release_id,initialization) VALUES(?,?,?,?,?,?,'PRIVATE',?,?)", scope.TenantID, scope.ProductID, w.WorldInstanceID, scope.SubjectID, init.ProjectID, init.DefinitionID, rel.ReleaseID, raw); e != nil {
		return e
	}
	for _, a := range init.Actors {
		if e = tx.RequireLock(resourceKey(w, "actor", a.ID)); e != nil {
			return e
		}
		raw, e = json.Marshal(a)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO actors(tenant_id,product_id,world_instance_id,actor_id,slot_id,document) VALUES(?,?,?,?,?,?)", scope.TenantID, scope.ProductID, w.WorldInstanceID, a.ID, a.Slot, raw); e != nil {
			return e
		}
	}
	for _, i := range init.Items {
		if e = tx.RequireLock(resourceKey(w, "item", i.ID)); e != nil {
			return e
		}
		raw, e = json.Marshal(i)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO items(tenant_id,product_id,world_instance_id,item_id,document) VALUES(?,?,?,?,?)", scope.TenantID, scope.ProductID, w.WorldInstanceID, i.ID, raw); e != nil {
			return e
		}
	}
	return r.RehearsalMemory.CreateRehearsalInitial(ctx, tx, w, root, init, catalog)
}
