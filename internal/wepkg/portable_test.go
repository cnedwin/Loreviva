package wepkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"os"
	"sort"
	"strings"
	"testing"
)

// Exercise the existing public preflight path before adding portable validators.
func TestPortableBodyAdmissionRejectsReadableWrongDefinition(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"readable_wrong_definition", []byte(`{"name":"readable but not a portable definition"}`), "PORTABLE_BODY_SCHEMA"},
		{"duplicate_body_key", []byte(`{"body_version":"1.0","body_version":"1.0"}`), CodeEntryBodyInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := schemaExample(t)
			sum := sha256.Sum256(tc.body)
			digest := hex.EncodeToString(sum[:])
			m.Files[0].SHA256 = digest
			m.Files[0].SizeBytes = int64(len(tc.body))
			m.Entries[0].SHA256 = digest
			content, err := ContentHash(m)
			if err != nil {
				t.Fatal(err)
			}
			m.ContentSHA256 = content
			encoded := mustJSON(t, m)
			if _, findings := ParseManifest(encoded, DefaultLimits); len(findings) != 0 {
				t.Fatalf("fixture manifest invalid: %v", codes(findings))
			}
			raw := build(t, member{name: "manifest.json", body: encoded}, member{name: m.Entries[0].Path, body: tc.body})
			idx, findings := InspectArchive(raw, DefaultLimits)
			if len(findings) != 0 {
				t.Fatalf("fixture archive invalid: %v", codes(findings))
			}
			if findings := CheckFiles(m, idx); len(findings) != 0 {
				t.Fatalf("fixture files invalid: %v", codes(findings))
			}
			if findings := CheckGraph(m); len(findings) != 0 {
				t.Fatalf("fixture graph invalid: %v", codes(findings))
			}
			rep := Check(raw, DefaultLimits)
			if rep.Verdict != "REJECT" || !has(rep.Findings, tc.want) {
				t.Fatalf("invalid portable body admitted: verdict=%s codes=%v; want REJECT/%s", rep.Verdict, codes(rep.Findings), tc.want)
			}
		})
	}
}

// The fixture is derived from registered shapes; production never consumes
// these synthetic defaults or infers schema from authored JSON.
func portableSchemaFixture(t *testing.T, kind string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../generated/wepkg-portable.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	value, err := wire.ParseBounded(raw, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defs := pmap(pmap(value)["$defs"])
	var sample func(map[string]any) any
	sample = func(s map[string]any) any {
		if c, ok := s["const"]; ok {
			return c
		}
		if e := parray(s["enum"]); len(e) > 0 {
			return e[0]
		}
		if ref := pstr(s["$ref"]); ref != "" {
			return sample(pmap(defs[strings.TrimPrefix(ref, "#/$defs/")]))
		}
		for _, branch := range parray(s["allOf"]) {
			if e := parray(pmap(branch)["enum"]); len(e) > 0 {
				return e[0]
			}
		}
		if choices := parray(s["oneOf"]); len(choices) > 0 {
			return sample(pmap(choices[0]))
		}
		switch pstr(s["type"]) {
		case "object":
			out := map[string]any{}
			for _, key := range parray(s["required"]) {
				name := pstr(key)
				out[name] = sample(pmap(pmap(s["properties"])[name]))
			}
			return out
		case "array":
			out := []any{}
			n := 0
			if min, ok := s["minItems"].(json.Number); ok {
				v, _ := min.Int64()
				n = int(v)
			}
			for i := 0; i < n; i++ {
				out = append(out, sample(pmap(s["items"])))
			}
			return out
		case "integer", "number":
			if min, ok := s["minimum"]; ok {
				return min
			}
			return json.Number("0")
		case "boolean":
			return false
		default:
			pattern := pstr(s["pattern"])
			if strings.Contains(pattern, "{64}") {
				return strings.Repeat("a", 64)
			}
			if strings.Contains(pattern, "[0-9]") {
				return "1"
			}
			return "root"
		}
	}
	var object map[string]any
	if kind == "WORLD" {
		raw, err = os.ReadFile("../../contracts/source/openapi.yaml")
		if err != nil {
			t.Fatal(err)
		}
		v, err := wire.ParseBounded(raw, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		doc := pmap(parray(pmap(pmap(pmap(pmap(v)["components"])["schemas"])["DraftInput"])["examples"])[0])
		delete(doc, "dependency_locks")
		doc["body_version"] = "1.0"
		var adapt func(any)
		adapt = func(v any) {
			switch v := v.(type) {
			case map[string]any:
				if pstr(v["kind"]) == "LOCAL_DRAFT" {
					v["kind"] = "BUNDLE_OBJECT"
				}
				for _, x := range v {
					adapt(x)
				}
			case []any:
				for _, x := range v {
					adapt(x)
				}
			}
		}
		adapt(doc)
		for _, v := range parray(doc["objects"]) {
			o := pmap(v)
			o["rights"] = map[string]any{"license": "Apache-2.0", "source_reference": "fixture:portable", "redistribution_allowed": true, "allowed_uses": []any{"WORLD_CONTENT"}}
			if _, ok := o["generation"]; ok {
				o["generation"] = map[string]any{"mode": "NONE"}
			}
			if pstr(o["kind"]) == "ROLE" {
				delete(o, "tool_ids")
				o["tool_capability_ids"] = []any{}
			}
		}
		return doc
	}
	for _, branch := range parray(pmap(defs["ContentObject"])["oneOf"]) {
		component := pmap(defs[strings.TrimPrefix(pstr(pmap(branch)["$ref"]), "#/$defs/")])
		if pstr(pmap(pmap(component["properties"])["kind"])["const"]) == kind {
			object = pmap(sample(component))
			break
		}
	}
	if object == nil {
		t.Fatalf("missing source component for %s", kind)
	}
	object["object_id"] = "root"
	pmap(object["rights"])["source_reference"] = "fixture:portable"
	return map[string]any{"body_version": "1.0", "name": "Synthetic portable definition", "description": "Synthetic test", "tags": []any{}, "root_object_id": "root", "objects": []any{object}, "module_agent_bindings": []any{}, "skill_bindings": []any{}}
}
func portableArchive(t *testing.T, m Manifest, bodies map[string][]byte) ([]byte, Manifest) {
	t.Helper()
	for i := range m.Files {
		if b, ok := bodies[m.Files[i].Path]; ok {
			m.Files[i].SizeBytes = int64(len(b))
			m.Files[i].SHA256 = portableDigest(b)
		}
	}
	for i := range m.Entries {
		m.Entries[i].SHA256 = portableDigest(bodies[m.Entries[i].Path])
	}
	var err error
	m.ContentSHA256, err = ContentHash(m)
	if err != nil {
		t.Fatal(err)
	}
	manifest := mustJSON(t, m)
	if _, f := ParseManifest(manifest, DefaultLimits); len(f) > 0 {
		t.Fatalf("fixture manifest: %v", f)
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	paths := []string{"manifest.json"}
	for path := range bodies {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		b := bodies[path]
		if path == "manifest.json" {
			b = manifest
		}
		w, err := z.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	idx, f := InspectArchive(raw, DefaultLimits)
	if len(f) > 0 {
		t.Fatalf("fixture container: %v", f)
	}
	if f = CheckFiles(m, idx); len(f) > 0 {
		t.Fatalf("fixture files: %v", f)
	}
	if f = CheckGraph(m); len(f) > 0 {
		t.Fatalf("fixture graph: %v", f)
	}
	return raw, m
}
func portableOne(t *testing.T, kind string, doc map[string]any) ([]byte, Manifest) {
	m := schemaExample(t)
	m.Kind = kind
	m.Entries[0].Kind = kind
	return portableArchive(t, m, map[string][]byte{m.Entries[0].Path: mustJSON(t, doc)})
}
func requirePortableCode(t *testing.T, raw []byte, code string) {
	t.Helper()
	rep := Check(raw, DefaultLimits)
	if rep.Verdict != "REJECT" || !has(rep.Findings, code) {
		t.Fatalf("want %s: %+v", code, rep.Findings)
	}
}
func TestPortableBodiesSixRootKinds(t *testing.T) {
	for _, kind := range []string{"WORLD", "CHARACTER", "ROLE", "SKILL", "KNOWLEDGE", "PRESENTATION_THEME"} {
		t.Run(kind, func(t *testing.T) {
			doc := portableSchemaFixture(t, kind)
			raw, _ := portableOne(t, kind, doc)
			if rep := Check(raw, DefaultLimits); rep.Verdict != "PASS" {
				t.Fatalf("complete fixture: %+v", rep.Findings)
			}
			for key := range doc {
				t.Run("missing_"+key, func(t *testing.T) {
					changed := portableSchemaFixture(t, kind)
					delete(changed, key)
					if err := generated.ValidatePortableBody(kind, mustJSON(t, changed)); err == nil {
						t.Fatal("required root field accepted")
					}
				})
			}
			doc["unknown"] = true
			if err := generated.ValidatePortableBody(kind, mustJSON(t, doc)); err == nil {
				t.Fatal("unknown root field accepted")
			}
		})
	}
}
func TestPortableBodiesRejectLocalAuthority(t *testing.T) {
	for _, field := range []string{"connection_ref", "model_id", "budget_policy_ref", "allowed_external_uses"} {
		t.Run(field, func(t *testing.T) {
			doc := portableSchemaFixture(t, "CHARACTER")
			pmap(parray(doc["objects"])[0])["generation"] = map[string]any{"mode": "NONE", field: "local"}
			raw, _ := portableOne(t, "CHARACTER", doc)
			requirePortableCode(t, raw, CodePortableBodySchema)
		})
	}
	for _, kind := range []string{"ROLE", "SKILL"} {
		doc := portableSchemaFixture(t, kind)
		pmap(parray(doc["objects"])[0])["builtin_original"] = true
		raw, _ := portableOne(t, kind, doc)
		requirePortableCode(t, raw, CodePortableBodySchema)
	}
	doc := portableSchemaFixture(t, "KNOWLEDGE")
	pmap(parray(doc["objects"])[0])["knowledge_type"] = "DOCUMENT_EXTRACT"
	raw, _ := portableOne(t, "KNOWLEDGE", doc)
	requirePortableCode(t, raw, CodePortableBodySchema)
}
func TestPortableGenerationAndCapabilityDeclarations(t *testing.T) {
	for _, mode := range []string{"NONE", "INHERIT", "REQUIREMENT"} {
		doc := portableSchemaFixture(t, "CHARACTER")
		g := map[string]any{"mode": mode}
		if mode == "REQUIREMENT" {
			g["capability_id"] = "text"
		}
		pmap(parray(doc["objects"])[0])["generation"] = g
		m := schemaExample(t)
		m.Kind = "CHARACTER"
		m.Entries[0].Kind = "CHARACTER"
		m.Requirements.Model = []Capability{{CapabilityID: "text", Required: true}}
		raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc)})
		if rep := Check(raw, DefaultLimits); rep.Verdict != "PASS" {
			t.Fatalf("%s: %+v", mode, rep.Findings)
		}
	}
	doc := portableSchemaFixture(t, "CHARACTER")
	pmap(parray(doc["objects"])[0])["generation"] = map[string]any{"mode": "REQUIREMENT", "capability_id": "undeclared"}
	raw, _ := portableOne(t, "CHARACTER", doc)
	requirePortableCode(t, raw, CodePortableReferenceInvalid)
	doc = portableSchemaFixture(t, "ROLE")
	pmap(parray(doc["objects"])[0])["generation"] = map[string]any{"mode": "INHERIT"}
	raw, _ = portableOne(t, "ROLE", doc)
	requirePortableCode(t, raw, CodePortableBodySchema)
}
func TestPortableIdentityAndSlotOwnership(t *testing.T) {
	doc := portableSchemaFixture(t, "WORLD")
	objects := parray(doc["objects"])
	pmap(objects[1])["object_id"] = pmap(objects[0])["object_id"]
	raw, _ := portableOne(t, "WORLD", doc)
	requirePortableCode(t, raw, CodePortableIdentityDuplicate)
	doc = portableSchemaFixture(t, "WORLD")
	pmap(parray(doc["objects"])[0])["primary_actor_slot"] = "alex-character"
	raw, _ = portableOne(t, "WORLD", doc)
	requirePortableCode(t, raw, CodePortableSlotInvalid)
	doc = portableSchemaFixture(t, "CHARACTER")
	doc["root_object_id"] = "missing"
	raw, _ = portableOne(t, "CHARACTER", doc)
	requirePortableCode(t, raw, CodePortableReferenceKind)
}
func TestPortableRightsAndSourceLocations(t *testing.T) {
	for _, value := range []string{" C:\\private\\item ", "d:/private/item", `\\host\share`, "//host/share", " /private/item ", " FiLe:///private/item"} {
		t.Run(fmt.Sprintf("value_%x", sha256.Sum256([]byte(value))), func(t *testing.T) {
			doc := portableSchemaFixture(t, "KNOWLEDGE")
			pmap(parray(doc["objects"])[0])["source_location"] = value
			raw, _ := portableOne(t, "KNOWLEDGE", doc)
			requirePortableCode(t, raw, CodePortableReferenceInvalid)
			m := schemaExample(t)
			m.Provenance.SourceReferences = []string{value}
			raw, _ = portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, portableSchemaFixture(t, "WORLD"))})
			requirePortableCode(t, raw, CodePortableReferenceInvalid)
			m = schemaExample(t)
			m.Files[0].Rights.SourceReference = value
			raw, _ = portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, portableSchemaFixture(t, "WORLD"))})
			requirePortableCode(t, raw, CodePortableReferenceInvalid)
		})
	}
	doc := portableSchemaFixture(t, "KNOWLEDGE")
	pmap(parray(doc["objects"])[0])["text"] = "The story mentions C:/a/stage as dialogue, not a source address."
	raw, _ := portableOne(t, "KNOWLEDGE", doc)
	if rep := Check(raw, DefaultLimits); rep.Verdict != "PASS" {
		t.Fatalf("story substring refused: %v", rep.Findings)
	}
}
func TestPortableDiagnosticsAreBoundedDeterministicAndSafe(t *testing.T) {
	doc := portableSchemaFixture(t, "SKILL")
	objects := []any{}
	for i := 0; i < 80; i++ {
		o := pmap(parray(portableSchemaFixture(t, "SKILL")["objects"])[0])
		o["object_id"] = fmt.Sprintf("skill-%02d", i)
		o["name"] = strings.Repeat("secret-token-do-not-echo", 5)
		objects = append(objects, o)
	}
	doc["root_object_id"] = "skill-00"
	doc["objects"] = objects
	raw, _ := portableOne(t, "SKILL", doc)
	rep := Check(raw, DefaultLimits)
	if len(rep.Findings) != 32 {
		t.Fatalf("want lex-smallest 32 body diagnostics, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	if rep.ReportSHA256 != Check(raw, DefaultLimits).ReportSHA256 {
		t.Fatal("unstable report")
	}
	encoded := string(mustJSON(t, rep))
	if strings.Contains(encoded, "secret-token") {
		t.Fatal("input value echoed")
	}
	for _, f := range rep.Findings {
		if !strings.Contains(f.Object, ".objects.") {
			t.Fatalf("safe body position lost: %s", f.Object)
		}
	}
}

func TestPortablePackageAddressUniqueness(t *testing.T) {
	doc := portableSchemaFixture(t, "SKILL")
	m := schemaExample(t)
	m.Kind = "SKILL"
	m.Entries[0].Kind = "SKILL"
	first := m.Entries[0]
	second := first
	second.Path = "definitions/second.json"
	m.Entries = append(m.Entries, second)
	file := m.Files[0]
	file.Path = second.Path
	m.Files = append(m.Files, file)
	raw, _ := portableArchive(t, m, map[string][]byte{first.Path: mustJSON(t, doc), second.Path: mustJSON(t, doc)})
	requirePortableCode(t, raw, CodePortableIdentityDuplicate)
	plan, findings := BuildPortablePlan(raw, DefaultLimits, first.LogicalID, strings.Repeat("a", 64))
	if len(findings) == 0 || plan.PlanVersion != "" {
		t.Fatal("ambiguous entrypoint produced a plan")
	}
}

func TestPortableBodiesStrictJSONAndLimits(t *testing.T) {
	valid := mustJSON(t, portableSchemaFixture(t, "SKILL"))
	for _, raw := range [][]byte{append(append([]byte{}, valid...), []byte(" true")...), []byte(`{"body_version":"1.0","body_\u0076ersion":"1.0"}`), []byte("\"\xff\""), []byte(`"\ud800"`), []byte(`{"x":{"a":1,"a":2}}`)} {
		if generated.ValidatePortableBody("SKILL", raw) == nil {
			t.Fatal("non-strict JSON admitted")
		}
	}
	padded := append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), (1<<20)-len(valid))...)
	if e := generated.ValidatePortableBody("SKILL", padded); e != nil {
		t.Fatalf("exact byte ceiling: %v", e)
	}
	if generated.ValidatePortableBody("SKILL", append(padded, ' ')) == nil {
		t.Fatal("byte ceiling exceeded")
	}
	for _, depth := range []int{64, 65} {
		raw := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
		_, err := wire.Parse(raw)
		if (err == nil) != (depth == 64) {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
	// Retention is a schema integer, while exact version/size fields stay strings.
	doc := portableSchemaFixture(t, "WORLD")
	pmap(parray(doc["objects"])[0])["retention_days"] = json.Number("30e0")
	raw, _ := portableOne(t, "WORLD", doc)
	if Check(raw, DefaultLimits).Verdict != "PASS" {
		t.Fatal("exact integral exponent refused")
	}
}
func TestPortableEntrySupportMatrix(t *testing.T) {
	for _, body := range []string{`{"primitives":[]}`, `{"primitives":["attribute","time"]}`} {
		if e := generated.ValidatePortableBody("RULES", []byte(body)); e != nil {
			t.Fatal(e)
		}
	}
	for _, body := range []string{`{"primitives":["time","time"]}`, `{"primitives":[],"code":"eval"}`, `{}`} {
		if generated.ValidatePortableBody("RULES", []byte(body)) == nil {
			t.Fatal("open RULES admitted")
		}
	}
	doc := portableSchemaFixture(t, "SKILL")
	m := schemaExample(t)
	m.Kind = "SKILL"
	m.Entries[0].Kind = "SKILL"
	f := m.Files[0]
	f.Path = "rules/core.json"
	m.Files = append(m.Files, f)
	m.Entries = append(m.Entries, Entry{LogicalID: "rules", Kind: "RULES", Path: f.Path, Version: "1.0.0", References: []string{}})
	raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc), f.Path: []byte(`{"primitives":["eval"]}`)})
	requirePortableCode(t, raw, CodePrimitiveForbidden)
	for _, kind := range []string{"PRODUCT_PACK", "SCHEMA"} {
		m.Entries[1].Kind = kind
		raw, _ = portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc), f.Path: []byte(`{}`)})
		requirePortableCode(t, raw, CodePortableBodyUnsupported)
	}
}
func portableGraphArchive(t *testing.T, graph map[string][]string) ([]byte, Manifest) {
	t.Helper()
	ids := map[string]bool{}
	for src, refs := range graph {
		ids[src] = true
		for _, dst := range refs {
			ids[dst] = true
		}
	}
	names := []string{}
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	m := schemaExample(t)
	m.Kind = "SKILL"
	rights := m.Files[0].Rights
	m.Entries = []Entry{}
	m.Files = []FileRecord{}
	m.Entrypoints = []string{names[0]}
	bodies := map[string][]byte{}
	for _, name := range names {
		doc := portableSchemaFixture(t, "SKILL")
		objects := []any{}
		refs := graph[name]
		for off := 0; off < len(refs) || off == 0; off += 30 {
			o := pmap(parray(portableSchemaFixture(t, "SKILL")["objects"])[0])
			o["object_id"] = fmt.Sprintf("object-%d", off/30)
			links := []any{}
			end := off + 30
			if end > len(refs) {
				end = len(refs)
			}
			for _, id := range refs[off:end] {
				links = append(links, map[string]any{"kind": "PACKAGE_ENTRY", "logical_id": id})
			}
			o["dependency_refs"] = links
			objects = append(objects, o)
		}
		doc["objects"] = objects
		doc["root_object_id"] = "object-0"
		body := mustJSON(t, doc)
		if err := generated.ValidatePortableBody("SKILL", body); err != nil {
			t.Fatalf("graph fixture body: %v", err)
		}
		path := "definitions/" + name + ".json"
		unique := map[string]bool{}
		for _, ref := range refs {
			unique[ref] = true
		}
		declared := []string{}
		for ref := range unique {
			declared = append(declared, ref)
		}
		sort.Strings(declared)
		bodies[path] = body
		m.Entries = append(m.Entries, Entry{LogicalID: name, Kind: "SKILL", Path: path, Version: "1.0.0", References: declared})
		m.Files = append(m.Files, FileRecord{Path: path, MediaType: "application/json", Rights: rights})
	}
	return portableArchive(t, m, bodies)
}
func TestPortableAssociationCyclesAndAssemblyCycles(t *testing.T) {
	raw, _ := portableOne(t, "WORLD", portableSchemaFixture(t, "WORLD"))
	if r := Check(raw, DefaultLimits); r.Verdict != "PASS" {
		t.Fatalf("source association cycle: %v", r.Findings)
	}
	assertGraph := func(graph map[string][]string, reject bool) {
		t.Helper()
		raw, _ := portableGraphArchive(t, graph)
		rep := Check(raw, DefaultLimits)
		if !reject {
			if rep.Verdict != "PASS" {
				t.Fatalf("legal graph: %+v", rep.Findings)
			}
			return
		}
		if len(rep.Findings) != 1 || rep.Findings[0].Code != CodePortableReferenceCycle || rep.Findings[0].Detail != "assembly graph is cyclic or exceeds bounds" {
			t.Fatalf("graph boundary not reached: %+v", rep.Findings)
		}
	}
	t.Run("cycle", func(t *testing.T) { assertGraph(map[string][]string{"a": {"b"}, "b": {"a"}}, true) })
	for _, n := range []int{64, 65} {
		t.Run(fmt.Sprintf("vertices_%d", n), func(t *testing.T) {
			graph := map[string][]string{}
			for i := 0; i < n-1; i++ {
				graph[fmt.Sprintf("node-%02d", i)] = []string{fmt.Sprintf("node-%02d", i+1)}
			}
			graph["node-00"] = append(graph["node-00"], fmt.Sprintf("node-%02d", n-1))
			assertGraph(graph, n == 65)
		})
	}
	t.Run("deduplicated_global_edges_10000_10001", func(t *testing.T) {
		graph := map[string][]string{}
		for i := 0; i < 100; i++ {
			name := fmt.Sprintf("source-%03d", i)
			for j := 0; j < 100; j++ {
				graph[name] = append(graph[name], fmt.Sprintf("target-%03d", j))
			}
		}
		assertGraph(graph, false)
		graph["source-000"] = append(graph["source-000"], "target-extra")
		assertGraph(graph, true)
	})
}
func TestPortableReferencesResolveQualifiedTargets(t *testing.T) {
	doc := portableSchemaFixture(t, "SKILL")
	o := pmap(parray(doc["objects"])[0])
	ref := map[string]any{"kind": "DEPENDENCY_EXPORT", "package_id": "dependency", "package_version": "1.2.3", "content_sha256": strings.Repeat("b", 64), "logical_id": "exported"}
	o["conflicting_refs"] = []any{ref}
	m := schemaExample(t)
	m.Kind = "SKILL"
	m.Entries[0].Kind = "SKILL"
	m.Entries[0].References = []string{"exported"}
	m.Dependencies = []Dependency{{PackageID: "dependency", PackageVersion: "1.2.3", ContentSHA256: strings.Repeat("b", 64), LogicalExports: []string{"exported"}}}
	raw, m := portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc)})
	p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
	if len(f) > 0 || len(p.References) != 1 || p.References[0].ExpectedKinds[0] != "SKILL" {
		t.Fatalf("external requirement lost: %+v", f)
	}
	ref["package_version"] = "1.2.4"
	raw, _ = portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc)})
	requirePortableCode(t, raw, CodePortableReferenceInvalid)
	ref["package_version"] = "1.2.3"
	m.Dependencies = append(m.Dependencies, Dependency{PackageID: "another", PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("c", 64), LogicalExports: []string{"exported"}})
	raw, _ = portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc)})
	requirePortableCode(t, raw, CodePortableReferenceInvalid)
}

func allPortableObjects(t *testing.T) (map[string]any, Manifest, map[string][]byte) {
	t.Helper()
	doc := portableSchemaFixture(t, "WORLD")
	objects := parray(doc["objects"])
	for _, kind := range []string{"SKILL", "KNOWLEDGE", "ASSET", "PRESENTATION_THEME"} {
		o := pmap(parray(portableSchemaFixture(t, kind)["objects"])[0])
		o["object_id"] = strings.ToLower(kind)
		objects = append(objects, o)
	}
	doc["objects"] = objects
	payload := []byte("Synthetic portable asset\n")
	m := schemaExample(t)
	rights := m.Files[0].Rights
	m.Files = append(m.Files, FileRecord{Path: "assets/synthetic.txt", SHA256: portableDigest(payload), SizeBytes: int64(len(payload)), MediaType: "text/plain", Rights: rights})
	for _, v := range objects {
		o := pmap(v)
		if pstr(o["kind"]) == "ASSET" {
			o["file_path"] = "assets/synthetic.txt"
			o["asset_sha256"] = portableDigest(payload)
			o["media_type"] = "text/plain"
			o["size_bytes"] = fmt.Sprint(len(payload))
		}
	}
	doc["module_agent_bindings"] = []any{map[string]any{"binding_id": "same", "owner_object_id": "coffee-world", "module_id": "AUTHORING", "role_ref": map[string]any{"kind": "BUNDLE_OBJECT", "object_id": "program-duties"}, "enabled": true, "generation": map[string]any{"mode": "NONE"}}}
	doc["skill_bindings"] = []any{map[string]any{"binding_id": "same", "role_object_id": "program-duties", "skill_ref": map[string]any{"kind": "BUNDLE_OBJECT", "object_id": "skill"}, "order": json.Number("0"), "trigger": "MANUAL", "enabled": true, "budget_owner": "PARENT"}}
	return doc, m, map[string][]byte{"assets/synthetic.txt": payload}
}
func TestPortableReferencesCoverAllSchemaPositions(t *testing.T) {
	meta, err := generated.PortableMetadata()
	if err != nil || len(meta.ReferencePaths) != 45 {
		t.Fatal("source reference inventory unavailable", err)
	}
	for _, position := range meta.ReferencePaths {
		t.Run(position.Component+"_"+strings.Join(position.Path, "_"), func(t *testing.T) {
			doc, m, bodies := allPortableObjects(t)
			var owner map[string]any
			if position.OwnerDomain == "OBJECT" {
				for _, v := range parray(doc["objects"]) {
					if pstr(pmap(v)["kind"]) == position.ObjectKind {
						owner = pmap(v)
						break
					}
				}
			} else {
				group := "module_agent_bindings"
				if position.OwnerDomain == "SKILL_BINDING" {
					group = "skill_bindings"
				}
				owner = pmap(parray(doc[group])[0])
			}
			if owner == nil {
				t.Fatal("source owner fixture missing")
			}
			if position.Path[0] == "condition" {
				owner["condition"] = map[string]any{"op": "PROP_CUSTODIAN_EQUALS", "slot_id": "mia", "prop_ref": map[string]any{"kind": "BUNDLE_OBJECT", "object_id": "wooden-sign"}}
			}
			if position.Path[0] == "motion_mappings" {
				owner["motion_mappings"] = []any{map[string]any{"action": "move"}}
			}
			if position.Path[0] == "voice_mappings" {
				owner["voice_mappings"] = []any{map[string]any{"voice": "synthetic"}}
			}
			target := "coffee-world"
			if len(position.ExpectedKinds) > 0 {
				for _, v := range parray(doc["objects"]) {
					if pstr(pmap(v)["kind"]) == position.ExpectedKinds[0] {
						target = pstr(pmap(v)["object_id"])
						break
					}
				}
			}
			var change func(any, []string, map[string]any) any
			change = func(v any, path []string, ref map[string]any) any {
				if len(path) == 0 {
					return ref
				}
				if path[0] == "*" {
					a := parray(v)
					if len(a) == 0 {
						a = []any{nil}
					}
					a[0] = change(a[0], path[1:], ref)
					return a
				}
				o := pmap(v)
				if o == nil {
					o = map[string]any{}
				}
				o[path[0]] = change(o[path[0]], path[1:], ref)
				return o
			}
			set := func(id string) {
				change(owner, position.Path, map[string]any{"kind": "BUNDLE_OBJECT", "object_id": id})
			}
			check := func() Report {
				bodies[m.Files[0].Path] = mustJSON(t, doc)
				raw, _ := portableArchive(t, m, bodies)
				return Check(raw, DefaultLimits)
			}
			set(target)
			if rep := check(); rep.Verdict != "PASS" {
				t.Fatalf("valid source position: %+v", rep.Findings)
			}
			set("missing-object")
			if rep := check(); !has(rep.Findings, CodePortableReferenceInvalid) {
				t.Fatalf("position was not checked: %+v", rep.Findings)
			}
			if len(position.ExpectedKinds) > 0 {
				set("coffee-world")
				if rep := check(); !has(rep.Findings, CodePortableReferenceKind) {
					t.Fatalf("known kind was not checked: %+v", rep.Findings)
				}
			}
		})
	}
}
func TestPortableBindingOwnersAndCeilings(t *testing.T) {
	doc, m, bodies := allPortableObjects(t)
	bodies[m.Files[0].Path] = mustJSON(t, doc)
	raw, m := portableArchive(t, m, bodies)
	p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
	if len(f) > 0 {
		t.Fatal(f)
	}
	same := []PortableID{}
	for _, id := range p.IDs {
		if id.LogicalID == "same" {
			same = append(same, id)
		}
	}
	if len(same) != 2 || same[0].LocalID == same[1].LocalID {
		t.Fatal("binding domains merged")
	}
	for _, group := range []string{"module_agent_bindings", "skill_bindings"} {
		for _, n := range []int{100, 101} {
			t.Run(fmt.Sprintf("%s_%d", group, n), func(t *testing.T) {
				d, m, bodies := allPortableObjects(t)
				original := pmap(parray(d[group])[0])
				bindings := []any{}
				for i := 0; i < n; i++ {
					o := map[string]any{}
					for k, v := range original {
						o[k] = v
					}
					o["binding_id"] = fmt.Sprintf("binding-%d", i)
					bindings = append(bindings, o)
				}
				d[group] = bindings
				bodies[m.Files[0].Path] = mustJSON(t, d)
				raw, _ := portableArchive(t, m, bodies)
				rep := Check(raw, DefaultLimits)
				if (rep.Verdict == "PASS") != (n == 100) {
					t.Fatalf("binding ceiling: %+v", rep.Findings)
				}
			})
		}
	}
	doc, m, bodies = allPortableObjects(t)
	pmap(parray(doc["skill_bindings"])[0])["role_object_id"] = "coffee-world"
	bodies[m.Files[0].Path] = mustJSON(t, doc)
	raw, _ = portableArchive(t, m, bodies)
	requirePortableCode(t, raw, CodePortableReferenceKind)
}
func TestPortableFileDeclarations(t *testing.T) {
	for _, field := range []string{"valid", "asset_sha256", "size_bytes", "media_type", "file_path"} {
		t.Run(field, func(t *testing.T) {
			doc, m, bodies := allPortableObjects(t)
			for _, v := range parray(doc["objects"]) {
				o := pmap(v)
				if pstr(o["kind"]) != "ASSET" {
					continue
				}
				switch field {
				case "asset_sha256":
					o[field] = strings.Repeat("a", 64)
				case "size_bytes":
					o[field] = "999"
				case "media_type":
					o[field] = "text/markdown"
				case "file_path":
					o[field] = "assets/missing.txt"
				}
			}
			bodies[m.Files[0].Path] = mustJSON(t, doc)
			raw, m := portableArchive(t, m, bodies)
			if field != "valid" {
				requirePortableCode(t, raw, CodePortableFileInvalid)
				return
			}
			p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
			if len(f) > 0 || len(p.Files) != 1 || p.Files[0].Path != "assets/synthetic.txt" || !p.Files[0].Rights.RedistributionAllowed {
				t.Fatalf("file requirement or source rights lost: %+v", f)
			}
		})
	}
	doc, m, bodies := allPortableObjects(t)
	f := m.Files[1]
	f.Rights.SourceReference = "file:///private/unselected"
	f.Path = "assets/unselected.txt"
	m.Files = append(m.Files, f)
	bodies[f.Path] = bodies["assets/synthetic.txt"]
	bodies[m.Files[0].Path] = mustJSON(t, doc)
	raw, _ := portableArchive(t, m, bodies)
	requirePortableCode(t, raw, CodePortableReferenceInvalid)
}
func TestPortableManifestReferenceInventory(t *testing.T) {
	raw, m := portableGraphArchive(t, map[string][]string{"first": {"second"}})
	manifestRaw, err := readMember(raw, "manifest.json", DefaultLimits.ManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	_ = manifestRaw
	bodies := map[string][]byte{}
	for _, e := range m.Entries {
		bodies[e.Path], err = readMember(raw, e.Path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
	}
	original := append([]Entry{}, m.Entries...)
	for i := range m.Entries {
		if m.Entries[i].LogicalID == "first" {
			m.Entries[i].References = []string{}
		}
	}
	raw, _ = portableArchive(t, m, bodies)
	requirePortableCode(t, raw, CodePortableReferenceMismatch)
	m.Entries = append([]Entry{}, original...)
	for i := range m.Entries {
		if m.Entries[i].LogicalID == "second" {
			m.Entries[i].References = []string{"first"}
		}
	}
	raw, _ = portableArchive(t, m, bodies)
	requirePortableCode(t, raw, CodePortableReferenceMismatch)
}

func TestPortableDiagnosticLexicalSelectionAndFallback(t *testing.T) {
	doc, m, bodies := allPortableObjects(t)
	base := pmap(parray(doc["module_agent_bindings"])[0])
	bindings := []any{}
	expected := []string{}
	for i := 0; i < 80; i++ {
		o := map[string]any{}
		for k, v := range base {
			o[k] = v
		}
		o["binding_id"] = fmt.Sprintf("binding-%d", i)
		o["enabled"] = "private-example-value"
		bindings = append(bindings, o)
		expected = append(expected, fmt.Sprintf("entries.0.module_agent_bindings.%d.enabled", i))
	}
	doc["module_agent_bindings"] = bindings
	bodies[m.Files[0].Path] = mustJSON(t, doc)
	raw, _ := portableArchive(t, m, bodies)
	rep := Check(raw, DefaultLimits)
	sort.Strings(expected)
	if len(rep.Findings) != 32 {
		t.Fatalf("diagnostics count %d", len(rep.Findings))
	}
	for i, f := range rep.Findings {
		if f.Code != CodePortableBodySchema || f.Object != expected[i] {
			t.Fatalf("lexical result %d: %+v", i, f)
		}
	}
	for _, unknown := range []string{"0", strings.Repeat("private-field-", 80)} {
		d := portableSchemaFixture(t, "SKILL")
		d[unknown] = "private-example-value"
		raw, _ := portableOne(t, "SKILL", d)
		r := Check(raw, DefaultLimits)
		if len(r.Findings) != 1 || r.Findings[0].Object != "entries.0" || strings.Contains(string(mustJSON(t, r)), "private-") {
			t.Fatalf("unknown key escaped safe parent: %+v", r.Findings)
		}
	}
}
func TestPortableSchemaAgreementCases(t *testing.T) {
	count := 0
	emit := func(kind string, body []byte, want bool) {
		t.Helper()
		got := generated.ValidatePortableBody(kind, body) == nil
		if got != want {
			t.Fatalf("agreement case precondition %s: got %v", kind, got)
		}
		row := struct {
			Kind  string          `json:"kind"`
			Body  json.RawMessage `json:"body"`
			Valid bool            `json:"go_valid"`
		}{kind, body, got}
		t.Logf("PORTABLE_SCHEMA_CASE %s", mustJSON(t, row))
		count++
	}
	for _, kind := range []string{"WORLD", "CHARACTER", "ROLE", "SKILL", "KNOWLEDGE", "PRESENTATION_THEME"} {
		doc := portableSchemaFixture(t, kind)
		emit(kind, mustJSON(t, doc), true)
		delete(doc, "body_version")
		emit(kind, mustJSON(t, doc), false)
	}
	for _, body := range []string{`{"primitives":["time"]}`, `{"primitives":["time","time"]}`, `{"primitives":[],"unknown":true}`} {
		emit("RULES", []byte(body), body == `{"primitives":["time"]}`)
	}
	doc := portableSchemaFixture(t, "WORLD")
	pmap(parray(doc["objects"])[0])["retention_days"] = json.Number("30e0")
	emit("WORLD", mustJSON(t, doc), true)
	if count != 16 {
		t.Fatal("agreement inventory changed")
	}
}

func TestPortableOwnershipAndIsolationEdges(t *testing.T) {
	t.Run("duplicate_file_metadata", func(t *testing.T) {
		doc, m, bodies := allPortableObjects(t)
		duplicate := m.Files[1]
		duplicate.Rights.SourceReference = "fixture:alternative"
		m.Files = append(m.Files, duplicate)
		bodies[m.Files[0].Path] = mustJSON(t, doc)
		raw, _ := portableArchive(t, m, bodies)
		requirePortableCode(t, raw, CodePortableFileInvalid)
	})
	for _, group := range []string{"module_agent_bindings", "skill_bindings"} {
		t.Run("duplicate_"+group, func(t *testing.T) {
			doc, m, bodies := allPortableObjects(t)
			binding := parray(doc[group])[0]
			doc[group] = []any{binding, binding}
			bodies[m.Files[0].Path] = mustJSON(t, doc)
			raw, _ := portableArchive(t, m, bodies)
			requirePortableCode(t, raw, CodePortableIdentityDuplicate)
		})
	}
	t.Run("duplicate_slot", func(t *testing.T) {
		doc := portableSchemaFixture(t, "WORLD")
		world := pmap(parray(doc["objects"])[0])
		slots := parray(world["initial_actor_setups"])
		pmap(slots[1])["slot_id"] = pmap(slots[0])["slot_id"]
		raw, _ := portableOne(t, "WORLD", doc)
		requirePortableCode(t, raw, CodePortableIdentityDuplicate)
	})
	t.Run("extra_world", func(t *testing.T) {
		doc := portableSchemaFixture(t, "WORLD")
		extra := pmap(parray(portableSchemaFixture(t, "WORLD")["objects"])[0])
		extra["object_id"] = "extra-world"
		doc["objects"] = append(parray(doc["objects"]), extra)
		raw, _ := portableOne(t, "WORLD", doc)
		requirePortableCode(t, raw, CodePortableReferenceInvalid)
	})
	t.Run("missing_module_owner", func(t *testing.T) {
		doc, m, bodies := allPortableObjects(t)
		pmap(parray(doc["module_agent_bindings"])[0])["owner_object_id"] = "missing"
		bodies[m.Files[0].Path] = mustJSON(t, doc)
		raw, _ := portableArchive(t, m, bodies)
		requirePortableCode(t, raw, CodePortableReferenceKind)
	})
	for _, n := range []int{200, 201} {
		t.Run(fmt.Sprintf("objects_%d", n), func(t *testing.T) {
			doc := portableSchemaFixture(t, "SKILL")
			original := pmap(parray(doc["objects"])[0])
			objects := []any{}
			for i := 0; i < n; i++ {
				o := map[string]any{}
				for k, v := range original {
					o[k] = v
				}
				o["object_id"] = fmt.Sprintf("object-%d", i)
				objects = append(objects, o)
			}
			doc["objects"] = objects
			doc["root_object_id"] = "object-0"
			raw, _ := portableOne(t, "SKILL", doc)
			r := Check(raw, DefaultLimits)
			if (r.Verdict == "PASS") != (n == 200) {
				t.Fatalf("object ceiling: %v", r.Findings)
			}
		})
	}
	t.Run("same_object_slot_address", func(t *testing.T) {
		doc := portableSchemaFixture(t, "WORLD")
		world := pmap(parray(doc["objects"])[0])
		pmap(parray(world["initial_actor_setups"])[0])["slot_id"] = "alex-character"
		world["primary_actor_slot"] = "alex-character"
		raw, m := portableOne(t, "WORLD", doc)
		p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
		if len(f) > 0 {
			t.Fatal(f)
		}
		ids := map[string]string{}
		for _, id := range p.IDs {
			if id.LogicalID == "alex-character" {
				ids[id.Domain] = id.LocalID
			}
		}
		if len(ids) != 2 || ids["OBJECT"] == ids["SLOT"] {
			t.Fatal("object and slot identities merged")
		}
	})
}
func TestPortableOriginalWireFailureCodes(t *testing.T) {
	valid := mustJSON(t, portableSchemaFixture(t, "SKILL"))
	m := schemaExample(t)
	m.Kind = "SKILL"
	m.Entries[0].Kind = "SKILL"
	for _, body := range [][]byte{[]byte{'"', 0xff, '"'}, []byte(`"\ud800"`), []byte(`{"body_version":"1.0","body_\u0076ersion":"1.0"}`), []byte(`{"nested":{"a":1,"a":2}}`), append(append([]byte{}, valid...), []byte(" true")...)} {
		raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: body})
		requirePortableCode(t, raw, CodeEntryBodyInvalid)
	}
	padded := append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), (1<<20)-len(valid))...)
	raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: padded})
	if r := Check(raw, DefaultLimits); r.Verdict != "PASS" {
		t.Fatalf("exact body byte limit: %v", r.Findings)
	}
	raw, _ = portableArchive(t, m, map[string][]byte{m.Files[0].Path: append(padded, ' ')})
	requirePortableCode(t, raw, CodePortableBodyLimit)
}

func TestPortableResidualAuthorityAndCoordinates(t *testing.T) {
	for _, field := range []string{"dependency_locks", "source_asset_id", "source_asset_hash", "extraction_report_id", "local_rights"} {
		t.Run("authority_"+field, func(t *testing.T) {
			doc := portableSchemaFixture(t, "KNOWLEDGE")
			o := pmap(parray(doc["objects"])[0])
			switch field {
			case "dependency_locks":
				doc[field] = []any{}
			case "local_rights":
				o["rights"] = map[string]any{"origin_kind": "SYNTHETIC", "source_label": "local", "license": "CC0-1.0", "allowed_uses": []string{"EDIT"}}
			default:
				o[field] = "local"
			}
			raw, _ := portableOne(t, "KNOWLEDGE", doc)
			requirePortableCode(t, raw, CodePortableBodySchema)
		})
	}
	for _, field := range []string{"tool_capability_ids", "required_capability_ids"} {
		t.Run("undeclared_"+field, func(t *testing.T) {
			doc := portableSchemaFixture(t, "SKILL")
			pmap(parray(doc["objects"])[0])[field] = []string{"sign_custody"}
			raw, _ := portableOne(t, "SKILL", doc)
			requirePortableCode(t, raw, CodePortableReferenceInvalid)
		})
	}
	for _, parameters := range []map[string]any{{"temperature": json.Number("2.01")}, {"max_output_tokens": json.Number("8193")}, {"max_output_tokens": json.Number("1.5")}, {"unregistered": true}} {
		doc := portableSchemaFixture(t, "CHARACTER")
		pmap(parray(doc["objects"])[0])["generation"] = map[string]any{"mode": "REQUIREMENT", "capability_id": "text", "parameters": parameters}
		raw, _ := portableOne(t, "CHARACTER", doc)
		requirePortableCode(t, raw, CodePortableBodySchema)
	}
	t.Run("non_world_slot", func(t *testing.T) {
		doc := portableSchemaFixture(t, "SKILL")
		var prop map[string]any
		for _, v := range parray(portableSchemaFixture(t, "WORLD")["objects"]) {
			if pstr(pmap(v)["kind"]) == "PROP" {
				prop = pmap(v)
			}
		}
		doc["objects"] = append(parray(doc["objects"]), prop)
		raw, _ := portableOne(t, "SKILL", doc)
		requirePortableCode(t, raw, CodePortableSlotInvalid)
	})
	for _, field := range []string{"rights", "source_refs"} {
		doc := portableSchemaFixture(t, "SKILL")
		o := pmap(parray(doc["objects"])[0])
		if field == "rights" {
			pmap(o["rights"])["source_reference"] = " /private/source "
		} else {
			o["source_refs"] = []string{" C:/private/source "}
		}
		raw, _ := portableOne(t, "SKILL", doc)
		requirePortableCode(t, raw, CodePortableReferenceInvalid)
	}
	for _, field := range []string{"package_id", "package_version", "content_sha256", "logical_id"} {
		t.Run("dependency_"+field, func(t *testing.T) {
			doc := portableSchemaFixture(t, "SKILL")
			ref := map[string]any{"kind": "DEPENDENCY_EXPORT", "package_id": "dep", "package_version": "1.0.0", "content_sha256": strings.Repeat("b", 64), "logical_id": "exported"}
			m := schemaExample(t)
			m.Kind = "SKILL"
			m.Entries[0].Kind = "SKILL"
			m.Entries[0].References = []string{"exported"}
			m.Dependencies = []Dependency{{PackageID: "dep", PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("b", 64), LogicalExports: []string{"exported"}}}
			switch field {
			case "package_id":
				ref[field] = "other"
			case "package_version":
				ref[field] = "1.0.1"
			case "content_sha256":
				ref[field] = strings.Repeat("c", 64)
			case "logical_id":
				ref[field] = "elsewhere"
				m.Entries[0].References = []string{"elsewhere"}
				m.Dependencies = append(m.Dependencies, Dependency{PackageID: "other", PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("d", 64), LogicalExports: []string{"elsewhere"}})
			}
			pmap(parray(doc["objects"])[0])["dependency_refs"] = []any{ref}
			raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc)})
			requirePortableCode(t, raw, CodePortableReferenceInvalid)
		})
	}
	t.Run("self_entry", func(t *testing.T) {
		doc := portableSchemaFixture(t, "SKILL")
		m := schemaExample(t)
		m.Kind = "SKILL"
		m.Entries[0].Kind = "SKILL"
		id := m.Entries[0].LogicalID
		m.Entries[0].References = []string{id}
		pmap(parray(doc["objects"])[0])["dependency_refs"] = []any{map[string]any{"kind": "PACKAGE_ENTRY", "logical_id": id}}
		raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc)})
		requirePortableCode(t, raw, CodePortableReferenceInvalid)
	})
	t.Run("non_root_entry", func(t *testing.T) {
		doc := portableSchemaFixture(t, "SKILL")
		m := schemaExample(t)
		m.Kind = "SKILL"
		m.Entries[0].Kind = "SKILL"
		m.Entries[0].References = []string{"rules"}
		pmap(parray(doc["objects"])[0])["dependency_refs"] = []any{map[string]any{"kind": "PACKAGE_ENTRY", "logical_id": "rules"}}
		m.Entries = append(m.Entries, Entry{LogicalID: "rules", Kind: "RULES", Path: "rules/core.json", Version: "1.0.0", References: []string{}})
		m.Files = append(m.Files, FileRecord{Path: "rules/core.json", MediaType: "application/json", Rights: m.Files[0].Rights})
		raw, _ := portableArchive(t, m, map[string][]byte{m.Files[0].Path: mustJSON(t, doc), "rules/core.json": []byte(`{"primitives":[]}`)})
		requirePortableCode(t, raw, CodePortableReferenceInvalid)
	})
}
func TestPortableAssemblyDedupAndDisconnectedBudget(t *testing.T) {
	graph := map[string][]string{"first": {"second", "second"}}
	raw, _ := portableGraphArchive(t, graph)
	if r := Check(raw, DefaultLimits); r.Verdict != "PASS" {
		t.Fatal(r.Findings)
	}
	graph = map[string][]string{}
	for component := 0; component < 2; component++ {
		for i := 0; i < 50; i++ {
			src := fmt.Sprintf("c%d-source-%03d", component, i)
			for j := 0; j < 100; j++ {
				graph[src] = append(graph[src], fmt.Sprintf("c%d-target-%03d", component, j))
			}
		}
	}
	graph["c0-source-000"] = append(graph["c0-source-000"], "c0-target-000")
	raw, _ = portableGraphArchive(t, graph)
	if r := Check(raw, DefaultLimits); r.Verdict != "PASS" {
		t.Fatalf("dedup10k disjoint graph: %+v", r.Findings)
	}
	graph["c1-source-000"] = append(graph["c1-source-000"], "c1-target-extra")
	raw, _ = portableGraphArchive(t, graph)
	r := Check(raw, DefaultLimits)
	if len(r.Findings) != 1 || r.Findings[0].Code != CodePortableReferenceCycle {
		t.Fatalf("global budget reset per component: %+v", r.Findings)
	}
}
