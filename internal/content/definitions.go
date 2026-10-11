package content

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"sort"
	"strings"
	"unicode/utf8"
)

var Duties = []string{"FACT_VALIDATION", "RIGHTS_CHECK", "AUDIENCE_CHECK", "NARRATIVE", "STATE_COMMIT"}
var SchemaIDs = map[string]bool{"DraftInput": true, "PublishCheckReport": true, "TurnRequest": true, "ApprovedPacket": true}

const MockConnection = "mock-local"
const MockModel = "synthetic-text-v1"

func Hash(value any) (string, error) {
	b, e := json.Marshal(value)
	if e != nil {
		return "", e
	}
	b, e = wire.Canonical(b)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func objects(in generated.DraftInput) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, raw := range in.Objects {
		v, e := wire.Parse(raw)
		if e != nil {
			return nil, e
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, ErrBlocked
		}
		id, _ := m["object_id"].(string)
		if _, found := out[id]; found {
			return nil, &FieldErrors{[]generated.FieldError{{Pointer: "/objects", Code: "DUPLICATE_OBJECT", Message: "Object logical IDs must be unique."}}}
		}
		out[id] = m
	}
	return out, nil
}
func Prepare(in generated.DraftInput) (generated.DraftInput, error) {
	b, e := json.Marshal(in)
	if e != nil {
		return in, e
	}
	if e = generated.Validate("DraftInput", b); e != nil {
		return in, &FieldErrors{[]generated.FieldError{{Pointer: "/input", Code: "SCHEMA", Message: "Definition does not match the complete typed Schema."}}}
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Tags = trimTags(in.Tags)
	if in.Name == "" {
		return in, &FieldErrors{[]generated.FieldError{{Pointer: "/name", Code: "EMPTY_NAME", Message: "Name must contain visible text."}}}
	}
	maps, e := objects(in)
	if e != nil {
		return in, e
	}
	for i, raw := range in.Objects {
		v, _ := wire.Parse(raw)
		m := v.(map[string]any)
		name := strings.TrimSpace(m["name"].(string))
		if name == "" || utf8.RuneCountInString(name) > 80 {
			return in, &FieldErrors{[]generated.FieldError{{Pointer: fmt.Sprintf("/objects/%d/name", i), Code: "NAME", Message: "Name must contain1 to80 characters."}}}
		}
		m["name"] = name
		tags := []string{}
		for _, tag := range m["tags"].([]any) {
			tags = append(tags, tag.(string))
		}
		m["tags"] = trimTags(tags)
		if original, _ := m["builtin_original"].(bool); original {
			return in, &FieldErrors{[]generated.FieldError{{Pointer: fmt.Sprintf("/objects/%d/builtin_original", i), Code: "PROTECTED_ORIGINAL", Message: "Distribution originals cannot be created or changed. Create an ordinary revision with origin_ref."}}}
		}
		m["object_id"] = m["object_id"]
		in.Objects[i], _ = json.Marshal(m)
	}
	ids := map[string]bool{}
	for id := range maps {
		ids[id] = true
	}
	modules := map[string]bool{}
	for _, binding := range in.ModuleAgentBindings {
		id := string(binding.BindingID)
		if ids[id] {
			return in, ErrBlocked
		}
		ids[id] = true
		k := string(binding.OwnerObjectID) + "/" + binding.ModuleID
		if binding.Enabled && (binding.ModuleID == "DEFAULT" || modules[k]) {
			return in, ErrBlocked
		}
		if binding.Enabled {
			modules[k] = true
		}
	}
	for _, binding := range in.SkillBindings {
		id := string(binding.BindingID)
		if ids[id] {
			return in, ErrBlocked
		}
		ids[id] = true
	}
	for _, binding := range in.ModuleAgentBindings {
		if maps[string(binding.OwnerObjectID)] == nil {
			return in, &FieldErrors{[]generated.FieldError{{Pointer: "/module_agent_bindings", Code: "MISSING_OWNER", Message: "Binding owner must be a local definition."}}}
		}
	}
	for _, binding := range in.SkillBindings {
		if maps[string(binding.RoleObjectID)] == nil {
			return in, &FieldErrors{[]generated.FieldError{{Pointer: "/skill_bindings", Code: "MISSING_OWNER", Message: "Skill owner must be a local role."}}}
		}
	}
	return in, nil
}
func trimTags(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
func Findings(in generated.DraftInput, rootKind string, options ...ResolvedDefinitions) ([]generated.FieldError, []string) {
	resolved := firstResolved(options)
	findings := []generated.FieldError{}
	duties := map[string]bool{}
	add := func(p, c, m string) {
		findings = append(findings, generated.FieldError{Pointer: p, Code: generated.Identity(c), Message: m})
	}
	maps, e := objects(in)
	if e != nil {
		add("/objects", "DEFINITIONS", "Definitions could not be read.")
		return findings, []string{}
	}
	root := maps[string(in.RootObjectID)]
	if root == nil || root["kind"] != rootKind {
		add("/root_object_id", "ROOT_KIND", "Root must reference a local definition of the selected type.")
	}
	slots := map[string]bool{}
	if rootKind == "WORLD" && root != nil && root["kind"] == "WORLD" {
		for _, raw := range root["duty_assignments"].([]any) {
			assignment := raw.(map[string]any)
			duty := assignment["duty_id"].(string)
			ref := assignment["role_ref"].(map[string]any)
			role := resolveDefinition(ref, maps, resolved)
			if role == nil || role["kind"] != "ROLE" {
				add("/duty_assignments", "DUTY_ROLE", "Duty requires a resolved scoped role definition.")
				continue
			}
			declares := false
			for _, v := range role["duty_ids"].([]any) {
				if v == duty {
					declares = true
				}
			}
			active := roleActive(in, ref, role, resolved)
			if declares && active {
				duties[duty] = true
			}
		}
		for _, a := range root["initial_actor_setups"].([]any) {
			m := a.(map[string]any)
			slot := m["slot_id"].(string)
			if slots[slot] {
				add("/initial_actor_setups", "DUPLICATE_SLOT", "Actor slots must be unique.")
			}
			slots[slot] = true
			ref := m["character_ref"].(map[string]any)
			target := resolveDefinition(ref, maps, resolved)
			if target == nil || target["kind"] != "CHARACTER" {
				add("/initial_actor_setups", "CHARACTER_REFERENCE", "Actor setup requires a resolved character definition.")
			}

		}
		if !slots[root["primary_actor_slot"].(string)] {
			add("/primary_actor_slot", "UNKNOWN_SLOT", "Primary actor must be one of the static slots.")
		}
	}
	expectedKinds := map[string]string{"role_ref": "ROLE", "skill_ref": "SKILL", "character_ref": "CHARACTER", "initial_scene_ref": "SCENE", "scene_ref": "SCENE", "adjacent_scene_refs": "SCENE", "rule_refs": "RULE", "asset_refs": "ASSET", "background_asset_ref": "ASSET", "appearance_asset_refs": "ASSET", "skill_refs": "SKILL", "knowledge_refs": "KNOWLEDGE", "initial_knowledge_refs": "KNOWLEDGE", "theme_ref": "PRESENTATION_THEME"}
	var checkKind func(any, string, string)
	checkKind = func(value any, p, kind string) {
		switch x := value.(type) {
		case map[string]any:
			if x["kind"] == "LOCAL_DRAFT" || x["kind"] == "PINNED_DEFINITION" {
				target := resolveDefinition(x, maps, resolved)
				if target == nil || target["kind"] != kind {
					add(p, "REFERENCE_KIND", "Reference requires a resolved "+kind+" definition.")
				}
			}
		case []any:
			for i, v := range x {
				checkKind(v, fmt.Sprintf("%s/%d", p, i), kind)
			}
		}
	}
	var walk func(any, string)
	walk = func(v any, p string) {
		switch x := v.(type) {
		case map[string]any:
			if x["kind"] == "LOCAL_DRAFT" {
				id, _ := x["object_id"].(string)
				if maps[id] == nil {
					add(p, "MISSING_REFERENCE", "Local reference does not exist.")
				}
			}
			if x["kind"] == "PINNED_DEFINITION" {
				found := false
				for _, lock := range in.DependencyLocks {
					if string(lock.ReleaseID) == x["release_id"] && string(lock.ObjectID) == x["object_id"] && string(lock.Version) == x["version"] && lock.ContentHash == x["content_hash"] {
						found = true
					}
				}
				if !found {
					add(p, "UNPINNED_DEPENDENCY", "External reference requires a matching dependency lock.")
				}
			}
			if mode, ok := x["mode"].(string); ok && (mode == "OVERRIDE" || mode == "INHERIT" || mode == "NONE") {
				if mode == "OVERRIDE" && (x["connection_ref"] != MockConnection || x["model_id"] != MockModel) {
					add(p, "UNREGISTERED_ROUTE", "R0 permits only the registered synthetic local route.")
				}
				if mode == "NONE" && (x["connection_ref"] != nil || x["model_id"] != nil) {
					add(p, "PROGRAM_ROUTE", "Program primitives cannot bind model routes.")
				}
			}
			for k, v := range x {
				if kind := expectedKinds[k]; kind != "" {
					checkKind(v, p+"/"+k, kind)
				}
				walk(v, p+"/"+k)
			}
		case []any:
			for i, v := range x {
				walk(v, fmt.Sprintf("%s/%d", p, i))
			}
		}
	}
	for id, m := range maps {
		p := "/objects/" + id
		walk(m, p)
		rights := m["rights"].(map[string]any)
		uses := map[string]bool{}
		for _, u := range rights["allowed_uses"].([]any) {
			uses[u.(string)] = true
		}
		if !uses["PUBLISH"] || strings.TrimSpace(rights["license"].(string)) == "" {
			add(p+"/rights", "RIGHTS", "Publishing rights and a source license are required.")
		}
		if expiry, _ := rights["expires_at"].(string); expiry != "" {
			add(p+"/rights", "EXPIRING_SOURCE", "R0 fixed release requires nonexpiring synthetic or owned source rights.")
		}
		if slot, ok := m["custodian_slot"].(string); ok && !slots[slot] {
			add(p+"/custodian_slot", "UNKNOWN_SLOT", "Prop custodian must reference a static actor slot.")
		}
		if m["kind"] == "ROLE" {
			for _, key := range []string{"input_schema_id", "output_schema_id"} {
				if !SchemaIDs[m[key].(string)] {
					add(p+"/"+key, "UNKNOWN_SCHEMA", "Only registered offline schemas may be bound.")
				}
			}
			if len(m["tool_ids"].([]any)) > 0 {
				add(p+"/tool_ids", "UNKNOWN_TOOL", "R0 has no registered external tools.")
			}
			if m["implementation_kind"] == "PROGRAM" && m["generation"].(map[string]any)["mode"] != "NONE" {
				add(p+"/generation", "PROGRAM_ROUTE", "Program duties must use generationNONE.")
			}
		}
		if m["kind"] == "SKILL" {
			for _, c := range m["registered_capability_ids"].([]any) {
				if c != "SIGN_CUSTODY" && c != "TEXT" {
					add(p, "UNKNOWN_CAPABILITY", "Unknown capability is blocked.")
				}
			}
			if len(m["tool_ids"].([]any)) > 0 {
				add(p, "UNKNOWN_TOOL", "R0 has no registered external tools.")
			}
		}
		if m["kind"] == "RULE" {
			if m["strength"] == "HARD" && m["primitive"] != "SIGN_CUSTODY" {
				add(p, "UNKNOWN_PRIMITIVE", "Only registered hard primitives may execute.")
			}
			condition := m["condition"].(map[string]any)
			if slot, ok := condition["slot_id"].(string); ok && !slots[slot] {
				add(p+"/condition", "UNKNOWN_SLOT", "Rule targets a missing actor slot.")
			}
		}
	}
	for _, b := range in.ModuleAgentBindings {
		raw, _ := json.Marshal(b)
		v, _ := wire.Parse(raw)
		walk(v, "/module_agent_bindings/"+string(b.BindingID))
		var ref map[string]any
		json.Unmarshal(b.RoleRef, &ref)
		if ref["kind"] == "LOCAL_DRAFT" {
			target := maps[fmt.Sprint(ref["object_id"])]
			if target == nil || target["kind"] != "ROLE" {
				add("/module_agent_bindings/"+string(b.BindingID), "ROLE_REFERENCE", "Module binding requires a role definition.")
			}
		}
	}
	for _, b := range in.SkillBindings {
		raw, _ := json.Marshal(b)
		v, _ := wire.Parse(raw)
		walk(v, "/skill_bindings/"+string(b.BindingID))
		if owner := maps[string(b.RoleObjectID)]; owner == nil || owner["kind"] != "ROLE" {
			add("/skill_bindings", "ROLE_REFERENCE", "Skill owner must be a role definition.")
		}
	}
	covered := []string{}
	for _, d := range Duties {
		if duties[d] {
			covered = append(covered, d)
		} else if rootKind == "WORLD" {
			add("/objects", "MISSING_DUTY", "Mandatory duty is unassigned: "+d)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Pointer+string(findings[i].Code) < findings[j].Pointer+string(findings[j].Code)
	})
	return findings, covered
}
