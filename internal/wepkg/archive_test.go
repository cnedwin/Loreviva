package wepkg

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func smallLimits() Limits {
	return Limits{ArchiveBytes: 1 << 20, ManifestBytes: 1 << 20, Entries: 5, FileBytes: 1 << 10, TotalBytes: 4 << 10, Ratio: 200, PathLength: 240, PathDepth: 8}
}

type member struct {
	name string
	body []byte
	mode os.FileMode
	flag uint16
}

func build(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		if m.mode != 0 {
			h.SetMode(m.mode)
		}
		h.Flags |= m.flag
		f, e := w.CreateHeader(h)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(m.body); e != nil {
			t.Fatal(e)
		}
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	return buf.Bytes()
}

func codes(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Code)
	}
	return out
}

func has(findings []Finding, code string) bool {
	for _, f := range findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestInspectArchiveAcceptsSafeMembers(t *testing.T) {
	raw := build(t, member{name: "manifest.json", body: []byte(`{}`)}, member{name: "content/world.json", body: []byte(`{"a":1}`)})
	index, findings := InspectArchive(raw, smallLimits())
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", codes(findings))
	}
	if index.Files != 2 || len(index.Entries) != 2 {
		t.Fatalf("index: %+v", index)
	}
	if index.Entries[0].Path != "content/world.json" || index.Entries[1].Path != "manifest.json" {
		t.Fatalf("entries not sorted: %+v", index.Entries)
	}
}

func TestInspectArchiveRejectsUnsafePaths(t *testing.T) {
	for name, want := range map[string]string{
		"../escape.json":   CodePathInvalid,
		"/etc/passwd":      CodePathInvalid,
		"a\\b.json":        CodePathInvalid,
		"a:b.json":         CodePathInvalid,
		"content//x.json":  CodePathInvalid,
		"content/./x.json": CodePathInvalid,
	} {
		_, findings := InspectArchive(build(t, member{name: name, body: []byte("x")}), smallLimits())
		if !has(findings, want) {
			t.Fatalf("%q: want %s, got %v", name, want, codes(findings))
		}
		if has(findings, CodeArchiveCorrupt) || has(findings, CodeArchiveHeaderIssue) {
			t.Fatalf("%q: path rejection polluted by container codes: %v", name, codes(findings))
		}
	}
}

func TestInspectArchiveRejectsDuplicateCaseAndPrefix(t *testing.T) {
	_, dup := InspectArchive(build(t,
		member{name: "a.json", body: []byte("1")},
		member{name: "a.json", body: []byte("2")}), smallLimits())
	if !has(dup, CodePathDuplicate) {
		t.Fatalf("duplicate accepted: %v", codes(dup))
	}

	_, cased := InspectArchive(build(t,
		member{name: "a.json", body: []byte("1")},
		member{name: "A.json", body: []byte("2")}), smallLimits())
	if !has(cased, CodePathCaseCollision) {
		t.Fatalf("case collision accepted: %v", codes(cased))
	}

	_, prefix := InspectArchive(build(t,
		member{name: "content", body: []byte("file")},
		member{name: "content/world.json", body: []byte("nested")}), smallLimits())
	if !has(prefix, CodePathPrefixCollision) {
		t.Fatalf("prefix collision accepted: %v", codes(prefix))
	}
}

func TestInspectArchiveRejectsLinksAndEncryption(t *testing.T) {
	link := build(t, member{name: "content/link.json", body: []byte("../../etc/passwd"), mode: os.ModeSymlink | 0o777})
	if _, findings := InspectArchive(link, smallLimits()); !has(findings, CodeArchiveUnsupported) {
		t.Fatalf("symlink accepted: %v", codes(findings))
	}
	enc := build(t, member{name: "a.json", body: []byte("x"), flag: 0x1})
	if _, findings := InspectArchive(enc, smallLimits()); !has(findings, CodeArchiveUnsupported) {
		t.Fatalf("encrypted member accepted: %v", codes(findings))
	}
}

func TestInspectArchiveEnforcesLimits(t *testing.T) {
	lim := smallLimits()
	lim.Entries = 2
	if _, findings := InspectArchive(build(t,
		member{name: "a.json", body: []byte("1")},
		member{name: "b.json", body: []byte("2")},
		member{name: "c.json", body: []byte("3")}), lim); !has(findings, CodeArchiveTooManyItems) {
		t.Fatalf("entry cap not enforced: %v", codes(findings))
	}

	lim = smallLimits()
	lim.FileBytes = 8
	if _, findings := InspectArchive(build(t, member{name: "a.json", body: []byte(strings.Repeat("x", 64))}), lim); !has(findings, CodeFileTooLarge) {
		t.Fatalf("file cap not enforced: %v", codes(findings))
	}

	lim = smallLimits()
	lim.TotalBytes = 16
	if _, findings := InspectArchive(build(t,
		member{name: "a.json", body: []byte(strings.Repeat("a", 12))},
		member{name: "b.json", body: []byte(strings.Repeat("b", 12))}), lim); !has(findings, CodeArchiveExpansion) {
		t.Fatalf("total expansion cap not enforced: %v", codes(findings))
	}

	lim = smallLimits()
	lim.Ratio = 5
	if _, findings := InspectArchive(build(t, member{name: "a.json", body: []byte(strings.Repeat("z", 900))}), lim); !has(findings, CodeArchiveExpansion) {
		t.Fatalf("ratio cap not enforced: %v", codes(findings))
	}

	lim = smallLimits()
	lim.ArchiveBytes = 10
	if _, findings := InspectArchive(build(t, member{name: "a.json", body: []byte("x")}), lim); !has(findings, CodeArchiveTooLarge) {
		t.Fatalf("archive cap not enforced: %v", codes(findings))
	}
}

func TestInspectArchiveRejectsCorruptContainer(t *testing.T) {
	if _, findings := InspectArchive([]byte("not a zip"), smallLimits()); !has(findings, CodeArchiveCorrupt) {
		t.Fatalf("corrupt container accepted: %v", codes(findings))
	}
}

func TestReportIsDeterministicAndIgnoresArchiveBytes(t *testing.T) {
	report := func(archiveSHA string) Report {
		r := Report{ReportVersion: "1", Verdict: "PASS", Limits: DefaultLimits, ArchiveSHA256: archiveSHA, ArchiveBytes: 42,
			Findings: []Finding{{Severity: Error, Code: CodePathInvalid, Path: "../x"}, {Severity: Error, Code: CodeFileMissing, Path: "a.json"}}}
		Order(r.Findings)
		r.ReportSHA256 = r.Hash()
		return r
	}
	first, second := report("aa"), report("bb")
	if first.ReportSHA256 != second.ReportSHA256 {
		t.Fatalf("archive digest changed the report hash: %s vs %s", first.ReportSHA256, second.ReportSHA256)
	}
	if first.Findings[0].Code != CodeFileMissing {
		t.Fatalf("ordering is not by code: %v", codes(first.Findings))
	}
	raw1, _ := json.Marshal(first)
	again := report("aa")
	raw2, _ := json.Marshal(again)
	if !bytes.Equal(raw1, raw2) {
		t.Fatal("same input produced a different report")
	}
}
