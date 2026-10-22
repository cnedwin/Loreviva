package wepkg

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,95}$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[A-Za-z0-9.-]+)?$`)
	hashPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Rights and Provenance are only read here: the manifest schema already
// requires them, so the preflight checks the semantic layer the schema cannot
// (see policy.go) rather than repeating required-field checks.
type Rights struct {
	License               string   `json:"license"`
	SourceReference       string   `json:"source_reference"`
	RedistributionAllowed bool     `json:"redistribution_allowed"`
	AllowedUses           []string `json:"allowed_uses"`
}

type FileRecord struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
	Rights    Rights `json:"rights"`
}

type Entry struct {
	LogicalID  string   `json:"logical_id"`
	Kind       string   `json:"kind"`
	Path       string   `json:"path"`
	Version    string   `json:"version"`
	SHA256     string   `json:"sha256"`
	References []string `json:"references"`
}

type Dependency struct {
	PackageID      string   `json:"package_id"`
	PackageVersion string   `json:"package_version"`
	ContentSHA256  string   `json:"content_sha256"`
	LogicalExports []string `json:"logical_exports"`
}

type Capability struct {
	CapabilityID string `json:"capability_id"`
	Required     bool   `json:"required"`
}

type Requirements struct {
	Model        []Capability `json:"model_capabilities"`
	Tool         []Capability `json:"tool_capabilities"`
	Presentation []Capability `json:"presentation_capabilities"`
}

type Provenance struct {
	AuthorDisplayName string   `json:"author_display_name"`
	License           string   `json:"license"`
	SourceReferences  []string `json:"source_references"`
}

type Manifest struct {
	FormatVersion  string       `json:"format_version"`
	PackageID      string       `json:"package_id"`
	Kind           string       `json:"kind"`
	PackageVersion string       `json:"package_version"`
	ContentSHA256  string       `json:"content_sha256"`
	Entrypoints    []string     `json:"entrypoints"`
	Entries        []Entry      `json:"entries"`
	Files          []FileRecord `json:"files"`
	Dependencies   []Dependency `json:"dependencies"`
	Requirements   Requirements `json:"requirements"`
	Provenance     Provenance   `json:"provenance"`
}

var (
	knownKinds      = map[string]bool{"WORLD": true, "CHARACTER": true, "ROLE": true, "SKILL": true, "KNOWLEDGE": true, "PRESENTATION_THEME": true}
	knownEntryKinds = map[string]bool{"WORLD": true, "CHARACTER": true, "ROLE": true, "SKILL": true, "KNOWLEDGE": true, "PRESENTATION_THEME": true, "PRODUCT_PACK": true, "RULES": true, "SCHEMA": true}
	knownMedia      = map[string]bool{"application/json": true, "text/plain": true, "text/markdown": true, "application/pdf": true,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true}
	knownUses = map[string]bool{"WORLD_CONTENT": true, "CHARACTER_CONTENT": true, "SKILL_CONTENT": true,
		"KNOWLEDGE": true, "PRESENTATION": true, "SYNTHETIC_TEST": true}
	rootKinds = map[string]bool{"WORLD": true, "CHARACTER": true, "ROLE": true, "SKILL": true,
		"KNOWLEDGE": true, "PRESENTATION_THEME": true}
	manifestFields = map[string]bool{"format_version": true, "package_id": true, "kind": true, "package_version": true,
		"content_sha256": true, "compatibility": true, "entrypoints": true, "entries": true, "files": true,
		"dependencies": true, "requirements": true, "provenance": true}
)

// ParseManifest decodes the manifest with the manifest ceiling and validates it
// against the published schema's required fields, patterns and enumerations.
// Unknown top-level members are refused rather than ignored: a package that
// carries a field this runtime does not understand must not be imported as if
// the field did not exist.
func ParseManifest(raw []byte, lim Limits) (Manifest, []Finding) {
	var m Manifest
	if int64(len(raw)) > lim.ManifestBytes {
		return m, []Finding{{Severity: Error, Code: CodeManifestInvalid,
			Detail: fmt.Sprintf("manifest %d bytes exceeds %d", len(raw), lim.ManifestBytes)}}
	}
	var top map[string]json.RawMessage
	if e := json.Unmarshal(raw, &top); e != nil {
		return m, []Finding{{Severity: Error, Code: CodeManifestInvalid, Detail: "manifest is not a JSON object"}}
	}
	var findings []Finding
	for key := range top {
		if !manifestFields[key] {
			findings = append(findings, Finding{Severity: Error, Code: CodeManifestSchema, Object: key, Detail: "unknown manifest member"})
		}
	}
	if e := json.Unmarshal(raw, &m); e != nil {
		return m, []Finding{{Severity: Error, Code: CodeManifestInvalid, Detail: "manifest does not match the declared shape"}}
	}

	check := func(ok bool, object, detail string) {
		if !ok {
			findings = append(findings, Finding{Severity: Error, Code: CodeManifestSchema, Object: object, Detail: detail})
		}
	}
	check(m.FormatVersion == "1.0", "format_version", "must be 1.0")
	check(idPattern.MatchString(m.PackageID), "package_id", "must match [a-z][a-z0-9._-]")
	check(knownKinds[m.Kind], "kind", "unknown package kind")
	check(versionPattern.MatchString(m.PackageVersion), "package_version", "must be semver")
	check(hashPattern.MatchString(m.ContentSHA256), "content_sha256", "must be 64 lowercase hex")
	check(len(m.Entrypoints) > 0, "entrypoints", "must not be empty")
	check(len(m.Files) > 0, "files", "must not be empty")
	check(len(m.Entries) > 0, "entries", "must not be empty")
	check(len(m.Provenance.AuthorDisplayName) > 0, "provenance.author_display_name", "required")
	check(len(m.Provenance.License) > 0, "provenance.license", "required")
	check(len(m.Provenance.SourceReferences) > 0, "provenance.source_references", "required")
	if len(m.Files) > lim.Entries || len(m.Entries) > lim.Entries {
		findings = append(findings, Finding{Severity: Error, Code: CodeManifestSchema, Object: "files", Detail: fmt.Sprintf("more than %d records", lim.Entries)})
	}

	for i, f := range m.Files {
		object := fmt.Sprintf("files[%d]", i)
		check(validatePath(f.Path, lim) == "", object+".path", "unsafe path")
		check(hashPattern.MatchString(f.SHA256), object+".sha256", "must be 64 lowercase hex")
		check(f.SizeBytes >= 0, object+".size_bytes", "must not be negative")
		check(knownMedia[f.MediaType], object+".media_type", "unsupported media type")
		check(f.Rights.License != "", object+".rights.license", "required")
		check(f.Rights.SourceReference != "", object+".rights.source_reference", "required")
		check(len(f.Rights.AllowedUses) > 0, object+".rights.allowed_uses", "must not be empty")
		if !f.Rights.RedistributionAllowed {
			findings = append(findings, Finding{Severity: Error, Code: CodeRightsNotDistributable, Path: f.Path, Object: object,
				Detail: "the source may not be redistributed"})
		}
		for _, use := range f.Rights.AllowedUses {
			check(knownUses[use], object+".rights.allowed_uses", "unknown use "+use)
		}
	}
	for i, e := range m.Entries {
		object := fmt.Sprintf("entries[%d]", i)
		check(idPattern.MatchString(e.LogicalID), object+".logical_id", "must match [a-z][a-z0-9._-]")
		check(knownEntryKinds[e.Kind], object+".kind", "unknown entry kind")
		check(validatePath(e.Path, lim) == "", object+".path", "unsafe path")
		check(versionPattern.MatchString(e.Version), object+".version", "must be semver")
		check(hashPattern.MatchString(e.SHA256), object+".sha256", "must be 64 lowercase hex")
	}
	for i, d := range m.Dependencies {
		object := fmt.Sprintf("dependencies[%d]", i)
		check(idPattern.MatchString(d.PackageID), object+".package_id", "must match [a-z][a-z0-9._-]")
		check(versionPattern.MatchString(d.PackageVersion), object+".package_version", "must be semver")
		check(hashPattern.MatchString(d.ContentSHA256), object+".content_sha256", "must be 64 lowercase hex")
	}
	Order(findings)
	return m, findings
}
