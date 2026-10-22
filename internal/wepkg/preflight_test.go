package wepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// schemaExample returns the manifest published inside the format schema. It is
// the one package the format itself calls valid, so the preflight must agree
// with it — including the content digest, which is the check that most easily
// drifts from the definition.
func schemaExample(t *testing.T) Manifest {
	t.Helper()
	raw, e := os.ReadFile("../../contracts/source/wepkg.schema.json")
	if e != nil {
		t.Skip("format schema not available in this checkout")
	}
	var schema struct {
		Examples []Manifest `json:"examples"`
	}
	if e := json.Unmarshal(raw, &schema); e != nil || len(schema.Examples) == 0 {
		t.Fatalf("schema example: %v", e)
	}
	return schema.Examples[0]
}

func TestContentHashMatchesPublishedExample(t *testing.T) {
	m := schemaExample(t)
	got, e := ContentHash(m)
	if e != nil {
		t.Fatal(e)
	}
	if got != m.ContentSHA256 {
		t.Fatalf("content digest disagrees with the published example: got %s want %s", got, m.ContentSHA256)
	}
	if !strings.HasPrefix(got, "c17bd376") {
		t.Fatalf("unexpected digest %s", got)
	}
	if findings := CheckContentHash(m); len(findings) != 0 {
		t.Fatalf("published example rejected: %v", codes(findings))
	}
}

func TestContentDigestCoversFileRecordsOnly(t *testing.T) {
	m := schemaExample(t)
	before, _ := ContentHash(m)
	// Changing metadata that is not part of the content digest must not change
	// it; changing a file record must.
	m.Files[0].MediaType = "text/markdown"
	after, _ := ContentHash(m)
	if before != after {
		t.Fatal("media type changed the content digest")
	}
	m.Files[0].SizeBytes++
	changed, _ := ContentHash(m)
	if changed == before {
		t.Fatal("file record change did not change the content digest")
	}
}

func TestParseManifestRejectsUnknownMemberAndBadIDs(t *testing.T) {
	_, findings := ParseManifest([]byte(`{"format_version":"1.0","package_id":"Bad ID","kind":"NOPE","package_version":"1","content_sha256":"zz","entrypoints":[],"entries":[],"files":[],"dependencies":[],"requirements":{},"provenance":{},"extra":1}`), DefaultLimits)
	for _, want := range []string{CodeManifestSchema} {
		if !has(findings, want) {
			t.Fatalf("missing %s: %v", want, codes(findings))
		}
	}
	// The unknown member must be named, not swallowed.
	found := false
	for _, f := range findings {
		if f.Object == "extra" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown member not reported: %v", findings)
	}
}

func TestCheckFilesReportsBothDirections(t *testing.T) {
	m := schemaExample(t)
	idx := Index{Members: []Member{
		{Path: "definitions/coffee.json", Size: uint64(m.Files[0].SizeBytes), SHA256: m.Files[0].SHA256},
		{Path: "definitions/extra.json", Size: 3, SHA256: strings.Repeat("a", 64)},
	}}
	findings := CheckFiles(m, idx)
	if !has(findings, CodeFileUndeclared) {
		t.Fatalf("undeclared member accepted: %v", codes(findings))
	}

	idx.Members[0].SHA256 = strings.Repeat("b", 64)
	findings = CheckFiles(m, idx)
	if !has(findings, CodeFileHashMismatch) {
		t.Fatalf("hash mismatch accepted: %v", codes(findings))
	}
	if has(findings, CodeFileSizeMismatch) {
		t.Fatalf("hash mismatch polluted by size finding: %v", codes(findings))
	}

	idx.Members = nil
	findings = CheckFiles(m, idx)
	if !has(findings, CodeFileMissing) {
		t.Fatalf("missing file accepted: %v", codes(findings))
	}
}

func TestCheckGraphCoversReferencesEntrypointsAndDependencies(t *testing.T) {
	m := schemaExample(t)
	m.Entries[0].SHA256 = strings.Repeat("c", 64)
	if findings := CheckGraph(m); !has(findings, CodeEntryHashMismatch) {
		t.Fatalf("entry/file digest mismatch accepted: %v", codes(findings))
	}

	m = schemaExample(t)
	m.Entrypoints = []string{"missing_entry"}
	if findings := CheckGraph(m); !has(findings, CodeEntrypointUnknown) {
		t.Fatalf("unknown entrypoint accepted: %v", codes(findings))
	}

	m = schemaExample(t)
	m.Entries[0].References = []string{"nowhere"}
	if findings := CheckGraph(m); !has(findings, CodeReferenceUnresolved) {
		t.Fatalf("unresolved reference accepted: %v", codes(findings))
	}

	m = schemaExample(t)
	m.Dependencies = []Dependency{{PackageID: m.PackageID, PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("d", 64)}}
	if findings := CheckGraph(m); !has(findings, CodeDependencySelf) {
		t.Fatalf("self dependency accepted: %v", codes(findings))
	}

	m = schemaExample(t)
	m.Dependencies = []Dependency{
		{PackageID: "other", PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("d", 64)},
		{PackageID: "other", PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("d", 64)},
	}
	if findings := CheckGraph(m); !has(findings, CodeDependencyDuplicate) {
		t.Fatalf("duplicate dependency accepted: %v", codes(findings))
	}

	m = schemaExample(t)
	m.Entries[0].References = []string{"exported"}
	m.Dependencies = []Dependency{{PackageID: "other", PackageVersion: "1.0.0", ContentSHA256: strings.Repeat("d", 64), LogicalExports: []string{"exported"}}}
	if findings := CheckGraph(m); len(findings) != 0 {
		t.Fatalf("dependency export not resolved: %v", codes(findings))
	}
}

func TestCheckGraphRejectsUnregisteredCapability(t *testing.T) {
	m := schemaExample(t)
	m.Requirements.Tool = []Capability{{CapabilityID: "teleport", Required: true}}
	if findings := CheckGraph(m); !has(findings, CodeCapabilityUnknown) {
		t.Fatalf("unregistered capability accepted: %v", codes(findings))
	}
}

func TestCheckRulePrimitivesRefusesUnknownPrimitive(t *testing.T) {
	m := schemaExample(t)
	if findings := CheckRulePrimitives(m, []byte(`{"primitives":["attribute","time"]}`)); len(findings) != 0 {
		t.Fatalf("declarative primitives refused: %v", codes(findings))
	}
	if findings := CheckRulePrimitives(m, []byte(`{"primitives":["eval"]}`)); !has(findings, CodePrimitiveForbidden) {
		t.Fatalf("scripting primitive accepted: %v", codes(findings))
	}
}

func TestCheckEndToEndRejectsAndAccepts(t *testing.T) {
	body := []byte(`{"name":"coffee"}`)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	records := []fileDigest{{Path: "definitions/coffee.json", SHA256: digest, SizeBytes: int64(len(body))}}
	raw, _ := json.Marshal(records)
	canonical := raw
	content, e := canonicalDigest(canonical)
	if e != nil {
		t.Fatal(e)
	}
	manifest := map[string]any{
		"format_version": "1.0", "package_id": "coffee.demo", "kind": "WORLD", "package_version": "1.0.0",
		"content_sha256": content,
		"compatibility":  map[string]any{"package_format_major": 1},
		"entrypoints":    []string{"coffee_demo"},
		"entries": []any{map[string]any{"logical_id": "coffee_demo", "kind": "WORLD", "path": "definitions/coffee.json",
			"version": "1.0.0", "sha256": digest, "references": []string{}}},
		"files": []any{map[string]any{"path": "definitions/coffee.json", "sha256": digest, "size_bytes": len(body),
			"media_type": "application/json", "rights": map[string]any{"license": "CC0-1.0", "source_reference": "synthetic",
				"redistribution_allowed": true, "allowed_uses": []string{"WORLD_CONTENT"}}}},
		"dependencies": []any{},
		"requirements": map[string]any{"model_capabilities": []any{}, "tool_capabilities": []any{}, "presentation_capabilities": []any{
			map[string]any{"capability_id": "text", "required": true}}},
		"provenance": map[string]any{"author_display_name": "test", "license": "CC0-1.0", "source_references": []string{"synthetic"}},
	}
	manifestRaw, _ := json.Marshal(manifest)
	archive := build(t, member{name: "manifest.json", body: manifestRaw}, member{name: "definitions/coffee.json", body: body})

	rep := Check(archive, DefaultLimits)
	if rep.Verdict != "PASS" {
		t.Fatalf("valid package rejected: %v", rep.Findings)
	}
	again := Check(archive, DefaultLimits)
	if again.ReportSHA256 != rep.ReportSHA256 {
		t.Fatal("report digest is not deterministic")
	}

	// Corrupt the file body but leave the manifest untouched: the file digest
	// must catch it, and the content digest must not be blamed.
	broken := build(t, member{name: "manifest.json", body: manifestRaw}, member{name: "definitions/coffee.json", body: []byte(`{"name":"tea"}`)})
	rejected := Check(broken, DefaultLimits)
	if !has(rejected.Findings, CodeFileHashMismatch) {
		t.Fatalf("tampered body accepted: %v", codes(rejected.Findings))
	}
	if has(rejected.Findings, CodeContentHashMismatch) {
		t.Fatalf("content digest blamed for a body change: %v", codes(rejected.Findings))
	}
}

func TestParseManifestRefusesNonRedistributableSource(t *testing.T) {
	m := schemaExample(t)
	m.Files[0].Rights.RedistributionAllowed = false
	if _, findings := ParseManifest(mustJSON(t, m), DefaultLimits); !has(findings, CodeRightsNotDistributable) {
		t.Fatalf("non-redistributable source accepted: %v", codes(findings))
	}
}

func TestParseManifestRefusesUnknownUse(t *testing.T) {
	m := schemaExample(t)
	m.Files[0].Rights.AllowedUses = []string{"WORLD_CONTENT", "EVERYTHING"}
	if _, findings := ParseManifest(mustJSON(t, m), DefaultLimits); !has(findings, CodeManifestSchema) {
		t.Fatalf("unknown use accepted: %v", codes(findings))
	}
}

func TestCheckGraphRefusesNonRootEntrypoint(t *testing.T) {
	m := schemaExample(t)
	m.Entries[0].Kind = "SCHEMA"
	if findings := CheckGraph(m); !has(findings, CodeEntrypointKind) {
		t.Fatalf("schema entrypoint accepted: %v", codes(findings))
	}
}

func TestCheckRefusesUnreadableEntryBody(t *testing.T) {
	m := schemaExample(t)
	body := []byte("not json at all")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	m.Files[0] = FileRecord{Path: m.Entries[0].Path, SHA256: digest, SizeBytes: int64(len(body)), MediaType: "application/json",
		Rights: Rights{License: "CC0-1.0", SourceReference: "synthetic", RedistributionAllowed: true, AllowedUses: []string{"WORLD_CONTENT"}}}
	m.Entries[0].SHA256 = digest
	content, e := ContentHash(m)
	if e != nil {
		t.Fatal(e)
	}
	m.ContentSHA256 = content
	archive := build(t, member{name: "manifest.json", body: mustJSON(t, m)}, member{name: m.Entries[0].Path, body: body})
	if rep := Check(archive, DefaultLimits); !has(rep.Findings, CodeEntryBodyInvalid) {
		t.Fatalf("unreadable entry body accepted: %v", codes(rep.Findings))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
