package content_test

import (
	"context"
	"encoding/json"

	"loreviva.local/loreviva/internal/generated"
	"testing"
)

func mockGeneration() generated.GenerationSettings {
	c, m := generated.Identity("mock-local"), generated.Identity("synthetic-text-v1")
	return generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: &c, ModelID: &m, RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}
}
func skillObject(t *testing.T) generated.ContentObject {
	t.Helper()
	b := []byte(`{"object_id":"skill-owner","kind":"SKILL","name":"Synthetic skill","description":"","tags":[],"rights":{"origin_kind":"SYNTHETIC","source_label":"test","license":"Apache-2.0","allowed_uses":["EDIT","PUBLISH"]},"audience_policy":"PRODUCT_READERS","version":"1","source_refs":[],"dependency_refs":[],"builtin_original":false,"implementation_kind":"PROGRAM","input_schema_id":"DraftInput","output_schema_id":"PublishCheckReport","registered_capability_ids":[],"tool_ids":[],"conflicting_refs":[],"failure_policy":"BLOCK","max_depth":1,"max_duration_ms":"100","max_cost_units":"0","synthetic_examples":[],"generation":{"mode":"NONE","required_capabilities":[],"allowed_external_uses":["NONE"]}}`)
	if e := generated.Validate("SkillDefinition", b); e != nil {
		t.Fatal(e)
	}
	return b
}
func TestFix1IneligibleOwnerBindingCannotPublish(t *testing.T) {
	for _, owner := range []string{"program-duties", "skill-owner", "alex-character"} {
		t.Run(owner, func(t *testing.T) {
			s, scope := fixture(t)
			in := coffee(t)
			if owner == "skill-owner" {
				in.Objects = append(in.Objects, skillObject(t))
			}
			in.ModuleAgentBindings = append(in.ModuleAgentBindings, generated.ModuleAgentBinding{BindingID: "owner-binding", OwnerObjectID: generated.Identity(owner), ModuleID: "INTERACTION", RoleRef: json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"program-duties"}`), Enabled: true, Generation: mockGeneration()})
			d, e := s.Create(context.Background(), scope, "WORLD", "", in, "owner-create")
			if e != nil {
				t.Fatal(e)
			}
			check, e := s.CheckPublish(context.Background(), scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "owner-check")
			if e != nil {
				t.Fatal(e)
			}
			if check.Passed != (owner == "alex-character") {
				t.Errorf("owner eligibility bypassed: %s passed=%v findings=%v", owner, check.Passed, check.Findings)
			}
			_, e = s.Publish(context.Background(), scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "owner-publish")
			if (e == nil) != (owner == "alex-character") {
				t.Errorf("publish owner=%s error=%v", owner, e)
			}
			var count int
			if e = s.Store.DB.QueryRow("SELECT COUNT(*) FROM content_releases").Scan(&count); e != nil {
				t.Fatal(e)
			}
			if owner != "alex-character" && count != 0 {
				t.Errorf("rejected owner mutated releases: %d", count)
			}
		})
	}
}

func TestFix1SkillDefaultCannotGenerate(t *testing.T) {
	s, scope := fixture(t)
	in := coffee(t)
	in.RootObjectID = "skill-owner"
	in.Objects = []generated.ContentObject{skillObject(t)}
	var skill map[string]any
	json.Unmarshal(in.Objects[0], &skill)
	g, _ := json.Marshal(mockGeneration())
	var settings any
	json.Unmarshal(g, &settings)
	skill["generation"] = settings
	in.Objects[0], _ = json.Marshal(skill)
	d, e := s.Create(context.Background(), scope, "SKILL", "", in, "skill-default-create")
	if e != nil {
		t.Fatal(e)
	}
	check, e := s.CheckPublish(context.Background(), scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "skill-default-check")
	if e != nil || check.Passed {
		t.Fatalf("Skill DEFAULT enabled generation: %v %v", check, e)
	}
	if _, e = s.Publish(context.Background(), scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "skill-default-publish"); e == nil {
		t.Fatal("Skill DEFAULT published")
	}
}
