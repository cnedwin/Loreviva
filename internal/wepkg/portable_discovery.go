package wepkg

// PortableEntries exposes only declarations supplied by the caller's archive.
// It repeats admission instead of trusting a client report or an unpacked path.
func PortableEntries(raw []byte, lim Limits) ([]Entry, []Finding) {
	report := Check(raw, lim)
	if report.Verdict != "PASS" {
		return nil, report.Findings
	}
	body, err := readMember(raw, "manifest.json", lim.ManifestBytes)
	if err != nil {
		return nil, []Finding{{Severity: Error, Code: CodePortablePlanInputInvalid, Detail: "Manifest is unavailable."}}
	}
	manifest, findings := ParseManifest(body, lim)
	if len(findings) > 0 {
		return nil, findings
	}
	entries := []Entry{}
	wanted := stringSet(manifest.Entrypoints)
	for _, entry := range manifest.Entries {
		if wanted[entry.LogicalID] && rootKinds[entry.Kind] {
			entries = append(entries, entry)
		}
	}
	return entries, []Finding{}
}
