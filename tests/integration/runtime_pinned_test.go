package integration

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	"testing"
)

func TestRuntimePinnedVersionsKeepQualifiedActorOwners(t *testing.T) {
	r, s, seed := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	base, e := r.Content.GetRelease(ctx, s, seed)
	if e != nil {
		t.Fatal(e)
	}
	var raw json.RawMessage
	for _, b := range base.Input.Objects {
		var h struct {
			ObjectID string `json:"object_id"`
		}
		_ = json.Unmarshal(b, &h)
		if h.ObjectID == "alex-character" {
			raw = b
		}
	}
	refs := []json.RawMessage{}
	locks := []generated.DependencyLock{}
	for _, version := range []string{"1", "2"} {
		var char map[string]any
		_ = json.Unmarshal(raw, &char)
		char["version"] = version
		char["expression_style"] = "version " + version
		object, e := json.Marshal(char)
		if e != nil {
			t.Fatal(e)
		}
		in := base.Input
		in.RootObjectID = "alex-character"
		in.Objects = []generated.ContentObject{object}
		in.DependencyLocks = []generated.DependencyLock{}
		d, e := r.Content.Create(ctx, s, "CHARACTER", "", in, "char-"+version)
		if e != nil {
			t.Fatal(e)
		}
		check, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "char-check-"+version)
		if e != nil || !check.Passed {
			t.Fatal(check, e)
		}
		rel, e := r.Content.Publish(ctx, s, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "char-pub-"+version)
		if e != nil {
			t.Fatal(e)
		}
		hash, e := content.Hash(char)
		if e != nil {
			t.Fatal(e)
		}
		ref, e := json.Marshal(map[string]any{"kind": "PINNED_DEFINITION", "release_id": rel.ReleaseID, "object_id": "alex-character", "version": version, "content_hash": hash})
		if e != nil {
			t.Fatal(e)
		}
		refs = append(refs, ref)
		locks = append(locks, generated.DependencyLock{ReleaseID: rel.ReleaseID, ObjectID: "alex-character", Version: generated.ExactUnsigned(version), ContentHash: hash})
	}
	in := base.Input
	in.Objects = []generated.ContentObject{}
	in.DependencyLocks = locks
	for _, b := range base.Input.Objects {
		var h struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(b, &h)
		if h.Kind == "CHARACTER" {
			continue
		}
		if h.Kind == "WORLD" {
			var w generated.WorldDefinition
			if e = generated.Decode("WorldDefinition", b, &w); e != nil {
				t.Fatal(e)
			}
			w.InitialActorSetups[0].CharacterRef = refs[0]
			w.InitialActorSetups[1].CharacterRef = refs[1]
			b, e = json.Marshal(w)
			if e != nil {
				t.Fatal(e)
			}
		}
		in.Objects = append(in.Objects, b)
	}
	d, e := r.Content.Create(ctx, s, "WORLD", "", in, "pinned-world")
	if e != nil {
		t.Fatal(e)
	}
	check, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "pinned-world-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	rel, e := r.Content.Publish(ctx, s, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "pinned-world-pub")
	if e != nil {
		t.Fatal(e)
	}
	i := createRuntimeWorld(t, r, string(rel.ReleaseID), "pinned-create")
	var initial []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&initial); e != nil {
		t.Fatal(e)
	}
	var init world.Initialization
	if e = json.Unmarshal(initial, &init); e != nil {
		t.Fatal(e)
	}
	if len(init.Actors) != 2 || init.Actors[0].ID == init.Actors[1].ID || string(init.Actors[0].OwnerReference) == string(init.Actors[1].OwnerReference) || init.Actors[0].OriginalCharacter.Version != "1" || init.Actors[1].OriginalCharacter.Version != "2" {
		t.Fatal("pinned versions collapsed", init.Actors)
	}
}

func TestRuntimePinnedPropsMaterializeQualifiedVersions(t *testing.T) {
	r, s, seed := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	base, e := r.Content.GetRelease(ctx, s, seed)
	if e != nil {
		t.Fatal(e)
	}
	refs := []generated.DefinitionRef{}
	locks := []generated.DependencyLock{}
	publish := func(in generated.DraftInput, key string) generated.Release {
		t.Helper()
		d, e := r.Content.Create(ctx, s, "WORLD", "", in, key)
		if e != nil {
			t.Fatal(e)
		}
		c, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 1, []string{"MOCK_DEMO"}, key+"-check")
		if e != nil || !c.Passed {
			t.Fatal(c, e)
		}
		rel, e := r.Content.Publish(ctx, s, string(d.DraftID), 1, string(c.ReportID), []string{"MOCK_DEMO"}, key+"-pub")
		if e != nil {
			t.Fatal(e)
		}
		return rel
	}
	for _, v := range []string{"1", "2"} {
		in := base.Input
		in.Objects = nil
		var hash string
		for _, raw := range base.Input.Objects {
			var m map[string]any
			if e = json.Unmarshal(raw, &m); e != nil {
				t.Fatal(e)
			}
			if m["kind"] == "SCENE" {
				m["child_refs"] = []any{}
			}
			if m["kind"] == "PROP" {
				m["version"] = v
				m["name"] = "community sign " + v
				m["rule_refs"] = []any{}
				hash, e = content.Hash(m)
				if e != nil {
					t.Fatal(e)
				}
			}
			b, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			in.Objects = append(in.Objects, b)
		}
		rel := publish(in, "prop-source-"+v)
		ref, e := json.Marshal(map[string]any{"kind": "PINNED_DEFINITION", "release_id": rel.ReleaseID, "object_id": "wooden-sign", "version": v, "content_hash": hash})
		if e != nil {
			t.Fatal(e)
		}
		refs = append(refs, ref)
		locks = append(locks, generated.DependencyLock{ReleaseID: rel.ReleaseID, ObjectID: "wooden-sign", Version: generated.ExactUnsigned(v), ContentHash: hash})
	}
	in := base.Input
	in.Objects = nil
	in.DependencyLocks = locks
	for _, raw := range base.Input.Objects {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["kind"] == "SCENE" {
			var scene generated.SceneDefinition
			_ = json.Unmarshal(raw, &scene)
			scene.ChildRefs = append(scene.ChildRefs, refs...)
			raw, e = json.Marshal(scene)
			if e != nil {
				t.Fatal(e)
			}
		}
		in.Objects = append(in.Objects, raw)
	}
	rel := publish(in, "prop-host")
	i := createRuntimeWorld(t, r, string(rel.ReleaseID), "prop-create")
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var initial world.Initialization
	if e = json.Unmarshal(raw, &initial); e != nil {
		t.Fatal(e)
	}
	if len(initial.Items) != 3 {
		t.Fatalf("pinned props silently omitted: expected local plus two qualified items, got %d", len(initial.Items))
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, item := range initial.Items {
		if ids[item.ID] || item.CustodianID == "" {
			t.Fatal("collapsed identity or missing authoritative custody/rule", item)
		}
		ids[item.ID] = true
		names[item.Definition.Name] = true
	}
	if !names["community sign 1"] || !names["community sign 2"] || !ids[content.Identity(s.TenantID, s.ProductID, i.Scope.WorldInstanceID, "item", "wooden-sign")] {
		t.Fatal("qualified versions or legacy local identity lost", initial.Items)
	}
	state := world.WorldState{Actors: map[string]world.ActorState{}, Items: map[string]world.ItemState{}}
	for _, a := range initial.Actors {
		state.Actors[a.ID] = a
	}
	for _, item := range initial.Items {
		state.Items[item.ID] = item
		if item.Definition.Name == "community sign 1" || item.Definition.Name == "community sign 2" {
			var origin map[string]string
			if e = json.Unmarshal(item.OwnerReference, &origin); e != nil {
				t.Fatal(e)
			}
			if origin["kind"] != "PINNED_DEFINITION" || origin["release_id"] != item.OriginalReleaseID || origin["version"] != string(item.Definition.Version) {
				t.Fatal("qualified origin lost", item)
			}
			if len(item.Rules) != 0 {
				t.Fatal("rule authority invented", item)
			}
			decision, e := world.EvaluateActions(state, []world.ActionIntent{{ID: "move", ActorID: item.CustodianID, Kind: "MOVE", ItemID: item.ID, TargetID: initial.Actors[0].ID}})
			if e != nil || decision.Accepted {
				t.Fatal("pinned reference invented MOVE authority", decision, e)
			}
		}
	}
	var actual int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM items WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&actual); e != nil || actual != 3 {
		t.Fatal("items not durably materialized", actual, e)
	}
	if again := createRuntimeWorld(t, r, string(rel.ReleaseID), "prop-create"); again != i {
		t.Fatal("replay changed")
	}
	// A scene child with non-runtime semantics must be explicitly refused, not omitted.
	in.DependencyLocks = []generated.DependencyLock{}
	for n, raw := range in.Objects {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["kind"] == "SCENE" {
			m["child_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "sign-custody"}}
			in.Objects[n], e = json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	unsupported := publish(in, "unsupported-host")
	runtimeHTTP(t, r, "POST", "/instances", "unsupported-create", generated.AdminCreateInstanceRequest{ProductID: "coffee", ReleaseID: string(unsupported.ReleaseID), Mode: "PRIVATE", InitialParticipants: []string{}, ClientCapabilities: []string{"TEXT"}}, 422, nil)
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM world_controls").Scan(&count); e != nil || count != 1 {
		t.Fatal("unsupported graph wrote a world", count, e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE operation='createAdminInstance'").Scan(&count); e != nil || count != 1 {
		t.Fatal("unsupported graph wrote a receipt", count, e)
	}
}
