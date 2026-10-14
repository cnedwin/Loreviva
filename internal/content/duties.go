package content

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
)

func roleActive(in generated.DraftInput, ref, role map[string]any, resolved ResolvedDefinitions, policies ...*domain.RoutePolicySnapshot) bool {
	if role["implementation_kind"] == "PROGRAM" {
		return true
	}
	for _, binding := range in.ModuleAgentBindings {
		if !binding.Enabled {
			continue
		}
		var boundRef map[string]any
		json.Unmarshal(binding.RoleRef, &boundRef)
		if boundRef["kind"] != ref["kind"] || boundRef["object_id"] != ref["object_id"] || (ref["kind"] != "LOCAL_DRAFT" && referenceKey(boundRef) != referenceKey(ref)) {
			continue
		}
		var policy *domain.RoutePolicySnapshot
		if len(policies) > 0 {
			policy = policies[0]
		}
		_, enabled, e := ResolveRoute(in, map[string]any{"kind": "LOCAL_DRAFT", "object_id": string(binding.OwnerObjectID)}, binding.ModuleID, resolved, policy)
		if e == nil && enabled {
			return true
		}
	}
	return false
}
// A duty counts only when its resolved role declares that duty and is active; an agent role needs an enabled binding with a usable route, while a program role needs no model route.
func dutyPerformers(in generated.DraftInput, duty string, resolved ResolvedDefinitions) []generated.Identity {
	out := []generated.Identity{}
	maps, e := objects(in)
	if e != nil {
		return out
	}
	world := maps[string(in.RootObjectID)]
	if world == nil || world["kind"] != "WORLD" {
		return out
	}
	seen := map[string]bool{}
	for _, raw := range world["duty_assignments"].([]any) {
		a := raw.(map[string]any)
		if a["duty_id"] != duty {
			continue
		}
		ref := a["role_ref"].(map[string]any)
		role := resolveDefinition(ref, maps, resolved)
		if role == nil || role["kind"] != "ROLE" || !roleActive(in, ref, role, resolved) {
			continue
		}
		declares := false
		for _, v := range role["duty_ids"].([]any) {
			if v == duty {
				declares = true
			}
		}
		id := fmt.Sprint(ref["object_id"])
		if declares && !seen[id] {
			seen[id] = true
			out = append(out, generated.Identity(id))
		}
	}
	return out
}
