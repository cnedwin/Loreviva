package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sort"
)

// PerceptionLocks is private server precollection, not a client visibility API.
// R0 refuses worlds exceeding the fixed initialized-resource support boundary.
func PerceptionLocks(ctx context.Context, s *store.Store, scope domain.Scope, actor string) ([]store.LockKey, error) {
	var raw []byte
	var init Initialization
	if body, handled, e := s.IntegrationTurnPrecollection(ctx, scope); handled {
		if e != nil {
			return nil, e
		}
		raw = body
	} else if e := s.DB.QueryRowContext(ctx, "SELECT initialization FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID).Scan(&raw); e != nil {
		return nil, e
	}
	if json.Unmarshal(raw, &init) != nil || len(init.Actors) > 32 || len(init.Items) > 32 {
		return nil, store.ErrDenied
	}
	keys := []store.LockKey{}
	found := false
	for _, a := range init.Actors {
		if a.ID == actor {
			found = true
		}
		keys = append(keys, resourceKey(scope, "actor", a.ID))
	}
	if !found {
		return nil, store.ErrDenied
	}
	for _, i := range init.Items {
		keys = append(keys, resourceKey(scope, "item", i.ID))
	}
	if e := s.DB.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", scope.TenantID, scope.ProductID, init.ReleaseID).Scan(&raw); e != nil {
		return nil, e
	}
	var rel generated.Release
	if e := generated.Decode("Release", raw, &rel); e != nil {
		return nil, e
	}
	if len(rel.DependencyLocks) > 200 {
		return nil, store.ErrDenied
	}
	return append(keys, releaseLocks(scope.ProductScope, rel)...), nil
}

func PerceptionTx(ctx context.Context, tx store.Tx, s domain.Scope, actorID string) (domain.WorldPerception, error) {
	out := domain.WorldPerception{ActorID: actorID, Actors: []domain.PerceivedActor{}, Items: []domain.PerceivedItem{}, Eligibility: map[string]string{}, Resources: []domain.CandidateResource{}}
	init, e := InitializationEvidence(ctx, tx, s)
	if e != nil {
		return out, e
	}
	if len(init.Actors) > 32 || len(init.Items) > 32 {
		return out, store.ErrDenied
	}
	out.CreationID = init.CreationCommandID
	if e = tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&out.Versions.ReleaseID, &out.Versions.WorldRevision, &out.Versions.ControlVersion, &out.Versions.PrivacyEpoch, &out.Versions.ViewEpoch); e != nil {
		return out, e
	}
	basis, synthetic, e := qualifiedRehearsalTx(ctx, tx, s)
	if e != nil {
		return out, e
	}
	loadRelease := func(id string) (generated.Release, error) {
		var rel generated.Release
		var raw []byte
		if e := tx.RequireLock(content.Key(s.ProductScope, "content_release", id)); e != nil {
			return rel, e
		}
		if basis != nil && id == string(synthetic.ReleaseID) {
			return synthetic, nil
		}
		if e := tx.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", s.TenantID, s.ProductID, id).Scan(&raw); e != nil {
			return rel, e
		}
		if e := generated.Decode("Release", raw, &rel); e != nil {
			return rel, e
		}
		return rel, nil
	}
	rel, e := loadRelease(init.ReleaseID)
	if e != nil {
		return out, e
	}
	if rel.ContentHash != init.ReleaseHash || len(rel.DependencyLocks) > 200 {
		return out, store.ErrConflict
	}
	graph := runtimeGraph{root: rel, catalog: map[string]generated.Release{init.ReleaseID: rel}, allowed: map[string]bool{}}
	for _, dep := range rel.DependencyLocks {
		source, e := loadRelease(string(dep.ReleaseID))
		if e != nil {
			return out, e
		}
		graph.catalog[string(dep.ReleaseID)] = source
		b, _ := json.Marshal(map[string]string{"kind": "PINNED_DEFINITION", "release_id": string(dep.ReleaseID), "object_id": string(dep.ObjectID), "version": string(dep.Version), "content_hash": dep.ContentHash})
		h, _ := content.Hash(json.RawMessage(b))
		graph.allowed[h] = true
	}
	for _, source := range graph.catalog {
		if basis != nil && source.ReleaseID == synthetic.ReleaseID {
			continue
		}
		if _, e := verifyRelease(ctx, tx, s.ProductScope, source); e != nil {
			return out, e
		}
	}
	scene := func(ref generated.DefinitionRef, owner string) (string, string, error) {
		raw, canonical, source, e := graph.resolve(ref, owner)
		if e != nil {
			return "", "", e
		}
		var v generated.SceneDefinition
		if e = generated.Decode("SceneDefinition", raw, &v); e != nil {
			return "", "", e
		}
		h, e := content.Hash(struct {
			Release string
			Ref     generated.DefinitionRef
		}{source, canonical})
		return h, v.Visibility, e
	}
	actors := map[string]ActorState{}
	scenes := map[string]string{}
	visibility := map[string]string{}
	for _, original := range init.Actors {
		if e = tx.RequireLock(resourceKey(s, "actor", original.ID)); e != nil {
			return out, e
		}
		var raw []byte
		var current ActorState
		if e = tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, original.ID).Scan(&raw); e != nil {
			return out, e
		}
		if json.Unmarshal(raw, &current) != nil {
			return out, store.ErrDenied
		}
		a, _ := content.Hash(current)
		b, _ := content.Hash(original)
		if a != b {
			return out, store.ErrConflict
		}
		sc, vis, e := scene(current.OriginalSetup.SceneRef, init.ReleaseID)
		if e != nil {
			return out, e
		}
		actors[current.ID] = current
		scenes[current.ID] = sc
		visibility[current.ID] = vis
	}
	primary, ok := actors[actorID]
	if !ok {
		return out, store.ErrDenied
	}
	out.Scene = scenes[actorID]
	visible := map[string]bool{actorID: true}
	for id := range actors {
		if visibility[actorID] == "PUBLIC" && visibility[id] == "PUBLIC" && scenes[id] == out.Scene {
			visible[id] = true
		}
	}
	ids := []string{}
	for id := range visible {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := actors[id]
		out.Actors = append(out.Actors, domain.PerceivedActor{ID: id, Slot: a.Slot})
		h, _ := content.Hash(a)
		out.Resources = append(out.Resources, domain.CandidateResource{Kind: "actor", ID: id, Hash: h})
		out.Eligibility["actor/"+id], _ = content.Hash(struct{ Creation, Scene, Visibility, Identity string }{init.CreationCommandID, scenes[id], visibility[id], h})
	}
	// Scene visibility, not authoring audience_policy, grants this minimal runtime
	// presence projection. ACTOR_SCOPED has only its requesting self identity.
	if visibility[primary.ID] != "PUBLIC" {
		return out, nil
	}
	for _, original := range init.Items {
		if e = tx.RequireLock(resourceKey(s, "item", original.ID)); e != nil {
			return out, e
		}
		var raw []byte
		var item ItemState
		if e = tx.QueryRowContext(ctx, "SELECT document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, original.ID).Scan(&raw); e != nil {
			return out, e
		}
		if json.Unmarshal(raw, &item) != nil {
			return out, store.ErrDenied
		}
		stable := item
		stable.State = original.State
		stable.HolderID = original.HolderID
		stable.Note = original.Note
		h, _ := content.Hash(stable)
		originalHash, _ := content.Hash(original)
		if h != originalHash {
			return out, store.ErrConflict
		}
		sc, vis, e := scene(item.Definition.SceneRef, item.OriginalReleaseID)
		if e != nil {
			return out, e
		}
		if sc != out.Scene || vis != "PUBLIC" || !visible[item.CustodianID] || item.HolderID != "" && !visible[item.HolderID] {
			continue
		}
		caps := []string{}
		for _, cap := range item.Definition.InteractionCapabilities {
			if cap == "MOVE" || cap == "WRITE_NOTE" {
				caps = append(caps, cap)
			}
		}
		sort.Strings(caps)
		out.Items = append(out.Items, domain.PerceivedItem{ID: item.ID, CustodianID: item.CustodianID, HolderID: item.HolderID, State: item.State, Capabilities: caps})
		resourceHash, _ := content.Hash(item)
		out.Resources = append(out.Resources, domain.CandidateResource{Kind: "item", ID: item.ID, Hash: resourceHash})
		out.Eligibility["item/"+item.ID], _ = content.Hash(struct{ Creation, Scene, Visibility, Origin string }{init.CreationCommandID, sc, vis, originalHash})
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID < out.Items[j].ID })
	sort.Slice(out.Resources, func(i, j int) bool {
		a, b := out.Resources[i], out.Resources[j]
		if a.Kind == b.Kind {
			return a.ID < b.ID
		}
		return a.Kind < b.Kind
	})
	return out, nil
}

// OriginalPerceptionStillAllowed validates the original selected closure only.
// Additional currently visible resources do not change the historical request.
func OriginalPerceptionStillAllowed(original, current domain.WorldPerception) bool {
	if original.ActorID != current.ActorID || original.CreationID != current.CreationID || original.Scene != current.Scene || len(original.Eligibility) == 0 {
		return false
	}
	v := current.Versions
	v.WorldRevision = original.Versions.WorldRevision
	if v != original.Versions {
		return false
	}
	for id, hash := range original.Eligibility {
		if current.Eligibility[id] != hash {
			return false
		}
	}
	return true
}
