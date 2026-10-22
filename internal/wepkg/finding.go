package wepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

type Severity string

const (
	Error   Severity = "ERROR"
	Warning Severity = "WARNING"
)

// Codes are stable identifiers: authors and tooling match on them, so they may
// gain detail but must never change meaning.
const (
	CodeArchiveTooLarge        = "ARCHIVE_TOO_LARGE"
	CodeArchiveCorrupt         = "ARCHIVE_CORRUPT"
	CodeArchiveUnsupported     = "ARCHIVE_UNSUPPORTED"
	CodeArchiveHeaderIssue     = "ARCHIVE_HEADER_MISMATCH"
	CodeArchiveExpansion       = "ARCHIVE_EXPANSION_LIMIT"
	CodeArchiveTooManyItems    = "ARCHIVE_TOO_MANY_ENTRIES"
	CodePathInvalid            = "PATH_INVALID"
	CodePathDuplicate          = "PATH_DUPLICATE"
	CodePathCaseCollision      = "PATH_CASE_COLLISION"
	CodePathPrefixCollision    = "PATH_PREFIX_COLLISION"
	CodeFileTooLarge           = "FILE_TOO_LARGE"
	CodeManifestMissing        = "MANIFEST_MISSING"
	CodeManifestInvalid        = "MANIFEST_INVALID"
	CodeManifestSchema         = "MANIFEST_SCHEMA"
	CodeFileMissing            = "FILE_MISSING"
	CodeFileUndeclared         = "FILE_UNDECLARED"
	CodeFileSizeMismatch       = "FILE_SIZE_MISMATCH"
	CodeFileHashMismatch       = "FILE_HASH_MISMATCH"
	CodeContentHashMismatch    = "CONTENT_HASH_MISMATCH"
	CodeEntryFileMissing       = "ENTRY_FILE_MISSING"
	CodeEntryHashMismatch      = "ENTRY_HASH_MISMATCH"
	CodeEntrypointUnknown      = "ENTRYPOINT_UNKNOWN"
	CodeEntrypointShort        = "ENTRYPOINT_INCOMPLETE"
	CodeDependencySelf         = "DEPENDENCY_SELF"
	CodeDependencyDuplicate    = "DEPENDENCY_DUPLICATE"
	CodeReferenceUnresolved    = "REFERENCE_UNRESOLVED"
	CodeCapabilityUnknown      = "CAPABILITY_UNKNOWN"
	CodePrimitiveForbidden     = "PRIMITIVE_FORBIDDEN"
	CodeRightsIncoherent       = "RIGHTS_INCOHERENT"
	CodeRightsNotDistributable = "RIGHTS_NOT_REDISTRIBUTABLE"
	CodeUsesUnknown            = "USES_UNKNOWN"
	CodeEntrypointKind         = "ENTRYPOINT_KIND"
	CodeEntryBodyInvalid       = "ENTRY_BODY_INVALID"
)

// Finding points at one package object. Detail is deterministic text: it may
// carry hashes and counts, never host paths or timestamps.
type Finding struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	Path     string   `json:"path,omitempty"`
	Object   string   `json:"object,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

type Report struct {
	ReportVersion       string    `json:"report_version"`
	Verdict             string    `json:"verdict"`
	PackageID           string    `json:"package_id,omitempty"`
	PackageVersion      string    `json:"package_version,omitempty"`
	Kind                string    `json:"kind,omitempty"`
	ContentHashDeclared string    `json:"content_sha256_declared,omitempty"`
	ContentHashComputed string    `json:"content_sha256_computed,omitempty"`
	ManifestSHA256      string    `json:"manifest_sha256,omitempty"`
	ArchiveSHA256       string    `json:"archive_sha256,omitempty"`
	ArchiveBytes        int64     `json:"archive_bytes"`
	FileCount           int       `json:"file_count"`
	TotalBytes          int64     `json:"total_bytes"`
	Limits              Limits    `json:"limits"`
	Findings            []Finding `json:"findings"`
	ReportSHA256        string    `json:"report_sha256"`
}

// Order sorts findings so two runs over the same bytes produce identical JSON.
// severity first (errors before warnings), then code, path, object, detail.
func Order(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if (a.Severity == Error) != (b.Severity == Error) {
			return a.Severity == Error
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Object != b.Object {
			return a.Object < b.Object
		}
		return a.Detail < b.Detail
	})
}

// Hash returns the report digest. The archive digest and byte count are
// excluded on purpose: repacking the same content must yield the same verdict
// and the same digest.
func (r Report) Hash() string {
	preimage := struct {
		Version  string    `json:"report_version"`
		Verdict  string    `json:"verdict"`
		ID       string    `json:"package_id,omitempty"`
		PVersion string    `json:"package_version,omitempty"`
		Kind     string    `json:"kind,omitempty"`
		Declared string    `json:"content_sha256_declared,omitempty"`
		Computed string    `json:"content_sha256_computed,omitempty"`
		Manifest string    `json:"manifest_sha256,omitempty"`
		Limits   Limits    `json:"limits"`
		Findings []Finding `json:"findings"`
	}{r.ReportVersion, r.Verdict, r.PackageID, r.PackageVersion, r.Kind, r.ContentHashDeclared, r.ContentHashComputed, r.ManifestSHA256, r.Limits, r.Findings}
	raw, e := json.Marshal(preimage)
	if e != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Verdict turns findings into PASS or REJECT. A warning never rejects.
func Verdict(findings []Finding) string {
	for _, f := range findings {
		if f.Severity == Error {
			return "REJECT"
		}
	}
	return "PASS"
}

// ShortHash renders a digest for a finding detail: fixed width, so details stay
// byte-comparable between runs.
func ShortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

func lower(b []byte) string { return strings.ToLower(string(b)) }
