package content_test

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"testing"
)

func TestStudioDefaultsCentralResolver(t *testing.T) {
	in := coffee(t)
	for i, raw := range in.Objects {
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		if object["object_id"] == string(in.RootObjectID) || object["object_id"] == "alex-character" {
			object["generation"] = map[string]any{"mode": "INHERIT"}
			in.Objects[i], _ = json.Marshal(object)
		}
	}
	none := generated.GenerationSettings{Mode: "NONE"}
	policy := &domain.RoutePolicySnapshot{DescriptorHash: domain.InstalledMockDescriptorHash(), TenantDefaults: []generated.StudioModuleDefault{{ModuleID: "AUTHORING", GenerationSettings: none}}}
	ref := map[string]any{"kind": "LOCAL_DRAFT", "object_id": "alex-character"}
	g, on, e := content.ResolveRoute(in, ref, "AUTHORING", nil, policy)
	if e != nil || on || g.Mode != "NONE" {
		t.Fatal("tenant NONE fallback", g, on, e)
	}
	policy.ProductDefaults = []generated.StudioModuleDefault{{ModuleID: "AUTHORING", GenerationSettings: generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: ptrID(content.MockConnection), ModelID: ptrID(content.MockModel), RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}}}
	g, on, e = content.ResolveRoute(in, ref, "AUTHORING", nil, policy)
	if e != nil || !on || g.Mode != "OVERRIDE" {
		t.Fatal("product priority", g, on, e)
	}
	policy.ProductDefaults[0].GenerationSettings = none
	g, on, e = content.ResolveRoute(in, ref, "AUTHORING", nil, policy)
	if e != nil || on {
		t.Fatal("product NONE", g, on, e)
	}
	g, on, e = content.ResolveRoute(in, ref, "DEFAULT", nil, policy)
	if e != nil || !on {
		t.Fatal("DEFAULT became an eighth module", g, on, e)
	}
	policy.ProductDefaults[0].GenerationSettings = generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: ptrID("not-installed"), ModelID: ptrID("unknown")}
	if _, _, e = content.ResolveRoute(in, ref, "AUTHORING", nil, policy); e == nil {
		t.Fatal("invalid override silently used Mock")
	}
}
