package wepkg

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"loreviva.local/loreviva/internal/wire"
)

func TestPortablePlanBindsOriginalInput(t *testing.T) {
	raw, m := portableOne(t, "WORLD", portableSchemaFixture(t, "WORLD"))
	namespace := strings.Repeat("a", 64)
	p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], namespace)
	if len(f) > 0 {
		t.Fatalf("plan: %+v", f)
	}
	rep := Check(raw, DefaultLimits)
	if p.ExecutionReady || p.PlanVersion != "1" || p.Namespace != namespace || p.ArchiveSHA256 != rep.ArchiveSHA256 || p.ManifestSHA256 != rep.ManifestSHA256 || p.ContentSHA256 != m.ContentSHA256 || p.ReportSHA256 != rep.ReportSHA256 || p.BodySHA256 != portableDigest(p.Body) || p.RootObjectID == "coffee-world" {
		t.Fatal("original inputs or non-execution boundary lost")
	}
	for _, tc := range []struct{ entry, ns string }{{"missing", namespace}, {m.Entrypoints[0], ""}, {m.Entrypoints[0], strings.Repeat("A", 64)}, {m.Entrypoints[0], namespace + "a"}} {
		plan, fs := BuildPortablePlan(raw, DefaultLimits, tc.entry, tc.ns)
		if !reflect.DeepEqual(plan, PortablePlan{}) || !has(fs, CodePortablePlanInputInvalid) {
			t.Fatal("invalid selector produced a plan")
		}
	}
	bad := append([]byte{}, raw...)
	bad[len(bad)/2] ^= 1
	plan, fs := BuildPortablePlan(bad, DefaultLimits, m.Entrypoints[0], namespace)
	if len(fs) == 0 || !reflect.DeepEqual(plan, PortablePlan{}) {
		t.Fatal("tampered input produced a plan")
	}
}
func TestPortableIDsPreserveLongLogicalAddresses(t *testing.T) {
	doc := portableSchemaFixture(t, "SKILL")
	a := "a" + strings.Repeat("x", 94) + "a"
	b := "a" + strings.Repeat("x", 94) + "b"
	first := pmap(parray(doc["objects"])[0])
	first["object_id"] = a
	second := pmap(parray(portableSchemaFixture(t, "SKILL")["objects"])[0])
	second["object_id"] = b
	doc["objects"] = []any{first, second}
	doc["root_object_id"] = a
	raw, m := portableOne(t, "SKILL", doc)
	ns := strings.Repeat("a", 64)
	p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], ns)
	if len(f) > 0 {
		t.Fatal(f)
	}
	if len(p.IDs) != 2 || p.IDs[0].LogicalID != a || p.IDs[1].LogicalID != b || p.IDs[0].LocalID == p.IDs[1].LocalID {
		t.Fatal("logical addresses truncated or merged")
	}
	inputs := []PortableID{{Domain: "OBJECT", LogicalID: a}, {Domain: "MODULE_BINDING", LogicalID: a}, {Domain: "SKILL_BINDING", LogicalID: a}, {Domain: "SLOT", Owner: a, LogicalID: a}, {Domain: "SLOT", Owner: b, LogicalID: a}}
	seen := map[string]bool{}
	for _, id := range inputs {
		x, e := portableID(ns, m, m.Entries[0], id)
		if e != nil || seen[x] {
			t.Fatal("qualified domains/owners collide")
		}
		seen[x] = true
		y, e := portableID(strings.Repeat("b", 64), m, m.Entries[0], id)
		if e != nil || x == y {
			t.Fatal("namespace not bound")
		}
	}
}
func TestPortableIDsRefuseTargetCollisions(t *testing.T) {
	for _, domain := range []string{"OBJECT", "MODULE_BINDING", "SKILL_BINDING"} {
		ids := []PortableID{{Domain: "OBJECT", LogicalID: "first", LocalID: strings.Repeat("a", 64)}, {Domain: domain, LogicalID: "second", LocalID: strings.Repeat("a", 64)}}
		if validatePortableIDs(ids) == nil {
			t.Fatalf("cross-address collision accepted: %s", domain)
		}
	}
}
func TestPortablePlanHashOrderingAndBounds(t *testing.T) {
	raw, m := portableOne(t, "WORLD", portableSchemaFixture(t, "WORLD"))
	p, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
	if len(f) > 0 {
		t.Fatal(f)
	}
	again, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
	if len(f) > 0 || !bytes.Equal(mustJSON(t, p), mustJSON(t, again)) {
		t.Fatal("same bytes changed plan")
	}
	for i := 1; i < len(p.IDs); i++ {
		a, b := p.IDs[i-1], p.IDs[i]
		if a.Domain+"\x00"+a.Owner+"\x00"+a.LogicalID > b.Domain+"\x00"+b.Owner+"\x00"+b.LogicalID {
			t.Fatal("IDs unordered")
		}
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(mustJSON(t, p), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "plan_sha256")
	canonical, e := wire.CanonicalBounded(mustJSON(t, fields), 8<<20)
	if e != nil || portableDigest(canonical) != p.PlanSHA256 {
		t.Fatal("hash omitted or added a field")
	}
	if _, e := portablePlanHash(p, 128); e == nil {
		t.Fatal("encoding bound ignored")
	}
	doc := portableSchemaFixture(t, "WORLD")
	objects := parray(doc["objects"])
	for l, r := 0, len(objects)-1; l < r; l, r = l+1, r-1 {
		objects[l], objects[r] = objects[r], objects[l]
	}
	raw, m = portableOne(t, "WORLD", doc)
	reordered, f := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
	if len(f) > 0 {
		t.Fatal(f)
	}
	// Content hash is part of every candidate address, so object reordering is a
	// different source namespace; canonical ID records remain deterministic.
	if reordered.PlanSHA256 == p.PlanSHA256 {
		t.Fatal("changed original body not bound")
	}
}

func TestPortablePlanZIPOrderPreservesContentAddresses(t *testing.T) {
	original, m := portableOne(t, "WORLD", portableSchemaFixture(t, "WORLD"))
	manifest, e := readMember(original, "manifest.json", DefaultLimits.ManifestBytes)
	if e != nil {
		t.Fatal(e)
	}
	body, e := readMember(original, m.Entries[0].Path, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	reordered := build(t, member{name: "manifest.json", body: manifest}, member{name: m.Entries[0].Path, body: body})
	namespace := strings.Repeat("a", 64)
	a, f := BuildPortablePlan(original, DefaultLimits, m.Entrypoints[0], namespace)
	if len(f) > 0 {
		t.Fatal(f)
	}
	b, f := BuildPortablePlan(reordered, DefaultLimits, m.Entrypoints[0], namespace)
	if len(f) > 0 {
		t.Fatal(f)
	}
	if a.ArchiveSHA256 == b.ArchiveSHA256 || a.PlanSHA256 == b.PlanSHA256 || a.ContentSHA256 != b.ContentSHA256 || a.ManifestSHA256 != b.ManifestSHA256 || a.BodySHA256 != b.BodySHA256 || !reflect.DeepEqual(a.IDs, b.IDs) {
		t.Fatal("container representation changed qualified content addresses")
	}
}

func TestPortablePlanEightMiBOverflowReturnsZeroPlan(t *testing.T) {
	raw, m := portableOne(t, "SKILL", portableSchemaFixture(t, "SKILL"))
	body, e := readMember(raw, m.Entries[0].Path, 1<<20)
	if e != nil {
		t.Fatal(e)
	}
	limitations := []string{}
	for i := 0; i < 1500; i++ {
		limitations = append(limitations, strings.Repeat("<", 1000))
	}
	m.Requirements.TestedConfigurations = []TestedConfiguration{{ConfigurationID: "synthetic", ModelCapabilities: []string{}, PresentationCapabilities: []string{}, EvidencePath: m.Entries[0].Path, EvidenceKind: "SYNTHETIC_MOCK", Limitations: limitations}}
	var manifest bytes.Buffer
	encoder := json.NewEncoder(&manifest)
	encoder.SetEscapeHTML(false)
	if e = encoder.Encode(m); e != nil {
		t.Fatal(e)
	}
	if manifest.Len() >= 4<<20 {
		t.Fatal("fixture original manifest unexpectedly oversized")
	}
	if _, findings := ParseManifest(manifest.Bytes(), DefaultLimits); len(findings) > 0 {
		t.Fatalf("fixture is not schema-valid: %v", findings)
	}
	var container bytes.Buffer
	z := zip.NewWriter(&container)
	for _, part := range []struct {
		path string
		body []byte
	}{{"manifest.json", manifest.Bytes()}, {m.Entries[0].Path, body}} {
		w, e := z.CreateHeader(&zip.FileHeader{Name: part.path, Method: zip.Store})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(part.body); e != nil {
			t.Fatal(e)
		}
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	raw = container.Bytes()
	if rep := Check(raw, DefaultLimits); rep.Verdict != "PASS" {
		t.Fatalf("preflight did not reach plan bound: %v", rep.Findings)
	}
	plan, findings := BuildPortablePlan(raw, DefaultLimits, m.Entrypoints[0], strings.Repeat("a", 64))
	if !reflect.DeepEqual(plan, PortablePlan{}) || !has(findings, CodePortablePlanLimit) {
		t.Fatalf("oversized plan escaped: %+v", findings)
	}
}

func TestPortablePlanHashExactExcludedFieldBoundary(t *testing.T) {
	p := PortablePlan{PlanVersion: "1", Body: json.RawMessage(`{}`), IDs: []PortableID{}, References: []PortableReference{}, Files: []PortableFile{}}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(mustJSON(t, p), &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "plan_sha256")
	preimage := mustJSON(t, fields)
	limit := int64(len(preimage))
	canonical, err := wire.CanonicalBounded(preimage, limit)
	if err != nil {
		t.Fatal(err)
	}
	want := portableDigest(canonical)
	for _, ownHash := range []string{"", strings.Repeat("a", 64)} {
		p.PlanSHA256 = ownHash
		got, err := portablePlanHash(p, limit)
		if err != nil || got != want {
			t.Fatalf("exact excluded-field preimage rejected or own hash leaked: got=%s err=%v", got, err)
		}
		if _, err = portablePlanHash(p, limit-1); err == nil {
			t.Fatal("one-byte oversized preimage accepted")
		}
	}
}
