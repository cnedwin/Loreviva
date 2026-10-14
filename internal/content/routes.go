package content

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"sort"
)

type routeMapping = struct {
	ConnectionRef     generated.Identity           `json:"connection_ref"`
	DescriptorHash    string                       `json:"descriptor_hash"`
	EffectiveSettings generated.GenerationSettings `json:"effective_settings"`
	ModelID           generated.Identity           `json:"model_id"`
	ModuleID          generated.Identity           `json:"module_id"`
	OwnerObjectID     generated.Identity           `json:"owner_object_id"`
	OwnerReference    generated.DefinitionRef      `json:"owner_reference"`
	SettingsHash      string                       `json:"settings_hash"`
}

// EffectiveRoute resolves one generating owner only. INHERIT advances through
// enabled owner/module binding, referenced role, owner, root, installation Mock;
// NONE terminates without fallback. Runtime actor materialization belongs to Task4.
func EffectiveRoute(in generated.DraftInput, owner, module string, options ...ResolvedDefinitions) (generated.GenerationSettings, bool, error) {
	return effectiveReferenceRoute(in, map[string]any{"kind": "LOCAL_DRAFT", "object_id": owner}, module, firstResolved(options))
}
func effectiveReferenceRoute(in generated.DraftInput, ownerRef map[string]any, module string, resolved ResolvedDefinitions) (generated.GenerationSettings, bool, error) {
	return ResolveRoute(in, ownerRef, module, resolved, nil)
}
func ResolveRoute(in generated.DraftInput, ownerRef map[string]any, module string, resolved ResolvedDefinitions, policy *domain.RoutePolicySnapshot) (generated.GenerationSettings, bool, error) {
	maps, e := objects(in)
	if e != nil {
		return generated.GenerationSettings{}, false, e
	}
	ownerDefinition := resolveDefinition(ownerRef, maps, resolved)
	if ownerDefinition == nil {
		return generated.GenerationSettings{}, false, ErrBlocked
	}
	// Check the selected owner's eligibility before binding or fallback precedence.
	// A non-generating resource root can still publish with explicit NONE.
	if ownerDefinition["kind"] == "SKILL" || (ownerDefinition["kind"] == "ROLE" && ownerDefinition["implementation_kind"] != "AGENT") {
		if module == "DEFAULT" {
			g, _ := ownerDefinition["generation"].(map[string]any)
			if g["mode"] == "NONE" {
				return generated.GenerationSettings{Mode: "NONE"}, false, nil
			}
		}
		return generated.GenerationSettings{}, false, ErrBlocked
	}
	return resolveOwnerRoute(in, ownerRef, ownerDefinition, maps, module, resolved, policy)
}

// Fixed editing-only producer selection. It never selects a runtime Skill generator.
func effectiveDraftCandidateRoute(in generated.DraftInput, owner, module string, resolved ResolvedDefinitions, policy *domain.RoutePolicySnapshot) (generated.GenerationSettings, bool, error) {
	maps, e := objects(in)
	if e != nil {
		return generated.GenerationSettings{}, false, e
	}
	definition := maps[owner]
	if definition == nil {
		return generated.GenerationSettings{}, false, ErrBlocked
	}
	if definition["kind"] != "SKILL" {
		return ResolveRoute(in, map[string]any{"kind": "LOCAL_DRAFT", "object_id": owner}, module, resolved, policy)
	}
	if definition["implementation_kind"] != "GENERATIVE" || module != "AUTHORING" {
		return generated.GenerationSettings{}, false, ErrBlocked
	}
	return resolveOwnerRoute(in, map[string]any{"kind": "LOCAL_DRAFT", "object_id": owner}, definition, maps, module, resolved, policy)
}

func resolveOwnerRoute(in generated.DraftInput, ownerRef, ownerDefinition map[string]any, maps map[string]map[string]any, module string, resolved ResolvedDefinitions, policy *domain.RoutePolicySnapshot) (generated.GenerationSettings, bool, error) {
	owner := fmt.Sprint(ownerRef["object_id"])
	layers := []generated.GenerationSettings{}
	var binding *generated.ModuleAgentBinding
	for _, b := range in.ModuleAgentBindings {
		if ownerRef["kind"] == "LOCAL_DRAFT" && b.Enabled && string(b.OwnerObjectID) == owner && b.ModuleID == module {
			if binding != nil {
				return generated.GenerationSettings{}, false, ErrBlocked
			}
			v := b
			binding = &v
		}
	}
	read := func(m map[string]any) {
		if m == nil {
			return
		}
		if raw, ok := m["generation"]; ok {
			b, _ := json.Marshal(raw)
			var g generated.GenerationSettings
			json.Unmarshal(b, &g)
			layers = append(layers, g)
		}
	}
	if binding != nil {
		layers = append(layers, binding.Generation)
		var ref map[string]any
		json.Unmarshal(binding.RoleRef, &ref)
		role := resolveDefinition(ref, maps, resolved)
		if role == nil || role["kind"] != "ROLE" {
			return generated.GenerationSettings{}, false, ErrBlocked
		}
		read(role)
	}
	read(ownerDefinition)
	if ownerRef["kind"] != "LOCAL_DRAFT" || owner != string(in.RootObjectID) {
		read(maps[string(in.RootObjectID)])
	}
	connection, model := generated.Identity(MockConnection), generated.Identity(MockModel)
	// With a policy snapshot, non-DEFAULT modules add product defaults then tenant defaults after owner/root layers and before the installation Mock fallback; NONE still stops resolution.
	if policy != nil {
		if policy.DescriptorHash != domain.InstalledMockDescriptorHash() {
			return generated.GenerationSettings{}, false, ErrBlocked
		}
		if module != "DEFAULT" {
			for _, defaults := range [][]generated.StudioModuleDefault{policy.ProductDefaults, policy.TenantDefaults} {
				for _, d := range defaults {
					if d.ModuleID == module {
						layers = append(layers, d.GenerationSettings)
					}
				}
			}
		}
	}
	layers = append(layers, generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: &connection, ModelID: &model, RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}})
	for _, g := range layers {
		switch g.Mode {
		case "NONE":
			return g, false, nil
		case "INHERIT":
			continue
		case "OVERRIDE":
			if g.ConnectionRef == nil || g.ModelID == nil || string(*g.ConnectionRef) != MockConnection || string(*g.ModelID) != MockModel {
				return g, false, ErrBlocked
			}
			for _, cap := range g.RequiredCapabilities {
				if cap != "TEXT" {
					return g, false, ErrBlocked
				}
			}
			return g, true, nil
		default:
			return g, false, ErrBlocked
		}
	}
	return generated.GenerationSettings{}, false, ErrBlocked
}
func freezeRoutes(in generated.DraftInput, options ...ResolvedDefinitions) ([]routeMapping, string, error) {
	return freezeRoutesWithPolicy(in, firstResolved(options), nil)
}
func freezeRoutesWithPolicy(in generated.DraftInput, resolved ResolvedDefinitions, policy *domain.RoutePolicySnapshot) ([]routeMapping, string, error) {
	mappings := []routeMapping{}
	pairs := map[string]struct {
		Ref    map[string]any
		Module string
	}{}
	addPair := func(ref map[string]any, module string) {
		key, _ := Hash(ref)
		pairs[key+"/"+module] = struct {
			Ref    map[string]any
			Module string
		}{ref, module}
	}
	root := string(in.RootObjectID)
	addPair(map[string]any{"kind": "LOCAL_DRAFT", "object_id": root}, "DEFAULT")
	for _, b := range in.ModuleAgentBindings {
		if b.Enabled {
			addPair(map[string]any{"kind": "LOCAL_DRAFT", "object_id": string(b.OwnerObjectID)}, b.ModuleID)
		}
	}
	maps, e := objects(in)
	if e != nil {
		return nil, "", e
	}
	world := maps[root]
	if world != nil && world["kind"] == "WORLD" {
		for _, v := range world["initial_actor_setups"].([]any) {
			ref := v.(map[string]any)["character_ref"].(map[string]any)
			addPair(ref, "INTERACTION")
		}
	}
	descriptor, _ := Hash(struct{ Connection, Model, Price string }{MockConnection, MockModel, "0"})
	keys := []string{}
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		pair := pairs[key]
		g, enabled, e := ResolveRoute(in, pair.Ref, pair.Module, resolved, policy)
		if e != nil {
			return nil, "", e
		}
		if !enabled {
			continue
		}
		settingsHash, e := Hash(g)
		if e != nil {
			return nil, "", e
		}
		ownerRaw, _ := json.Marshal(pair.Ref)
		mappings = append(mappings, routeMapping{ConnectionRef: *g.ConnectionRef, DescriptorHash: descriptor, EffectiveSettings: g, ModelID: *g.ModelID, ModuleID: generated.Identity(pair.Module), OwnerObjectID: generated.Identity(fmt.Sprint(pair.Ref["object_id"])), OwnerReference: ownerRaw, SettingsHash: settingsHash})
	}
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].ModuleID != mappings[j].ModuleID {
			return mappings[i].ModuleID < mappings[j].ModuleID
		}
		if mappings[i].OwnerObjectID != mappings[j].OwnerObjectID {
			return mappings[i].OwnerObjectID < mappings[j].OwnerObjectID
		}
		return string(mappings[i].OwnerReference) < string(mappings[j].OwnerReference)
	})
	hash, e := Hash(struct {
		Mappings     []routeMapping
		Capabilities []string
		Input        generated.DraftInput
	}{mappings, []string{"TEXT", "SIGN_CUSTODY"}, in})
	return mappings, hash, e
}
