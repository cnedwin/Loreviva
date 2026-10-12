package integration

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestGenerationContextActorInitialAndDirectionalTarget(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "generation-context"), "START")
	br, q := actionTask(t, r, s, i, "context-turn")
	request := memory.ActorGenerationRequest{Scope: q.Scope, TaskID: q.TaskID, ActorID: q.Intents[0].ActorID, DataSubjectID: s.SubjectID, TargetKind: "ACTOR", TargetID: q.Intents[0].TargetID, Audience: memory.Private, Use: memory.PrivateResponse}
	br.Locks = append(br.Locks, memory.GenerationContextLocks(request)...)
	read := func(key string) (memory.ActorGenerationContext, error) {
		var result memory.ActorGenerationContext
		next := br
		next.CommandKey = key
		next.CommitKey = key
		_, e := r.Store.WithBarrier(context.Background(), next, func(tx store.Tx) error {
			var e error
			result, e = memory.AssembleGenerationContextTx(context.Background(), tx, request)
			return e
		})
		return result, e
	}
	// Administrative body access must never manufacture generation authority.
	if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.private_body.read','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	if _, e := read("no-generation-grant"); !errors.Is(e, store.ErrDenied) {
		t.Fatal("body read became generation permission", e)
	}
	if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator','memory.generation.use','PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)"); e != nil {
		t.Fatal(e)
	}
	result, e := read("context-positive")
	if e != nil {
		t.Fatal("legitimate actor generation context unavailable", e)
	}
	if result.Core.ActorID != request.ActorID || len(result.Initial) == 0 {
		t.Fatal("actor INITIAL missing for actual actor target", result)
	}
	for _, v := range result.Initial {
		if v.ActorID != request.ActorID {
			t.Fatal("another actor INITIAL leaked")
		}
	}
	if len(result.Relationship) != 0 {
		t.Fatal("invented actor relationship")
	}
	if result.Proof == nil {
		t.Fatal("missing proof")
	}
	// Recheck at a later short transaction, then reject a withdrawn independent grant.
	next := br
	next.CommandKey = "revalidate-context"
	next.CommitKey = next.CommandKey
	if _, e = r.Store.WithBarrier(context.Background(), next, func(tx store.Tx) error {
		return memory.ValidateGenerationContextTx(context.Background(), tx, request, result.Proof)
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
		t.Fatal(e)
	}
	next.CommandKey = "withdraw-context"
	next.CommitKey = next.CommandKey
	if _, e = r.Store.WithBarrier(context.Background(), next, func(tx store.Tx) error {
		return memory.ValidateGenerationContextTx(context.Background(), tx, request, result.Proof)
	}); !errors.Is(e, store.ErrDenied) {
		t.Fatal("withdrawn grant allowed old context", e)
	}
}

func TestGenerationContextDirectionalDerivedAndDeletedInitial(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "direction-context"), "START")
	br, q := actionTask(t, r, s, i, "direction-turn")
	ctx := context.Background()
	request := memory.ActorGenerationRequest{Scope: q.Scope, TaskID: q.TaskID, ActorID: q.Intents[0].ActorID, DataSubjectID: s.SubjectID, TargetKind: "ACTOR", TargetID: q.Intents[0].TargetID, Audience: memory.Private, Use: memory.PrivateResponse}
	br.Locks = append(br.Locks, memory.GenerationContextLocks(request)...)
	for _, action := range []string{"memory.generation.use", "memory.govern", "memory.source.govern"} {
		if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
			t.Fatal(e)
		}
	}
	read := func(key string, req memory.ActorGenerationRequest) (memory.ActorGenerationContext, error) {
		var out memory.ActorGenerationContext
		next := br
		next.CommandKey = key
		next.CommitKey = key
		next.Locks = append(next.Locks, memory.GenerationContextLocks(req)...)
		_, e := r.Store.WithBarrier(ctx, next, func(tx store.Tx) error {
			var e error
			out, e = memory.AssembleGenerationContextTx(ctx, tx, req)
			return e
		})
		return out, e
	}
	original, e := read("initial-context", request)
	if e != nil || len(original.Initial) == 0 {
		t.Fatal(original, e)
	}
	parent := original.Initial[0]
	ns := memory.Namespace{Scope: q.Scope, DataSubjectID: s.SubjectID}
	next := br
	next.CommandKey = "actual-direction-memory"
	next.CommitKey = next.CommandKey
	_, e = r.Store.WithBarrier(ctx, next, func(tx store.Tx) error {
		_, e := memory.ApplyUpdates(tx, memory.MemoryUpdateRequest{Namespace: ns, TaskID: q.TaskID, Nodes: []memory.MemoryRecord{
			{ID: "actor-direction", ActorID: request.ActorID, TargetKind: "ACTOR", TargetID: request.TargetID, Kind: "RELATIONSHIP", Domain: memory.ActorBelief, Audience: memory.Private, Uses: []memory.Use{memory.PrivateResponse}, Parents: []string{parent.ID}, Body: "Mia trusts Alex based on a governed experience", Revision: 1},
			{ID: "player-direction", ActorID: request.ActorID, TargetKind: "PLAYER", TargetID: s.SubjectID, Kind: "RELATIONSHIP", Domain: memory.ActorBelief, Audience: memory.Private, Uses: []memory.Use{memory.PrivateResponse}, Parents: []string{parent.ID}, Body: "private creator relationship", Revision: 1},
		}})
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	forward, e := read("forward", request)
	if e != nil || len(forward.Relationship) != 1 || forward.Relationship[0].ID != "actor-direction" {
		t.Fatal("direction projection incorrect", forward, e)
	}
	reverse := request
	reverse.ActorID, reverse.TargetID = request.TargetID, request.ActorID
	backward, e := read("reverse", reverse)
	if e != nil || len(backward.Relationship) != 0 || len(backward.Initial) == 0 {
		t.Fatal("reverse direction leak or missing independent initial", backward, e)
	}
	player := request
	player.TargetKind = "PLAYER"
	player.TargetID = s.SubjectID
	private, e := read("player", player)
	if e != nil || len(private.Relationship) != 1 || private.Relationship[0].ID != "player-direction" {
		t.Fatal("private player projection incorrect", private, e)
	}
	if len(private.Initial) != len(forward.Initial) {
		t.Fatal("initial duplicated or rebound per target")
	}
	wrong := player
	wrong.TargetID = "another-player"
	if _, e = read("wrong-player", wrong); e == nil {
		t.Fatal("another target accepted")
	}
	// Deletion is a real G0/G1/G2/G3 action; retained authoring fields stay in the
	// original actor document but must not refill INITIAL or its descendants.
	source := parent.Sources[0]
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Memory.Govern(ctx, domain.RelationshipScope{Scope: q.Scope, ActorInstanceID: request.ActorID, PlayerID: s.SubjectID}, memory.MemoryGovernanceCommand{Key: "delete-initial", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{source}, Expected: map[string]uint64{source: 1}}}); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &i)
	updated := runtimeVersions(t, i)
	br.Versions = &updated
	br.GateVersions, e = content.Gate(ctx, r.Store, s)
	if e != nil {
		t.Fatal(e)
	}
	after, e := read("after-delete", request)
	if e != nil {
		t.Fatal(e)
	}
	if len(after.Relationship) != 0 {
		t.Fatal("deleted ancestor relationship survived", after.Relationship)
	}
	for _, record := range after.Initial {
		if record.ID == parent.ID {
			t.Fatal("retained authoring resurrected INITIAL")
		}
	}
	if len(after.Initial) != len(original.Initial)-1 {
		t.Fatal("deletion incorrectly removed unrelated INITIAL")
	}
	var retained []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM actors WHERE world_instance_id=? AND actor_id=?", q.Scope.WorldInstanceID, request.ActorID).Scan(&retained); e != nil {
		t.Fatal(e)
	}
	var material map[string]any
	if e = json.Unmarshal(retained, &material); e != nil || material["OriginalCharacter"] == nil {
		t.Fatal("test lost retained authoring evidence", e)
	}
	next = br
	next.CommandKey = "stale-proof"
	next.CommitKey = next.CommandKey
	if _, e = r.Store.WithBarrier(ctx, next, func(tx store.Tx) error { return memory.ValidateGenerationContextTx(ctx, tx, request, forward.Proof) }); e == nil {
		t.Fatal("deleted source accepted frozen proof")
	}
}
