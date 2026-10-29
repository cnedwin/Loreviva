package content

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
	"os"
	"strings"
	"testing"
)

func exportExample(t *testing.T) generated.Release {
	t.Helper()
	raw, e := os.ReadFile("../../contracts/source/openapi.yaml")
	if e != nil {
		t.Fatal(e)
	}
	var source struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if json.Unmarshal(raw, &source) != nil {
		t.Fatal("source example")
	}
	var schema struct {
		Examples []generated.DraftInput `json:"examples"`
	}
	if e = json.Unmarshal(source.Components.Schemas["DraftInput"], &schema); e != nil {
		t.Fatal(e)
	}
	input := schema.Examples[0]
	for i, raw := range input.Objects {
		var node map[string]any
		json.Unmarshal(raw, &node)
		node["rights"] = map[string]any{"origin_kind": "SYNTHETIC", "source_label": "fixture:projection", "license": "CC0-1.0", "allowed_uses": []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}}
		input.Objects[i] = mustMarshal(node)
	}
	input.Description = "Keep coffee-world, mia and mock-local in narrative."
	return generated.Release{ReleaseID: "original-installation-release", Input: input, PublishedBySubjectID: "creator"}
}

func TestPackageExportProgramCapabilitiesRemainDeclared(t *testing.T) {
	raw := []byte(`{"object_id":"program-skill","kind":"SKILL","name":"Synthetic static program","description":"","tags":[],"rights":{"origin_kind":"SYNTHETIC","source_label":"fixture:program","license":"CC0-1.0","allowed_uses":["EDIT","COPY","PUBLISH","REDISTRIBUTE"]},"audience_policy":"PRODUCT_READERS","version":"1","source_refs":[],"dependency_refs":[],"builtin_original":false,"implementation_kind":"PROGRAM","input_schema_id":"DraftInput","output_schema_id":"PublishCheckReport","registered_capability_ids":["TEXT","SIGN_CUSTODY"],"tool_ids":[],"conflicting_refs":[],"failure_policy":"BLOCK","max_depth":1,"max_duration_ms":"100","max_cost_units":"0","synthetic_examples":[],"generation":{"mode":"NONE","required_capabilities":[],"allowed_external_uses":["NONE"]}}`)
	if e := generated.Validate("SkillDefinition", raw); e != nil {
		t.Fatal(e)
	}
	input := generated.DraftInput{Name: "Program", Description: "Synthetic static definition", Tags: []string{}, RootObjectID: "program-skill", Objects: []generated.ContentObject{raw}, DependencyLocks: []generated.DependencyLock{}, ModuleAgentBindings: []generated.ModuleAgentBinding{}, SkillBindings: []generated.SkillBinding{}}
	root := generated.Release{ReleaseID: "program-release", Input: input, PublishedBySubjectID: "creator"}
	projection, e := projectPackageExport(root, map[string]generated.Release{"program-release": root}, nil, "creator")
	if e != nil {
		t.Fatal(e)
	}
	manifest := wepkg.Manifest{FormatVersion: "1.0", PackageID: "synthetic.program", PackageVersion: "1.0.0", Kind: "SKILL", Compatibility: wepkg.Compatibility{PublicAPIMajor: 1, PackageFormatMajor: 1, MinimumRuntimeContract: "1.9.0", RulePrimitiveSet: "core.v1"}, Entrypoints: []string{"content"}, Entries: []wepkg.Entry{{LogicalID: "content", Kind: "SKILL", Path: "definitions/content.json", Version: "1.0.0", References: []string{}}}, Files: []wepkg.FileRecord{{Path: "definitions/content.json", MediaType: "application/json", Rights: wepkg.Rights{License: "CC0-1.0", SourceReference: "fixture:program", RedistributionAllowed: true, AllowedUses: []string{"WORLD_CONTENT"}}}}, Dependencies: []wepkg.Dependency{}, Requirements: projection.Requirements, Provenance: wepkg.Provenance{AuthorDisplayName: "Synthetic creator", License: "CC0-1.0", SourceReferences: []string{"fixture:program"}, CreatedAt: "2026-10-29T00:00:00Z", SyntheticExamplesOnly: true}}
	first, _, e := wepkg.ExportBundle(manifest, projection.Body, nil)
	if e != nil {
		t.Fatal("declared program capabilities failed full archive admission", e)
	}
	second, _, e := wepkg.ExportBundle(manifest, projection.Body, nil)
	if e != nil || string(first) != string(second) {
		t.Fatal("static archive is not deterministic", e)
	}
	report := wepkg.Check(first, wepkg.DefaultLimits)
	if report.Verdict != "PASS" {
		t.Fatal(report.Findings)
	}
	plan, findings := wepkg.BuildPortablePlan(first, wepkg.DefaultLimits, "content", strings.Repeat("b", 64))
	if len(findings) != 0 {
		t.Fatal(findings)
	}
	local, refused := projectPortable(plan, packagePreviewOptions{RightsAttested: true, LocalCapabilities: map[string]string{"text": "TEXT", "sign_custody": "SIGN_CUSTODY"}, Rights: []generated.PackageRightsMapping{{ObjectID: "o0001", AllowedUses: []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}}}})
	if len(refused) != 0 || len(local.Objects) != 1 {
		t.Fatal("registered native capabilities did not map locally", refused)
	}
}
func TestPackageExportProjectionUsesDeclaredReferences(t *testing.T) {
	root := exportExample(t)
	out, e := projectPackageExport(root, map[string]generated.Release{string(root.ReleaseID): root}, nil, "creator")
	if e != nil {
		t.Fatal(e)
	}
	var body map[string]any
	json.Unmarshal(out.Body, &body)
	if body["description"] != root.Input.Description {
		t.Fatal("narrative transformed")
	}
	if body["root_object_id"] == string(root.Input.RootObjectID) {
		t.Fatal("local object identity escaped")
	}
	if strings.Contains(string(out.Body), "connection_ref") || strings.Contains(string(out.Body), "model_id") || strings.Contains(string(out.Body), "budget_policy_ref") {
		t.Fatal("local route configuration escaped")
	}
	for _, raw := range body["objects"].([]any) {
		o := raw.(map[string]any)
		if e := exportRefs(o, "OBJECT", o["kind"].(string), func(ref map[string]any) (map[string]any, error) {
			if ref["kind"] != "BUNDLE_OBJECT" {
				t.Fatal("local structural reference escaped", ref)
			}
			return ref, nil
		}); e != nil {
			t.Fatal(e)
		}
	}
}
func TestPackageExportAttributionAndRights(t *testing.T) {
	for _, value := range []string{"file:///private/source", "C:/private/source", "https://example.org/source?api_key=synthetic-secret", "https://user:secret@example.org/source", "http://127.0.0.1/private", "https://10.1.2.3/source", "http://localhost/source", "https://private.internal/source", "https://example.org/#token"} {
		t.Run(value, func(t *testing.T) {
			if publicAttribution(value) {
				t.Fatal("private or credential-bearing structured source accepted")
			}
		})
	}
	for _, value := range []string{"fixture:projection", "https://example.org/public/source", "wepkg:static-authoring"} {
		if !publicAttribution(value) {
			t.Fatal("public attribution refused", value)
		}
	}
	for _, uses := range [][]string{{"EDIT", "COPY"}, {"EDIT", "REDISTRIBUTE"}} {
		root := exportExample(t)
		var object map[string]any
		json.Unmarshal(root.Input.Objects[0], &object)
		object["rights"].(map[string]any)["allowed_uses"] = uses
		root.Input.Objects[0] = mustMarshal(object)
		if _, e := projectPackageExport(root, map[string]generated.Release{string(root.ReleaseID): root}, nil, "creator"); e == nil {
			t.Fatal("missing share rights accepted", uses)
		}
	}
}
func TestPackageExportExactPinnedAudienceClosure(t *testing.T) {
	note := func(id, audience string, refs []any) map[string]any {
		return map[string]any{"object_id": id, "kind": "KNOWLEDGE", "name": id, "description": "Synthetic", "tags": []string{}, "rights": map[string]any{"origin_kind": "SYNTHETIC", "source_label": "fixture:closure", "license": "CC0-1.0", "allowed_uses": []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}}, "audience_policy": audience, "version": "1", "source_refs": []string{}, "dependency_refs": refs, "knowledge_type": "NOTE", "text": "A static source", "binding_refs": []any{}}
	}
	shared := note("shared", "PRODUCT_READERS", []any{})
	hash, _ := Hash(shared)
	ref := map[string]any{"kind": "PINNED_DEFINITION", "release_id": "external-release", "object_id": "shared", "version": "1", "content_hash": hash}
	local := note("local", "PRODUCT_READERS", []any{ref})
	input := func(node map[string]any) generated.DraftInput {
		return generated.DraftInput{Name: "Closure", Description: "Synthetic", Tags: []string{}, RootObjectID: generated.Identity(node["object_id"].(string)), Objects: []generated.ContentObject{mustMarshal(node)}, DependencyLocks: []generated.DependencyLock{}, ModuleAgentBindings: []generated.ModuleAgentBinding{}, SkillBindings: []generated.SkillBinding{}}
	}
	root := generated.Release{ReleaseID: "root-release", Input: input(local), PublishedBySubjectID: "creator"}
	dep := generated.Release{ReleaseID: "external-release", Input: input(shared), PublishedBySubjectID: "other"}
	unrelated := note("unrelated", "PRODUCT_EDITORS", []any{})
	unrelated["kind"] = "ASSET"
	delete(unrelated, "text")
	delete(unrelated, "knowledge_type")
	delete(unrelated, "binding_refs")
	unrelated["asset_id"] = "unavailable-unrelated-asset"
	unrelated["asset_sha256"] = strings.Repeat("a", 64)
	unrelated["size_bytes"] = "1"
	unrelated["media_type"] = "text/plain"
	unrelated["client_capabilities"] = []string{}
	unrelated["fallback"] = "BLOCK"
	dep.Input.Objects = append(dep.Input.Objects, mustMarshal(unrelated))
	called := false
	catalog := map[string]generated.Release{"root-release": root, "external-release": dep}
	out, e := projectPackageExportWithAssets(root, catalog, func(string) (generated.FileRecord, error) { called = true; return generated.FileRecord{}, ErrBlocked }, "creator")
	if e != nil || called {
		t.Fatal("unrelated asset affected exact closure", e, called)
	}
	var body struct {
		Objects []any `json:"objects"`
	}
	json.Unmarshal(out.Body, &body)
	if len(body.Objects) != 2 {
		t.Fatal("unrelated definition included")
	}
	shared["audience_policy"] = "PRODUCT_EDITORS"
	dep.Input.Objects[0] = mustMarshal(shared)
	catalog["external-release"] = dep
	hash, _ = Hash(shared)
	ref["content_hash"] = hash
	root.Input = input(local)
	catalog["root-release"] = root
	if _, e = projectPackageExport(root, catalog, nil, "creator"); e == nil {
		t.Fatal("other creator private definition exported")
	}
}
