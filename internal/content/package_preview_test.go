package content

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
	"os"
	"strings"
	"testing"
)

func TestPackagePreviewContractDiscoverStage(t *testing.T) {
	if err := generated.Validate("PackagePreviewOptions", []byte(`{"stage":"DISCOVER"}`)); err != nil {
		t.Fatalf("a bounded discovery request must have a canonical contract: %v", err)
	}
	for _, raw := range []string{`{"stage":"DISCOVER","rights_attested":true}`, `{"stage":"RESOLVE"}`, `{"stage":"DISCOVER","command":"IMPORT"}`} {
		if err := generated.Validate("PackagePreviewOptions", []byte(raw)); err == nil {
			t.Fatal("unowned request field or incomplete resolve accepted")
		}
	}
}

func TestPackagePreviewContractNeverExecutable(t *testing.T) {
	valid := `{"status":"BLOCKED","execution_ready":false,"persisted":false,"findings":[],"id_mappings":[]}`
	if err := generated.Validate("PackageImportPreview", []byte(valid)); err != nil {
		t.Fatalf("safe blocked report must validate: %v", err)
	}
	for _, raw := range []string{`{"status":"BLOCKED","execution_ready":true,"persisted":false,"findings":[],"id_mappings":[]}`, `{"status":"RESOLVED","execution_ready":false,"persisted":false,"findings":[],"id_mappings":[]}`} {
		if err := generated.Validate("PackageImportPreview", []byte(raw)); err == nil {
			t.Fatal("executable or incomplete report accepted")
		}
	}
}

func previewFixture(t *testing.T) (wepkg.PortablePlan, packagePreviewOptions) {
	t.Helper()
	body := json.RawMessage(`{"body_version":"1.0","name":"A portable note","description":"Synthetic P2","tags":[],"root_object_id":"note","objects":[{"object_id":"note","kind":"KNOWLEDGE","name":"A note","description":"Synthetic","tags":[],"rights":{"license":"Apache-2.0","source_reference":"fixture:p2","redistribution_allowed":true,"allowed_uses":["WORLD_CONTENT"]},"audience_policy":"PRODUCT_READERS","version":"7","source_refs":[],"dependency_refs":[],"knowledge_type":"NOTE","text":"A declared source note","binding_refs":[]}],"module_agent_bindings":[],"skill_bindings":[]}`)
	local := strings.Repeat("a", 64)
	return wepkg.PortablePlan{Kind: "KNOWLEDGE", Entrypoint: "note-entry", ArchiveSHA256: strings.Repeat("b", 64), RootObjectID: local, Body: body, IDs: []wepkg.PortableID{{Domain: "OBJECT", LogicalID: "note", LocalID: local}}}, packagePreviewOptions{RightsAttested: true, Rights: []generated.PackageRightsMapping{{ObjectID: "note", AllowedUses: []string{"EDIT", "COPY"}}}}
}
func TestPackagePreviewProjectionExplicitRightsAndSourceVersion(t *testing.T) {
	plan, opts := previewFixture(t)
	in, findings := projectPortable(plan, opts)
	if len(findings) != 0 {
		t.Fatalf("complete explicit mapping must project: %+v", findings)
	}
	if string(in.RootObjectID) != plan.RootObjectID || len(in.Objects) != 1 {
		t.Fatal("local candidate identity differs")
	}
	var obj map[string]any
	json.Unmarshal(in.Objects[0], &obj)
	if obj["version"] != "1" {
		t.Fatal("source version was used as a local revision")
	}
	if obj["rights"].(map[string]any)["origin_kind"] != "LICENSED" {
		t.Fatal("portable rights became local ownership")
	}
	if !strings.Contains(string(in.Objects[0]), "7") {
		t.Fatal("source version provenance was lost")
	}
	if _, err := Prepare(in); err != nil {
		t.Fatalf("projected input not locally typed: %v", err)
	}
}
func TestPackagePreviewProjectionNoPartialCandidate(t *testing.T) {
	plan, opts := previewFixture(t)
	opts.Rights = nil
	in, findings := projectPortable(plan, opts)
	if len(findings) == 0 || in.RootObjectID != "" || len(in.Objects) != 0 {
		t.Fatal("missing source-use mapping yielded an editing candidate")
	}
	opts.Rights = []generated.PackageRightsMapping{{ObjectID: "note", AllowedUses: []string{"EDIT", "COPY"}}, {ObjectID: "note", AllowedUses: []string{"EDIT", "COPY"}}}
	in, findings = projectPortable(plan, opts)
	if len(findings) == 0 || len(in.Objects) != 0 {
		t.Fatal("duplicate mapping was silently accepted")
	}
}

func TestPackagePreviewProjectionRequiredPresentationIsExplicit(t *testing.T) {
	plan, opts := previewFixture(t)
	plan.Requirements.Presentation = []wepkg.Capability{{CapabilityID: "text", Required: true}}
	opts.LocalCapabilities = map[string]string{"text": "TEXT"}
	in, findings := projectPortable(plan, opts)
	if len(findings) != 0 || len(in.Objects) != 1 {
		t.Fatalf("explicit registered text presentation was not consumed: %+v", findings)
	}
}
func TestPackagePreviewProjectionBindingOnlyModelMapping(t *testing.T) {
	plan, opts := previewFixture(t)
	var doc map[string]any
	json.Unmarshal(plan.Body, &doc)
	raw, err := os.ReadFile("../../contracts/source/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	json.Unmarshal(raw, &source)
	var schema struct {
		Examples []generated.DraftInput `json:"examples"`
	}
	json.Unmarshal(source.Components.Schemas["DraftInput"], &schema)
	var role map[string]any
	for _, raw := range schema.Examples[0].Objects {
		var object map[string]any
		json.Unmarshal(raw, &object)
		if object["kind"] == "ROLE" {
			role = object
			break
		}
	}
	if role == nil {
		t.Fatal("registered example has no role")
	}
	id := role["object_id"].(string)
	role["rights"] = map[string]any{"license": "Apache-2.0", "source_reference": "fixture:p2", "redistribution_allowed": true, "allowed_uses": []string{"WORLD_CONTENT"}}
	delete(role, "tool_ids")
	role["tool_capability_ids"] = []string{}
	role["generation"] = map[string]any{"mode": "NONE"}
	doc["objects"] = append(doc["objects"].([]any), role)
	doc["module_agent_bindings"] = []any{map[string]any{"binding_id": "binding-only", "owner_object_id": "note", "module_id": "AUTHORING", "enabled": true, "role_ref": map[string]any{"kind": "BUNDLE_OBJECT", "object_id": id}, "generation": map[string]any{"mode": "REQUIREMENT", "capability_id": "text"}}}
	plan.Body, _ = json.Marshal(doc)
	plan.IDs = append(plan.IDs, wepkg.PortableID{Domain: "OBJECT", LogicalID: id, LocalID: strings.Repeat("c", 64)}, wepkg.PortableID{Domain: "MODULE_BINDING", LogicalID: "binding-only", LocalID: strings.Repeat("d", 64)})
	plan.References = []wepkg.PortableReference{{Pointer: "/module_agent_bindings/0/role_ref", Kind: "BUNDLE_OBJECT", ObjectID: id, ExpectedKinds: []string{"ROLE"}}}
	plan.Requirements.Model = []wepkg.Capability{{CapabilityID: "text", Required: true}}
	connection, model := generated.Identity(MockConnection), generated.Identity(MockModel)
	opts.ModelMappings = []generated.PackageModelMapping{{CapabilityID: "text", Settings: generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: &connection, ModelID: &model, RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}}}
	opts.Rights = append(opts.Rights, generated.PackageRightsMapping{ObjectID: id, AllowedUses: []string{"EDIT", "COPY"}})
	in, findings := projectPortable(plan, opts)
	if len(findings) != 0 {
		t.Fatalf("binding-only requirement was evaluated before binding conversion: %+v", findings)
	}
	if len(in.ModuleAgentBindings) != 1 || in.ModuleAgentBindings[0].Generation.Mode != "OVERRIDE" || string(in.ModuleAgentBindings[0].BindingID) != strings.Repeat("d", 64) {
		t.Fatal("binding/model identity projection incomplete")
	}
	for _, mutate := range []func(*generated.GenerationSettings){
		func(g *generated.GenerationSettings) { g.RequiredCapabilities = []generated.Identity{"UNKNOWN"} },
		func(g *generated.GenerationSettings) { g.AllowedExternalUses = []string{"MODEL"} },
		func(g *generated.GenerationSettings) {
			id := generated.Identity("unknown-budget")
			g.BudgetPolicyRef = &id
		},
	} {
		bad := opts
		bad.ModelMappings = append([]generated.PackageModelMapping{}, opts.ModelMappings...)
		mutate(&bad.ModelMappings[0].Settings)
		if input, findings := projectPortable(plan, bad); len(findings) == 0 || len(input.Objects) != 0 {
			t.Fatal("unsupported settings escaped on an explicitly consumed mapping")
		}
	}
}

func TestPackagePreviewRequiredModelDeclaration(t *testing.T) {
	plan, options := previewFixture(t)
	plan.Requirements.Model = []wepkg.Capability{{CapabilityID: "text", Required: true}}
	options.LocalCapabilities = map[string]string{"text": "TEXT"}
	if _, findings := projectPortable(plan, options); len(findings) != 0 {
		t.Fatal(findings)
	}
	plan.Requirements.Presentation = []wepkg.Capability{{CapabilityID: "text", Required: true}}
	options.LocalCapabilities["text"] = "SIGN_CUSTODY"
	if _, findings := projectPortable(plan, options); len(findings) == 0 {
		t.Fatal("non-text skill capability fulfilled a text model/presentation requirement")
	}
}

func TestPackagePreviewSourceEdgesIncludeMediaAndTheme(t *testing.T) {
	node := map[string]any{"asset_refs": []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "media"}}, "theme_ref": map[string]any{"kind": "PINNED_DEFINITION", "object_id": "theme", "release_id": "release", "version": "1", "content_hash": strings.Repeat("a", 64)}}
	if got := previewSourceEdges(node); len(got) != 2 {
		t.Fatalf("current source-use closure omitted media/theme refs: %v", got)
	}
}

func TestPackagePreviewBindingReferencesAreSourceRoots(t *testing.T) {
	input := generated.DraftInput{ModuleAgentBindings: []generated.ModuleAgentBinding{{RoleRef: json.RawMessage(`{"kind":"PINNED_DEFINITION","release_id":"role-release","object_id":"role","version":"1","content_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)}}, SkillBindings: []generated.SkillBinding{{SkillRef: json.RawMessage(`{"kind":"PINNED_DEFINITION","release_id":"skill-release","object_id":"skill","version":"1","content_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)}}}
	if got := previewBindingRefs(input); len(got) != 2 {
		t.Fatalf("binding-only source roots omitted: %v", got)
	}
}
