package app

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// studio_dist contains the actual native Vue production build. Regenerate with
// pnpm --dir apps/studio build and node scripts/build-studio.mjs before Go build.
//
//go:embed studio_dist
var studioFiles embed.FS

// Serve only the enumerated SPA routes or clean embedded asset paths using GET/HEAD and the restrictive CSP below.
// Unknown paths must not fall back to the SPA.
func studioHandler() http.Handler {
	files, err := fs.Sub(studioFiles, "studio_dist")
	if err != nil {
		panic(err)
	}
	pages := map[string]bool{"/studio/": true, "/studio/editor": true, "/studio/resources": true, "/studio/runtime": true, "/studio/settings": true}
	return http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-cache")
		if q.Method != "GET" && q.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", 405)
			return
		}
		p := q.URL.Path
		if p == "/studio" {
			http.Redirect(w, q, "/studio/", http.StatusTemporaryRedirect)
			return
		}
		name := ""
		if pages[p] {
			name = "index.html"
		} else if strings.HasPrefix(p, "/studio/assets/") && !strings.Contains(p, "\\") && path.Clean(p) == p {
			name = strings.TrimPrefix(p, "/studio/")
		}
		if name == "" {
			http.NotFound(w, q)
			return
		}
		data, e := fs.ReadFile(files, name)
		if e != nil {
			http.NotFound(w, q)
			return
		}
		if name == "index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else {
			kind := mime.TypeByExtension(path.Ext(name))
			if kind == "" {
				kind = "application/octet-stream"
			}
			w.Header().Set("Content-Type", kind)
		}
		http.ServeContent(w, q, path.Base(name), time.Time{}, bytes.NewReader(data))
	})
}
