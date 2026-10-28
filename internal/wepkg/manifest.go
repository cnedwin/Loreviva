package wepkg

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
)

var (
	idPattern              = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,95}$`)
	versionPattern         = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[A-Za-z0-9.-]+)?$`)
	hashPattern            = regexp.MustCompile(`^[a-f0-9]{64}$`)
	diagnosticIndexPattern = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// The generated manifest validator enforces required fields before decoding.
// Preflight then checks registered capabilities, references, and file integrity.
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
	SchemaPath   string `json:"schema_path,omitempty"`
}

type ToolRequirement struct {
	ToolCapabilityID          string `json:"tool_capability_id"`
	RegisteredContractVersion string `json:"registered_contract_version"`
	Required                  bool   `json:"required"`
}
type TestedConfiguration struct {
	ConfigurationID          string   `json:"configuration_id"`
	ModelCapabilities        []string `json:"model_capabilities"`
	PresentationCapabilities []string `json:"presentation_capabilities"`
	EvidencePath             string   `json:"evidence_path"`
	EvidenceKind             string   `json:"evidence_kind"`
	Limitations              []string `json:"limitations"`
}
type Compatibility struct {
	PublicAPIMajor         int64  `json:"public_api_major"`
	PackageFormatMajor     int64  `json:"package_format_major"`
	MinimumRuntimeContract string `json:"minimum_runtime_contract"`
	RulePrimitiveSet       string `json:"rule_primitive_set"`
}

type Requirements struct {
	Model                []Capability          `json:"model_capabilities"`
	Tool                 []ToolRequirement     `json:"tool_capabilities"`
	Presentation         []Capability          `json:"presentation_capabilities"`
	TestedConfigurations []TestedConfiguration `json:"tested_configurations"`
}

type Provenance struct {
	AuthorDisplayName     string   `json:"author_display_name"`
	License               string   `json:"license"`
	SourceReferences      []string `json:"source_references"`
	CreatedAt             string   `json:"created_at"`
	SyntheticExamplesOnly bool     `json:"synthetic_examples_only"`
}

type Manifest struct {
	FormatVersion  string        `json:"format_version"`
	PackageID      string        `json:"package_id"`
	Kind           string        `json:"kind"`
	PackageVersion string        `json:"package_version"`
	ContentSHA256  string        `json:"content_sha256"`
	Compatibility  Compatibility `json:"compatibility"`
	Entrypoints    []string      `json:"entrypoints"`
	Entries        []Entry       `json:"entries"`
	Files          []FileRecord  `json:"files"`
	Dependencies   []Dependency  `json:"dependencies"`
	Requirements   Requirements  `json:"requirements"`
	Provenance     Provenance    `json:"provenance"`
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
	if err := generated.ValidateManifest(raw, lim.ManifestBytes); err != nil {
		return m, manifestAdmissionFindings(raw, err)
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
	// Schema integer accepts exact decimal/exponent spellings. Normalize only
	// validated integer tokens for DTO decoding; validation and input hashes
	// always use the original bytes, never this derived representation.
	decoded, err := manifestDTOBytes(top)
	if err != nil {
		return Manifest{}, []Finding{{Severity: Error, Code: CodeManifestInvalid, Detail: "manifest integer representation is invalid"}}
	}
	if e := json.Unmarshal(decoded, &m); e != nil {
		return Manifest{}, []Finding{{Severity: Error, Code: CodeManifestInvalid, Detail: "manifest does not match the declared shape"}}
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

// Select bounded, sorted unique locations across the complete error tree. Never
// expose the schema library's rendered errors, which may include input values.
func manifestAdmissionFindings(raw []byte, err error) []Finding {
	if errors.Is(err, wire.ErrJSON) {
		return []Finding{{Severity: Error, Code: CodeManifestInvalid, Object: "manifest", Detail: "manifest is not strict bounded JSON"}}
	}
	locations := []string{}
	add := func(location string) {
		if location == "" {
			location = "manifest"
		}
		i := sort.SearchStrings(locations, location)
		if i < len(locations) && locations[i] == location {
			return
		}
		if i >= 32 {
			return
		}
		locations = append(locations, "")
		copy(locations[i+1:], locations[i:])
		locations[i] = location
		if len(locations) > 32 {
			locations = locations[:32]
		}
	}
	safe := func(parts []string) string {
		out := ""
		for i, part := range parts {
			// Only format-owned field names and array indices are displayable.
			// Arbitrary property names may themselves contain private payloads.
			index := i > 0 && manifestDiagnosticArray(parts[i-1]) && diagnosticIndexPattern.MatchString(part)
			if !manifestDiagnosticField(part) && !index {
				break
			}
			next := part
			if out != "" {
				next = out + "." + part
			}
			if utf8.RuneCountInString(next) > 128 {
				break
			}
			out = next
		}
		return out
	}
	var visit func(*jsonschema.ValidationError)
	visit = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			add(safe(e.InstanceLocation))
			return
		}
		for _, child := range e.Causes {
			visit(child)
		}
	}
	var validation *jsonschema.ValidationError
	if errors.As(err, &validation) {
		visit(validation)
	} else {
		add("manifest")
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	for key := range top {
		if !manifestFields[key] {
			add(safe([]string{key}))
		}
	}
	// Explicit false keeps its stable semantic refusal code, but is still an
	// invalid const:true Schema value and cannot yield a usable manifest.
	var files []struct {
		Rights struct {
			RedistributionAllowed json.RawMessage `json:"redistribution_allowed"`
		} `json:"rights"`
	}
	_ = json.Unmarshal(top["files"], &files)
	falseRights := false
	for _, file := range files {
		if strings.TrimSpace(string(file.Rights.RedistributionAllowed)) == "false" {
			falseRights = true
			break
		}
	}
	if falseRights && len(locations) > 31 {
		locations = locations[:31]
	}
	findings := make([]Finding, 0, len(locations)+1)
	for _, location := range locations {
		findings = append(findings, Finding{Severity: Error, Code: CodeManifestSchema, Object: location, Detail: "manifest does not satisfy the package format"})
	}
	if falseRights {
		findings = append(findings, Finding{Severity: Error, Code: CodeRightsNotDistributable, Object: "files", Detail: "the source may not be redistributed"})
	}
	Order(findings)
	return findings
}

func manifestDiagnosticField(name string) bool {
	if manifestFields[name] {
		return true
	}
	switch name {
	case "extra", "public_api_major", "package_format_major", "minimum_runtime_contract", "rule_primitive_set", "logical_id", "path", "version", "sha256", "references", "size_bytes", "media_type", "rights", "license", "source_reference", "redistribution_allowed", "allowed_uses", "logical_exports", "model_capabilities", "tool_capabilities", "presentation_capabilities", "tested_configurations", "capability_id", "required", "schema_path", "tool_capability_id", "registered_contract_version", "configuration_id", "evidence_path", "evidence_kind", "limitations", "author_display_name", "created_at", "source_references", "synthetic_examples_only":
		return true
	}
	return false
}

func manifestDiagnosticArray(name string) bool {
	switch name {
	case "entrypoints", "entries", "files", "dependencies", "references", "logical_exports", "allowed_uses", "model_capabilities", "tool_capabilities", "presentation_capabilities", "tested_configurations", "limitations", "source_references":
		return true
	}
	return false
}

func manifestDTOBytes(top map[string]json.RawMessage) ([]byte, error) {
	normalize := func(raw json.RawMessage) (json.RawMessage, error) {
		n, ok := new(big.Rat).SetString(strings.TrimSpace(string(raw)))
		if !ok || !n.IsInt() || !n.Num().IsInt64() {
			return nil, errors.New("noninteger DTO token")
		}
		return json.Marshal(n.Num().Int64())
	}
	var compatibility map[string]json.RawMessage
	if err := json.Unmarshal(top["compatibility"], &compatibility); err != nil {
		return nil, err
	}
	for _, key := range []string{"public_api_major", "package_format_major"} {
		value, err := normalize(compatibility[key])
		if err != nil {
			return nil, err
		}
		compatibility[key] = value
	}
	var err error
	top["compatibility"], err = json.Marshal(compatibility)
	if err != nil {
		return nil, err
	}
	var files []map[string]json.RawMessage
	if err = json.Unmarshal(top["files"], &files); err != nil {
		return nil, err
	}
	for _, file := range files {
		value, e := normalize(file["size_bytes"])
		if e != nil {
			return nil, e
		}
		file["size_bytes"] = value
	}
	top["files"], err = json.Marshal(files)
	if err != nil {
		return nil, err
	}
	return json.Marshal(top)
}
