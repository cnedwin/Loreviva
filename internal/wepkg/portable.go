package wepkg

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
)

// A portable bundle describes content and unresolved requirements. It never
// carries installation authority, a model connection, or a local revision.
type portableBundle struct {
	doc        map[string]any
	objects    map[string]map[string]any
	ids        []PortableID
	references []PortableReference
	files      []PortableFile
	root       string
}

type portableDiagnostics struct {
	rows           []Finding
	fields, arrays map[string]bool
	limit          int
}

func newPortableDiagnostics(meta generated.PortableSchemaMetadata) *portableDiagnostics {
	d := &portableDiagnostics{limit: meta.DiagnosticLimit, fields: map[string]bool{}, arrays: map[string]bool{}}
	for _, s := range meta.DiagnosticFields {
		d.fields[s] = true
	}
	for _, s := range meta.DiagnosticArrays {
		d.arrays[s] = true
	}
	return d
}
func (d *portableDiagnostics) add(code, path, object, detail string) {
	f := Finding{Severity: Error, Code: code, Path: path, Object: object, Detail: detail}
	if utf8.RuneCountInString(f.Path) > 240 {
		f.Path = ""
	}
	if utf8.RuneCountInString(f.Object) > 128 {
		f.Object = "entries"
	}
	key := func(a Finding) string { return a.Code + "\x00" + a.Path + "\x00" + a.Object }
	k := key(f)
	i := sort.Search(len(d.rows), func(i int) bool { return key(d.rows[i]) >= k })
	if i < len(d.rows) && key(d.rows[i]) == k {
		return
	}
	if i >= d.limit {
		return
	}
	d.rows = append(d.rows, Finding{})
	copy(d.rows[i+1:], d.rows[i:])
	d.rows[i] = f
	if len(d.rows) > d.limit {
		d.rows = d.rows[:d.limit]
	}
}
func (d *portableDiagnostics) safe(prefix string, parts []string) string {
	out := prefix
	prev := ""
	for _, p := range parts {
		numeric := diagnosticIndexPattern.MatchString(p)
		if numeric {
			if !d.arrays[prev] {
				break
			}
		} else if !d.fields[p] {
			break
		}
		if utf8.RuneCountInString(out+"."+p) > 128 {
			break
		}
		out += "." + p
		prev = p
	}
	return out
}
func (d *portableDiagnostics) schema(err error, entry Entry, index int) {
	prefix := fmt.Sprintf("entries.%d", index)
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		d.add(CodePortableBodySchema, entry.Path, prefix, "body does not satisfy the portable definition")
		return
	}
	var visit func(*jsonschema.ValidationError)
	visit = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			d.add(CodePortableBodySchema, entry.Path, d.safe(prefix, v.InstanceLocation), "body does not satisfy the portable definition")
			return
		}
		for _, c := range v.Causes {
			visit(c)
		}
	}
	visit(validation)
}
func pmap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func pstr(v any) string         { s, _ := v.(string); return s }
func parray(v any) []any        { a, _ := v.([]any); return a }
func stringSet(values []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range values {
		m[s] = true
	}
	return m
}
func portableSourceAbsolute(value string) bool {
	s := strings.TrimSpace(value)
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\\`) || strings.HasPrefix(strings.ToLower(s), "file:") || (len(s) >= 3 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) && s[1] == ':' && (s[2] == '/' || s[2] == '\\'))
}
func portableSourceCheck(d *portableDiagnostics, m Manifest) {
	for i, s := range m.Provenance.SourceReferences {
		if portableSourceAbsolute(s) {
			d.add(CodePortableReferenceInvalid, "", fmt.Sprintf("manifest.provenance.source_references.%d", i), "source location is not portable")
		}
	}
	for i, f := range m.Files {
		if portableSourceAbsolute(f.Rights.SourceReference) {
			d.add(CodePortableReferenceInvalid, "", fmt.Sprintf("manifest.files.%d.rights.source_reference", i), "source location is not portable")
		}
	}
}

// Generated positions are paths in the schema, not guessed objects in arbitrary
// authored JSON. Array indexes are supplied only while walking those positions.
func walkPortablePosition(value any, parts []string, pointer string, visit func(map[string]any, string)) {
	if len(parts) == 0 {
		if m := pmap(value); m != nil {
			visit(m, pointer)
		}
		return
	}
	if parts[0] == "*" {
		for i, v := range parray(value) {
			walkPortablePosition(v, parts[1:], pointer+"/"+strconv.Itoa(i), visit)
		}
		return
	}
	m := pmap(value)
	v, ok := m[parts[0]]
	if ok {
		walkPortablePosition(v, parts[1:], pointer+"/"+parts[0], visit)
	}
}
func portableBundleCheck(body []byte, m Manifest, entry Entry, index int, meta generated.PortableSchemaMetadata, d *portableDiagnostics) portableBundle {
	value, _ := wire.ParseBounded(body, meta.BodyBytes)
	doc := pmap(value)
	b := portableBundle{doc: doc, objects: map[string]map[string]any{}, ids: []PortableID{}, references: []PortableReference{}, files: []PortableFile{}}
	prefix := fmt.Sprintf("entries.%d", index)
	add := func(code, where, detail string) { d.add(code, entry.Path, prefix+where, detail) }
	seen := map[string]bool{}
	identify := func(domain, owner, id, where string) {
		key := domain + "\x00" + owner + "\x00" + id
		if seen[key] {
			add(CodePortableIdentityDuplicate, where, "duplicate logical identity")
			return
		}
		seen[key] = true
		b.ids = append(b.ids, PortableID{Domain: domain, Owner: owner, LogicalID: id})
	}
	objs := parray(doc["objects"])
	for i, v := range objs {
		o := pmap(v)
		id := pstr(o["object_id"])
		where := fmt.Sprintf(".objects.%d", i)
		identify("OBJECT", "", id, where+".object_id")
		b.objects[id] = o
	}
	b.root = pstr(doc["root_object_id"])
	root := b.objects[b.root]
	if root == nil || pstr(root["kind"]) != entry.Kind {
		add(CodePortableReferenceKind, ".root_object_id", "root object is missing or has the wrong kind")
	}
	world := ""
	if pstr(root["kind"]) == "WORLD" {
		world = b.root
	}
	for i, v := range objs {
		o := pmap(v)
		where := fmt.Sprintf(".objects.%d", i)
		if pstr(o["kind"]) == "WORLD" && pstr(o["object_id"]) != b.root {
			add(CodePortableReferenceInvalid, where, "only the root may be a world")
		}
	}
	slots := map[string]bool{}
	if world != "" {
		for i, v := range parray(root["initial_actor_setups"]) {
			s := pstr(pmap(v)["slot_id"])
			identify("SLOT", world, s, fmt.Sprintf(".objects.initial_actor_setups.%d", i))
			slots[s] = true
		}
	}
	models, tools, allCaps := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range m.Requirements.Model {
		models[c.CapabilityID] = true
		allCaps[c.CapabilityID] = true
	}
	for _, c := range m.Requirements.Tool {
		tools[c.ToolCapabilityID] = true
		allCaps[c.ToolCapabilityID] = true
	}
	for _, c := range m.Requirements.Presentation {
		allCaps[c.CapabilityID] = true
	}
	generation := func(g map[string]any, where string) {
		if pstr(g["mode"]) == "REQUIREMENT" && !models[pstr(g["capability_id"])] {
			add(CodePortableReferenceInvalid, where, "model requirement is not declared")
		}
	}
	source := func(v any, where string) {
		if portableSourceAbsolute(pstr(v)) {
			add(CodePortableReferenceInvalid, where, "source location is not portable")
		}
	}
	fileRecords := map[string]FileRecord{}
	for _, f := range m.Files {
		fileRecords[f.Path] = f
	}
	usedFiles := map[string]bool{}
	for i, v := range objs {
		o := pmap(v)
		where := fmt.Sprintf(".objects.%d", i)
		kind := pstr(o["kind"])
		source(pmap(o["rights"])["source_reference"], where+".rights.source_reference")
		for j, s := range parray(o["source_refs"]) {
			source(s, where+".source_refs."+strconv.Itoa(j))
		}
		if kind == "KNOWLEDGE" {
			source(o["source_location"], where+".source_location")
		}
		generation(pmap(o["generation"]), where+".generation")
		if kind == "ROLE" && pstr(o["implementation_kind"]) == "PROGRAM" && pstr(pmap(o["generation"])["mode"]) != "NONE" {
			add(CodePortableBodySchema, where+".generation", "program roles cannot request model generation")
		}
		for j, c := range parray(o["tool_capability_ids"]) {
			if !tools[pstr(c)] {
				add(CodePortableReferenceInvalid, where+".tool_capability_ids."+strconv.Itoa(j), "tool requirement is not declared")
			}
		}
		for j, c := range parray(o["required_capability_ids"]) {
			if !allCaps[pstr(c)] {
				add(CodePortableReferenceInvalid, where+".required_capability_ids."+strconv.Itoa(j), "capability requirement is not declared")
			}
		}
		checkSlot := func(v any, loc string) {
			s, ok := v.(string)
			if ok && (world == "" || !slots[s]) {
				add(CodePortableSlotInvalid, loc, "slot does not belong to this world")
			}
		}
		if kind == "WORLD" {
			checkSlot(o["primary_actor_slot"], where+".primary_actor_slot")
		}
		if kind == "PROP" {
			checkSlot(o["custodian_slot"], where+".custodian_slot")
		}
		if kind == "RULE" {
			checkSlot(pmap(o["condition"])["slot_id"], where+".condition.slot_id")
		}
		if kind == "ASSET" {
			path := pstr(o["file_path"])
			f, ok := fileRecords[path]
			size, exact := new(big.Int).SetString(pstr(o["size_bytes"]), 10)
			equalSize := exact && size.IsInt64() && size.Int64() == f.SizeBytes
			if !ok || path == "manifest.json" || pstr(o["asset_sha256"]) != f.SHA256 || pstr(o["media_type"]) != f.MediaType || f.MediaType == "application/json" || !knownMedia[f.MediaType] || !equalSize {
				add(CodePortableFileInvalid, where+".file_path", "asset does not match a declared supported file")
			} else if !usedFiles[path] {
				usedFiles[path] = true
				b.files = append(b.files, PortableFile{Path: f.Path, SHA256: f.SHA256, MediaType: f.MediaType, SizeBytes: f.SizeBytes, Rights: f.Rights})
			}
		}
	}
	for _, group := range []struct{ name, domain, ownerField string }{{"module_agent_bindings", "MODULE_BINDING", "owner_object_id"}, {"skill_bindings", "SKILL_BINDING", "role_object_id"}} {
		for i, v := range parray(doc[group.name]) {
			binding := pmap(v)
			where := "." + group.name + "." + strconv.Itoa(i)
			identify(group.domain, "", pstr(binding["binding_id"]), where+".binding_id")
			owner := b.objects[pstr(binding[group.ownerField])]
			if owner == nil || (group.domain == "SKILL_BINDING" && pstr(owner["kind"]) != "ROLE") {
				add(CodePortableReferenceKind, where+"."+group.ownerField, "binding owner is missing or has the wrong kind")
			}
			generation(pmap(binding["generation"]), where+".generation")
		}
	}
	for _, position := range meta.ReferencePaths {
		if position.OwnerDomain == "OBJECT" {
			for i, v := range objs {
				o := pmap(v)
				if pstr(o["kind"]) != position.ObjectKind {
					continue
				}
				owner := pstr(o["object_id"])
				walkPortablePosition(o, position.Path, "/objects/"+strconv.Itoa(i), func(ref map[string]any, pointer string) {
					b.references = append(b.references, portableReference(ref, pointer, owner, position.ExpectedKinds))
				})
			}
		} else {
			group := "module_agent_bindings"
			if position.OwnerDomain == "SKILL_BINDING" {
				group = "skill_bindings"
			}
			for i, v := range parray(doc[group]) {
				binding := pmap(v)
				owner := pstr(binding["binding_id"])
				walkPortablePosition(binding, position.Path, "/"+group+"/"+strconv.Itoa(i), func(ref map[string]any, pointer string) {
					b.references = append(b.references, portableReference(ref, pointer, owner, position.ExpectedKinds))
				})
			}
		}
	}
	sort.Slice(b.files, func(i, j int) bool { return b.files[i].Path < b.files[j].Path })
	return b
}
func portableReference(r map[string]any, pointer, owner string, kinds []string) PortableReference {
	return PortableReference{Pointer: pointer, OwnerObjectID: owner, Kind: pstr(r["kind"]), ObjectID: pstr(r["object_id"]), LogicalID: pstr(r["logical_id"]), PackageID: pstr(r["package_id"]), PackageVersion: pstr(r["package_version"]), ContentSHA256: pstr(r["content_sha256"]), ExpectedKinds: append([]string{}, kinds...)}
}
func portableReferenceCheck(b portableBundle, m Manifest, entry Entry, index int, d *portableDiagnostics, edges map[[2]string]bool, edgeLimit int, overflow *bool) {
	entries := map[string]Entry{}
	counts := map[string]int{}
	for _, e := range m.Entries {
		entries[e.LogicalID] = e
		counts[e.LogicalID]++
	}
	for _, dep := range m.Dependencies {
		for _, id := range dep.LogicalExports {
			counts[id]++
		}
	}
	found := map[string]bool{}
	prefix := fmt.Sprintf("entries.%d", index)
	for _, r := range b.references {
		targetKind := ""
		valid := true
		switch r.Kind {
		case "BUNDLE_OBJECT":
			target := b.objects[r.ObjectID]
			valid = target != nil
			targetKind = pstr(target["kind"])
		case "PACKAGE_ENTRY":
			found[r.LogicalID] = true
			target, ok := entries[r.LogicalID]
			valid = ok && rootKinds[target.Kind] && r.LogicalID != entry.LogicalID && counts[r.LogicalID] == 1
			targetKind = target.Kind
			if valid && !(*overflow) {
				edge := [2]string{entry.LogicalID, r.LogicalID}
				edges[edge] = true
				if len(edges) > edgeLimit {
					*overflow = true
				}
			}
		case "DEPENDENCY_EXPORT":
			found[r.LogicalID] = true
			valid = false
			for _, dep := range m.Dependencies {
				if dep.PackageID == r.PackageID && dep.PackageVersion == r.PackageVersion && dep.ContentSHA256 == r.ContentSHA256 {
					for _, id := range dep.LogicalExports {
						if id == r.LogicalID {
							valid = true
						}
					}
				}
			}
			valid = valid && counts[r.LogicalID] == 1
		default:
			valid = false
		}
		parts := strings.Split(strings.TrimPrefix(r.Pointer, "/"), "/")
		where := d.safe(prefix, parts)
		if !valid {
			d.add(CodePortableReferenceInvalid, entry.Path, where, "logical reference is missing, ambiguous, or invalid")
		} else if r.Kind != "DEPENDENCY_EXPORT" && len(r.ExpectedKinds) > 0 && !stringSet(r.ExpectedKinds)[targetKind] {
			d.add(CodePortableReferenceKind, entry.Path, where, "logical target has the wrong kind")
		}
	}
	declared := stringSet(entry.References)
	mismatch := len(found) != len(declared)
	for id := range found {
		if !declared[id] {
			mismatch = true
		}
	}
	if mismatch {
		d.add(CodePortableReferenceMismatch, entry.Path, prefix+".references", "manifest references do not match the body")
	}
}
func portableAssemblyInvalid(edges map[[2]string]bool, limit int) bool {
	next := map[string][]string{}
	incoming := map[string]int{}
	depth := map[string]int{}
	for e := range edges {
		next[e[0]] = append(next[e[0]], e[1])
		incoming[e[1]]++
		if _, ok := incoming[e[0]]; !ok {
			incoming[e[0]] = 0
		}
	}
	queue := []string{}
	for id, n := range incoming {
		depth[id] = 1
		if n == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for head := 0; head < len(queue); head++ {
		id := queue[head]
		visited++
		if depth[id] > limit {
			return true
		}
		for _, dst := range next[id] {
			if depth[dst] < depth[id]+1 {
				depth[dst] = depth[id] + 1
			}
			incoming[dst]--
			if incoming[dst] == 0 {
				queue = append(queue, dst)
			}
		}
	}
	return visited != len(incoming)
}

// CheckPortableBodies reads one bounded body at a time. Metadata and a bounded
// assembly graph survive the iteration; expanded documents do not accumulate.
func CheckPortableBodies(raw []byte, m Manifest, lim Limits) []Finding {
	meta, err := generated.PortableMetadata()
	if err != nil {
		return []Finding{{Severity: Error, Code: CodePortableBodySchema, Detail: "portable schema unavailable"}}
	}
	d := newPortableDiagnostics(meta)
	portableSourceCheck(d, m)
	// Logical entry addresses and file declarations must each resolve once.
	entryIDs := map[string]bool{}
	filePaths := map[string]bool{}
	for i, e := range m.Entries {
		if entryIDs[e.LogicalID] {
			d.add(CodePortableIdentityDuplicate, e.Path, fmt.Sprintf("entries.%d", i), "duplicate package entry identity")
		}
		entryIDs[e.LogicalID] = true
	}
	for i, f := range m.Files {
		if filePaths[f.Path] {
			d.add(CodePortableFileInvalid, f.Path, fmt.Sprintf("manifest.files.%d", i), "duplicate file declaration")
		}
		filePaths[f.Path] = true
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return d.rows
	}
	members := map[string]*zip.File{}
	for _, f := range zr.File {
		members[f.Name] = f
	}
	files := map[string]FileRecord{}
	for _, f := range m.Files {
		files[f.Path] = f
	}
	edges := map[[2]string]bool{}
	overflow := false
	for i, entry := range m.Entries {
		prefix := fmt.Sprintf("entries.%d", i)
		if !rootKinds[entry.Kind] && entry.Kind != "RULES" {
			d.add(CodePortableBodyUnsupported, entry.Path, prefix, "entry kind has no portable body support")
			continue
		}
		if files[entry.Path].MediaType != "application/json" {
			d.add(CodePortableBodySchema, entry.Path, prefix, "portable body requires application/json")
			continue
		}
		f := members[entry.Path]
		if f == nil {
			continue
		}
		rc, e := f.Open()
		if e != nil {
			continue
		}
		body, e := io.ReadAll(io.LimitReader(rc, meta.BodyBytes+1))
		rc.Close()
		if e != nil {
			continue
		}
		if int64(len(body)) > meta.BodyBytes {
			d.add(CodePortableBodyLimit, entry.Path, prefix, "portable body exceeds its fixed byte limit")
			continue
		}
		if _, e = wire.ParseBounded(body, meta.BodyBytes); e != nil {
			d.add(CodeEntryBodyInvalid, entry.Path, prefix, "entry body is not strict bounded JSON")
			continue
		}
		if e = generated.ValidatePortableBody(entry.Kind, body); e != nil {
			d.schema(e, entry, i)
			continue
		}
		if entry.Kind == "RULES" {
			if len(entry.References) > 0 {
				d.add(CodePortableReferenceMismatch, entry.Path, prefix+".references", "rules entries cannot declare references")
			}
			for _, f := range CheckRulePrimitives(m, body) {
				d.add(f.Code, entry.Path, prefix, f.Detail)
			}
			continue
		}
		b := portableBundleCheck(body, m, entry, i, meta, d)
		portableReferenceCheck(b, m, entry, i, d, edges, meta.AssemblyEdgeLimit, &overflow)
	}
	if overflow || portableAssemblyInvalid(edges, meta.AssemblyVertexLimit) {
		d.add(CodePortableReferenceCycle, "", "entries", "assembly graph is cyclic or exceeds bounds")
	}
	return d.rows
}
