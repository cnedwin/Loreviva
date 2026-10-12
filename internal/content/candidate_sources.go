package content

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Planning returns locks only. It cannot authorize a source or return its body.
func (s *Service) candidateSourceLocks(ctx context.Context, scope domain.ProductScope, in generated.DraftInput) ([]store.LockKey, error) {
	keys := dependencyLocks(scope, in)
	seen := map[string]bool{}
	for _, dep := range in.DependencyLocks {
		id := string(dep.ReleaseID)
		if seen[id] {
			continue
		}
		seen[id] = true
		var raw []byte
		if e := s.Store.DB.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", scope.TenantID, scope.ProductID, id).Scan(&raw); e != nil {
			return nil, e
		}
		var release generated.Release
		if e := generated.Decode("Release", raw, &release); e != nil {
			return nil, e
		}
		for _, key := range dependencyLocks(scope, release.Input) {
			if key.Kind == "asset" {
				keys = append(keys, key)
			}
		}
	}
	return keys, nil
}

func candidateRights(m map[string]any, now time.Time) error {
	if m == nil {
		return ErrBlocked
	}
	if m["audience_policy"] != "PRODUCT_EDITORS" && m["audience_policy"] != "PRODUCT_READERS" {
		return ErrBlocked
	}
	var rights generated.SourceRights
	raw, e := json.Marshal(m["rights"])
	if e != nil || json.Unmarshal(raw, &rights) != nil {
		return ErrBlocked
	}
	return candidateUse(rights, now)
}
func candidateUse(rights generated.SourceRights, now time.Time) error {
	edit := false
	for _, use := range rights.AllowedUses {
		edit = edit || use == "EDIT"
	}
	if !edit || strings.TrimSpace(rights.License) == "" {
		return ErrBlocked
	}
	if rights.ExpiresAt != nil {
		expiry, e := time.Parse(time.RFC3339Nano, *rights.ExpiresAt)
		if e != nil || !now.Before(expiry) {
			return ErrBlocked
		}
	}
	return nil
}

// Only the selected object's declared origin and dependency edges are traversed.
// String source_refs remain hashed provenance, not dereferenceable permission.
// Display asset_refs and route configuration are not selected model input.
func candidateEdges(m map[string]any) []map[string]any {
	out := []map[string]any{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["kind"] == "LOCAL_DRAFT" || x["kind"] == "PINNED_DEFINITION" {
				out = append(out, x)
				return
			}
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k])
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	for _, field := range []string{"origin_ref", "source_refs", "dependency_refs"} {
		walk(m[field])
	}
	return out
}

func candidateSourcesTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput, object string) ([]domain.CandidateSource, error) {
	return sourceGraphTx(ctx, tx, scope, in, object, false)
}

func sourceGraphTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput, object string, rehearsal bool) ([]domain.CandidateSource, error) {
	resolved, e := ResolvePinned(ctx, tx, scope, in)
	if e != nil {
		return nil, e
	}
	local, e := objects(in)
	if e != nil {
		return nil, e
	}
	catalog := map[string]map[string]map[string]any{"": local}
	releaseOwners := map[string]string{}
	for _, dep := range in.DependencyLocks {
		id := string(dep.ReleaseID)
		if catalog[id] != nil {
			continue
		}
		if e = tx.RequireLock(Key(scope, "content_release", id)); e != nil {
			return nil, e
		}
		var raw []byte
		if e = tx.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", scope.TenantID, scope.ProductID, id).Scan(&raw); e != nil {
			return nil, e
		}
		var release generated.Release
		if e = generated.Decode("Release", raw, &release); e != nil {
			return nil, e
		}
		catalog[id], e = objects(release.Input)
		releaseOwners[id] = string(release.PublishedBySubjectID)
		if e != nil {
			return nil, e
		}
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return nil, e
	}
	out := []domain.CandidateSource{}
	state := map[string]int{}
	provenance := map[string]int{}
	var checkProvenance func(string, string, int) error
	checkProvenance = func(release, id string, depth int) error {
		key := release + "/" + id
		if depth > 64 || len(provenance) >= 10000 || provenance[key] == 1 {
			return ErrBlocked
		}
		if provenance[key] == 2 {
			return nil
		}
		m := catalog[release][id]
		if m == nil {
			return ErrBlocked
		}
		provenance[key] = 1
		for _, ref := range candidateEdges(m) {
			r := release
			if ref["kind"] == "PINNED_DEFINITION" {
				if resolved[referenceKey(ref)] == nil {
					return ErrBlocked
				}
				r = fmt.Sprint(ref["release_id"])
			}
			if e := checkProvenance(r, fmt.Sprint(ref["object_id"]), depth+1); e != nil {
				return e
			}
		}
		provenance[key] = 2
		return nil
	}
	var visit func(string, string, int) error
	visit = func(release, id string, depth int) error {
		key := release + "/" + id
		if rehearsal && state[key] == 1 {
			return nil
		} // bounded runtime scene/item/rule back-reference
		if depth > 64 || len(state) >= 10000 || state[key] == 1 {
			return ErrBlocked
		}
		if state[key] == 2 {
			return nil
		}
		if rehearsal {
			if e := checkProvenance(release, id, 0); e != nil {
				return e
			}
		}
		m := catalog[release][id]
		if e := candidateRights(m, now); e != nil {
			return e
		}
		if release != "" && m["audience_policy"] == "PRODUCT_EDITORS" && releaseOwners[release] != scope.SubjectID {
			return ErrBlocked
		}
		state[key] = 1
		hash, e := Hash(m)
		if e != nil {
			return e
		}
		node := domain.CandidateSource{ReleaseID: release, ObjectID: id, Kind: fmt.Sprint(m["kind"]), Version: fmt.Sprint(m["version"]), Hash: hash}
		if node.Kind == "ASSET" {
			node.AssetID, _ = m["asset_id"].(string)
			node.AssetHash, _ = m["asset_sha256"].(string)
			if e = tx.RequireLock(Key(scope, "asset", node.AssetID)); e != nil {
				return e
			}
			var raw []byte
			var state, owner string
			if e = tx.QueryRowContext(ctx, "SELECT state,owner_subject_id,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, node.AssetID).Scan(&state, &owner, &raw); e != nil {
				return e
			}
			var asset generated.FileRecord
			if generated.Decode("FileRecord", raw, &asset) != nil || state != "READY" || asset.Scope != Scope(scope) || string(asset.AssetID) != node.AssetID || asset.Sha256 != node.AssetHash || string(asset.SizeBytes) != fmt.Sprint(m["size_bytes"]) || string(asset.OwnerSubjectID) != owner {
				return ErrBlocked
			}
			if asset.AudiencePolicy != "PRODUCT_READERS" && (asset.AudiencePolicy != "PRODUCT_EDITORS" || owner != scope.SubjectID) {
				return ErrBlocked
			}
			if e = candidateUse(asset.Rights, now); e != nil {
				return e
			}
		}
		out = append(out, node)
		edges := candidateEdges(m)
		if rehearsal {
			edges = rehearsalEdges(m)
		}
		for _, ref := range edges {
			r := release
			if ref["kind"] == "PINNED_DEFINITION" {
				if resolved[referenceKey(ref)] == nil {
					return ErrBlocked
				}
				r = fmt.Sprint(ref["release_id"])
			}
			if e = visit(r, fmt.Sprint(ref["object_id"]), depth+1); e != nil {
				return e
			}
		}
		state[key] = 2
		return nil
	}
	if e = visit("", object, 0); e != nil {
		return nil, e
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReleaseID != out[j].ReleaseID {
			return out[i].ReleaseID < out[j].ReleaseID
		}
		return out[i].ObjectID < out[j].ObjectID
	})
	return out, nil
}

func (s *Service) PlanCandidateHistoryLocks(ctx context.Context, scope domain.ProductScope, m domain.DraftCandidateMaterial) ([]store.LockKey, error) {
	if len(m.Sources) == 0 || len(m.Sources) > 10000 || len(m.SourceHash) != 64 {
		return nil, ErrBlocked
	}
	// This private read is lock planning only, not permission to disclose a body.
	var raw []byte
	if e := s.Store.DB.QueryRowContext(ctx, "SELECT document FROM content_revisions WHERE tenant_id=? AND product_id=? AND draft_id=? AND revision=?", scope.TenantID, scope.ProductID, m.Selection.DraftID, m.Selection.Revision).Scan(&raw); e != nil {
		return nil, e
	}
	var d generated.ContentDraft
	if e := generated.Decode("ContentDraft", raw, &d); e != nil {
		return nil, e
	}
	keys, e := s.candidateSourceLocks(ctx, scope, candidateHistoryInput(d.Input, m))
	if e != nil {
		return nil, e
	}
	return append(keys, Key(scope, "content_draft", m.Selection.DraftID)), nil
}

// Fixed S55 terminal source inspection. No route resolution, assist permission,
// execution lease or equality with the latest draft revision is used here.
func (s *Service) InspectCandidateSourcesTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, m domain.DraftCandidateMaterial) error {
	if len(m.Sources) == 0 || len(m.Sources) > 10000 || len(m.SourceHash) != 64 {
		return ErrBlocked
	}
	if e := tx.RequireLock(Key(scope, "content_draft", m.Selection.DraftID)); e != nil {
		return e
	}
	var original, currentRaw, storedHash []byte
	e := tx.QueryRowContext(ctx, "SELECT old.document,old.content_hash,cur.document FROM content_drafts d JOIN content_revisions old ON old.tenant_id=d.tenant_id AND old.product_id=d.product_id AND old.draft_id=d.draft_id AND old.revision=? JOIN content_revisions cur ON cur.tenant_id=d.tenant_id AND cur.product_id=d.product_id AND cur.draft_id=d.draft_id AND cur.revision=d.current_revision WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=? AND d.deleted=0", m.Selection.Revision, scope.TenantID, scope.ProductID, m.Selection.DraftID, scope.SubjectID).Scan(&original, &storedHash, &currentRaw)
	if e != nil {
		return e
	}
	var old, currentDraft generated.ContentDraft
	if generated.Decode("ContentDraft", original, &old) != nil || generated.Decode("ContentDraft", currentRaw, &currentDraft) != nil || old.Scope != Scope(scope) || currentDraft.Scope != Scope(scope) || string(old.DraftID) != m.Selection.DraftID || hex.EncodeToString(storedHash) != m.DraftHash || old.ContentHash != m.DraftHash {
		return ErrBlocked
	}
	h, e := Hash(old.Input)
	if e != nil || h != m.DraftHash {
		return ErrBlocked
	}
	sources, e := candidateSourcesTx(ctx, tx, scope, candidateHistoryInput(old.Input, m), m.Selection.ObjectID)
	if e != nil {
		return e
	}
	h, e = Hash(sources)
	if e != nil || h != m.SourceHash || !reflect.DeepEqual(sources, m.Sources) {
		return ErrBlocked
	}
	currentObjects, e := objects(currentDraft.Input)
	if e != nil {
		return e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	for _, source := range sources {
		if source.ReleaseID == "" {
			current := currentObjects[source.ObjectID]
			if current == nil || current["kind"] != source.Kind {
				return ErrBlocked
			}
			if e = candidateRights(current, now); e != nil {
				return e
			}
			if source.AssetID != "" && (current["asset_id"] != source.AssetID || current["asset_sha256"] != source.AssetHash) {
				return ErrBlocked
			}
		}
	}
	return nil
}

func candidateHistoryInput(in generated.DraftInput, m domain.DraftCandidateMaterial) generated.DraftInput {
	releases := map[string]bool{}
	for _, source := range m.Sources {
		if source.ReleaseID != "" {
			releases[source.ReleaseID] = true
		}
	}
	deps := []generated.DependencyLock{}
	for _, dep := range in.DependencyLocks {
		if releases[string(dep.ReleaseID)] {
			deps = append(deps, dep)
		}
	}
	in.DependencyLocks = deps
	return in
}

// Cleanup consumes the same original source closure without obtaining any
// result-reading or generation permission. Infrastructure/replan errors are
// not interpreted as source withdrawal and cannot cause a purge.
func (s *Service) CandidateSourcesLiveTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, m domain.DraftCandidateMaterial) (bool, error) {
	e := s.InspectCandidateSourcesTx(ctx, tx, scope, m)
	if e == nil {
		return true, nil
	}
	if errors.Is(e, ErrBlocked) || errors.Is(e, ErrNotFound) || errors.Is(e, sql.ErrNoRows) || errors.Is(e, store.ErrDenied) {
		return false, nil
	}
	return false, e
}
