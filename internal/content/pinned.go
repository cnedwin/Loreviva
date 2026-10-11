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
	"sort"
)

type ResolvedDefinitions map[string]map[string]any

// ResolvePinned exposes the same immutable-release verifier to runtime assembly.
func ResolvePinned(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput) (ResolvedDefinitions, error) {
	resolved, findings, e := resolvePinned(ctx, tx, scope, in)
	if e != nil {
		return nil, e
	}
	if len(findings) > 0 {
		return nil, ErrBlocked
	}
	return resolved, nil
}
func ResolveDefinition(ref generated.DefinitionRef, in generated.DraftInput, resolved ResolvedDefinitions) (json.RawMessage, error) {
	var r map[string]any
	if e := json.Unmarshal(ref, &r); e != nil {
		return nil, e
	}
	local, e := objects(in)
	if e != nil {
		return nil, e
	}
	m := resolveDefinition(r, local, resolved)
	if m == nil {
		return nil, ErrBlocked
	}
	return json.Marshal(m)
}

func referenceKey(ref map[string]any) string {
	return Identity(fmt.Sprint(ref["release_id"]), fmt.Sprint(ref["object_id"]), fmt.Sprint(ref["version"]), fmt.Sprint(ref["content_hash"]))
}
func resolveDefinition(ref map[string]any, local map[string]map[string]any, resolved ResolvedDefinitions) map[string]any {
	if ref["kind"] == "LOCAL_DRAFT" {
		return local[fmt.Sprint(ref["object_id"])]
	}
	if ref["kind"] == "PINNED_DEFINITION" {
		return resolved[referenceKey(ref)]
	}
	return nil
}
func firstResolved(options []ResolvedDefinitions) ResolvedDefinitions {
	if len(options) > 0 {
		return options[0]
	}
	return ResolvedDefinitions{}
}
func pinReference(dep generated.DependencyLock) map[string]any {
	return map[string]any{"kind": "PINNED_DEFINITION", "release_id": string(dep.ReleaseID), "object_id": string(dep.ObjectID), "version": string(dep.Version), "content_hash": dep.ContentHash}
}

// Reads only explicitly declared scoped immutable releases under registered locks.
// No draft/secret/runtime state is materialized into the maintained author input.
func resolvePinned(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput) (ResolvedDefinitions, []generated.FieldError, error) {
	resolved := ResolvedDefinitions{}
	findings := []generated.FieldError{}
	catalog := map[string]map[string]map[string]any{}
	add := func(code, message string) {
		if len(findings) < 200 {
			findings = append(findings, generated.FieldError{Pointer: "/dependency_locks", Code: generated.Identity(code), Message: message})
		}
	}
	for _, dep := range in.DependencyLocks {
		releaseID := string(dep.ReleaseID)
		if e := tx.RequireLock(Key(scope, "content_release", releaseID)); e != nil {
			return nil, nil, e
		}
		if catalog[releaseID] == nil {
			var b, storedHash []byte
			e := tx.QueryRowContext(ctx, "SELECT c.document,c.content_hash FROM content_releases c JOIN releases r ON r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.release_id=c.release_id WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", scope.TenantID, scope.ProductID, releaseID).Scan(&b, &storedHash)
			if errors.Is(e, sql.ErrNoRows) {
				add("DEPENDENCY_UNAVAILABLE", "Pinned definition is unavailable in this product.")
				continue
			}
			if e != nil {
				return nil, nil, e
			}
			var release generated.Release
			if e = generated.Decode("Release", b, &release); e != nil {
				return nil, nil, e
			}
			hash, e := Hash(struct {
				Input        generated.DraftInput
				Dependencies []generated.DependencyLock
				Mappings     any
			}{release.Input, release.DependencyLocks, release.RouteMappings})
			if e != nil {
				return nil, nil, e
			}
			if release.Scope != Scope(scope) || string(release.ReleaseID) != releaseID || hash != release.ContentHash || hex.EncodeToString(storedHash) != hash {
				add("RELEASE_HASH", "Immutable release identity or content hash differs.")
				continue
			}
			maps, e := objects(release.Input)
			if e != nil {
				return nil, nil, e
			}
			catalog[releaseID] = maps
		}
		m := catalog[releaseID][string(dep.ObjectID)]
		if m == nil {
			add("DEPENDENCY_HASH", "Pinned object is absent.")
			continue
		}
		hash, e := Hash(m)
		if e != nil {
			return nil, nil, e
		}
		if hash != dep.ContentHash || m["version"] != string(dep.Version) {
			add("DEPENDENCY_HASH", "Pinned version or definition hash differs.")
			continue
		}
		resolved[referenceKey(pinReference(dep))] = m
	}
	state := map[string]int{}
	nodes := 0
	var visit func(string, string, int) bool
	var refs func(any, func(map[string]any) bool) bool
	refs = func(v any, emit func(map[string]any) bool) bool {
		switch x := v.(type) {
		case map[string]any:
			if x["kind"] == "LOCAL_DRAFT" || x["kind"] == "PINNED_DEFINITION" {
				return emit(x)
			}
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if !refs(x[k], emit) {
					return false
				}
			}
		case []any:
			for _, v := range x {
				if !refs(v, emit) {
					return false
				}
			}
		}
		return true
	}
	visit = func(releaseID, objectID string, depth int) bool {
		key := releaseID + "/" + objectID
		if depth > 64 || nodes >= 10000 || state[key] == 1 {
			add("REFERENCE_GRAPH", "Pinned dependency resolution is cyclic or exceeds bounded depth/work.")
			return false
		}
		if state[key] == 2 {
			return true
		}
		if state[key] == 3 {
			return false
		}
		m := catalog[releaseID][objectID]
		if m == nil {
			add("MISSING_REFERENCE", "Pinned release contains an unresolved local definition.")
			return false
		}
		state[key] = 1
		nodes++
		ok := refs(m, func(ref map[string]any) bool {
			if ref["kind"] == "LOCAL_DRAFT" {
				return visit(releaseID, fmt.Sprint(ref["object_id"]), depth+1)
			}
			if resolved[referenceKey(ref)] == nil {
				add("UNPINNED_DEPENDENCY", "Transitive pinned reference requires an exact declared dependency lock.")
				return false
			}
			return visit(fmt.Sprint(ref["release_id"]), fmt.Sprint(ref["object_id"]), depth+1)
		})
		if ok {
			state[key] = 2
		} else {
			state[key] = 3
		}
		return ok
	}
	for _, dep := range in.DependencyLocks {
		if resolved[referenceKey(pinReference(dep))] != nil && !visit(string(dep.ReleaseID), string(dep.ObjectID), 0) {
			delete(resolved, referenceKey(pinReference(dep)))
		}
	}
	return resolved, findings, nil
}
