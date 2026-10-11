package content_test

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	"testing"
)

func TestStaticSelectedTargetsIncomingRefsAndIndependentResourceCopy(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	d, e := s.Create(ctx, scope, "WORLD", "", coffee(t), "impact-create")
	if e != nil {
		t.Fatal(e)
	}
	blocked, e := s.Impact(ctx, scope, string(d.DraftID), 1, "DELETE", "delete-character", "alex-character")
	if e != nil || len(blocked.SelectedTargets) != 1 || len(blocked.IncomingRefs) == 0 || len(blocked.BlockingFindings) == 0 {
		t.Fatal("referenced object delete not blocked", blocked, e)
	}
	copy, e := s.Impact(ctx, scope, string(d.DraftID), 1, "COPY", "copy-character", "alex-character")
	if e != nil || len(copy.SelectedTargets) != 1 || len(copy.BlockingFindings) != 0 {
		t.Fatal("eligible character copy blocked", copy, e)
	}
	result, e := s.Command(ctx, scope, string(d.DraftID), 1, "COPY", string(copy.ReportID), "", "Alex copy", "copy")
	if e != nil {
		t.Fatal(e)
	}
	var receipt generated.CopyReceipt
	if e = generated.Decode("CopyReceipt", result, &receipt); e != nil {
		t.Fatal(e)
	}
	if receipt.Draft.RootKind != "CHARACTER" || receipt.Draft.ProjectID != nil || len(receipt.Draft.Input.Objects) != 1 {
		t.Fatal("selection copy included unrelated world/runtime", receipt)
	}
}
func TestBindingRemovalPlanReportsDutyGapWithoutMutatingDraft(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	in := coffee(t)
	var role map[string]any
	if e := json.Unmarshal(in.Objects[len(in.Objects)-1], &role); e != nil {
		t.Fatal(e)
	}
	role["implementation_kind"] = "AGENT"
	role["generation"] = map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}}
	in.Objects[len(in.Objects)-1], _ = json.Marshal(role)
	binding := generated.ModuleAgentBinding{BindingID: "director-binding", OwnerObjectID: "coffee-world", ModuleID: "INTERACTION", RoleRef: json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"program-duties"}`), Enabled: true, Generation: generated.GenerationSettings{Mode: "INHERIT", RequiredCapabilities: []generated.Identity{}, AllowedExternalUses: []string{"NONE"}}}
	in.ModuleAgentBindings = append(in.ModuleAgentBindings, binding)
	d, e := s.Create(ctx, scope, "WORLD", "", in, "binding-create")
	if e != nil {
		t.Fatal(e)
	}
	report, e := s.Impact(ctx, scope, string(d.DraftID), 1, "REMOVE_BINDING", "remove-binding", "director-binding")
	if e != nil || len(report.SelectedTargets) != 1 || len(report.DutyCoverage) != 5 || len(report.BlockingFindings) == 0 {
		t.Fatal("required duty removal accepted", report, e)
	}
	before, e := s.GetDraft(ctx, scope, string(d.DraftID), "")
	if e != nil || len(before.Input.ModuleAgentBindings) != 1 {
		t.Fatal("impact mutated binding", e)
	}
	in.ModuleAgentBindings = []generated.ModuleAgentBinding{}
	if _, e = s.SaveDraft(ctx, scope, string(d.DraftID), in, 1, "save-unassigned"); e != nil {
		t.Fatal(e)
	}
	check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 2, []string{"MOCK_DEMO"}, "check-unassigned")
	if e != nil || check.Passed {
		t.Fatal("unassigned agent duties published", check, e)
	}
}
func TestWoodenSignPrimitiveActuallyEvaluatesSlotCustody(t *testing.T) {
	in := coffee(t)
	var rule generated.RuleDefinition
	if e := generated.Decode("RuleDefinition", in.Objects[5], &rule); e != nil {
		t.Fatal(e)
	}
	if got := content.EvaluateSignCustody(rule, "mia", "wooden-sign", "MOVE", "mia"); !got.Allowed {
		t.Fatal("custodian incorrectly denied", got)
	}
	if got := content.EvaluateSignCustody(rule, "alex", "wooden-sign", "MOVE", "mia"); got.Allowed {
		t.Fatal("noncustodian moved sign")
	}
	if got := content.EvaluateSignCustody(rule, "mia", "other-prop", "MOVE", "mia"); got.Allowed {
		t.Fatal("unbound target allowed")
	}
}

func TestBindingsValidateReferencesAndFreezeEffectiveRoutes(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	in := coffee(t)
	binding := generated.ModuleAgentBinding{BindingID: "bad-binding", OwnerObjectID: "coffee-world", ModuleID: "INTERACTION", RoleRef: json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"missing-role"}`), Enabled: true, Generation: generated.GenerationSettings{Mode: "INHERIT", RequiredCapabilities: []generated.Identity{}, AllowedExternalUses: []string{"NONE"}}}
	in.ModuleAgentBindings = append(in.ModuleAgentBindings, binding)
	d, e := s.Create(ctx, scope, "WORLD", "", in, "bad-binding-create")
	if e != nil {
		t.Fatal(e)
	}
	check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "bad-binding-check")
	if e != nil || check.Passed {
		t.Fatal("unresolved module role published", check, e)
	}
	in = coffee(t)
	binding.RoleRef = json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"program-duties"}`)
	binding.Generation = generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: ptrID("mock-local"), ModelID: ptrID("synthetic-text-v1"), RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}
	in.ModuleAgentBindings = append(in.ModuleAgentBindings, binding)
	d, e = s.Create(ctx, scope, "WORLD", "", in, "mapped-binding-create")
	if e != nil {
		t.Fatal(e)
	}
	check, e = s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "mapped-binding-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	release, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "mapped-binding-publish")
	if e != nil {
		t.Fatal(e)
	}
	if len(release.RouteMappings) != 4 || release.RouteMappings[0].ModuleID != "DEFAULT" || release.RouteMappings[1].ModuleID != "INTERACTION" {
		t.Fatal("actual routes not frozen", release.RouteMappings)
	}
	in.ModuleAgentBindings = append(in.ModuleAgentBindings, binding)
	in.ModuleAgentBindings[1].BindingID = "second-binding"
	if _, e = s.Create(ctx, scope, "WORLD", "", in, "ambiguous-module"); e == nil {
		t.Fatal("duplicate active module accepted")
	}
}
func ptrID(v string) *generated.Identity { x := generated.Identity(v); return &x }

func TestSelectedCopyIncludesOwnedBindingDependencies(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	in := coffee(t)
	in.ModuleAgentBindings = append(in.ModuleAgentBindings, generated.ModuleAgentBinding{BindingID: "alex-method", OwnerObjectID: "alex-character", ModuleID: "INTERACTION", RoleRef: json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"program-duties"}`), Enabled: true, Generation: generated.GenerationSettings{Mode: "INHERIT", RequiredCapabilities: []generated.Identity{}, AllowedExternalUses: []string{"NONE"}}})
	d, e := s.Create(ctx, scope, "WORLD", "", in, "copy-method-create")
	if e != nil {
		t.Fatal(e)
	}
	report, e := s.Impact(ctx, scope, string(d.DraftID), 1, "COPY", "copy-method-check", "alex-character")
	if e != nil || len(report.SelectedTargets) != 2 || len(report.BlockingFindings) != 0 {
		t.Fatal("owned binding dependency closure", report, e)
	}
	raw, e := s.Command(ctx, scope, string(d.DraftID), 1, "COPY", string(report.ReportID), "", "Alex method copy", "copy-method")
	if e != nil {
		t.Fatal(e)
	}
	var out generated.CopyReceipt
	if e = generated.Decode("CopyReceipt", raw, &out); e != nil {
		t.Fatal(e)
	}
	if out.Draft.Input.RootObjectID != "alex-character" || len(out.Draft.Input.Objects) != 2 || len(out.Draft.Input.ModuleAgentBindings) != 1 {
		t.Fatal("unrelated content or dependency omission", out)
	}
}

func TestTwoCharacterOwnersAndNoneNeverFallback(t *testing.T) {
	in := coffee(t)
	g := generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: ptrID("mock-local"), ModelID: ptrID("synthetic-text-v1"), RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}
	for _, owner := range []string{"alex-character", "mia-character"} {
		in.ModuleAgentBindings = append(in.ModuleAgentBindings, generated.ModuleAgentBinding{BindingID: generated.Identity(owner + "-binding"), OwnerObjectID: generated.Identity(owner), ModuleID: "INTERACTION", RoleRef: json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"program-duties"}`), Enabled: true, Generation: g})
	}
	if _, e := content.Prepare(in); e != nil {
		t.Fatal("valid two owners rejected", e)
	}
	for _, owner := range []string{"alex-character", "mia-character"} {
		route, enabled, e := content.EffectiveRoute(in, owner, "INTERACTION")
		if e != nil || !enabled || route.Mode != "OVERRIDE" {
			t.Fatal("owner route resolution", owner, e)
		}
	}
	in.ModuleAgentBindings[0].Generation.Mode = "NONE"
	route, enabled, e := content.EffectiveRoute(in, "alex-character", "INTERACTION")
	if e != nil || enabled || route.Mode != "NONE" {
		t.Fatal("NONE fell back to world/default", e)
	}
}
