package app

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"loreviva.local/loreviva/internal/generated"
)

// studio_dist contains the actual native Vue production build. Regenerate with
// pnpm build:studio; pnpm build:server checks source freshness before Go build.
//
//go:embed studio_dist
var studioFiles embed.FS

// Runtime checks need only embedded bytes. Source freshness is checked by the
// build entry point, because an installed server has no frontend source tree.
func validateStudioManifest(files fs.FS, sourceHash string) error {
	data, err := fs.ReadFile(files, "manifest.json")
	if err != nil {
		return err
	}
	var manifest struct {
		Format     string `json:"format"`
		SourceHash string `json:"source_hash"`
		Files      []struct {
			Path   string `json:"path"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if manifest.Format != "loreviva.studio-build.v1" || manifest.SourceHash != sourceHash {
		return fmt.Errorf("stale Studio manifest")
	}
	expected := make(map[string]bool)
	for _, file := range manifest.Files {
		if !fs.ValidPath(file.Path) || strings.Contains(file.Path, "\\") || expected[file.Path] || (file.Path != "index.html" && !strings.HasPrefix(file.Path, "assets/")) {
			return fmt.Errorf("invalid Studio inventory")
		}
		body, err := fs.ReadFile(files, file.Path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		if int64(len(body)) != file.Size || hex.EncodeToString(digest[:]) != file.SHA256 {
			return fmt.Errorf("Studio asset mismatch")
		}
		expected[file.Path] = true
	}
	if !expected["index.html"] {
		return fmt.Errorf("Studio entry missing")
	}
	return fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || name == "manifest.json" {
			return nil
		}
		if !entry.Type().IsRegular() || !expected[name] {
			return fmt.Errorf("unlisted Studio asset")
		}
		return nil
	})
}

var studioIntegrity = sync.OnceValue(func() error {
	files, err := fs.Sub(studioFiles, "studio_dist")
	if err != nil {
		return err
	}
	return validateStudioManifest(files, generated.SourceHash)
})

// Serve only the enumerated SPA routes or clean embedded asset paths using GET/HEAD and the restrictive CSP below.
// Unknown paths must not fall back to the SPA.
func studioHandler() http.Handler {
	integrityErr := studioIntegrity()
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
		if integrityErr != nil {
			http.Error(w, "Studio build is unavailable", http.StatusServiceUnavailable)
			return
		}
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
