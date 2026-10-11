package content

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"sort"
	"strings"
)

func localRefs(raw any, pointer string, emit func(string, string)) {
	switch x := raw.(type) {
	case map[string]any:
		if x["kind"] == "LOCAL_DRAFT" {
			if id, ok := x["object_id"].(string); ok {
				emit(id, pointer)
			}
		}
		for key, value := range x {
			localRefs(value, pointer+"/"+strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"), emit)
		}
	case []any:
		for i, value := range x {
			localRefs(value, fmt.Sprintf("%s/%d", pointer, i), emit)
		}
	}
}
func targetSet(targets []generated.ImpactTarget, kind string) map[string]bool {
	out := map[string]bool{}
	for _, t := range targets {
		if t.Kind == kind {
			out[string(t.TargetID)] = true
		}
	}
	return out
}
func selectedInput(in generated.DraftInput, targets []generated.ImpactTarget, keep bool) generated.DraftInput {
	objects := targetSet(targets, "OBJECT")
	modules := targetSet(targets, "MODULE_BINDING")
	skills := targetSet(targets, "SKILL_BINDING")
	out := in
	out.Objects = []generated.ContentObject{}
	out.ModuleAgentBindings = []generated.ModuleAgentBinding{}
	out.SkillBindings = []generated.SkillBinding{}
	for _, raw := range in.Objects {
		v, _ := wire.Parse(raw)
		id := v.(map[string]any)["object_id"].(string)
		if objects[id] == keep {
			out.Objects = append(out.Objects, raw)
		}
	}
	for _, b := range in.ModuleAgentBindings {
		if keep {
			if objects[string(b.OwnerObjectID)] {
				out.ModuleAgentBindings = append(out.ModuleAgentBindings, b)
			}
		} else if !modules[string(b.BindingID)] {
			out.ModuleAgentBindings = append(out.ModuleAgentBindings, b)
		}
	}
	for _, b := range in.SkillBindings {
		if keep {
			if objects[string(b.RoleObjectID)] {
				out.SkillBindings = append(out.SkillBindings, b)
			}
		} else if !skills[string(b.BindingID)] {
			out.SkillBindings = append(out.SkillBindings, b)
		}
	}
	return out
}
func copyInput(d generated.ContentDraft, targets []generated.ImpactTarget) (generated.DraftInput, string, error) {
	in := selectedInput(d.Input, targets, true)
	maps, e := objects(in)
	if e != nil {
		return in, "", e
	}
	root := string(in.RootObjectID)
	kind := string(d.RootKind)
	if maps[root] == nil {
		root = ""
		referenced := map[string]bool{}
		for _, m := range maps {
			localRefs(m, "", func(id, _ string) { referenced[id] = true })
		}
		for _, binding := range in.ModuleAgentBindings {
			raw, _ := json.Marshal(binding)
			v, _ := wire.Parse(raw)
			localRefs(v, "", func(id, _ string) { referenced[id] = true })
		}
		for _, binding := range in.SkillBindings {
			raw, _ := json.Marshal(binding)
			v, _ := wire.Parse(raw)
			localRefs(v, "", func(id, _ string) { referenced[id] = true })
		}
		for _, raw := range in.Objects {
			v, _ := wire.Parse(raw)
			m := v.(map[string]any)
			if referenced[m["object_id"].(string)] {
				continue
			}
			switch m["kind"] {
			case "WORLD", "CHARACTER", "ROLE", "SKILL", "KNOWLEDGE", "PRESENTATION_THEME":
				if root != "" {
					return in, "", ErrBlocked
				}
				root = m["object_id"].(string)
				kind = m["kind"].(string)
			}
		}
		if root == "" {
			return in, "", ErrBlocked
		}
		in.RootObjectID = generated.Identity(root)
	}
	return in, kind, nil
}
func staticImpact(d generated.ContentDraft, action string, selected []string, options ...ResolvedDefinitions) ([]generated.ImpactTarget, []generated.IncomingReference, []generated.DutyCoverage, []generated.FieldError, error) {
	maps, e := objects(d.Input)
	if e != nil {
		return nil, nil, nil, nil, e
	}
	targets := []generated.ImpactTarget{}
	incoming := []generated.IncomingReference{}
	coverage := []generated.DutyCoverage{}
	findings := []generated.FieldError{}
	add := func(pointer, code, message string) {
		findings = append(findings, generated.FieldError{Pointer: pointer, Code: generated.Identity(code), Message: message})
	}
	requested := map[string]bool{}
	if len(selected) > 200 {
		return nil, nil, nil, nil, ErrBlocked
	}
	for _, id := range selected {
		if requested[id] {
			return nil, nil, nil, nil, ErrBlocked
		}
		requested[id] = true
	}
	if len(requested) == 0 {
		if action == "REMOVE_BINDING" {
			return nil, nil, nil, nil, ErrBlocked
		}
		for id := range maps {
			requested[id] = true
		}
	}
	for id := range requested {
		if maps[id] != nil && action != "REMOVE_BINDING" {
			targets = append(targets, generated.ImpactTarget{Kind: "OBJECT", TargetID: generated.Identity(id)})
			continue
		}
		found := false
		for _, b := range d.Input.ModuleAgentBindings {
			if string(b.BindingID) == id && action == "REMOVE_BINDING" {
				targets = append(targets, generated.ImpactTarget{Kind: "MODULE_BINDING", TargetID: b.BindingID})
				found = true
			}
		}
		for _, b := range d.Input.SkillBindings {
			if string(b.BindingID) == id && action == "REMOVE_BINDING" {
				targets = append(targets, generated.ImpactTarget{Kind: "SKILL_BINDING", TargetID: b.BindingID})
				found = true
			}
		}
		if !found {
			return nil, nil, nil, nil, ErrBlocked
		}
	}
	if action == "COPY" {
		set := targetSet(targets, "OBJECT")
		var visit func(string)
		visit = func(id string) {
			emit := func(ref, _ string) {
				if !set[ref] && maps[ref] != nil {
					set[ref] = true
					targets = append(targets, generated.ImpactTarget{Kind: "OBJECT", TargetID: generated.Identity(ref)})
					visit(ref)
				}
			}
			localRefs(maps[id], "", emit)
			for _, b := range d.Input.ModuleAgentBindings {
				if string(b.OwnerObjectID) == id {
					raw, _ := json.Marshal(b)
					v, _ := wire.Parse(raw)
					localRefs(v, "", emit)
				}
			}
			for _, b := range d.Input.SkillBindings {
				if string(b.RoleObjectID) == id {
					raw, _ := json.Marshal(b)
					v, _ := wire.Parse(raw)
					localRefs(v, "", emit)
				}
			}
		}
		ids := []string{}
		for id := range set {
			ids = append(ids, id)
		}
		for _, id := range ids {
			visit(id)
		}
		for id := range set {
			rights := maps[id]["rights"].(map[string]any)
			allowed := false
			for _, use := range rights["allowed_uses"].([]any) {
				if use == "COPY" {
					allowed = true
				}
			}
			if !allowed {
				add("/objects/"+id+"/rights", "COPY_RIGHTS", "Selected static definition or dependency has no copy right.")
			}
		}
		if _, _, e = copyInput(d, targets); e != nil {
			add("/object_ids", "COPY_ROOT", "Selection requires one unambiguous resource root or the original root.")
		}
	}
	set := targetSet(targets, "OBJECT")
	emit := func(source, kind, pointer string, value any) {
		localRefs(value, pointer, func(target, p string) {
			if set[target] {
				incoming = append(incoming, generated.IncomingReference{SourceID: generated.Identity(source), SourceKind: kind, TargetID: generated.Identity(target), Pointer: p})
				if action == "DELETE" && !set[source] {
					add(p, "INCOMING_REFERENCE", "Unselected definition or binding still references the selected target. Rebind or remove it in a saved revision first.")
				}
			}
		})
	}
	for id, m := range maps {
		emit(id, "OBJECT", "/objects/"+id, m)
	}
	for _, b := range d.Input.ModuleAgentBindings {
		raw, _ := json.Marshal(b)
		v, _ := wire.Parse(raw)
		emit(string(b.BindingID), "MODULE_BINDING", "/module_agent_bindings/"+string(b.BindingID), v)
		if action == "DELETE" && set[string(b.OwnerObjectID)] {
			add("/module_agent_bindings/"+string(b.BindingID), "INCOMING_BINDING", "Binding still requires the selected owner. Save an explicit rebind/removal first.")
		}
	}
	for _, b := range d.Input.SkillBindings {
		raw, _ := json.Marshal(b)
		v, _ := wire.Parse(raw)
		emit(string(b.BindingID), "SKILL_BINDING", "/skill_bindings/"+string(b.BindingID), v)
		if action == "DELETE" && set[string(b.RoleObjectID)] {
			add("/skill_bindings/"+string(b.BindingID), "INCOMING_BINDING", "Skill binding still requires the selected role. Save an explicit rebind/removal first.")
		}
	}
	_, before := Findings(d.Input, string(d.RootKind), firstResolved(options))
	after := before
	afterInput := d.Input
	wholeDelete := action == "DELETE" && len(set) == len(maps)
	if action == "REMOVE_BINDING" || (action == "DELETE" && !wholeDelete) {
		variant := selectedInput(d.Input, targets, false)
		afterInput = variant
		_, after = Findings(variant, string(d.RootKind), firstResolved(options))
		if set[string(d.Input.RootObjectID)] {
			add("/root_object_id", "ROOT_REMOVAL", "Partial deletion cannot remove the aggregate root.")
		}
	}
	for _, duty := range Duties {
		has := func(list []string) bool {
			for _, v := range list {
				if v == duty {
					return true
				}
			}
			return false
		}
		c := generated.DutyCoverage{DutyID: generated.Duty(duty), CoveredBefore: has(before), CoveredAfter: has(after), PerformerObjectIDS: []generated.Identity{}}
		if c.CoveredAfter {
			c.PerformerObjectIDS = dutyPerformers(afterInput, duty, firstResolved(options))
		}
		if c.CoveredBefore && !c.CoveredAfter {
			add("/duty_assignments", "UNASSIGNED_DUTY", "Plan leaves required duty unassigned: "+duty)
		}
		sort.Slice(c.PerformerObjectIDS, func(i, j int) bool { return c.PerformerObjectIDS[i] < c.PerformerObjectIDS[j] })
		coverage = append(coverage, c)
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Kind+string(targets[i].TargetID) < targets[j].Kind+string(targets[j].TargetID)
	})
	sort.Slice(incoming, func(i, j int) bool { return incoming[i].Pointer < incoming[j].Pointer })
	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Pointer+string(findings[i].Code) < findings[j].Pointer+string(findings[j].Code)
	})
	return targets, incoming, coverage, findings, nil
}
