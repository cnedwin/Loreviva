package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"sort"
)

const migrationPolicy = "r0-presentation-preserve-v1"

type migrationRows struct{ Tasks, Actors, Items, Memories, MemorySubjects []string }

func (r *Runtime) migrationRows(ctx context.Context, s domain.Scope) (migrationRows, error) {
	out := migrationRows{}
	for _, spec := range []struct {
		table, id string
		dest      *[]string
	}{{"tasks", "task_id", &out.Tasks}, {"actors", "actor_id", &out.Actors}, {"items", "item_id", &out.Items}, {"memory_aggregates", "aggregate_id", &out.Memories}, {"memory_aggregates", "data_subject_id", &out.MemorySubjects}} {
		rows, e := r.Store.DB.QueryContext(ctx, "SELECT "+spec.id+" FROM "+spec.table+" WHERE tenant_id=? AND product_id=? AND world_instance_id=? ORDER BY "+spec.id+" LIMIT 201", s.TenantID, s.ProductID, s.WorldInstanceID)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return out, e
			}
			*spec.dest = append(*spec.dest, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		if len(*spec.dest) > 200 {
			return out, content.ErrBlocked
		}
	}
	return out, nil
}
func migrationLocks(s domain.Scope, rows migrationRows) []store.LockKey {
	keys := []store.LockKey{}
	for _, id := range rows.MemorySubjects {
		keys = append(keys, store.LockKey{Rank: 30, IDs: []string{s.TenantID, s.ProductID, id}})
	}
	for _, id := range rows.Memories {
		keys = append(keys, resourceKey(s, "memory", id))
	}
	for _, id := range rows.Tasks {
		keys = append(keys, store.LockKey{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}})
	}
	for _, id := range rows.Actors {
		keys = append(keys, resourceKey(s, "actor", id))
	}
	for _, id := range rows.Items {
		keys = append(keys, resourceKey(s, "item", id))
	}
	return keys
}
func preserveHash(ctx context.Context, tx store.Tx, s domain.Scope, rows migrationRows, own string) (string, error) {
	var memoryCount int
	if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&memoryCount); e != nil {
		return "", e
	}
	if memoryCount != len(rows.Memories) {
		return "", store.ErrReplan
	}
	var taskCount, actorCount, itemCount, blocked, attempts int
	e := tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM tasks WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND task_id<>?),(SELECT COUNT(*) FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=?),(SELECT COUNT(*) FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=?)", s.TenantID, s.ProductID, s.WorldInstanceID, own, s.TenantID, s.ProductID, s.WorldInstanceID, s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&taskCount, &actorCount, &itemCount)
	if e != nil {
		return "", e
	}
	expected := 0
	for _, id := range rows.Tasks {
		if id != own {
			expected++
		}
	}
	if taskCount != expected || actorCount != len(rows.Actors) || itemCount != len(rows.Items) {
		return "", store.ErrReplan
	}
	// Until a closed ACCOUNTING_ONLY adapter exists, preserve and block every
	// attempt, including terminal-looking rows. A terminal task is not that proof.
	e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM attempts a JOIN tasks t ON t.tenant_id=a.tenant_id AND t.product_id=a.product_id AND t.task_id=a.task_id WHERE t.tenant_id=? AND t.product_id=? AND t.world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&attempts)
	if e != nil {
		return "", e
	}
	if attempts != 0 {
		return "", content.ErrBlocked
	}
	e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks t WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND task_id<>? AND NOT ((state='COMMITTED' AND EXISTS(SELECT 1 FROM commit_receipts c WHERE c.tenant_id=t.tenant_id AND c.product_id=t.product_id AND c.task_id=t.task_id)) OR (state='CANCELLED' AND cancelled=1))", s.TenantID, s.ProductID, s.WorldInstanceID, own).Scan(&blocked)
	if e != nil {
		return "", e
	}
	if blocked != 0 {
		return "", content.ErrBlocked
	}
	parts := []string{migrationPolicy}
	var initial []byte
	if e = tx.QueryRowContext(ctx, "SELECT initialization FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&initial); e != nil {
		return "", e
	}
	parts = append(parts, string(initial))
	for _, spec := range []struct {
		table, kind, column string
		ids                 []string
	}{{"actors", "actor", "actor_id", rows.Actors}, {"items", "item", "item_id", rows.Items}, {"memory_aggregates", "memory", "aggregate_id", rows.Memories}} {
		for _, id := range spec.ids {
			if e = tx.RequireLock(resourceKey(s, spec.kind, id)); e != nil {
				return "", e
			}
			var raw []byte
			if e = tx.QueryRowContext(ctx, "SELECT document FROM "+spec.table+" WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND "+spec.column+"=?", s.TenantID, s.ProductID, s.WorldInstanceID, id).Scan(&raw); e != nil {
				return "", e
			}
			parts = append(parts, spec.table, id, string(raw))
		}
	}
	for _, table := range []string{"events", "approved_results", "postings", "reservations"} {
		var count int
		id := "record_id"
		if table == "reservations" {
			id = "reservation_id"
		}
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" a JOIN tasks t ON t.tenant_id=a.tenant_id AND t.product_id=a.product_id AND t.task_id=a.task_id WHERE t.tenant_id=? AND t.product_id=? AND t.world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&count); e != nil {
			return "", e
		}
		if count > 1000 {
			return "", content.ErrBlocked
		}
		var raw []byte
		if e = tx.QueryRowContext(ctx, "SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('id',HEX(a."+id+"),'status',a.status,'revision',a.revision)),JSON_ARRAY()) FROM "+table+" a JOIN tasks t ON t.tenant_id=a.tenant_id AND t.product_id=a.product_id AND t.task_id=a.task_id WHERE t.tenant_id=? AND t.product_id=? AND t.world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&raw); e != nil {
			return "", e
		}
		var entries []json.RawMessage
		if e = json.Unmarshal(raw, &entries); e != nil {
			return "", e
		}
		texts := []string{}
		for _, entry := range entries {
			h, e := content.Hash(entry)
			if e != nil {
				return "", e
			}
			texts = append(texts, h)
		}
		sort.Strings(texts)
		parts = append(parts, table)
		parts = append(parts, texts...)
	}
	var bodies []byte
	e = tx.QueryRowContext(ctx, "SELECT COALESCE(JSON_ARRAYAGG(JSON_OBJECT('id',HEX(d.record_id),'hash',SHA2(d.document,256))),JSON_ARRAY()) FROM runtime_events d JOIN events e ON e.tenant_id=d.tenant_id AND e.product_id=d.product_id AND e.record_id=d.record_id WHERE e.tenant_id=? AND e.product_id=? AND e.world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&bodies)
	if e != nil {
		return "", e
	}
	var documents []json.RawMessage
	if e = json.Unmarshal(bodies, &documents); e != nil {
		return "", e
	}
	hashes := []string{}
	for _, doc := range documents {
		h, e := content.Hash(doc)
		if e != nil {
			return "", e
		}
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	parts = append(parts, hashes...)
	return content.Hash(parts)
}
func compatible(from, to generated.Release) ([]generated.MigrationDifference, error) {
	if from.ProjectID == nil || to.ProjectID == nil || *from.ProjectID != *to.ProjectID || from.Input.RootObjectID != to.Input.RootObjectID || from.DraftID != to.DraftID || from.Scope != to.Scope {
		return nil, content.ErrBlocked
	}
	a, b := from.Input, to.Input
	a.Objects = nil
	b.Objects = nil
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(from.RouteMappings, to.RouteMappings) {
		return nil, content.ErrBlocked
	}
	if len(from.Input.Objects) != len(to.Input.Objects) {
		return nil, content.ErrBlocked
	}
	targets := map[string]map[string]any{}
	for _, raw := range to.Input.Objects {
		var m map[string]any
		if e := json.Unmarshal(raw, &m); e != nil {
			return nil, e
		}
		id, _ := m["object_id"].(string)
		targets[id] = m
	}
	out := []generated.MigrationDifference{}
	for _, raw := range from.Input.Objects {
		var a map[string]any
		if e := json.Unmarshal(raw, &a); e != nil {
			return nil, e
		}
		id, _ := a["object_id"].(string)
		b := targets[id]
		if b == nil {
			return nil, content.ErrBlocked
		}
		ah, e := content.Hash(a)
		if e != nil {
			return nil, e
		}
		bh, e := content.Hash(b)
		if e != nil {
			return nil, e
		}
		if ah == bh {
			continue
		}
		if a["kind"] != "CHARACTER" || b["kind"] != "CHARACTER" {
			return nil, content.ErrBlocked
		}
		for _, key := range []string{"expression_style", "example_lines", "appearance_asset_refs", "initial_knowledge_refs", "version"} {
			delete(a, key)
			delete(b, key)
		}
		if !reflect.DeepEqual(a, b) {
			return nil, content.ErrBlocked
		}
		out = append(out, generated.MigrationDifference{Category: "REFERENCE", LogicalID: generated.Identity(id), FromHash: ah, TargetHash: bh, SupportedResolutions: []string{"KEEP_STATE"}, Blocking: false})
	}
	if len(out) == 0 || len(out) > 200 {
		return nil, content.ErrBlocked
	}
	return out, nil
}
