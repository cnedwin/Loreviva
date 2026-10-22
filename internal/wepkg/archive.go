package wepkg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Member is one archive entry as the preflight saw it. Only the archive is
// inspected here: no extraction to disk happens, so an entry name can never
// escape a directory. SHA256 is computed from the decompressed bytes, so a
// header that lies about content is caught.
type Member struct {
	Path       string
	Size       uint64
	Compressed uint64
	Method     uint16
	Mode       os.FileMode
	IsDir      bool
	SHA256     string
}

type Index struct {
	Members    []Member
	TotalBytes int64
	Files      int
}

// InspectArchive performs the container stages: the archive must be readable,
// unencrypted, free of links and special files, its members must have safe
// names, and every member must expand within the published limits. It never
// writes to disk and never trusts a size the header claims: the expansion is
// counted while streaming the decompressed bytes.
func InspectArchive(raw []byte, lim Limits) (Index, []Finding) {
	var out Index
	var findings []Finding
	if !lim.valid() {
		return out, []Finding{{Severity: Error, Code: CodeArchiveUnsupported, Detail: "invalid limits"}}
	}
	if int64(len(raw)) > lim.ArchiveBytes {
		return out, []Finding{{Severity: Error, Code: CodeArchiveTooLarge,
			Detail: fmt.Sprintf("archive %d bytes exceeds %d", len(raw), lim.ArchiveBytes)}}
	}
	zr, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		return out, []Finding{{Severity: Error, Code: CodeArchiveCorrupt, Detail: "not a readable ZIP container"}}
	}
	if len(zr.File) > lim.Entries {
		return out, []Finding{{Severity: Error, Code: CodeArchiveTooManyItems,
			Detail: fmt.Sprintf("%d entries exceeds %d", len(zr.File), lim.Entries)}}
	}

	seen := map[string]string{}
	lowered := map[string]string{}
	for _, f := range zr.File {
		name := f.Name
		isDir := strings.HasSuffix(name, "/") || f.FileInfo().IsDir()
		mode := f.Mode()

		switch {
		case f.Flags&1 != 0:
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveUnsupported, Path: name, Detail: "encrypted member"})
			continue
		case mode&os.ModeSymlink != 0:
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveUnsupported, Path: name, Detail: "symbolic link member"})
			continue
		case !isDir && !mode.IsRegular():
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveUnsupported, Path: name, Detail: "not a regular file"})
			continue
		case f.Method != zip.Store && f.Method != zip.Deflate:
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveUnsupported, Path: name, Detail: "compression method"})
			continue
		}

		clean := strings.TrimSuffix(name, "/")
		if clean == "" {
			findings = append(findings, Finding{Severity: Error, Code: CodePathInvalid, Path: name, Detail: "empty path"})
			continue
		}
		if e := validatePath(clean, lim); e != "" {
			findings = append(findings, Finding{Severity: Error, Code: CodePathInvalid, Path: name, Detail: e})
			continue
		}
		if first, ok := seen[clean]; ok {
			findings = append(findings, Finding{Severity: Error, Code: CodePathDuplicate, Path: clean,
				Detail: "declared twice (first at " + first + ")"})
			continue
		}
		seen[clean] = clean
		if other, ok := lowered[strings.ToLower(clean)]; ok && other != clean {
			findings = append(findings, Finding{Severity: Error, Code: CodePathCaseCollision, Path: clean, Object: other,
				Detail: "differs only by case"})
		} else {
			lowered[strings.ToLower(clean)] = clean
		}

		if isDir {
			out.Members = append(out.Members, Member{Path: clean, IsDir: true, Mode: mode})
			continue
		}

		if f.UncompressedSize64 > uint64(lim.FileBytes) {
			findings = append(findings, Finding{Severity: Error, Code: CodeFileTooLarge, Path: clean,
				Detail: fmt.Sprintf("%d bytes exceeds %d", f.UncompressedSize64, lim.FileBytes)})
			continue
		}
		if f.CompressedSize64 > 0 && f.UncompressedSize64 > uint64(lim.Ratio)*f.CompressedSize64 {
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveExpansion, Path: clean,
				Detail: "compression ratio exceeds limit"})
			continue
		}
		// Stream the member to verify the declared size and CRC and to count the
		// real expansion; a header that lies about its size is caught here.
		rc, e := f.Open()
		if e != nil {
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveCorrupt, Path: clean, Detail: "member unreadable"})
			continue
		}
		sum := sha256.New()
		n, e := io.Copy(io.MultiWriter(io.Discard, sum), io.LimitReader(rc, lim.FileBytes+1))
		rc.Close()
		if e != nil {
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveCorrupt, Path: clean, Detail: "member failed integrity check"})
			continue
		}
		if n > lim.FileBytes {
			findings = append(findings, Finding{Severity: Error, Code: CodeFileTooLarge, Path: clean,
				Detail: fmt.Sprintf("expands beyond %d", lim.FileBytes)})
			continue
		}
		if uint64(n) != f.UncompressedSize64 {
			findings = append(findings, Finding{Severity: Error, Code: CodeArchiveHeaderIssue, Path: clean,
				Detail: fmt.Sprintf("declared %d, actual %d", f.UncompressedSize64, n)})
			continue
		}
		out.TotalBytes += n
		if out.TotalBytes > lim.TotalBytes {
			return out, []Finding{{Severity: Error, Code: CodeArchiveExpansion,
				Detail: fmt.Sprintf("total expansion exceeds %d", lim.TotalBytes)}}
		}
		out.Files++
		out.Members = append(out.Members, Member{Path: clean, Size: uint64(n), Compressed: f.CompressedSize64, Method: f.Method, Mode: mode, SHA256: hex.EncodeToString(sum.Sum(nil))})
	}

	// A file may not also be a directory prefix of another member: extraction
	// would have to turn it into one.
	for _, e := range out.Members {
		if e.IsDir {
			continue
		}
		for _, other := range out.Members {
			if other.Path != e.Path && strings.HasPrefix(other.Path, e.Path+"/") {
				findings = append(findings, Finding{Severity: Error, Code: CodePathPrefixCollision, Path: e.Path, Object: other.Path,
					Detail: "a file is also used as a directory"})
				break
			}
		}
	}

	sort.Slice(out.Members, func(i, j int) bool { return out.Members[i].Path < out.Members[j].Path })
	Order(findings)
	return out, findings
}

// validatePath reports the first reason a member path is unsafe, or "" when it
// is acceptable. The rule matches the manifest schema pattern so an author sees
// one rule, not two.
func validatePath(p string, lim Limits) string {
	if len(p) > lim.PathLength {
		return fmt.Sprintf("longer than %d characters", lim.PathLength)
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "absolute path"
	}
	if strings.ContainsAny(p, "\\:") {
		return "backslash or colon"
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "control character"
		}
		if !(r == '/' || r == '.' || r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return "character outside [A-Za-z0-9._/-]"
		}
	}
	segments := strings.Split(p, "/")
	if len(segments) > lim.PathDepth {
		return fmt.Sprintf("deeper than %d segments", lim.PathDepth)
	}
	for _, s := range segments {
		if s == "" {
			return "empty segment"
		}
		if s == "." || s == ".." {
			return "relative segment"
		}
	}
	return ""
}
