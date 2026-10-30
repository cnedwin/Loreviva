// Package sourceawarereply contains a source-owned, inert declarative example.
// It builds static bytes only; importing or executing them requires the normal
// current-authority creator and parent-task paths.
package sourceawarereply

import (
	_ "embed"
	"errors"
	"reflect"

	"loreviva.local/loreviva/internal/wepkg"
)

//go:embed manifest.json
var manifestBytes []byte

//go:embed definitions.json
var definitionBytes []byte

// Build uses the sole existing format builder, then requires the authored
// manifest's hashes to match. Editing content never silently repairs a stale
// source manifest or publishes another version under the creator's identity.
func Build() ([]byte, wepkg.Manifest, error) {
	m, findings := wepkg.ParseManifest(manifestBytes, wepkg.DefaultLimits)
	if len(findings) > 0 {
		return nil, wepkg.Manifest{}, errors.New("example manifest is invalid")
	}
	raw, checked, err := wepkg.ExportBundle(m, definitionBytes, map[string][]byte{})
	if err != nil {
		return nil, wepkg.Manifest{}, err
	}
	if !reflect.DeepEqual(m, checked) {
		return nil, wepkg.Manifest{}, errors.New("example source digests differ; update the manifest and version deliberately")
	}
	return raw, checked, nil
}

// Source returns a copy so callers can inspect or test variants without
// mutating the embedded original used by Build.
func Source() []byte { return append([]byte{}, definitionBytes...) }
