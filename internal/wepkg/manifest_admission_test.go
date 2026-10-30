package wepkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"loreviva.local/loreviva/internal/generated"
)

func admissionExample(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/source/wepkg.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Examples []map[string]any `json:"examples"`
	}
	if err = json.Unmarshal(raw, &schema); err != nil || len(schema.Examples) != 1 {
		t.Fatal("source example", err)
	}
	return schema.Examples[0]
}
func admissionRaw(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func admissionObject(t *testing.T, m map[string]any, path ...string) map[string]any {
	t.Helper()
	var current any = m
	for _, part := range path {
		if part == "0" {
			current = current.([]any)[0]
		} else {
			current = current.(map[string]any)[part]
		}
	}
	return current.(map[string]any)
}

// This catches missing full-schema validation, duplicate-name acceptance and
// partial DTOs escaping an unsuccessful admission. Fixtures use the sole source.
func TestManifestAdmissionMatchesSoleSchema(t *testing.T) {
	if m, f := ParseManifest(admissionRaw(t, admissionExample(t)), DefaultLimits); len(f) != 0 || m.PackageID != "coffee.demo" {
		t.Fatalf("source example refused: %v", f)
	}
	groups := []struct {
		path     []string
		required []string
	}{
		{nil, []string{"format_version", "package_id", "kind", "package_version", "content_sha256", "compatibility", "entrypoints", "entries", "files", "dependencies", "requirements", "provenance"}},
		{[]string{"compatibility"}, []string{"public_api_major", "package_format_major", "minimum_runtime_contract", "rule_primitive_set"}},
		{[]string{"files", "0"}, []string{"path", "sha256", "size_bytes", "media_type", "rights"}},
		{[]string{"files", "0", "rights"}, []string{"license", "source_reference", "redistribution_allowed", "allowed_uses"}},
		{[]string{"entries", "0"}, []string{"logical_id", "kind", "path", "version", "sha256", "references"}},
		{[]string{"requirements"}, []string{"model_capabilities", "tool_capabilities", "presentation_capabilities", "tested_configurations"}},
		{[]string{"provenance"}, []string{"author_display_name", "created_at", "license", "source_references", "synthetic_examples_only"}},
	}
	for _, group := range groups {
		for _, key := range group.required {
			t.Run("missing_"+strings.Join(append(append([]string{}, group.path...), key), "_"), func(t *testing.T) {
				doc := admissionExample(t)
				delete(admissionObject(t, doc, group.path...), key)
				m, f := ParseManifest(admissionRaw(t, doc), DefaultLimits)
				if len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
					t.Fatalf("incomplete manifest admitted or partial DTO escaped: %s", key)
				}
			})
		}
		t.Run("unknown_"+strings.Join(group.path, "_"), func(t *testing.T) {
			doc := admissionExample(t)
			admissionObject(t, doc, group.path...)["unexpected"] = true
			m, f := ParseManifest(admissionRaw(t, doc), DefaultLimits)
			if len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
				t.Fatal("unknown member admitted or partial DTO escaped")
			}
		})
	}
	for _, kind := range []string{"duplicate", "escaped_duplicate", "trailing", "invalid_datetime", "rights_false", "rights_null", "created_null", "unknown_extra"} {
		t.Run(kind, func(t *testing.T) {
			doc := admissionExample(t)
			raw := admissionRaw(t, doc)
			switch kind {
			case "duplicate":
				raw = append([]byte(`{"package_id":"shadow",`), raw[1:]...)
			case "escaped_duplicate":
				raw = append([]byte(`{"package_\u0069d":"shadow",`), raw[1:]...)
			case "trailing":
				raw = append(raw, []byte(" true")...)
			case "invalid_datetime":
				admissionObject(t, doc, "provenance")["created_at"] = "2026-99-99T99:00:00Z"
				raw = admissionRaw(t, doc)
			case "rights_false":
				admissionObject(t, doc, "files", "0", "rights")["redistribution_allowed"] = false
				raw = admissionRaw(t, doc)
			case "rights_null":
				admissionObject(t, doc, "files", "0", "rights")["redistribution_allowed"] = nil
				raw = admissionRaw(t, doc)
			case "created_null":
				admissionObject(t, doc, "provenance")["created_at"] = nil
				raw = admissionRaw(t, doc)
			case "unknown_extra":
				doc["extra"] = true
				raw = admissionRaw(t, doc)
			}
			m, f := ParseManifest(raw, DefaultLimits)
			if len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
				t.Fatal("bad manifest admitted or partial DTO escaped")
			}
			if kind == "rights_false" && !has(f, CodeRightsNotDistributable) {
				t.Fatal("stable explicit-false refusal missing")
			}
		})
	}
}

// The digest must admit a legal record preimage beyond the ordinary wire limit
// and still equal independently marshaled, sorted file records.
func TestContentHashAcceptsLegalLargeRecordPreimage(t *testing.T) {
	m := Manifest{Files: make([]FileRecord, 5000)}
	expected := make([]map[string]any, 5000)
	for i := 0; i < 5000; i++ {
		path := fmt.Sprintf("definitions/%04d-%s.json", i, strings.Repeat("x", 200))
		expected[i] = map[string]any{"path": path, "sha256": strings.Repeat("a", 64), "size_bytes": 0}
		m.Files[4999-i] = FileRecord{Path: path, SHA256: strings.Repeat("a", 64)}
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1<<20 {
		t.Fatal("fixture does not exercise the hidden wire ceiling")
	}
	sum := sha256.Sum256(raw)
	want := hex.EncodeToString(sum[:])
	got, err := ContentHash(m)
	if err != nil {
		t.Fatalf("legal large file-record digest rejected: %v", err)
	}
	if got != want {
		t.Fatal("large content digest differs from sorted canonical record bytes")
	}
}

func manifestArchive(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range map[string][]byte{"manifest.json": raw, "definitions/extra.json": []byte(`{"primitives":["eval"]}`)} {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestMalformedManifestProducesConformantRejectedReport(t *testing.T) {
	for _, kind := range []string{"long_id", "bad_hash", "long_unknown", "many_unknown", "wrong_kind", "secret_key", "numeric_key"} {
		t.Run(kind, func(t *testing.T) {
			doc := admissionExample(t)
			switch kind {
			case "long_id":
				doc["package_id"] = strings.Repeat("a", 300)
			case "bad_hash":
				doc["content_sha256"] = "not a hash"
			case "long_unknown":
				doc[strings.Repeat("sensitive", 1000)] = true
			case "many_unknown":
				for i := 0; i < 80; i++ {
					doc[fmt.Sprintf("extra_%02d", i)] = true
				}
			case "wrong_kind":
				doc["kind"] = "UNRECOGNIZED"
			case "secret_key":
				doc["syntheticcredential123"] = true
			case "numeric_key":
				doc["12345"] = true
			}
			archive := manifestArchive(t, admissionRaw(t, doc))
			rep := Check(archive, DefaultLimits)
			raw, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if err = generated.Validate("PackagePreflightReport", raw); err != nil {
				t.Fatalf("malformed manifest produces invalid report: %v", err)
			}
			if kind == "secret_key" && bytes.Contains(raw, []byte("syntheticcredential123")) {
				t.Fatal("unknown input property echoed as diagnostic location")
			}
			if kind == "numeric_key" {
				for _, f := range rep.Findings {
					if strings.Contains(f.Object, "12345") {
						t.Fatal("non-index numeric key echoed as diagnostic location")
					}
				}
			}
			if rep.Verdict != "REJECT" || rep.PackageID != "" || rep.PackageVersion != "" || rep.Kind != "" || rep.ContentHashDeclared != "" || rep.ContentHashComputed != "" {
				t.Fatal("untrusted manifest identity or computed state leaked")
			}
			if rep.ManifestSHA256 == "" || rep.ArchiveSHA256 == "" {
				t.Fatal("actual input fingerprints missing")
			}
			if len(rep.Findings) > 32 {
				t.Fatal("manifest diagnostics are not bounded")
			}
			if has(rep.Findings, CodeFileUndeclared) || has(rep.Findings, CodeFileMissing) || has(rep.Findings, CodeContentHashMismatch) {
				t.Fatal("semantic traversal ran after failed admission")
			}
			for i := 0; i < 5; i++ {
				again := Check(archive, DefaultLimits)
				a, _ := json.Marshal(again)
				if !bytes.Equal(raw, a) {
					t.Fatal("rejection report not deterministic")
				}
			}
		})
	}
}

func TestGeneratedManifestPathPatternParity(t *testing.T) {
	compiled, err := generated.CompileManifestPattern("^(?!/)(?!.*(?:^|/)\\.{1,2}(?:/|$))(?!.*[\\\\:])[A-Za-z0-9._/-]+$")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		input          string
		pattern, valid bool
	}{
		{"", false, false},
		{"world.json", true, true},
		{"a/b.json", true, true},
		{"/a", false, false},
		{"C:/a", false, false},
		{"a:b", false, false},
		{"a\\\\b", false, false},
		{".", false, false},
		{"..", false, false},
		{"a/.", false, false},
		{"a/..", false, false},
		{"a/../b", false, false},
		{"a//b", true, true},
		{"a.../b", true, true},
		{"\u00e9.json", false, false},
		{"a.json\n", false, false},
		{"a.json\r", false, false},
		{"a.json\r\n", false, false},
		{"a.json\u2028", false, false},
		{"a.json\u2029", false, false},
		{"a\n.json", false, false},
		{"a/..\n", false, false},
		{"../a\n", false, false},
		{"a.json\n\n", false, false},
	}
	for _, tc := range cases {
		if got := compiled.MatchString(tc.input); got != tc.pattern {
			t.Errorf("source JS pattern differs for %q: got %v want %v", tc.input, got, tc.pattern)
		}
		doc := admissionExample(t)
		admissionObject(t, doc, "files", "0")["path"] = tc.input
		err := generated.ValidateManifest(admissionRaw(t, doc), 8<<20)
		if (err == nil) != tc.valid {
			t.Errorf("AJV schema differs for %q: %v", tc.input, err)
		}
	}
}

func TestGeneratedManifestPatternTimeoutFailsClosed(t *testing.T) {
	pattern, err := generated.CompileManifestPattern(`(a+)+$`)
	if err != nil {
		t.Fatal(err)
	}
	if pattern.MatchString(strings.Repeat("a", 10000) + "!") {
		t.Fatal("timed-out/nonmatching pattern granted a match")
	}
	if !pattern.MatchString("aaaa") {
		t.Fatal("ordinary valid pattern refused")
	}
	if _, err = generated.CompileManifestPattern(`[`); err == nil {
		t.Fatal("invalid pattern compilation accepted")
	}
}

func admissionRichExample(t *testing.T) map[string]any {
	t.Helper()
	doc := admissionExample(t)
	doc["dependencies"] = []any{map[string]any{"package_id": "library.demo", "package_version": "1.0.0", "content_sha256": strings.Repeat("b", 64), "logical_exports": []any{"shared"}}}
	req := admissionObject(t, doc, "requirements")
	req["model_capabilities"] = []any{map[string]any{"capability_id": "text", "required": false, "schema_path": "schemas/text.json"}}
	req["tool_capabilities"] = []any{map[string]any{"tool_capability_id": "sign_custody", "registered_contract_version": "1.0.0", "required": false}}
	req["tested_configurations"] = []any{map[string]any{"configuration_id": "synthetic", "model_capabilities": []any{"text"}, "presentation_capabilities": []any{"text"}, "evidence_path": "evidence/mock.json", "evidence_kind": "SYNTHETIC_MOCK", "limitations": []any{"Synthetic only"}}}
	return doc
}

func TestManifestMetadataRoundTrip(t *testing.T) {
	doc := admissionRichExample(t)
	m, f := ParseManifest(admissionRaw(t, doc), DefaultLimits)
	if len(f) != 0 {
		t.Fatal(f)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(mustJSON(t, m), &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, roundTrip) {
		t.Fatal("valid manifest metadata was dropped or altered")
	}
	for _, group := range []struct {
		path []string
		keys []string
	}{
		{[]string{"dependencies", "0"}, []string{"package_id", "package_version", "content_sha256", "logical_exports"}},
		{[]string{"requirements", "model_capabilities", "0"}, []string{"capability_id", "required"}},
		{[]string{"requirements", "tool_capabilities", "0"}, []string{"tool_capability_id", "registered_contract_version", "required"}},
		{[]string{"requirements", "tested_configurations", "0"}, []string{"configuration_id", "model_capabilities", "presentation_capabilities", "evidence_path", "evidence_kind", "limitations"}},
	} {
		for _, key := range append(group.keys, "unexpected") {
			t.Run(strings.Join(group.path, "_")+"_"+key, func(t *testing.T) {
				bad := admissionRichExample(t)
				o := admissionObject(t, bad, group.path...)
				if key == "unexpected" {
					o[key] = true
				} else {
					delete(o, key)
				}
				m, f := ParseManifest(admissionRaw(t, bad), DefaultLimits)
				if len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
					t.Fatal("nested manifest schema violation admitted")
				}
			})
		}
	}
}

func TestManifestAdmissionLargeAndConstraintBoundaries(t *testing.T) {
	doc := admissionExample(t)
	files := make([]any, 5000)
	for i := range files {
		// Each object is independent so mutations cannot silently affect all rows.
		raw := admissionRaw(t, admissionObject(t, doc, "files", "0"))
		var file map[string]any
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			file["path"] = fmt.Sprintf("definitions/%04d-%s.json", i, strings.Repeat("x", 200))
		}
		files[i] = file
	}
	doc["files"] = files
	raw := admissionRaw(t, doc)
	if len(raw) <= 1<<20 || len(raw) > 8<<20 {
		t.Fatal("large manifest fixture outside declared boundary")
	}
	limits := DefaultLimits
	limits.ManifestBytes = int64(len(raw))
	if _, f := ParseManifest(raw, limits); len(f) != 0 {
		t.Fatal("exact explicit manifest ceiling refused", f)
	}
	limits.ManifestBytes--
	if m, f := ParseManifest(raw, limits); len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
		t.Fatal("manifest byte ceiling exceeded")
	}
	for _, kind := range []string{"negative_size", "over_size", "unknown_use", "duplicate_use", "invalid_version", "invalid_tool_version", "non_boolean", "bad_evidence", "unknown_compatibility"} {
		t.Run(kind, func(t *testing.T) {
			bad := admissionRichExample(t)
			switch kind {
			case "negative_size":
				admissionObject(t, bad, "files", "0")["size_bytes"] = -1
			case "over_size":
				admissionObject(t, bad, "files", "0")["size_bytes"] = 104857601
			case "unknown_use":
				admissionObject(t, bad, "files", "0", "rights")["allowed_uses"] = []any{"UNKNOWN"}
			case "duplicate_use":
				admissionObject(t, bad, "files", "0", "rights")["allowed_uses"] = []any{"WORLD_CONTENT", "WORLD_CONTENT"}
			case "invalid_version":
				bad["package_version"] = "latest"
			case "invalid_tool_version":
				admissionObject(t, bad, "requirements", "tool_capabilities", "0")["registered_contract_version"] = "latest"
			case "non_boolean":
				admissionObject(t, bad, "requirements", "model_capabilities", "0")["required"] = "false"
			case "bad_evidence":
				admissionObject(t, bad, "requirements", "tested_configurations", "0")["evidence_kind"] = "CLAIM"
			case "unknown_compatibility":
				admissionObject(t, bad, "compatibility")["extra"] = true
			}
			if m, f := ParseManifest(admissionRaw(t, bad), DefaultLimits); len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
				t.Fatal("Schema boundary admitted")
			}
		})
	}
}

func TestManifestIntegerRepresentations(t *testing.T) {
	original := admissionRaw(t, admissionExample(t))
	for _, tc := range []struct{ field, from, to string }{
		{"public_api_major", "1", "1.0"}, {"public_api_major", "1", "1e0"},
		{"package_format_major", "1", "1.0"}, {"package_format_major", "1", "1e0"},
		{"size_bytes", "83", "83.0"}, {"size_bytes", "83", "8.3e1"},
	} {
		t.Run(tc.field+"_"+tc.to, func(t *testing.T) {
			needle := []byte(`"` + tc.field + `":` + tc.from)
			if bytes.Count(original, needle) != 1 {
				t.Fatal("numeric fixture mismatch")
			}
			raw := bytes.Replace(original, needle, []byte(`"`+tc.field+`":`+tc.to), 1)
			if err := generated.ValidateManifest(raw, 8<<20); err != nil {
				t.Fatal("source-schema valid integer representation refused", err)
			}
			m, f := ParseManifest(raw, DefaultLimits)
			if len(f) != 0 || m.Compatibility.PublicAPIMajor != 1 || m.Files[0].SizeBytes != 83 {
				t.Fatal("valid integer representation lost during DTO admission", f)
			}
		})
	}
}

func TestManifestDiagnosticsChooseStableSmallestLocations(t *testing.T) {
	doc := admissionExample(t)
	entries := make([]any, 80)
	for i := range entries {
		entry := map[string]any{"logical_id": fmt.Sprintf("entry_%02d", i), "kind": "WORLD", "path": "definitions/coffee.json", "version": "1.0.0", "sha256": "bad", "references": []any{}}
		entries[i] = entry
	}
	doc["entries"] = entries
	_, first := ParseManifest(admissionRaw(t, doc), DefaultLimits)
	if len(first) != 32 {
		t.Fatalf("different schema locations were not capped at32: %d", len(first))
	}
	all := make([]string, 80)
	for i := range all {
		all[i] = fmt.Sprintf("entries.%d.sha256", i)
	}
	sort.Strings(all)
	for i, f := range first {
		if f.Object != all[i] {
			t.Fatalf("wrong stable location %d: %s", i, f.Object)
		}
	}
	for i := 0; i < 8; i++ {
		_, again := ParseManifest(admissionRaw(t, doc), DefaultLimits)
		if !reflect.DeepEqual(first, again) {
			t.Fatal("diagnostics depend on map traversal order")
		}
	}
}

func TestManifestDependencyArrayBoundaries(t *testing.T) {
	doc := admissionRichExample(t)
	dep := admissionObject(t, doc, "dependencies", "0")
	duplicates := admissionRichExample(t)
	admissionObject(t, duplicates, "dependencies", "0")["logical_exports"] = []any{"shared", "shared"}
	if m, f := ParseManifest(admissionRaw(t, duplicates), DefaultLimits); len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
		t.Fatal("duplicate logical export accepted")
	}
	deps := make([]any, 1000)
	for i := range deps {
		deps[i] = dep
	}
	doc["dependencies"] = deps
	if _, f := ParseManifest(admissionRaw(t, doc), DefaultLimits); len(f) != 0 {
		t.Fatal("schema-valid dependency array at ceiling refused", f)
	}
	doc["dependencies"] = append(deps, dep)
	if m, f := ParseManifest(admissionRaw(t, doc), DefaultLimits); len(f) == 0 || !reflect.DeepEqual(m, Manifest{}) {
		t.Fatal("dependency array beyond format maximum accepted")
	}
}
