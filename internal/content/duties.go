package content

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/generated"
)

func roleActive(in generated.DraftInput, ref, role map[string]any, resolved ResolvedDefinitions) bool {
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
		_, enabled, e := EffectiveRoute(in, string(binding.OwnerObjectID), binding.ModuleID, resolved)
		if e == nil && enabled {
			return true
		}
	}
	return false
}
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
