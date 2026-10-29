package content

import (
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wepkg"
	"net"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type packageProjection struct {
	Body                   []byte
	Files                  map[string]string // portable path -> governed asset ID
	FileRecords            []wepkg.FileRecord
	Requirements           wepkg.Requirements
	License                string
	MinimumRuntimeContract string
}

// The complete resolved export closure determines compatibility. Unrelated
// roots do not hide a folded pinned dependency using the registered TEXT pair.
func minimumPackageRuntime(nodes []exportNode) string {
	for _, node := range nodes {
		if node.object["kind"] == "SKILL" && (node.object["input_schema_id"] == "GenerationMaterial" || node.object["output_schema_id"] == "DispatchAuthorization") {
			return "1.11.0"
		}
	}
	return "1.9.0"
}

type exportNode struct {
	context, id string
	object      map[string]any
}

func allowedShare(rights generated.SourceRights) bool {
	uses := map[string]bool{}
	for _, use := range rights.AllowedUses {
		uses[use] = true
	}
	return uses["COPY"] && uses["REDISTRIBUTE"] && strings.TrimSpace(rights.License) != "" && utf8.RuneCountInString(rights.License) <= 128 && rights.ExpiresAt == nil
}
func publicAttribution(value string) bool {
	if len(value) > 500 || strings.ContainsAny(value, "\r\n\\") || strings.HasPrefix(value, "/") || strings.Contains(value, "://") && strings.Contains(value, "@") {
		return false
	}
	if len(value) > 1 && value[1] == ':' {
		return false
	}
	u, e := url.Parse(value)
	if e != nil {
		return false
	}
	if u.Scheme == "file" || u.User != nil {
		return false
	}
	if u.Host != "" || strings.Contains(value, "://") {
		if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || !strings.Contains(host, ".") {
			return false
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
			return false
		}
	}
	return true
}

// Only generated schema positions identify references. Narrative strings and
// arbitrary JSON examples are deliberately outside this traversal.
func exportRefs(node map[string]any, domain, kind string, visit func(map[string]any) (map[string]any, error)) error {
	meta, e := generated.PortableMetadata()
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	var walk func(any, []string, string) error
	walk = func(value any, parts []string, pointer string) error {
		if len(parts) == 0 {
			return nil
		}
		switch object := value.(type) {
		case map[string]any:
			key := parts[0]
			child, ok := object[key]
			if !ok {
				return nil
			}
			if len(parts) == 1 {
				ref, ok := child.(map[string]any)
				if !ok {
					return ErrBlocked
				}
				if seen[pointer+"/"+key] {
					return nil
				}
				seen[pointer+"/"+key] = true
				converted, e := visit(ref)
				if e != nil {
					return e
				}
				object[key] = converted
				return nil
			}
			return walk(child, parts[1:], pointer+"/"+key)
		case []any:
			if parts[0] != "*" {
				return ErrBlocked
			}
			for i, child := range object {
				p := pointer + "/" + strconv.Itoa(i)
				if len(parts) == 1 {
					ref, ok := child.(map[string]any)
					if !ok {
						return ErrBlocked
					}
					if seen[p] {
						continue
					}
					seen[p] = true
					converted, e := visit(ref)
					if e != nil {
						return e
					}
					object[i] = converted
				} else if e := walk(child, parts[1:], p); e != nil {
					return e
				}
			}
		}
		return nil
	}
	for _, ref := range meta.ReferencePaths {
		if ref.OwnerDomain == domain && (domain != "OBJECT" || ref.ObjectKind == kind) {
			if e := walk(node, ref.Path, ""); e != nil {
				return e
			}
		}
	}
	return nil
}

func projectPackageExport(root generated.Release, catalog map[string]generated.Release, assets map[string]generated.FileRecord, owner string) (packageProjection, error) {
	return projectPackageExportWithAssets(root, catalog, func(id string) (generated.FileRecord, error) {
		f, ok := assets[id]
		if !ok {
			return f, ErrBlocked
		}
		return f, nil
	}, owner)
}
func projectPackageExportWithAssets(root generated.Release, catalog map[string]generated.Release, asset func(string) (generated.FileRecord, error), owner string) (packageProjection, error) {
	out := packageProjection{Files: map[string]string{}, FileRecords: []wepkg.FileRecord{}, Requirements: wepkg.Requirements{Model: []wepkg.Capability{}, Tool: []wepkg.ToolRequirement{}, Presentation: []wepkg.Capability{}, TestedConfigurations: []wepkg.TestedConfiguration{}}}
	local := map[string]map[string]map[string]any{}
	for context, release := range catalog {
		values, e := objects(release.Input)
		if e != nil {
			return out, e
		}
		local[context] = values
	}
	rootContext := string(root.ReleaseID)
	ids := map[string]string{}
	nodes := []exportNode{}
	nodeKey := func(context, id string) string { return context + "\x00" + id }
	add := func(context, id string) (string, error) {
		key := nodeKey(context, id)
		if ids[key] != "" {
			return ids[key], nil
		}
		object := local[context][id]
		if object == nil || len(nodes) >= 200 {
			return "", ErrBlocked
		}
		logical := fmt.Sprintf("o%04d", len(nodes)+1)
		ids[key] = logical
		nodes = append(nodes, exportNode{context, id, object})
		return logical, nil
	}
	for _, raw := range root.Input.Objects {
		var node map[string]any
		json.Unmarshal(raw, &node)
		if _, e := add(rootContext, fmt.Sprint(node["object_id"])); e != nil {
			return out, e
		}
	}
	resolve := func(context string, ref map[string]any) (string, error) {
		switch ref["kind"] {
		case "LOCAL_DRAFT":
			return add(context, fmt.Sprint(ref["object_id"]))
		case "PINNED_DEFINITION":
			releaseID, id := fmt.Sprint(ref["release_id"]), fmt.Sprint(ref["object_id"])
			obj := local[releaseID][id]
			if obj == nil {
				return "", ErrBlocked
			}
			h, e := Hash(obj)
			if e != nil || h != ref["content_hash"] || obj["version"] != ref["version"] {
				return "", ErrBlocked
			}
			return add(releaseID, id)
		default:
			return "", ErrBlocked
		}
	}
	// Each dependency binding is included only when its declared owner is in the
	// exported closure; its own references can extend that exact closure.
	type binding struct {
		context, domain, owner string
		value                  map[string]any
	}
	bindings := []binding{}
	seenBindings := map[string]bool{}
	for index := 0; index < len(nodes); index++ {
		node := nodes[index]
		copyBytes, _ := json.Marshal(node.object)
		var copy map[string]any
		json.Unmarshal(copyBytes, &copy)
		if e := exportRefs(copy, "OBJECT", fmt.Sprint(copy["kind"]), func(ref map[string]any) (map[string]any, error) { _, e := resolve(node.context, ref); return ref, e }); e != nil {
			return out, e
		}
		release := catalog[node.context]
		groups := []struct {
			domain, owner string
			rows          any
		}{{"MODULE_BINDING", "owner_object_id", release.Input.ModuleAgentBindings}, {"SKILL_BINDING", "role_object_id", release.Input.SkillBindings}}
		for _, group := range groups {
			raw, _ := json.Marshal(group.rows)
			var rows []map[string]any
			json.Unmarshal(raw, &rows)
			for _, row := range rows {
				if row[group.owner] != node.id {
					continue
				}
				key := node.context + "\x00" + group.domain + "\x00" + fmt.Sprint(row["binding_id"])
				if seenBindings[key] {
					continue
				}
				seenBindings[key] = true
				if e := exportRefs(row, group.domain, "", func(ref map[string]any) (map[string]any, error) { _, e := resolve(node.context, ref); return ref, e }); e != nil {
					return out, e
				}
				bindings = append(bindings, binding{node.context, group.domain, group.owner, row})
			}
		}
	}
	slots := map[string]string{}
	for _, node := range nodes {
		if node.object["kind"] == "WORLD" {
			for _, row := range node.object["initial_actor_setups"].([]any) {
				id := fmt.Sprint(row.(map[string]any)["slot_id"])
				key := nodeKey(node.context, id)
				if slots[key] == "" {
					slots[key] = fmt.Sprintf("s%04d", len(slots)+1)
				}
			}
		}
	}
	modelNeeded := false
	nativeCaps := map[string]bool{}
	generation := func(value any) (any, error) {
		g, ok := value.(map[string]any)
		if !ok {
			return nil, ErrBlocked
		}
		mode, _ := g["mode"].(string)
		if mode == "NONE" || mode == "INHERIT" {
			return map[string]any{"mode": mode}, nil
		}
		if mode != "OVERRIDE" {
			return nil, ErrBlocked
		}
		modelNeeded = true
		m := map[string]any{"mode": "REQUIREMENT", "capability_id": "text"}
		if parameters, yes := g["parameters"]; yes {
			m["parameters"] = parameters
		}
		return m, nil
	}
	objectsOut := []any{}
	for _, node := range nodes {
		raw, _ := json.Marshal(node.object)
		var object map[string]any
		json.Unmarshal(raw, &object)
		kind := fmt.Sprint(object["kind"])
		if node.context != rootContext && object["audience_policy"] == "PRODUCT_EDITORS" && string(catalog[node.context].PublishedBySubjectID) != owner {
			return out, ErrBlocked
		}
		var rights generated.SourceRights
		if json.Unmarshal(mustMarshal(object["rights"]), &rights) != nil || !allowedShare(rights) {
			return out, ErrBlocked
		}
		source := rights.SourceLabel
		if rights.SourceLocation != nil {
			source = *rights.SourceLocation
		}
		if !publicAttribution(source) {
			return out, ErrBlocked
		}
		for _, value := range object["source_refs"].([]any) {
			if !publicAttribution(fmt.Sprint(value)) {
				return out, ErrBlocked
			}
		}
		object["rights"] = wepkg.Rights{License: rights.License, SourceReference: source, RedistributionAllowed: true, AllowedUses: []string{"WORLD_CONTENT"}}
		object["object_id"] = ids[nodeKey(node.context, node.id)]
		if e := exportRefs(object, "OBJECT", kind, func(ref map[string]any) (map[string]any, error) {
			id, e := resolve(node.context, ref)
			return map[string]any{"kind": "BUNDLE_OBJECT", "object_id": id}, e
		}); e != nil {
			return out, e
		}
		if value, ok := object["generation"]; ok {
			v, e := generation(value)
			if e != nil {
				return out, e
			}
			object["generation"] = v
		}
		if tools, ok := object["tool_ids"].([]any); ok {
			if len(tools) > 0 {
				return out, ErrBlocked
			}
			delete(object, "tool_ids")
			object["tool_capability_ids"] = []string{}
		}
		if capabilities, ok := object["registered_capability_ids"].([]any); ok {
			converted := []string{}
			for _, value := range capabilities {
				switch value {
				case "TEXT":
					converted = append(converted, "text")
					nativeCaps["text"] = true
				case "SIGN_CUSTODY":
					converted = append(converted, "sign_custody")
					nativeCaps["sign_custody"] = true
				default:
					return out, ErrBlocked
				}
			}
			delete(object, "registered_capability_ids")
			object["required_capability_ids"] = converted
		}
		slot := func(value any) (string, error) {
			mapped := slots[nodeKey(node.context, fmt.Sprint(value))]
			if mapped == "" {
				return "", ErrBlocked
			}
			return mapped, nil
		}
		if kind == "WORLD" {
			value, e := slot(object["primary_actor_slot"])
			if e != nil {
				return out, e
			}
			object["primary_actor_slot"] = value
			for _, row := range object["initial_actor_setups"].([]any) {
				actor := row.(map[string]any)
				value, e := slot(actor["slot_id"])
				if e != nil {
					return out, e
				}
				actor["slot_id"] = value
			}
		}
		if kind == "PROP" {
			if value, ok := object["custodian_slot"]; ok {
				mapped, e := slot(value)
				if e != nil {
					return out, e
				}
				object["custodian_slot"] = mapped
			}
		}
		if kind == "RULE" {
			if condition, ok := object["condition"].(map[string]any); ok {
				if value, ok := condition["slot_id"]; ok {
					mapped, e := slot(value)
					if e != nil {
						return out, e
					}
					condition["slot_id"] = mapped
				}
			}
		}
		if kind == "ASSET" {
			id := fmt.Sprint(object["asset_id"])
			file, err := asset(id)
			if err != nil || file.State != "READY" || !allowedShare(file.Rights) || file.Sha256 != object["asset_sha256"] || string(file.SizeBytes) != object["size_bytes"] || file.MediaType != object["media_type"] || (file.AudiencePolicy == "PRODUCT_EDITORS" && string(file.OwnerSubjectID) != owner) {
				return out, ErrBlocked
			}
			fileSource := file.Rights.SourceLabel
			if file.Rights.SourceLocation != nil {
				fileSource = *file.Rights.SourceLocation
			}
			if !publicAttribution(fileSource) {
				return out, ErrBlocked
			}
			ext := map[string]string{"text/plain": ".txt", "text/markdown": ".md", "application/json": ".json", "application/pdf": ".pdf", "application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx"}[file.MediaType]
			if ext == "" {
				return out, ErrBlocked
			}
			name := path.Join("files", fmt.Sprint(object["object_id"])+ext)
			delete(object, "asset_id")
			object["file_path"] = name
			out.Files[name] = id
			out.FileRecords = append(out.FileRecords, wepkg.FileRecord{Path: name, MediaType: file.MediaType, Rights: wepkg.Rights{License: file.Rights.License, SourceReference: fileSource, RedistributionAllowed: true, AllowedUses: []string{"WORLD_CONTENT"}}})
		}
		if node.context == rootContext && node.id == string(root.Input.RootObjectID) {
			out.License = rights.License
		}
		objectsOut = append(objectsOut, object)
	}
	modules, skills := []any{}, []any{}
	for index, row := range bindings {
		v := row.value
		v["binding_id"] = fmt.Sprintf("b%04d", index+1)
		v[row.owner] = ids[nodeKey(row.context, fmt.Sprint(v[row.owner]))]
		if e := exportRefs(v, row.domain, "", func(ref map[string]any) (map[string]any, error) {
			id, e := resolve(row.context, ref)
			return map[string]any{"kind": "BUNDLE_OBJECT", "object_id": id}, e
		}); e != nil {
			return out, e
		}
		if value, ok := v["generation"]; ok {
			converted, e := generation(value)
			if e != nil {
				return out, e
			}
			v["generation"] = converted
		}
		if row.domain == "MODULE_BINDING" {
			modules = append(modules, v)
		} else {
			skills = append(skills, v)
		}
	}
	if modelNeeded {
		nativeCaps["text"] = true
	}
	for _, id := range []string{"sign_custody", "text"} {
		if nativeCaps[id] {
			out.Requirements.Model = append(out.Requirements.Model, wepkg.Capability{CapabilityID: id, Required: true})
		}
	}
	out.MinimumRuntimeContract = minimumPackageRuntime(nodes)
	document := map[string]any{"body_version": "1.0", "name": root.Input.Name, "description": root.Input.Description, "tags": root.Input.Tags, "root_object_id": ids[nodeKey(rootContext, string(root.Input.RootObjectID))], "objects": objectsOut, "module_agent_bindings": modules, "skill_bindings": skills}
	out.Body = mustMarshal(document)
	if generated.ValidatePortableBody(packageRootKind(root), out.Body) != nil {
		return out, ErrBlocked
	}
	sort.Slice(out.FileRecords, func(i, j int) bool { return out.FileRecords[i].Path < out.FileRecords[j].Path })
	return out, nil
}
func mustMarshal(value any) []byte { raw, _ := json.Marshal(value); return raw }

func packageRootKind(release generated.Release) string {
	for _, raw := range release.Input.Objects {
		var object struct {
			ID   string `json:"object_id"`
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &object) != nil {
			return ""
		}
		if object.ID == string(release.Input.RootObjectID) {
			return object.Kind
		}
	}
	return ""
}
