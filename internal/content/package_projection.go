package content

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
	"loreviva.local/loreviva/internal/wire"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This DTO decodes only bytes already validated against the canonical
// PackagePreviewOptions schema; it is not an independent wire authority.
type packagePreviewOptions struct {
	Stage              string                               `json:"stage"`
	Entrypoint         string                               `json:"entrypoint"`
	Namespace          string                               `json:"namespace"`
	DependencyMappings []generated.PackageDependencyMapping `json:"dependency_mappings"`
	AdditionalLocks    []generated.DependencyLock           `json:"additional_locks"`
	ModelMappings      []generated.PackageModelMapping      `json:"model_mappings"`
	LocalCapabilities  map[string]string                    `json:"local_capabilities"`
	AssetMappings      []generated.PackageAssetMapping      `json:"asset_mappings"`
	Rights             []generated.PackageRightsMapping     `json:"rights"`
	RightsAttested     bool                                 `json:"rights_attested"`
}

// The caller's original portable bytes stay untouched. Only schema-declared
// identity/reference positions are projected; narrative strings are never searched.
func projectPortable(plan wepkg.PortablePlan, options packagePreviewOptions) (generated.DraftInput, []generated.FieldError) {
	fail := func(code string) (generated.DraftInput, []generated.FieldError) {
		return generated.DraftInput{}, []generated.FieldError{{Pointer: "/options", Code: generated.Identity(code), Message: "Local mapping is missing, incompatible or cannot preserve the source."}}
	}
	if !options.RightsAttested {
		return fail("SOURCE_USE_REQUIRED")
	}
	value, err := wire.ParseBounded(plan.Body, 1<<20)
	if err != nil {
		return fail("PORTABLE_BODY_INVALID")
	}
	doc, ok := value.(map[string]any)
	if !ok {
		return fail("PORTABLE_BODY_INVALID")
	}
	ids := map[string]string{}
	for _, id := range plan.IDs {
		ids[id.Domain+"\x00"+id.Owner+"\x00"+id.LogicalID] = id.LocalID
	}
	identity := func(domain, owner, id string) string { return ids[domain+"\x00"+owner+"\x00"+id] }
	rights := map[string][]string{}
	for _, r := range options.Rights {
		if _, yes := rights[r.ObjectID]; yes {
			return fail("DUPLICATE_MAPPING")
		}
		rights[r.ObjectID] = r.AllowedUses
	}
	deps := map[string]generated.DependencyLock{}
	for _, d := range options.DependencyMappings {
		if _, yes := deps[d.Pointer]; yes {
			return fail("DUPLICATE_MAPPING")
		}
		deps[d.Pointer] = d.Target
	}
	models := map[string]generated.GenerationSettings{}
	for _, m := range options.ModelMappings {
		if _, yes := models[m.CapabilityID]; yes {
			return fail("DUPLICATE_MAPPING")
		}
		models[m.CapabilityID] = m.Settings
	}
	assets := map[string]string{}
	for _, a := range options.AssetMappings {
		if _, yes := assets[a.FilePath]; yes {
			return fail("DUPLICATE_MAPPING")
		}
		assets[a.FilePath] = string(a.AssetID)
	}
	usedDeps, usedModels, usedAssets, usedCaps := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	locks := map[string]generated.DependencyLock{}
	addLock := func(d generated.DependencyLock) {
		key := string(d.ReleaseID) + "/" + string(d.ObjectID) + "/" + string(d.Version) + "/" + d.ContentHash
		locks[key] = d
	}
	for _, d := range options.AdditionalLocks {
		key := string(d.ReleaseID) + "/" + string(d.ObjectID) + "/" + string(d.Version) + "/" + d.ContentHash
		if _, yes := locks[key]; yes {
			return fail("DUPLICATE_MAPPING")
		}
		addLock(d)
	}
	for _, r := range plan.References {
		var replacement any
		if r.Kind == "BUNDLE_OBJECT" {
			replacement = map[string]any{"kind": "LOCAL_DRAFT", "object_id": identity("OBJECT", "", r.ObjectID)}
		} else {
			target, yes := deps[r.Pointer]
			if !yes {
				return fail("DEPENDENCY_MAPPING_REQUIRED")
			}
			usedDeps[r.Pointer] = true
			addLock(target)
			replacement = pinReference(target)
		}
		if !replacePreviewPointer(doc, r.Pointer, replacement) {
			return fail("REFERENCE_POSITION")
		}
	}
	generation := func(v any) (any, bool) {
		g, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		mode, _ := g["mode"].(string)
		if mode == "NONE" || mode == "INHERIT" {
			return map[string]any{"mode": mode, "required_capabilities": []string{}, "allowed_external_uses": []string{"NONE"}}, true
		}
		cap, _ := g["capability_id"].(string)
		local, yes := models[cap]
		if !yes || local.Mode != "OVERRIDE" || local.ConnectionRef == nil || local.ModelID == nil || string(*local.ConnectionRef) != MockConnection || string(*local.ModelID) != MockModel || local.BudgetPolicyRef != nil {
			return nil, false
		}
		// Validate every consumed mapping, including disabled and otherwise
		// unreachable roles; route enumeration is not a configuration validator.
		for _, cap := range local.RequiredCapabilities {
			if cap != "TEXT" {
				return nil, false
			}
		}
		for _, use := range local.AllowedExternalUses {
			if use != "NONE" {
				return nil, false
			}
		}
		usedModels[cap] = true
		raw, _ := json.Marshal(local)
		mapped, er := wire.Parse(raw)
		if er != nil {
			return nil, false
		}
		m := mapped.(map[string]any)
		a, hasA := g["parameters"]
		b, hasB := m["parameters"]
		if hasA != hasB {
			return nil, false
		}
		if hasA {
			ha, _ := Hash(a)
			hb, _ := Hash(b)
			if ha != hb {
				return nil, false
			}
		}
		return m, true
	}
	root, _ := doc["root_object_id"].(string)
	objects, ok := doc["objects"].([]any)
	if !ok {
		return fail("PORTABLE_BODY_INVALID")
	}
	for _, item := range objects {
		o, ok := item.(map[string]any)
		if !ok {
			return fail("PORTABLE_BODY_INVALID")
		}
		id, _ := o["object_id"].(string)
		kind, _ := o["kind"].(string)
		uses, yes := rights[id]
		if !yes {
			return fail("SOURCE_USE_REQUIRED")
		}
		hasEdit, hasCopy := false, false
		for _, u := range uses {
			hasEdit = hasEdit || u == "EDIT"
			hasCopy = hasCopy || u == "COPY"
		}
		if !hasEdit || !hasCopy {
			return fail("SOURCE_USE_REQUIRED")
		}
		source, ok := o["rights"].(map[string]any)
		if !ok {
			return fail("SOURCE_REPRESENTATION")
		}
		license, _ := source["license"].(string)
		location, _ := source["source_reference"].(string)
		if strings.TrimSpace(license) == "" || utf8.RuneCountInString(license) > 200 || utf8.RuneCountInString(location) > 500 {
			return fail("SOURCE_REPRESENTATION")
		}
		o["rights"] = map[string]any{"origin_kind": "LICENSED", "license": license, "source_label": "Imported portable content", "source_location": location, "allowed_uses": uses}
		version, _ := o["version"].(string)
		provenance := "wepkg:" + plan.ArchiveSHA256 + ":" + plan.Entrypoint + ":" + id + ":" + version
		refs, ok := o["source_refs"].([]any)
		if !ok || len(refs) >= 30 || utf8.RuneCountInString(provenance) > 500 {
			return fail("SOURCE_REPRESENTATION")
		}
		o["source_refs"] = append(refs, provenance)
		o["version"] = "1"
		o["object_id"] = identity("OBJECT", "", id)
		if g, yes := o["generation"]; yes {
			converted, yes := generation(g)
			if !yes {
				return fail("MODEL_MAPPING_REQUIRED")
			}
			o["generation"] = converted
		}
		if tools, yes := o["tool_capability_ids"]; yes {
			if len(tools.([]any)) != 0 {
				return fail("TOOL_UNAVAILABLE")
			}
			delete(o, "tool_capability_ids")
			o["tool_ids"] = []string{}
		}
		if wanted, yes := o["required_capability_ids"]; yes {
			caps := []string{}
			seen := map[string]bool{}
			for _, v := range wanted.([]any) {
				key := v.(string)
				cap, yes := options.LocalCapabilities[key]
				if !yes || (cap != "TEXT" && cap != "SIGN_CUSTODY") {
					return fail("CAPABILITY_MAPPING_REQUIRED")
				}
				usedCaps[key] = true
				if !seen[cap] {
					caps = append(caps, cap)
					seen[cap] = true
				}
			}
			delete(o, "required_capability_ids")
			o["registered_capability_ids"] = caps
		}
		if kind == "ASSET" {
			path, _ := o["file_path"].(string)
			local, yes := assets[path]
			if !yes {
				return fail("ASSET_MAPPING_REQUIRED")
			}
			usedAssets[path] = true
			delete(o, "file_path")
			o["asset_id"] = local
		}
		if kind == "WORLD" {
			o["primary_actor_slot"] = identity("SLOT", root, fmt.Sprint(o["primary_actor_slot"]))
			for _, v := range o["initial_actor_setups"].([]any) {
				a := v.(map[string]any)
				a["slot_id"] = identity("SLOT", root, fmt.Sprint(a["slot_id"]))
			}
		}
		if kind == "PROP" {
			if slot, yes := o["custodian_slot"].(string); yes {
				o["custodian_slot"] = identity("SLOT", root, slot)
			}
		}
		if kind == "RULE" {
			if c, yes := o["condition"].(map[string]any); yes {
				if slot, yes := c["slot_id"].(string); yes {
					c["slot_id"] = identity("SLOT", root, slot)
				}
			}
		}
	}
	if len(rights) != len(objects) || len(usedDeps) != len(deps) || len(usedAssets) != len(assets) {
		return fail("UNUSED_MAPPING")
	}
	if len(plan.Requirements.Tool) > 0 {
		return fail("TOOL_UNAVAILABLE")
	}
	for _, group := range []struct{ name, domain, owner string }{{"module_agent_bindings", "MODULE_BINDING", "owner_object_id"}, {"skill_bindings", "SKILL_BINDING", "role_object_id"}} {
		rows, ok := doc[group.name].([]any)
		if !ok {
			return fail("PORTABLE_BODY_INVALID")
		}
		for _, v := range rows {
			b := v.(map[string]any)
			b["binding_id"] = identity(group.domain, "", fmt.Sprint(b["binding_id"]))
			b[group.owner] = identity("OBJECT", "", fmt.Sprint(b[group.owner]))
			if g, yes := b["generation"]; yes {
				converted, yes := generation(g)
				if !yes {
					return fail("MODEL_MAPPING_REQUIRED")
				}
				b["generation"] = converted
			}
		}
	}
	// Bindings may themselves consume a model mapping. Check unused models only
	// after the complete schema-declared binding pass.
	for family, list := range [][]wepkg.Capability{plan.Requirements.Model, plan.Requirements.Presentation} {
		for _, c := range list {
			if c.Required {
				if family == 0 && c.CapabilityID == "sign_custody" && usedCaps[c.CapabilityID] && options.LocalCapabilities[c.CapabilityID] == "SIGN_CUSTODY" {
					continue
				}
				if c.CapabilityID != "text" {
					return fail("CAPABILITY_MAPPING_REQUIRED")
				}
				if family == 0 && usedModels[c.CapabilityID] {
					continue
				}
				if options.LocalCapabilities[c.CapabilityID] != "TEXT" {
					return fail("CAPABILITY_MAPPING_REQUIRED")
				}
				usedCaps[c.CapabilityID] = true
			}
		}
	}
	if len(usedModels) != len(models) || len(usedCaps) != len(options.LocalCapabilities) {
		return fail("UNUSED_MAPPING")
	}
	if len(locks) > 100 {
		return fail("DEPENDENCY_LIMIT")
	}
	keys := []string{}
	for k := range locks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	dependencies := []generated.DependencyLock{}
	for _, k := range keys {
		dependencies = append(dependencies, locks[k])
	}
	delete(doc, "body_version")
	doc["root_object_id"] = identity("OBJECT", "", root)
	doc["dependency_locks"] = dependencies
	raw, err := json.Marshal(doc)
	if err != nil {
		return fail("LOCAL_SCHEMA")
	}
	var in generated.DraftInput
	if generated.Decode("DraftInput", raw, &in) != nil {
		return fail("LOCAL_SCHEMA")
	}
	in, err = Prepare(in)
	if err != nil {
		return fail("LOCAL_SCHEMA")
	}
	return in, []generated.FieldError{}
}

func replacePreviewPointer(root any, pointer string, replacement any) bool {
	if !strings.HasPrefix(pointer, "/") {
		return false
	}
	parts := strings.Split(pointer[1:], "/")
	current := root
	for i, part := range parts {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		last := i == len(parts)-1
		switch node := current.(type) {
		case map[string]any:
			v, yes := node[part]
			if !yes {
				return false
			}
			if last {
				node[part] = replacement
				return true
			}
			current = v
		case []any:
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n >= len(node) {
				return false
			}
			if last {
				node[n] = replacement
				return true
			}
			current = node[n]
		default:
			return false
		}
	}
	return false
}
