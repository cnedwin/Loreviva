package wepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
)

// PortablePlan is a deterministic candidate, never a persistable local draft.
// ExecutionReady remains false until a separate authorized installation flow.
type PortablePlan struct {
	PlanVersion    string              `json:"plan_version"`
	ExecutionReady bool                `json:"execution_ready"`
	Namespace      string              `json:"namespace"`
	ArchiveSHA256  string              `json:"archive_sha256"`
	ManifestSHA256 string              `json:"manifest_sha256"`
	ContentSHA256  string              `json:"content_sha256"`
	ReportSHA256   string              `json:"report_sha256"`
	Entrypoint     string              `json:"entrypoint"`
	BodySHA256     string              `json:"body_sha256"`
	Kind           string              `json:"kind"`
	RootObjectID   string              `json:"root_object_id"`
	Body           json.RawMessage     `json:"body"`
	IDs            []PortableID        `json:"ids"`
	References     []PortableReference `json:"references"`
	Files          []PortableFile      `json:"files"`
	Requirements   Requirements        `json:"requirements"`
	PlanSHA256     string              `json:"plan_sha256"`
}
type PortableID struct {
	Domain    string `json:"domain"`
	Owner     string `json:"owner"`
	LogicalID string `json:"logical_id"`
	LocalID   string `json:"local_id"`
}
type PortableReference struct {
	Pointer        string   `json:"pointer"`
	OwnerObjectID  string   `json:"owner_object_id"`
	Kind           string   `json:"kind"`
	ObjectID       string   `json:"object_id"`
	LogicalID      string   `json:"logical_id"`
	PackageID      string   `json:"package_id"`
	PackageVersion string   `json:"package_version"`
	ContentSHA256  string   `json:"content_sha256"`
	ExpectedKinds  []string `json:"expected_kinds"`
}
type PortableFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	Rights    Rights `json:"rights"`
}

func portableDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func portableID(namespace string, m Manifest, entry Entry, id PortableID) (string, error) {
	raw, err := json.Marshal([]string{"loreviva.portable-map.v1", namespace, m.PackageID, m.PackageVersion, m.ContentSHA256, entry.LogicalID, id.Domain, id.Owner, id.LogicalID})
	if err != nil {
		return "", err
	}
	canonical, err := wire.Canonical(raw)
	if err != nil {
		return "", err
	}
	return portableDigest(canonical), nil
}

// Reject even hypothetical collisions across domains rather than silently
// sharing an address. No caller can replace the production digest function.
func validatePortableIDs(ids []PortableID) error {
	seen := map[string]string{}
	for _, id := range ids {
		if !hashPattern.MatchString(id.LocalID) {
			return errors.New("invalid candidate identity")
		}
		address := id.Domain + "\x00" + id.Owner + "\x00" + id.LogicalID
		if prior, ok := seen[id.LocalID]; ok && prior != address {
			return errors.New("candidate identity collision")
		}
		seen[id.LocalID] = address
	}
	return nil
}
func portablePlanHash(p PortablePlan, max int64) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", wire.ErrJSON
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return "", err
	}
	// The bound applies to the encoded preimage after excluding its own hash.
	// Inserting a final hash must not change the accepted preimage boundary.
	delete(fields, "plan_sha256")
	raw, err = json.Marshal(fields)
	if err != nil {
		return "", err
	}
	canonical, err := wire.CanonicalBounded(raw, max)
	if err != nil {
		return "", err
	}
	return portableDigest(canonical), nil
}

// BuildPortablePlan rechecks original bytes. A supplied report, logical name,
// or namespace cannot authorize installation or manufacture a local binding.
func BuildPortablePlan(raw []byte, lim Limits, entrypoint, namespace string) (PortablePlan, []Finding) {
	fail := func(code, detail string) (PortablePlan, []Finding) {
		return PortablePlan{}, []Finding{{Severity: Error, Code: code, Detail: detail}}
	}
	rep := Check(raw, lim)
	if rep.Verdict != "PASS" {
		return PortablePlan{}, rep.Findings
	}
	if !hashPattern.MatchString(namespace) {
		return fail(CodePortablePlanInputInvalid, "namespace must be an explicit lowercase digest")
	}
	manifestRaw, err := readMember(raw, "manifest.json", lim.ManifestBytes)
	if err != nil {
		return fail(CodePortablePlanInputInvalid, "manifest is unavailable")
	}
	m, findings := ParseManifest(manifestRaw, lim)
	if Verdict(findings) != "PASS" {
		return PortablePlan{}, findings
	}
	if !stringSet(m.Entrypoints)[entrypoint] {
		return fail(CodePortablePlanInputInvalid, "entrypoint is not declared")
	}
	var selected Entry
	for _, entry := range m.Entries {
		if entry.LogicalID == entrypoint {
			selected = entry
			break
		}
	}
	if !rootKinds[selected.Kind] {
		return fail(CodePortablePlanInputInvalid, "entrypoint has no portable root")
	}
	meta, err := generated.PortableMetadata()
	if err != nil {
		return fail(CodePortableBodySchema, "portable schema unavailable")
	}
	body, err := readMember(raw, selected.Path, meta.BodyBytes)
	if err != nil {
		return fail(CodePortablePlanInputInvalid, "entrypoint body is unavailable")
	}
	d := newPortableDiagnostics(meta)
	b := portableBundleCheck(body, m, selected, 0, meta, d)
	if len(d.rows) > 0 {
		return PortablePlan{}, d.rows
	}
	for i := range b.ids {
		id, err := portableID(namespace, m, selected, b.ids[i])
		if err != nil {
			return fail(CodePortablePlanLimit, "candidate identity cannot be encoded")
		}
		b.ids[i].LocalID = id
	}
	if err = validatePortableIDs(b.ids); err != nil {
		return fail(CodePortableIdentityDuplicate, "candidate identities collide")
	}
	sort.Slice(b.ids, func(i, j int) bool {
		a, c := b.ids[i], b.ids[j]
		return a.Domain+"\x00"+a.Owner+"\x00"+a.LogicalID < c.Domain+"\x00"+c.Owner+"\x00"+c.LogicalID
	})
	refKey := func(r PortableReference) string {
		return strings.Join([]string{r.Pointer, r.Kind, r.ObjectID, r.LogicalID, r.PackageID, r.PackageVersion, r.ContentSHA256}, "\x00")
	}
	sort.Slice(b.references, func(i, j int) bool { return refKey(b.references[i]) < refKey(b.references[j]) })
	root := ""
	for _, id := range b.ids {
		if id.Domain == "OBJECT" && id.LogicalID == b.root {
			root = id.LocalID
		}
	}
	p := PortablePlan{PlanVersion: "1", ExecutionReady: false, Namespace: namespace, ArchiveSHA256: rep.ArchiveSHA256, ManifestSHA256: rep.ManifestSHA256, ContentSHA256: m.ContentSHA256, ReportSHA256: rep.ReportSHA256, Entrypoint: entrypoint, BodySHA256: portableDigest(body), Kind: selected.Kind, RootObjectID: root, Body: append(json.RawMessage{}, body...), IDs: b.ids, References: b.references, Files: b.files, Requirements: m.Requirements}
	p.PlanSHA256, err = portablePlanHash(p, meta.PlanBytes)
	if err != nil {
		return fail(CodePortablePlanLimit, "portable plan exceeds its encoding limit")
	}
	return p, []Finding{}
}
