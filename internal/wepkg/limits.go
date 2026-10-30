package wepkg

// Limits bounds one static preflight. They are a single value object rather
// than scattered constants because the same numbers are published to authors
// (GET /world/v1/admin/content-packages/capabilities) before they choose a file:
// an author must be able to see the ceiling that will judge the upload.
type Limits struct {
	ArchiveBytes  int64 `json:"archive_bytes"`  // compressed container
	ManifestBytes int64 `json:"manifest_bytes"` // manifest.json, parsed bounded
	Entries       int   `json:"entries"`        // every archive entry, directories included
	FileBytes     int64 `json:"file_bytes"`     // one decompressed file
	TotalBytes    int64 `json:"total_bytes"`    // all decompressed files together
	Ratio         int64 `json:"ratio"`          // decompressed per compressed byte
	PathLength    int   `json:"path_length"`    // characters in a declared path
	PathDepth     int   `json:"path_depth"`     // '/'-separated segments
}

// DefaultLimits shares the ordinary upload byte ceiling (files.MaxFileBytes).
// Passing static preflight does not grant import authority or imply import support.
var DefaultLimits = Limits{
	ArchiveBytes:  64 << 20,
	ManifestBytes: 8 << 20,
	Entries:       10000,
	FileBytes:     16 << 20,
	TotalBytes:    512 << 20,
	Ratio:         200,
	PathLength:    240,
	PathDepth:     8,
}

func (l Limits) valid() bool {
	return l.ArchiveBytes > 0 && l.ManifestBytes > 0 && l.Entries > 0 &&
		l.FileBytes > 0 && l.TotalBytes > 0 && l.Ratio > 0 && l.PathLength > 0 && l.PathDepth > 0
}
