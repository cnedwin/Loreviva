package content

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sort"
)

func importLineageKey(scope domain.ProductScope, kind, id string) store.LockKey {
	return Key(scope, "content_check", Identity("package-import-lineage", kind, id))
}
func importLineageLocks(scope domain.ProductScope, id string, input generated.DraftInput) []store.LockKey {
	keys := []store.LockKey{importLineageKey(scope, "DRAFT", id)}
	for _, raw := range input.Objects {
		var object struct {
			ID string `json:"object_id"`
		}
		json.Unmarshal(raw, &object)
		keys = append(keys, importLineageKey(scope, "OBJECT", object.ID))
	}
	return keys
}
func importOrigins(ctx context.Context, tx store.Tx, scope domain.ProductScope, kind, id string) (map[string]bool, error) {
	if err := tx.RequireLock(importLineageKey(scope, kind, id)); err != nil {
		return nil, err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(JSON_ARRAYAGG(CAST(import_id AS CHAR)),JSON_ARRAY()) FROM content_package_import_lineage WHERE tenant_id=? AND product_id=? AND resource_kind=? AND resource_id=?", scope.TenantID, scope.ProductID, kind, id).Scan(&raw); err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
func keepImportOrigins(ctx context.Context, tx store.Tx, scope domain.ProductScope, kind, id string, origins map[string]bool) error {
	if len(origins) == 0 {
		return nil
	}
	if err := tx.RequireLock(importLineageKey(scope, kind, id)); err != nil {
		return err
	}
	ids := []string{}
	for origin := range origins {
		ids = append(ids, origin)
	}
	sort.Strings(ids)
	for _, origin := range ids {
		if _, err := tx.ExecContext(ctx, "INSERT INTO content_package_import_lineage(tenant_id,product_id,resource_kind,resource_id,import_id) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE import_id=import_id", scope.TenantID, scope.ProductID, kind, id, origin); err != nil {
			return err
		}
	}
	return nil
}

// Taint is product scoped, not owner scoped: changing author or editing/removing
// source_refs cannot wash an imported identity into an ordinary draft.
func (s *Service) applyImportLineage(ctx context.Context, tx store.Tx, scope domain.ProductScope, d *generated.ContentDraft, extra map[string]bool) error {
	origins, err := importOrigins(ctx, tx, scope, "DRAFT", string(d.DraftID))
	if err != nil {
		return err
	}
	old := map[string]bool{}
	for id := range origins {
		old[id] = true
	}
	for id := range extra {
		origins[id] = true
	}
	ids := []string{}
	for _, raw := range d.Input.Objects {
		var object struct {
			ID string `json:"object_id"`
		}
		json.Unmarshal(raw, &object)
		ids = append(ids, object.ID)
		found, err := importOrigins(ctx, tx, scope, "OBJECT", object.ID)
		if err != nil {
			return err
		}
		for id := range found {
			origins[id] = true
		}
	}
	if len(origins) == 0 {
		return nil
	}
	if len(origins) > 200 {
		return ErrBlocked
	}
	for id := range origins {
		if !old[id] {
			if err = s.Store.RequirePackageImportCopy(ctx, tx, scope); err != nil {
				return err
			}
			break
		}
	}
	if d.State == "DRAFT" || d.State == "ISOLATED_IMPORT" {
		d.State = "ISOLATED_IMPORT"
	}
	if err = keepImportOrigins(ctx, tx, scope, "DRAFT", string(d.DraftID), origins); err != nil {
		return err
	}
	for _, id := range ids {
		if err = keepImportOrigins(ctx, tx, scope, "OBJECT", id, origins); err != nil {
			return err
		}
	}
	return nil
}
