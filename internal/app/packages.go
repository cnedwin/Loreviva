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

// packageRoutes serves the content-package preflight and the limits it applies.
// Both are control-plane reads: they require the operator action, but they do
// not need every worker adapter to be ready, because a degraded runtime still
// has to tell an author what it will accept.
func (r *Runtime) packageRoutes(mux *http.ServeMux) {
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
