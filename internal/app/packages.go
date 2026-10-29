package app

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/wepkg"
	"loreviva.local/loreviva/internal/wire"
)

// Package preflight, preview, capabilities and receipt recovery are control-plane
// reads. The persistent import uses ordinary write admission and the owning
// transaction; degraded-read access never grants an import capability.
func (r *Runtime) packageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /world/v1/admin/content-packages/imports", r.admin("", "content.edit", func(ctx context.Context, scope domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		archive, options, err := packagePreviewBody(q)
		if err != nil {
			return 0, nil, err
		}
		result, err := r.Content.ImportPackage(ctx, scope, archive, options, q.Header.Get("Idempotency-Key"))
		return 201, result, err
	}))
	mux.HandleFunc("GET /world/v1/admin/content-packages/imports/{request_key}", r.adminControl("", "content.read", func(ctx context.Context, scope domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
		if len(q.URL.Query()) != 1 {
			return 0, nil, wire.ErrJSON
		}
		result, err := r.Content.GetPackageImportResult(ctx, scope, q.PathValue("request_key"))
		return 200, result, err
	}))
	mux.HandleFunc("GET /world/v1/admin/content-packages/capabilities",
		r.adminControl("", "content.read", func(_ context.Context, _ domain.ProductScope, _ *http.Request, _ []byte) (int, any, error) {
			ids := make([]string, 0, len(wepkg.RegisteredCapabilities))
			for id := range wepkg.RegisteredCapabilities {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			l := wepkg.DefaultLimits
			return 200, map[string]any{
				"registry_version": "1",
				"limits": map[string]any{"archive_bytes": l.ArchiveBytes, "manifest_bytes": l.ManifestBytes,
					"entries": l.Entries, "file_bytes": l.FileBytes, "total_bytes": l.TotalBytes,
					"ratio": l.Ratio, "path_length": l.PathLength, "path_depth": l.PathDepth},
				"capabilities": ids,
			}, nil
		}))

	mux.HandleFunc("POST /world/v1/admin/content-packages/preflight",
		r.adminControl("", "content.read", func(_ context.Context, _ domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
			archive, e := preflightBody(q)
			if e != nil {
				return 0, nil, e
			}
			report := wepkg.Check(archive, wepkg.DefaultLimits)
			return 200, report, nil
		}))
	mux.HandleFunc("POST /world/v1/admin/content-packages/preview",
		r.adminControl("", "content.check", func(ctx context.Context, scope domain.ProductScope, q *http.Request, _ []byte) (int, any, error) {
			archive, options, err := packagePreviewBody(q)
			if err != nil {
				return 0, nil, err
			}
			report, err := r.Content.PreviewPackage(ctx, scope, archive, options)
			return 200, report, err
		}))
}

// preflightBody reads the multipart body with the archive ceiling applied. The
// request is accepted or refused as a whole: a partly read archive cannot be
// judged, so there is no partial report.
func preflightBody(q *http.Request) ([]byte, error) {
	media, params, e := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if e != nil || media != "multipart/form-data" {
		return nil, errUnsupportedMedia
	}
	if len(q.URL.Query()) != 1 || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
		return nil, wire.ErrJSON
	}
	lim := wepkg.DefaultLimits
	reader := multipart.NewReader(q.Body, params["boundary"])
	var archive []byte
	seenArchive := false
	for {
		part, e := reader.NextPart()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, wire.ErrJSON
		}
		switch part.FormName() {
		case "archive":
			if seenArchive {
				return nil, wire.ErrJSON
			}
			seenArchive = true
			archive, e = io.ReadAll(io.LimitReader(part, lim.ArchiveBytes+1))
			if e != nil {
				return nil, wire.ErrJSON
			}
			if int64(len(archive)) > lim.ArchiveBytes {
				return nil, errArchiveTooLarge
			}
		case "options":
			// Reserved for future options; only an empty object is accepted, so a
			// caller cannot smuggle settings this build does not honour.
			raw, e := io.ReadAll(io.LimitReader(part, 4096))
			if e != nil || len(raw) > 0 && string(raw) != "{}" {
				return nil, wire.ErrJSON
			}
		default:
			return nil, wire.ErrJSON
		}
	}
	if !seenArchive {
		return nil, wire.ErrJSON
	}
	return archive, nil
}

var (
	errUnsupportedMedia = errors.New("unsupported media")
	errArchiveTooLarge  = errors.New("archive too large")
)

// Separate from static preflight: exactly one archive and one closed options
// object, with independent raw-byte bounds and no temporary filesystem writes.
func packagePreviewBody(q *http.Request) ([]byte, []byte, error) {
	media, params, err := mime.ParseMediaType(q.Header.Get("Content-Type"))
	if err != nil || media != "multipart/form-data" {
		return nil, nil, errUnsupportedMedia
	}
	if len(q.URL.Query()) != 1 || domain.ValidateIdempotencyKey(q.Header.Get("Idempotency-Key")) != nil {
		return nil, nil, wire.ErrJSON
	}
	q.Body = http.MaxBytesReader(nil, q.Body, wepkg.DefaultLimits.ArchiveBytes+65536+(1<<20))
	reader := multipart.NewReader(q.Body, params["boundary"])
	var archive, options []byte
	seen := map[string]bool{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, wire.ErrJSON
		}
		name := part.FormName()
		if seen[name] || (name != "archive" && name != "options") {
			return nil, nil, wire.ErrJSON
		}
		seen[name] = true
		limit := int64(65536)
		if name == "archive" {
			limit = wepkg.DefaultLimits.ArchiveBytes
		}
		raw, err := io.ReadAll(io.LimitReader(part, limit+1))
		if err != nil {
			return nil, nil, wire.ErrJSON
		}
		if int64(len(raw)) > limit {
			if name == "archive" {
				return nil, nil, errArchiveTooLarge
			}
			return nil, nil, wire.ErrJSON
		}
		if name == "archive" {
			archive = raw
		} else {
			options = raw
		}
	}
	if !seen["archive"] || !seen["options"] {
		return nil, nil, wire.ErrJSON
	}
	return archive, options, nil
}
