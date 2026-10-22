package wepkg

// Limits bounds one static preflight. They are a single value object rather
// than scattered constants because the same numbers are published to authors
// (CreatorCapabilities.package_preflight_limits) before they choose a file:
// an author must be able to see the ceiling that will judge the upload.
type Limits struct {
	ArchiveBytes  int64 // compressed container
	ManifestBytes int64 // manifest.json, parsed bounded
	Entries       int   // every archive entry, directories included
	FileBytes     int64 // one decompressed file
	TotalBytes    int64 // all decompressed files together
	Ratio         int64 // decompressed per compressed byte
	PathLength    int   // characters in a declared path
	PathDepth     int   // '/'-separated segments
}

// DefaultLimits mirrors the runtime: FileBytes equals files.MaxFileBytes, so a
// package the preflight accepts can actually be imported later.
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
