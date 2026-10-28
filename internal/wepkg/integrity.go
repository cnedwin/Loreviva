package wepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"loreviva.local/loreviva/internal/wire"
)

// fileDigest is the record the content digest covers. It deliberately excludes
// rights, media type and version: those are manifest metadata, not content.
type fileDigest struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// ContentHash reproduces the manifest's content_sha256 exactly as the format
// defines it: the SHA-256 of the UTF-8 compact sorted-key JSON array of file
// records ordered by path. The array is sorted here explicitly because
// wire.Canonical only orders object keys.
func ContentHash(m Manifest) (string, error) {
	records := make([]fileDigest, 0, len(m.Files))
	for _, f := range m.Files {
		records = append(records, fileDigest{Path: f.Path, SHA256: f.SHA256, SizeBytes: f.SizeBytes})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })
	raw, e := json.Marshal(records)
	if e != nil {
		return "", e
	}
	canonical, e := wire.CanonicalBounded(raw, 8<<20)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalDigest hashes a compact canonical JSON document. Kept separate so a
// test can build a manifest with the same rule the check applies.
func canonicalDigest(raw []byte) (string, error) {
	canonical, e := wire.Canonical(raw)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// CheckFiles compares declared file records with the archive members that were
// actually read. Both directions are checked: a declared file that is absent,
// and an archive member nobody declared.
func CheckFiles(m Manifest, idx Index) []Finding {
	var findings []Finding
	members := map[string]Member{}
	for _, mem := range idx.Members {
		if !mem.IsDir {
			members[mem.Path] = mem
		}
	}
	declared := map[string]bool{}
	for i, f := range m.Files {
		object := fmt.Sprintf("files[%d]", i)
		declared[f.Path] = true
		mem, ok := members[f.Path]
		if !ok {
			findings = append(findings, Finding{Severity: Error, Code: CodeFileMissing, Path: f.Path, Object: object,
				Detail: "declared but absent from the archive"})
			continue
		}
		if mem.Size != uint64(f.SizeBytes) {
			findings = append(findings, Finding{Severity: Error, Code: CodeFileSizeMismatch, Path: f.Path, Object: object,
				Detail: fmt.Sprintf("declared %d, actual %d", f.SizeBytes, mem.Size)})
		}
		if mem.SHA256 != f.SHA256 {
			findings = append(findings, Finding{Severity: Error, Code: CodeFileHashMismatch, Path: f.Path, Object: object,
				Detail: fmt.Sprintf("declared %s, actual %s", ShortHash(f.SHA256), ShortHash(mem.SHA256))})
		}
	}
	for path := range members {
		if path == "manifest.json" {
			continue
		}
		if !declared[path] {
			findings = append(findings, Finding{Severity: Error, Code: CodeFileUndeclared, Path: path,
				Detail: "present in the archive but not declared"})
		}
	}
	Order(findings)
	return findings
}

// CheckContentHash compares the declared digest with the recomputed one.
func CheckContentHash(m Manifest) []Finding {
	computed, e := ContentHash(m)
	if e != nil {
		return []Finding{{Severity: Error, Code: CodeManifestInvalid, Detail: "content digest cannot be computed"}}
	}
	if computed != m.ContentSHA256 {
		return []Finding{{Severity: Error, Code: CodeContentHashMismatch,
			Detail: fmt.Sprintf("declared %s, computed %s", ShortHash(m.ContentSHA256), ShortHash(computed))}}
	}
	return nil
}
