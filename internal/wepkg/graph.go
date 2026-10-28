package wepkg

import (
	"encoding/json"
	"fmt"

	"loreviva.local/loreviva/internal/wire"
)

// RegisteredCapabilities are the capability identifiers this runtime can map.
// They are published with the preflight limits so an author sees the same set
// the check uses; an unlisted identifier is refused instead of silently
// ignored. The values mirror what the runtime actually registers today.
var RegisteredCapabilities = map[string]bool{
	"text":         true,
	"sign_custody": true,
}

// RulePrimitives are the declarative primitives a RULES entry may use, per the
// requirement that rules describe conditions rather than run code.
var RulePrimitives = map[string]bool{
	"attribute": true, "identity": true, "location": true, "item": true,
	"relationship": true, "time": true, "event": true, "budget": true,
}

// CheckGraph validates the manifest's internal references: entries point at
// declared files with matching digests, entrypoints name real entries,
// references resolve locally or into a declared dependency export, and the
// dependency list has no self or duplicate entries. Cross-package cycles are
// out of scope: a dependency's own manifest is not part of this archive.
func CheckGraph(m Manifest) []Finding {
	var findings []Finding
	files := map[string]FileRecord{}
	for _, f := range m.Files {
		files[f.Path] = f
	}
	entries := map[string]bool{}
	for i, e := range m.Entries {
		object := fmt.Sprintf("entries[%d]", i)
		entries[e.LogicalID] = true
		f, ok := files[e.Path]
		if !ok {
			findings = append(findings, Finding{Severity: Error, Code: CodeEntryFileMissing, Path: e.Path, Object: object,
				Detail: "entry path is not a declared file"})
			continue
		}
		if f.SHA256 != e.SHA256 {
			findings = append(findings, Finding{Severity: Error, Code: CodeEntryHashMismatch, Path: e.Path, Object: object,
				Detail: fmt.Sprintf("entry %s, file %s", ShortHash(e.SHA256), ShortHash(f.SHA256))})
		}
	}

	kinds := map[string]string{}
	for _, e := range m.Entries {
		kinds[e.LogicalID] = e.Kind
	}
	for _, id := range m.Entrypoints {
		if !entries[id] {
			findings = append(findings, Finding{Severity: Error, Code: CodeEntrypointUnknown, Object: "entrypoints",
				Detail: "names an unknown entry"})
			continue
		}
		if !rootKinds[kinds[id]] {
			findings = append(findings, Finding{Severity: Error, Code: CodeEntrypointKind, Object: "entrypoints",
				Detail: "entry kind is not a publishable root"})
		}
	}

	exports := map[string]bool{}
	for i, d := range m.Dependencies {
		object := fmt.Sprintf("dependencies[%d]", i)
		if d.PackageID == m.PackageID {
			findings = append(findings, Finding{Severity: Error, Code: CodeDependencySelf, Object: object,
				Detail: "package depends on itself"})
		}
		for j := 0; j < i; j++ {
			if m.Dependencies[j].PackageID == d.PackageID && m.Dependencies[j].PackageVersion == d.PackageVersion {
				findings = append(findings, Finding{Severity: Error, Code: CodeDependencyDuplicate, Object: object,
					Detail: "dependency declared twice"})
			}
		}
		for _, x := range d.LogicalExports {
			exports[x] = true
		}
	}

	for i, e := range m.Entries {
		for _, ref := range e.References {
			if !entries[ref] && !exports[ref] {
				findings = append(findings, Finding{Severity: Error, Code: CodeReferenceUnresolved,
					Object: fmt.Sprintf("entries[%d]", i), Detail: "unresolved logical reference"})
			}
		}
	}

	for _, group := range [][]Capability{m.Requirements.Model, m.Requirements.Presentation} {
		for _, c := range group {
			if !RegisteredCapabilities[c.CapabilityID] {
				findings = append(findings, Finding{Severity: Error, Code: CodeCapabilityUnknown,
					Object: "requirements", Detail: "unregistered capability"})
			}
		}
	}
	for _, c := range m.Requirements.Tool {
		if !RegisteredCapabilities[c.ToolCapabilityID] {
			findings = append(findings, Finding{Severity: Error, Code: CodeCapabilityUnknown, Object: "requirements", Detail: "unregistered capability"})
		}
	}
	Order(findings)
	return findings
}

// CheckRulePrimitives refuses a RULES entry that uses anything outside the
// declarative primitive set. The entry body is JSON; an unknown primitive name
// is refused rather than ignored, because ignoring it would execute a rule the
// runtime does not understand.
func CheckRulePrimitives(m Manifest, body []byte) []Finding {
	var doc struct {
		Primitives []string `json:"primitives"`
	}
	if e := decodeStrict(body, &doc); e != nil {
		return []Finding{{Severity: Error, Code: CodeManifestInvalid, Object: "rules", Detail: "rules body is not readable"}}
	}
	var findings []Finding
	for _, p := range doc.Primitives {
		if !RulePrimitives[p] {
			findings = append(findings, Finding{Severity: Error, Code: CodePrimitiveForbidden, Object: "rules", Detail: "unsupported rule primitive"})
		}
	}
	Order(findings)
	return findings
}

func decodeStrict(raw []byte, v any) error {
	// The repository's own strict reader enforces UTF-8, duplicate keys and the
	// JSON depth/size ceiling, so rule bodies get the same treatment as prompts.
	_, e := wire.Canonical(raw)
	if e != nil {
		return e
	}
	return json.Unmarshal(raw, v)
}
