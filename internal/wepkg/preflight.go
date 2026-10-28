package wepkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

// Check runs every stage the preflight can decide from the archive alone and
// returns one report. It never writes to disk, never touches a database and
// never calls a model: it answers what can be known about the package before
// anything is imported.
func Check(raw []byte, lim Limits) Report {
	rep := Report{ReportVersion: "1", Limits: lim, ArchiveBytes: int64(len(raw)), Findings: []Finding{}}
	sum := sha256.Sum256(raw)
	rep.ArchiveSHA256 = hex.EncodeToString(sum[:])

	idx, findings := InspectArchive(raw, lim)
	rep.Findings = append(rep.Findings, findings...)
	rep.FileCount = idx.Files
	rep.TotalBytes = idx.TotalBytes

	manifestRaw, e := readMember(raw, "manifest.json", lim.ManifestBytes)
	if e != nil {
		rep.Findings = append(rep.Findings, Finding{Severity: Error, Code: CodeManifestMissing, Detail: "manifest.json at the archive root is required"})
		rep.Verdict = Verdict(rep.Findings)
		Order(rep.Findings)
		rep.ReportSHA256 = rep.Hash()
		return rep
	}
	mSum := sha256.Sum256(manifestRaw)
	rep.ManifestSHA256 = hex.EncodeToString(mSum[:])

	m, manifestFindings := ParseManifest(manifestRaw, lim)
	rep.Findings = append(rep.Findings, manifestFindings...)
	if m.FormatVersion == "" {
		Order(rep.Findings)
		rep.Verdict = Verdict(rep.Findings)
		rep.ReportSHA256 = rep.Hash()
		return rep
	}
	rep.PackageID, rep.PackageVersion, rep.Kind = m.PackageID, m.PackageVersion, m.Kind
	rep.ContentHashDeclared = m.ContentSHA256
	if computed, e := ContentHash(m); e == nil {
		rep.ContentHashComputed = computed
	}

	rep.Findings = append(rep.Findings, CheckFiles(m, idx)...)
	rep.Findings = append(rep.Findings, CheckContentHash(m)...)
	rep.Findings = append(rep.Findings, CheckGraph(m)...)
	media := map[string]string{}
	for _, f := range m.Files {
		media[f.Path] = f.MediaType
	}
	for _, entry := range m.Entries {
		if media[entry.Path] != "application/json" {
			continue
		}
		body, e := readMember(raw, entry.Path, lim.FileBytes)
		if e != nil {
			continue
		}
		// The entry body is untrusted JSON. It is parsed here only to prove it is
		// readable; the import stage must never hand it to the draft assembler
		// without a schema of its own.
		var probe any
		if json.Unmarshal(body, &probe) != nil {
			rep.Findings = append(rep.Findings, Finding{Severity: Error, Code: CodeEntryBodyInvalid, Path: entry.Path,
				Detail: "entry body is not readable JSON"})
		}
	}
	for _, entry := range m.Entries {
		if entry.Kind != "RULES" {
			continue
		}
		body, e := readMember(raw, entry.Path, lim.FileBytes)
		if e != nil {
			continue
		}
		rep.Findings = append(rep.Findings, CheckRulePrimitives(m, body)...)
	}

	Order(rep.Findings)
	rep.Verdict = Verdict(rep.Findings)
	rep.ReportSHA256 = rep.Hash()
	return rep
}

// readMember returns one member's decompressed bytes, bounded by limit. The
// archive is reopened deliberately: Check keeps no decompressed state, so a
// large package cannot pin memory between stages.
func readMember(raw []byte, name string, limit int64) ([]byte, error) {
	zr, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		return nil, e
	}
	for _, f := range zr.File {
		if f.Name != name || f.FileInfo().IsDir() {
			continue
		}
		rc, e := f.Open()
		if e != nil {
			return nil, e
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, limit+1))
	}
	return nil, zip.ErrFormat
}
