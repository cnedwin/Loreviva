package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
)

func (r *Runtime) DeliverySnapshotStore() *store.Store {
	if r == nil {
		return nil
	}
	return r.Store
}
func (r *Runtime) BindIntegrationDelivery(ctx context.Context) error {
	if r == nil || r.Store == nil || r.Content == nil || r.Content.Store != r.Store || r.integrationInitialMemory == nil {
		return store.ErrDenied
	}
	return r.Store.BindDeliverySnapshotOwner(r)
}
func (r *Runtime) PlanIntegrationSnapshot(ctx context.Context, p *store.IntegrationSnapshotPlan) ([]store.LockKey, error) {
	f, e := p.Facts(r)
	if e != nil {
		return nil, e
	}
	out := []store.LockKey{}
	for _, id := range f.ActorIDs {
		out = append(out, resourceKey(f.Scope, "actor", id))
	}
	for _, id := range f.ItemIDs {
		out = append(out, resourceKey(f.Scope, "item", id))
	}
	return out, nil
}

// Presence is projected from qualified PUBLIC scenes, never from PrimaryActorID
// or an actor/player string match. No raw definitions or arbitrary attributes
// are forwarded to a customer; holder/custodian remain in the actor domain.
func (r *Runtime) SnapshotTx(ctx context.Context, u *store.IntegrationSnapshotUnit) (store.IntegrationSnapshotProjection, error) {
	out := store.IntegrationSnapshotProjection{State: map[string]json.RawMessage{}, Actors: []generated.ActorView{}, Relationships: []generated.RelationshipView{}}
	if r == nil || r.Store == nil {
		return out, store.ErrDenied
	}
	if e := r.Store.CheckIntegrationSnapshotUnit(ctx, u, r); e != nil {
		return out, e
	}
	f, e := u.Facts(ctx, r)
	if e != nil {
		return out, e
	}
	scenes := map[string]store.SnapshotSceneFact{}
	for _, x := range f.Scenes {
		key := x.ResourceKind + "/" + x.ResourceID
		if _, found := scenes[key]; found {
			return out, store.ErrDenied
		}
		scenes[key] = x
	}
	visible := map[string]string{}
	for _, row := range f.Actors {
		sc, ok := scenes["ACTOR/"+row.ID]
		if !ok {
			return out, store.ErrDenied
		}
		if sc.Visibility != "PUBLIC" {
			continue
		}
		var a ActorState
		if json.Unmarshal(row.Body, &a) != nil || a.ID != row.ID || a.DefinitionID == "" {
			return out, store.ErrDenied
		}
		visible[a.ID] = sc.SceneIdentityHash
		out.Actors = append(out.Actors, generated.ActorView{ActorInstanceID: a.ID, CharacterDefinitionID: a.DefinitionID, Revision: strconv.FormatUint(f.WorldRevision, 10), VisibleState: map[string]json.RawMessage{}})
	}
	items := map[string]any{}
	for _, row := range f.Items {
		sc, ok := scenes["ITEM/"+row.ID]
		if !ok {
			return out, store.ErrDenied
		}
		if sc.Visibility != "PUBLIC" {
			continue
		}
		var i ItemState
		if json.Unmarshal(row.Body, &i) != nil || i.ID != row.ID {
			return out, store.ErrDenied
		}
		if i.CustodianID != "" && visible[i.CustodianID] != sc.SceneIdentityHash || i.HolderID != "" && visible[i.HolderID] != sc.SceneIdentityHash {
			continue
		}
		v := map[string]string{"state": i.State}
		if i.CustodianID != "" {
			v["custodian_actor_id"] = i.CustodianID
		}
		if i.HolderID != "" {
			v["holder_actor_id"] = i.HolderID
		}
		items[i.ID] = v
	}
	if len(items) > 0 {
		b, e := json.Marshal(items)
		if e != nil {
			return out, e
		}
		out.State["items"] = b
	}
	return out, nil
}
