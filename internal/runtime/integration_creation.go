package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
)

type IntegrationInitialMemoryAdapter interface {
	InitialMemoryAdapter
	IntegrationCreationStore() *store.Store
	CreateIntegrationInitial(context.Context, store.Tx, *store.IntegrationCreationPlan, Initialization) error
	ValidateIntegrationInitial(context.Context, store.Tx, *store.IntegrationCreationPlan, Initialization) error
}

func (r *Runtime) BindIntegrationCreation(memory IntegrationInitialMemoryAdapter) error {
	if memory == nil || r.integrationInitialMemory != nil || memory.IntegrationCreationStore() != r.Store || r.Content == nil || r.Content.Store != r.Store {
		return store.ErrDenied
	}
	if e := r.Store.BindIntegrationCreationOwners(r.Content, r); e != nil {
		return e
	}
	r.integrationInitialMemory = memory
	return nil
}
func (r *Runtime) CreateIntegrationInstance(ctx context.Context, key string, request generated.CreateInstanceRequest) (generated.Instance, error) {
	p, e := r.Store.PrepareIntegrationCreation(ctx, key, request)
	if e != nil {
		return generated.Instance{}, e
	}
	return r.Store.RunIntegrationCreation(ctx, p)
}
func integrationCatalog(p *store.IntegrationCreationPlan) map[string]generated.Release {
	root := p.Release()
	m := map[string]generated.Release{string(root.ReleaseID): root}
	for _, r := range p.Dependencies() {
		m[string(r.ReleaseID)] = r
	}
	return m
}
func (r *Runtime) IntegrationCreationLocks(p *store.IntegrationCreationPlan) ([]store.LockKey, error) {
	if r.integrationInitialMemory == nil || p == nil || p.WorldID() == "" {
		return nil, store.ErrDenied
	}
	scope := p.Scope()
	rel := p.Release()
	keys := r.integrationInitialMemory.CreationLocks(p.PlayerScope())
	props, e := collectRuntimeProps(rel, integrationCatalog(p))
	if e != nil {
		return nil, e
	}
	for _, v := range props {
		keys = append(keys, resourceKey(scope, "item", runtimeItemID(scope.TenantID, scope.ProductID, scope.WorldInstanceID, v)))
	}
	for _, raw := range rel.Input.Objects {
		var h struct {
			ID     string `json:"object_id"`
			Kind   string
			Setups []generated.InitialActorSetup `json:"initial_actor_setups"`
		}
		if json.Unmarshal(raw, &h) != nil {
			return nil, store.ErrDenied
		}
		if h.ID == string(rel.Input.RootObjectID) && h.Kind == "WORLD" {
			for _, a := range h.Setups {
				keys = append(keys, resourceKey(scope, "actor", content.Identity(scope.TenantID, scope.ProductID, scope.WorldInstanceID, "actor", string(a.SlotID))))
			}
		}
	}
	return keys, nil
}
func (r *Runtime) integrationInitialization(ctx context.Context, tx store.Tx, p *store.IntegrationCreationPlan) (Initialization, error) {
	var zero Initialization
	if e := r.Store.CheckIntegrationCreationTx(ctx, tx, p); e != nil {
		return zero, e
	}
	scope := p.Scope()
	rel := p.Release()
	catalog := integrationCatalog(p)
	resolved, e := verifyRelease(ctx, tx, scope.ProductScope, rel)
	if e != nil {
		return zero, e
	}
	for _, dep := range catalog {
		if _, e = verifyRelease(ctx, tx, scope.ProductScope, dep); e != nil {
			return zero, e
		}
	}
	props, e := collectRuntimeProps(rel, catalog)
	if e != nil {
		return zero, e
	}
	init, e := assemble(scope, rel, resolved, props)
	if e != nil {
		return zero, e
	}
	init.Participants = []string{p.PlayerScope().SubjectID}
	init.CreationCommandID = p.WorldID()
	init.IntegrationInitialLineage = p.InitialLineage()
	if init.IntegrationInitialLineage == nil {
		return zero, store.ErrDenied
	}
	return init, nil
}

// This is the sole app-bound materializer. Its Tx is the Store's sealed unit,
// not a database handle or a request-supplied SQL callback.
func (r *Runtime) MaterializeIntegrationCreation(ctx context.Context, tx store.Tx, p *store.IntegrationCreationPlan) (generated.Instance, error) {
	var out generated.Instance
	init, e := r.integrationInitialization(ctx, tx, p)
	if e != nil {
		return out, e
	}
	s := p.Scope()
	rel := p.Release()
	if e = tx.CreateWorld(ctx, string(rel.ReleaseID)); e != nil {
		return out, e
	}
	b, e := json.Marshal(init)
	if e != nil {
		return out, e
	}
	if e = tx.RequireLock(worldKey(s)); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO runtime_instances(tenant_id,product_id,world_instance_id,owner_subject_id,project_id,definition_id,mode,initial_release_id,initialization) VALUES(?,?,?,?,?,?,'PRIVATE',?,?)", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID, init.ProjectID, init.DefinitionID, rel.ReleaseID, b); e != nil {
		return out, e
	}
	for _, a := range init.Actors {
		if e = tx.RequireLock(resourceKey(s, "actor", a.ID)); e != nil {
			return out, e
		}
		b, e = json.Marshal(a)
		if e != nil {
			return out, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO actors(tenant_id,product_id,world_instance_id,actor_id,slot_id,document) VALUES(?,?,?,?,?,?)", s.TenantID, s.ProductID, s.WorldInstanceID, a.ID, a.Slot, b); e != nil {
			return out, e
		}
	}
	for _, v := range init.Items {
		if e = tx.RequireLock(resourceKey(s, "item", v.ID)); e != nil {
			return out, e
		}
		b, e = json.Marshal(v)
		if e != nil {
			return out, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO items(tenant_id,product_id,world_instance_id,item_id,document) VALUES(?,?,?,?,?)", s.TenantID, s.ProductID, s.WorldInstanceID, v.ID, b); e != nil {
			return out, e
		}
	}
	if e = r.integrationInitialMemory.CreateIntegrationInitial(ctx, tx, p, init); e != nil {
		return out, e
	}
	return generated.Instance{Scope: generated.WorldScope{TenantID: s.TenantID, ProductID: s.ProductID, WorldInstanceID: s.WorldInstanceID}, Mode: "PRIVATE", InstanceState: "CREATED", Versions: generated.Versions{ReleaseID: string(rel.ReleaseID), WorldRevision: "0", ControlVersion: "0", PrivacyEpoch: "1", ViewEpoch: "1"}}, nil
}
func (r *Runtime) ValidateIntegrationCreation(ctx context.Context, tx store.Tx, p *store.IntegrationCreationPlan) error {
	init, e := r.integrationInitialization(ctx, tx, p)
	if e != nil {
		return e
	}
	s := p.Scope()
	var raw []byte
	var good bool
	e = tx.QueryRowContext(ctx, `SELECT i.initialization,(w.status='CREATED' AND w.release_id=? AND w.world_revision=0 AND w.control_version=0 AND w.privacy_epoch=1 AND w.view_epoch=1 AND i.owner_subject_id=? AND i.mode='PRIVATE' AND i.project_id=? AND i.definition_id=?) FROM runtime_instances i JOIN world_controls w USING(tenant_id,product_id,world_instance_id) WHERE i.tenant_id=? AND i.product_id=? AND i.world_instance_id=?`, init.ReleaseID, s.SubjectID, init.ProjectID, init.DefinitionID, s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&raw, &good)
	var actual Initialization
	if e != nil || !good || json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(actual, init) {
		return store.ErrDenied
	}
	var count int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&count); e != nil {
		return e
	}
	if count != len(init.Actors) {
		return store.ErrDenied
	}
	for _, a := range init.Actors {
		if e = tx.RequireLock(resourceKey(s, "actor", a.ID)); e != nil {
			return e
		}
		var slot string
		e = tx.QueryRowContext(ctx, "SELECT document,slot_id FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, a.ID).Scan(&raw, &slot)
		want, _ := json.Marshal(a)
		if e != nil || slot != a.Slot || !bytes.Equal(raw, want) {
			return store.ErrDenied
		}
	}
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&count); e != nil {
		return e
	}
	if count != len(init.Items) {
		return store.ErrDenied
	}
	for _, v := range init.Items {
		if e = tx.RequireLock(resourceKey(s, "item", v.ID)); e != nil {
			return e
		}
		e = tx.QueryRowContext(ctx, "SELECT document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, v.ID).Scan(&raw)
		want, _ := json.Marshal(v)
		if e != nil || !bytes.Equal(raw, want) {
			return store.ErrDenied
		}
	}
	return r.integrationInitialMemory.ValidateIntegrationInitial(ctx, tx, p, init)
}
