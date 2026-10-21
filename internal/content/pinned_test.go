package content_test

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/tests/integration/testprincipal"
	"testing"
)

func TestPinnedRoleResolvesDutyAndInheritedRouteWithoutInlining(t *testing.T) {
	s, scope, ctx := fixture(t)
	in := coffee(t)
	raw := in.Objects[len(in.Objects)-1]
	var role map[string]any
	json.Unmarshal(raw, &role)
	role["implementation_kind"] = "AGENT"
	role["generation"] = map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}}
	raw, _ = json.Marshal(role)
	resource := in
	resource.RootObjectID = "program-duties"
	resource.Objects = []generated.ContentObject{raw}
	resource.ModuleAgentBindings = []generated.ModuleAgentBinding{}
	resource.SkillBindings = []generated.SkillBinding{}
	d, e := s.Create(ctx, scope, "ROLE", "", resource, "pinned-role-create")
	if e != nil {
		t.Fatal(e)
	}
	check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "pinned-role-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	release, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "pinned-role-publish")
	if e != nil {
		t.Fatal(e)
	}
	definitionHash, _ := content.Hash(role)
	ref := map[string]any{"kind": "PINNED_DEFINITION", "release_id": release.ReleaseID, "object_id": "program-duties", "version": "1", "content_hash": definitionHash}
	refRaw, _ := json.Marshal(ref)
	in.Objects = in.Objects[:len(in.Objects)-1]
	var world map[string]any
	json.Unmarshal(in.Objects[0], &world)
	for _, a := range world["duty_assignments"].([]any) {
		a.(map[string]any)["role_ref"] = ref
	}
	in.Objects[0], _ = json.Marshal(world)
	in.DependencyLocks = []generated.DependencyLock{{ReleaseID: release.ReleaseID, ObjectID: "program-duties", Version: "1", ContentHash: definitionHash}}
	in.ModuleAgentBindings = append(in.ModuleAgentBindings, generated.ModuleAgentBinding{BindingID: "pinned-director", OwnerObjectID: "alex-character", ModuleID: "INTERACTION", RoleRef: refRaw, Enabled: true, Generation: generated.GenerationSettings{Mode: "INHERIT", RequiredCapabilities: []generated.Identity{}, AllowedExternalUses: []string{"NONE"}}})
	d, e = s.Create(ctx, scope, "WORLD", "", in, "pinned-world-create")
	if e != nil {
		t.Fatal(e)
	}
	check, e = s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "pinned-world-check")
	if e != nil || !check.Passed || len(check.CoveredDutyIDS) != 5 {
		t.Fatal("pinned duty or route failed", check, e)
	}
	frozen, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "pinned-world-publish")
	if e != nil {
		t.Fatal(e)
	}
	if len(frozen.Input.Objects) != 6 || frozen.Input.DependencyLocks[0].ContentHash != definitionHash {
		t.Fatal("pinned input was inlined or hash replaced")
	}
	bad := in
	bad.DependencyLocks = append([]generated.DependencyLock(nil), in.DependencyLocks...)
	bad.DependencyLocks[0].ContentHash = release.ContentHash
	badDraft, e := s.Create(ctx, scope, "WORLD", "", bad, "wrong-definition-hash")
	if e != nil {
		t.Fatal(e)
	}
	badCheck, e := s.CheckPublish(ctx, scope, string(badDraft.DraftID), 1, []string{"MOCK_DEMO"}, "wrong-definition-check")
	if e != nil || badCheck.Passed {
		t.Fatal("release hash substituted for definition hash", badCheck, e)
	}
	impact, e := s.Impact(ctx, scope, string(d.DraftID), 1, "REMOVE_BINDING", "pinned-impact", "pinned-director")
	if e != nil || len(impact.DutyCoverage) != 5 || impact.DutyCoverage[0].CoveredBefore != true || impact.DutyCoverage[0].CoveredAfter != false {
		t.Fatal("pinned impact coverage", impact, e)
	}
	other := scope
	other.ProductID = "other-product"
	for _, q := range []string{"INSERT INTO product_gates VALUES('demo-local','other-product',1,1)", "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('demo-local','other-product','creator',1,1,1,1)"} {
		if _, e = s.Store.DB.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	otherCtx := testprincipal.Register(t, s.Store, ctx, other)
	od, e := s.Create(otherCtx, other, "ROLE", "", resource, "outside-role")
	if e != nil {
		t.Fatal(e)
	}
	oc, e := s.CheckPublish(otherCtx, other, string(od.DraftID), 1, []string{"MOCK_DEMO"}, "outside-check")
	if e != nil || !oc.Passed {
		t.Fatal(oc, e)
	}
	or, e := s.Publish(otherCtx, other, string(od.DraftID), 1, string(oc.ReportID), []string{"MOCK_DEMO"}, "outside-publish")
	if e != nil {
		t.Fatal(e)
	}
	outside := in
	outside.DependencyLocks = append([]generated.DependencyLock(nil), in.DependencyLocks...)
	outside.DependencyLocks[0].ReleaseID = or.ReleaseID
	x, e := s.Create(ctx, scope, "WORLD", "", outside, "outside-reference")
	if e != nil {
		t.Fatal(e)
	}
	xc, e := s.CheckPublish(ctx, scope, string(x.DraftID), 1, []string{"MOCK_DEMO"}, "outside-reference-check")
	if e != nil || xc.Passed {
		t.Fatal("cross-product immutable release leaked", xc, e)
	}
}

func TestPinnedResolutionRejectsCyclesAndExcessiveDepth(t *testing.T) {
	s, scope, ctx := fixture(t)
	for _, count := range []int{1, 70} {
		in := coffee(t)
		template := in.Objects[len(in.Objects)-1]
		in.RootObjectID = "role-0"
		in.Objects = []generated.ContentObject{}
		for i := 0; i < count; i++ {
			var role map[string]any
			json.Unmarshal(template, &role)
			role["object_id"] = fmt.Sprintf("role-%d", i)
			refs := []any{}
			if count == 1 {
				refs = append(refs, map[string]any{"kind": "LOCAL_DRAFT", "object_id": "role-0"})
			} else if i+1 < count {
				refs = append(refs, map[string]any{"kind": "LOCAL_DRAFT", "object_id": fmt.Sprintf("role-%d", i+1)})
			}
			role["dependency_refs"] = refs
			raw, _ := json.Marshal(role)
			in.Objects = append(in.Objects, raw)
		}
		key := fmt.Sprintf("graph-%d", count)
		d, e := s.Create(ctx, scope, "ROLE", "", in, key)
		if e != nil {
			t.Fatal(e)
		}
		check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, key+"-check")
		if e != nil || !check.Passed {
			t.Fatal(check, e)
		}
		r, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, key+"-publish")
		if e != nil {
			t.Fatal(e)
		}
		var root map[string]any
		json.Unmarshal(in.Objects[0], &root)
		hash, _ := content.Hash(root)
		target := coffee(t)
		target.DependencyLocks = []generated.DependencyLock{{ReleaseID: r.ReleaseID, ObjectID: "role-0", Version: "1", ContentHash: hash}}
		draft, e := s.Create(ctx, scope, "WORLD", "", target, key+"-target")
		if e != nil {
			t.Fatal(e)
		}
		result, e := s.CheckPublish(ctx, scope, string(draft.DraftID), 1, []string{"MOCK_DEMO"}, key+"-target-check")
		if e != nil || result.Passed {
			t.Fatal("cyclic/deep pinned graph accepted", count, result, e)
		}
	}
}

func TestPinnedCharacterVersionsKeepQualifiedGenerationOwners(t *testing.T) {
	s, scope, ctx := fixture(t)
	in := coffee(t)
	refs := []map[string]any{}
	locks := []generated.DependencyLock{}
	for i := 0; i < 2; i++ {
		var character map[string]any
		json.Unmarshal(in.Objects[2], &character)
		character["version"] = fmt.Sprint(i + 1)
		character["generation"] = map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "parameters": map[string]any{"temperature": float64(i) + 0.2}, "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}}
		raw, _ := json.Marshal(character)
		resource := in
		resource.RootObjectID = "alex-character"
		resource.Objects = []generated.ContentObject{raw}
		key := fmt.Sprintf("character-version-%d", i)
		d, e := s.Create(ctx, scope, "CHARACTER", "", resource, key)
		if e != nil {
			t.Fatal(e)
		}
		check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, key+"-check")
		if e != nil || !check.Passed {
			t.Fatal(check, e)
		}
		r, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, key+"-publish")
		if e != nil {
			t.Fatal(e)
		}
		hash, _ := content.Hash(character)
		refs = append(refs, map[string]any{"kind": "PINNED_DEFINITION", "release_id": r.ReleaseID, "object_id": "alex-character", "version": fmt.Sprint(i + 1), "content_hash": hash})
		locks = append(locks, generated.DependencyLock{ReleaseID: r.ReleaseID, ObjectID: "alex-character", Version: generated.ExactUnsigned(fmt.Sprint(i + 1)), ContentHash: hash})
	}
	var world map[string]any
	json.Unmarshal(in.Objects[0], &world)
	for i, a := range world["initial_actor_setups"].([]any) {
		a.(map[string]any)["character_ref"] = refs[i]
	}
	in.Objects[0], _ = json.Marshal(world)
	in.Objects = append(append([]generated.ContentObject{}, in.Objects[:2]...), in.Objects[4:]...)
	in.DependencyLocks = locks
	d, e := s.Create(ctx, scope, "WORLD", "", in, "pinned-actors-world")
	if e != nil {
		t.Fatal(e)
	}
	check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "pinned-actors-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	r, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "pinned-actors-release")
	if e != nil {
		t.Fatal(e)
	}
	if len(r.RouteMappings) != 3 || r.RouteMappings[1].OwnerObjectID != r.RouteMappings[2].OwnerObjectID || string(r.RouteMappings[1].OwnerReference) == string(r.RouteMappings[2].OwnerReference) || r.RouteMappings[1].SettingsHash == r.RouteMappings[2].SettingsHash {
		t.Fatal("qualified pinned owners collapsed", r.RouteMappings)
	}
	wrong := in
	var invalidWorld map[string]any
	json.Unmarshal(wrong.Objects[0], &invalidWorld)
	invalidWorld["initial_actor_setups"].([]any)[0].(map[string]any)["character_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": "program-duties"}
	wrong.Objects = append([]generated.ContentObject(nil), in.Objects...)
	wrong.Objects[0], _ = json.Marshal(invalidWorld)
	bad, e := s.Create(ctx, scope, "WORLD", "", wrong, "wrong-character-kind")
	if e != nil {
		t.Fatal(e)
	}
	badCheck, e := s.CheckPublish(ctx, scope, string(bad.DraftID), 1, []string{"MOCK_DEMO"}, "wrong-character-kind-check")
	if e != nil || badCheck.Passed {
		t.Fatal("role impersonated a character", badCheck, e)
	}
}
