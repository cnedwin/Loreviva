package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sort"
)

type CandidateOwner struct{ Store *store.Store }

func (o *CandidateOwner) ApplyOriginalCandidateTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt, result string, v domain.Versions, body []byte) (domain.CandidateCommit, error) {
	out := domain.CandidateCommit{ResultID: result, Versions: v, EventIDs: []string{}, MemoryIDs: []string{}}
	if _, e := o.EvaluateOriginalCandidateTx(ctx, tx, s, task, attempt, body); e != nil {
		return out, e
	}
	var c Candidate
	if e := json.Unmarshal(body, &c); e != nil {
		return out, e
	}
	if len(c.Actions) > 0 {
		a, e := world.ApplyActions(ctx, tx, world.ApplyActionsRequest{Scope: s, TaskID: task, ResultID: result, Versions: v, Intents: c.Actions})
		if e != nil {
			return out, e
		}
		out.EventIDs = a.CommittedEventIDs
		out.Versions.WorldRevision = a.NewWorldRevision
	}
	if len(c.MemoryProposals) > 0 {
		if _, e := memory.ApplyUpdates(tx, memory.MemoryUpdateRequest{Namespace: memory.Namespace{Scope: s, DataSubjectID: s.SubjectID}, TaskID: task, Nodes: c.MemoryProposals}); e != nil {
			return out, e
		}
		for _, n := range c.MemoryProposals {
			out.MemoryIDs = append(out.MemoryIDs, n.ID)
		}
	}
	return out, nil
}

func candidateResourceKey(s domain.Scope, kind, id string) store.LockKey {
	return store.LockKey{Rank: 90, Kind: kind, IDs: []string{s.TenantID, s.ProductID, s.WorldInstanceID, id}}
}

func (o *CandidateOwner) PlanOriginalCandidate(ctx context.Context, s domain.Scope, task, result string, body []byte) ([]store.LockKey, error) {
	if (OriginalResultProgram{}).ValidateOriginalCandidate(body) != "STRUCTURE_VALID" {
		return nil, store.ErrDenied
	}
	var c Candidate
	if e := json.Unmarshal(body, &c); e != nil {
		return nil, e
	}
	keys := world.ActionLocks(world.ApplyActionsRequest{Scope: s, TaskID: task, ResultID: result, Intents: c.Actions})
	for _, a := range c.Actions {
		var raw []byte
		if e := o.Store.DB.QueryRowContext(ctx, "SELECT document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, a.ItemID).Scan(&raw); e != nil {
			return nil, e
		}
		var item world.ItemState
		if e := json.Unmarshal(raw, &item); e != nil {
			return nil, e
		}
		if item.CustodianID != "" {
			keys = append(keys, candidateResourceKey(s, "actor", item.CustodianID))
		}
	}
	for _, n := range c.MemoryProposals {
		keys = append(keys, candidateResourceKey(s, "actor", n.ActorID))
		if n.TargetKind == "ACTOR" {
			keys = append(keys, candidateResourceKey(s, "actor", n.TargetID))
		}
	}
	keys = append(keys, (memory.Namespace{Scope: s, DataSubjectID: s.SubjectID}).Locks()...)
	return keys, nil
}

func (o *CandidateOwner) EvaluateOriginalCandidateTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt string, body []byte) ([]domain.CandidateResource, error) {
	return o.evaluateCandidateTx(ctx, tx, s, task, attempt, body, false)
}

// Complete original semantic inspection is distinct from current world-write
// validation. Its fixed caller has already revalidated S46 current authority.
func (o *CandidateOwner) ValidateOriginalRecoveryCandidateTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt string, body []byte) error {
	_, e := o.evaluateCandidateTx(ctx, tx, s, task, attempt, body, true)
	return e
}

// The typed program output is the actual local rule decision, never a packet.
func (o *CandidateOwner) EvaluateSkillActionsTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt string, body []byte) ([]domain.CandidateResource, [32]byte, error) {
	var decision world.ActionDecision
	resources, e := o.evaluateCandidateTx(ctx, tx, s, task, attempt, body, false, &decision)
	if e != nil {
		return nil, [32]byte{}, e
	}
	if !decision.Accepted {
		return nil, [32]byte{}, store.ErrDenied
	}
	raw, e := json.Marshal(decision)
	return resources, sha256.Sum256(raw), e
}
func (o *CandidateOwner) evaluateCandidateTx(ctx context.Context, tx store.Tx, s domain.Scope, task, attempt string, body []byte, originalWorld bool, program ...*world.ActionDecision) ([]domain.CandidateResource, error) {
	if e := tx.AssertWorldTask(s, task); e != nil {
		return nil, e
	}
	var c Candidate
	if (OriginalResultProgram{}).ValidateOriginalCandidate(body) != "STRUCTURE_VALID" || json.Unmarshal(body, &c) != nil {
		return nil, store.ErrDenied
	}
	var primary string
	if e := tx.QueryRowContext(ctx, "SELECT actor_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&primary); e != nil {
		return nil, e
	}
	// R0 emits fixed neutral text or an explicit program-owned operation template.
	// It never accepts an ungrounded operation assertion as ordinary dialogue.
	if len(c.Actions) == 0 && c.Dialogue != "我听到了。" || len(c.Actions) > 0 && c.Dialogue != "我已完成请求的操作。" {
		return nil, store.ErrDenied
	}
	var snapshot domain.OriginalContextSnapshot
	var snapshotRaw []byte
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_attempt_contexts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", s.TenantID, s.ProductID, task, attempt).Scan(&snapshotRaw); e != nil {
		return nil, e
	}
	if json.Unmarshal(snapshotRaw, &snapshot) != nil || snapshot.Binding.ActorID != primary {
		return nil, store.ErrDenied
	}
	originalResources := map[string]string{}
	for _, r := range snapshot.World.Resources {
		originalResources[r.Kind+"/"+r.ID] = r.Hash
	}
	checkOriginal := func(kind, id, hash string) error {
		key := kind + "/" + id
		if originalResources[key] == "" || snapshot.World.Eligibility[key] == "" {
			return store.ErrDenied
		}
		if originalResources[key] != hash && !originalWorld {
			return &store.CandidateResourceConflict{Kind: kind, ID: id, OriginalHash: originalResources[key], CurrentHash: hash}
		}
		return nil
	}
	state := world.WorldState{Actors: map[string]world.ActorState{}, Items: map[string]world.ItemState{}}
	refs := map[string]domain.CandidateResource{}
	loadActor := func(id string) error {
		if e := tx.RequireLock(candidateResourceKey(s, "actor", id)); e != nil {
			return e
		}
		var raw []byte
		var a world.ActorState
		if e := tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, id).Scan(&raw); e != nil {
			return e
		}
		if e := json.Unmarshal(raw, &a); e != nil {
			return e
		}
		hash, e := content.Hash(a)
		if e != nil {
			return e
		}
		if e = checkOriginal("actor", id, hash); e != nil {
			return e
		}
		state.Actors[id] = a
		refs["actor/"+id] = domain.CandidateResource{Kind: "actor", ID: id, Hash: hash}
		return nil
	}
	for _, a := range c.Actions {
		// R0 Mock registers MOVE without free text. Annotation support would need
		// actual deletion lineage for item/event prose before it can be enabled.
		if a.ActorID != primary || a.Kind != "MOVE" || a.Text != "" || len(a.DependsOn) != 0 {
			return nil, store.ErrDenied
		}
		if e := tx.RequireLock(candidateResourceKey(s, "item", a.ItemID)); e != nil {
			return nil, e
		}
		var raw []byte
		var item world.ItemState
		if e := tx.QueryRowContext(ctx, "SELECT document FROM items WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND item_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, a.ItemID).Scan(&raw); e != nil {
			return nil, e
		}
		if e := json.Unmarshal(raw, &item); e != nil {
			return nil, e
		}
		hash, e := content.Hash(item)
		if e != nil {
			return nil, e
		}
		if e = checkOriginal("item", a.ItemID, hash); e != nil {
			return nil, e
		}
		if originalWorld {
			found := false
			for _, original := range snapshot.World.Items {
				if original.ID == item.ID {
					item.State = original.State
					item.HolderID = original.HolderID
					found = true
					break
				}
			}
			if !found {
				return nil, store.ErrDenied
			}
		}
		state.Items[a.ItemID] = item
		refs["item/"+a.ItemID] = domain.CandidateResource{Kind: "item", ID: a.ItemID, Hash: hash}
		for _, id := range []string{a.ActorID, a.TargetID, item.CustodianID} {
			if id != "" {
				if e := loadActor(id); e != nil {
					return nil, e
				}
			}
		}
	}
	if len(c.Actions) > 0 {
		decision, e := world.EvaluateActions(state, c.Actions)
		if len(program) == 1 && program[0] != nil {
			*program[0] = decision
		}
		if e != nil {
			return nil, e
		}
		if !decision.Accepted {
			return nil, store.ErrDenied
		}
	}
	for _, n := range c.MemoryProposals {
		if n.ActorID != primary || n.TargetKind != snapshot.Binding.TargetKind || n.TargetID != snapshot.Binding.TargetID {
			return nil, store.ErrDenied
		}
		for _, parent := range n.Parents {
			if snapshot.Sources.Nodes[parent] == "" {
				return nil, store.ErrDenied
			}
		}
	}
	if len(c.MemoryProposals) > 0 {
		if e := memory.ValidateUpdatesTx(ctx, tx, memory.MemoryUpdateRequest{Namespace: memory.Namespace{Scope: s, DataSubjectID: s.SubjectID}, TaskID: task, Nodes: c.MemoryProposals}); e != nil {
			return nil, e
		}
	}
	keys := []string{}
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := []domain.CandidateResource{}
	for _, key := range keys {
		out = append(out, refs[key])
	}
	return out, nil
}
