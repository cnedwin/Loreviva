package sourceawarereply

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"loreviva.local/loreviva/internal/wepkg"
)

func TestSourceDefinitionUsesCanonicalPortableContract(t *testing.T) {
	m, findings := wepkg.ParseManifest(manifestBytes, wepkg.DefaultLimits)
	if len(findings) > 0 {
		t.Fatal(findings)
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	v, _ := z.Create("manifest.json")
	raw, _ := json.Marshal(m)
	v.Write(raw)
	v, _ = z.Create("definitions.json")
	v.Write(definitionBytes)
	z.Close()
	report := wepkg.Check(b.Bytes(), wepkg.DefaultLimits)
	if report.Verdict != "PASS" {
		t.Fatalf("source admission findings: %+v", report.Findings)
	}
}

func TestRepositorySkillArchiveIsReproducible(t *testing.T) {
	a, m, e := Build()
	if e != nil {
		t.Fatal(e)
	}
	b, _, e := Build()
	if e != nil || !bytes.Equal(a, b) {
		t.Fatal("build is not deterministic", e)
	}
	_, file, _, _ := runtime.Caller(0)
	stored, e := os.ReadFile(filepath.Join(filepath.Dir(file), "source-aware-reply.wepkg"))
	if e != nil || !bytes.Equal(a, stored) {
		t.Fatal("checked-in archive differs from actual source", e)
	}
	if report := wepkg.Check(stored, wepkg.DefaultLimits); report.Verdict != "PASS" {
		t.Fatal(report.Findings)
	}
	if m.Provenance.AuthorDisplayName != "Loreviva contributors" || m.Provenance.License != "Apache-2.0" || len(m.Requirements.Tool) != 0 || len(m.Requirements.TestedConfigurations) != 0 {
		t.Fatal("example metadata authority or unsupported tested claim")
	}
}
func TestRepositorySkillMethodHashAndUnknownTools(t *testing.T) {
	original, m, e := Build()
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]any
	if e = json.Unmarshal(Source(), &doc); e != nil {
		t.Fatal(e)
	}
	var skill map[string]any
	for _, v := range doc["objects"].([]any) {
		o := v.(map[string]any)
		if o["kind"] == "SKILL" {
			skill = o
		}
	}
	if skill == nil {
		t.Fatal("actual source lacks Skill")
	}
	skill["description"] = "Changed authored method"
	changed, _ := json.Marshal(doc)
	out, updated, e := wepkg.ExportBundle(m, changed, nil)
	if e != nil || bytes.Equal(original, out) || updated.ContentSHA256 == m.ContentSHA256 {
		t.Fatal("method edit failed to change digest", e)
	}
	skill["tool_capability_ids"] = []string{"unregistered-network-tool"}
	bad, _ := json.Marshal(doc)
	if out, _, e = wepkg.ExportBundle(m, bad, nil); e == nil || len(out) != 0 {
		t.Fatal("unknown tool installed by example")
	}
}
