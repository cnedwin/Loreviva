package wepkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

// ExportBundle accepts a static projection only. It cannot discover a database,
// credentials or runtime state. Full preflight remains the final format gate.
func ExportBundle(m Manifest, body []byte, files map[string][]byte) ([]byte, Manifest, error) {
	fail := func() ([]byte, Manifest, error) {
		return nil, Manifest{}, errors.New("static export is not representable")
	}
	if len(m.Entries) != 1 || len(body) > 1<<20 {
		return fail()
	}
	path := m.Entries[0].Path
	all := map[string][]byte{path: body}
	total := int64(len(body))
	hash := sha256.Sum256(body)
	m.Entries[0].SHA256 = hex.EncodeToString(hash[:])
	for name, data := range files {
		if _, exists := all[name]; exists || name == "manifest.json" || int64(len(data)) > DefaultLimits.FileBytes {
			return fail()
		}
		total += int64(len(data))
		// Reserve bounded metadata and ZIP headers before allocating the archive.
		if total > DefaultLimits.ArchiveBytes-DefaultLimits.ManifestBytes-1<<20 {
			return fail()
		}
		all[name] = data
	}
	for i := range m.Files {
		data, ok := all[m.Files[i].Path]
		if !ok {
			return fail()
		}
		h := sha256.Sum256(data)
		m.Files[i].SHA256 = hex.EncodeToString(h[:])
		m.Files[i].SizeBytes = int64(len(data))
	}
	digest, e := ContentHash(m)
	if e != nil {
		return fail()
	}
	m.ContentSHA256 = digest
	raw, e := json.Marshal(m)
	if e != nil {
		return fail()
	}
	all["manifest.json"] = raw
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		h.SetMode(0600)
		entry, e := writer.CreateHeader(h)
		if e != nil {
			return fail()
		}
		if _, e = entry.Write(all[name]); e != nil {
			return fail()
		}
	}
	if writer.Close() != nil || int64(buf.Len()) > DefaultLimits.ArchiveBytes {
		return fail()
	}
	archive := buf.Bytes()
	if Check(archive, DefaultLimits).Verdict != "PASS" {
		return fail()
	}
	return archive, m, nil
}

// ExtractDeclaredFile rechecks the original archive and the exact declared
// leaf. It does not create a local asset or grant the caller import authority.
func ExtractDeclaredFile(archive []byte, path string) ([]byte, FileRecord, error) {
	fail := func() ([]byte, FileRecord, error) {
		return nil, FileRecord{}, errors.New("declared package file unavailable")
	}
	if Check(archive, DefaultLimits).Verdict != "PASS" {
		return fail()
	}
	reader, e := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if e != nil {
		return fail()
	}
	var manifest Manifest
	for _, f := range reader.File {
		if f.Name == "manifest.json" {
			r, e := f.Open()
			if e != nil {
				return fail()
			}
			raw, e := io.ReadAll(io.LimitReader(r, DefaultLimits.ManifestBytes+1))
			r.Close()
			if e != nil || json.Unmarshal(raw, &manifest) != nil {
				return fail()
			}
		}
	}
	var record *FileRecord
	for i := range manifest.Files {
		if manifest.Files[i].Path == path {
			record = &manifest.Files[i]
			break
		}
	}
	if record == nil {
		return fail()
	}
	for _, f := range reader.File {
		if f.Name == path {
			r, e := f.Open()
			if e != nil {
				return fail()
			}
			raw, e := io.ReadAll(io.LimitReader(r, DefaultLimits.FileBytes+1))
			r.Close()
			hash := sha256.Sum256(raw)
			if e != nil || int64(len(raw)) != record.SizeBytes || hex.EncodeToString(hash[:]) != record.SHA256 {
				return fail()
			}
			return raw, *record, nil
		}
	}
	return fail()
}
