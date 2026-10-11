package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
)

type ApplyActionsRequest struct {
	Scope            domain.Scope
	TaskID, ResultID string
	Versions         domain.Versions
	Intents          []ActionIntent
}
type ActionResult struct {
	CommittedEventIDs []string
	NewWorldRevision  uint64
}

// ActionLocks is pure precollection; all corresponding facts are loaded again
// inside the authoritative execution barrier before evaluation.
func ActionLocks(q ApplyActionsRequest) []store.LockKey {
	keys := eventLocks(q.Scope.ProductScope, q.ResultID)
	for _, a := range q.Intents {
		keys = append(keys, resourceKey(q.Scope, "actor", a.ActorID), resourceKey(q.Scope, "item", a.ItemID))
		if a.TargetID != "" {
			keys = append(keys, resourceKey(q.Scope, "actor", a.TargetID))
		}
	}
	return keys
}
func ApplyActions(ctx context.Context, tx store.Tx, q ApplyActionsRequest) (ActionResult, error) {
	out := ActionResult{CommittedEventIDs: []string{}}
	if e := tx.AssertWorldTask(q.Scope, q.TaskID); e != nil {
		return out, e
	}
	if len(q.Intents) == 0 || len(q.Intents) > 32 {
		return out, store.ErrDenied
	}
	if e := tx.RequireLock(worldKey(q.Scope)); e != nil {
		return out, e
	}
	var status string
	var v domain.Versions
	e := tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", q.Scope.TenantID, q.Scope.ProductID, q.Scope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status)
	if e != nil {
		return out, e
	}
	if status != "RUNNING" || v != q.Versions {
		return out, store.ErrDenied
	}
	state := WorldState{Actors: map[string]ActorState{}, Items: map[string]ItemState{}}
	for _, a := range q.Intents {
		if e = tx.RequireLock(resourceKey(q.Scope, "item", a.ItemID)); e != nil {
			return out, e
		}
		var raw []byte
		if e = tx.QueryRowContext(ctx, "SELECT document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", q.Scope.TenantID, q.Scope.ProductID, q.Scope.WorldInstanceID, a.ItemID).Scan(&raw); e != nil {
			return out, e
		}
		var item ItemState
		if e = json.Unmarshal(raw, &item); e != nil {
			return out, e
		}
		state.Items[a.ItemID] = item
		for _, id := range []string{a.ActorID, a.TargetID, item.CustodianID} {
			if id == "" {
				continue
			}
			if e = tx.RequireLock(resourceKey(q.Scope, "actor", id)); e != nil {
				return out, e
			}
			if e = tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", q.Scope.TenantID, q.Scope.ProductID, q.Scope.WorldInstanceID, id).Scan(&raw); e != nil {
				return out, e
			}
			var actor ActorState
			if e = json.Unmarshal(raw, &actor); e != nil {
				return out, e
			}
			state.Actors[id] = actor
		}
	}
	decision, e := EvaluateActions(state, q.Intents)
	if e != nil {
		return out, e
	}
	if !decision.Accepted {
		return out, store.ErrDenied
	}
	for id, item := range decision.Items {
		if e = tx.RequireLock(resourceKey(q.Scope, "item", id)); e != nil {
			return out, e
		}
		raw, e := json.Marshal(item)
		if e != nil {
			return out, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE items SET document=? WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", raw, q.Scope.TenantID, q.Scope.ProductID, q.Scope.WorldInstanceID, id); e != nil {
			return out, e
		}
	}
	if e = tx.AdvanceWorld(ctx, "ACTIONS", ""); e != nil {
		return out, e
	}
	if e = recordResult(ctx, tx, q.Scope, q.TaskID, q.ResultID, v.WorldRevision+1, struct {
		Kind    string
		Actions []ActionIntent
	}{"ACTION_BUNDLE", q.Intents}); e != nil {
		return out, e
	}
	if e = tx.SetTaskState(ctx, "COMMITTED"); e != nil {
		return out, e
	}
	return ActionResult{CommittedEventIDs: []string{q.ResultID}, NewWorldRevision: v.WorldRevision + 1}, nil
}
