package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"testing"
	"time"
)

func runtimeVersions(t *testing.T, i generated.Instance) domain.Versions {
	t.Helper()
	v := domain.Versions{ReleaseID: i.Versions.ReleaseID}
	for n, s := range []string{i.Versions.WorldRevision, i.Versions.ControlVersion, i.Versions.PrivacyEpoch, i.Versions.ViewEpoch} {
		x, e := strconv.ParseUint(s, 10, 64)
		if e != nil {
			t.Fatal(e)
		}
		switch n {
		case 0:
			v.WorldRevision = x
		case 1:
			v.ControlVersion = x
		case 2:
			v.PrivacyEpoch = x
		case 3:
			v.ViewEpoch = x
		}
	}
	return v
}
func actionTask(t *testing.T, r *app.Runtime, s domain.ProductScope, i generated.Instance, id string) (store.BarrierRequest, world.ApplyActionsRequest) {
	t.Helper()
	ctx := context.Background()
	scope := domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}
	v := runtimeVersions(t, i)
	g, e := content.Gate(ctx, r.Store, s)
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.Store.WithCommandBarrier(ctx, store.CommandBarrierRequest{ProductScope: s, WorldScope: &scope, Versions: &v, OwnerSubjectID: s.SubjectID, Operation: "turn", CommandKey: id, CommitKey: id, PayloadHash: sha256.Sum256([]byte(id)), GateVersions: g, Locks: []store.LockKey{{Rank: 50, IDs: []string{s.TenantID, s.ProductID, id}}}}, func(tx store.Tx) error {
		return tx.CreateTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: s, WorldScope: &scope, OwnerSubjectID: s.SubjectID}, TaskID: id, Kind: "TURN", NextRunAt: time.Now().UTC()})
	})
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimTask(ctx, "action-worker", time.Now(), time.Minute)
	if e != nil || lease.TaskID != id {
		t.Fatal(lease, e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var init world.Initialization
	if e = json.Unmarshal(raw, &init); e != nil {
		t.Fatal(e)
	}
	mia, alex := "", ""
	for _, a := range init.Actors {
		if a.Slot == "mia" {
			mia = a.ID
		}
		if a.Slot == "alex" {
			alex = a.ID
		}
	}
	q := world.ApplyActionsRequest{Scope: scope, TaskID: id, ResultID: id + "-result", Versions: v, Intents: []world.ActionIntent{{ID: "move", ActorID: mia, Kind: "MOVE", ItemID: init.Items[0].ID, TargetID: alex}}}
	br := store.BarrierRequest{ProductScope: s, WorldScope: &scope, Versions: &v, TaskID: id, OwnerSubjectID: s.SubjectID, WorkerID: "action-worker", Fence: lease.Fence, GateVersions: g, Operation: "turnCommit", CommandKey: id, CommitKey: id + "-commit", PayloadHash: sha256.Sum256([]byte(id)), Locks: world.ActionLocks(q)}
	return br, q
}
func commitAction(r *app.Runtime, br store.BarrierRequest, q world.ApplyActionsRequest) (world.ActionResult, error) {
	var out world.ActionResult
	_, e := r.Store.WithBarrier(context.Background(), br, func(tx store.Tx) error {
		var e error
		out, e = world.ApplyActions(context.Background(), tx, q)
		return e
	})
	return out, e
}
func TestConcurrentSingleItemHasOneWinner(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reverse), func(t *testing.T) {
			r, s, rel := runtimeFixture(t)
			i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
			a, qa := actionTask(t, r, s, i, "turn-a")
			b, qb := actionTask(t, r, s, i, "turn-b")
			if reverse {
				a, b = b, a
				qa, qb = qb, qa
			}
			start := make(chan struct{})
			done := make(chan error, 2)
			go func() { <-start; _, e := commitAction(r, a, qa); done <- e }()
			go func() { <-start; _, e := commitAction(r, b, qb); done <- e }()
			close(start)
			wins := 0
			for n := 0; n < 2; n++ {
				if <-done == nil {
					wins++
				}
			}
			if wins != 1 {
				t.Fatal("expected one authoritative winner", wins)
			}
			var n int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id IN ('turn-a','turn-b')").Scan(&n); e != nil || n != 1 {
				t.Fatal("duplicate success events", n, e)
			}
		})
	}
}
func TestPauseBeforeCommitBlocksNarrative(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	br, q := actionTask(t, r, s, i, "late-turn")
	runtimeControl(t, r, i, "PAUSE")
	if _, e := commitAction(r, br, q); e == nil {
		t.Fatal("late candidate committed")
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id='late-turn'").Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestCommittedActionRetainsEventEvidence(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	br, q := actionTask(t, r, s, i, "event-turn")
	out, e := commitAction(r, br, q)
	if e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM runtime_events WHERE record_id=?", out.CommittedEventIDs[0]).Scan(&raw); e != nil {
		t.Fatalf("committed event has no durable action evidence: %v", e)
	}
	var doc struct {
		Kind    string
		Actions []world.ActionIntent
	}
	if e = json.Unmarshal(raw, &doc); e != nil || doc.Kind != "ACTION_BUNDLE" || len(doc.Actions) != 1 || doc.Actions[0].ItemID != q.Intents[0].ItemID {
		t.Fatal("event cannot substantiate committed result", doc, e)
	}
}
func TestActionRollbackAndDependentBundleNoPartialFacts(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	br, q := actionTask(t, r, s, i, "atomic-turn")
	q.Intents = append(q.Intents, world.ActionIntent{ID: "bad", ActorID: q.Intents[0].ActorID, ItemID: q.Intents[0].ItemID, Kind: "TRANSFER_OWNERSHIP", DependsOn: []string{"move"}})
	br.Locks = world.ActionLocks(q)
	if _, e := commitAction(r, br, q); e == nil {
		t.Fatal("unknown action admitted")
	}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM items").Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var item world.ItemState
	_ = json.Unmarshal(raw, &item)
	if item.HolderID != "" {
		t.Fatal("partial MOVE after bundle refusal")
	}
	q.Intents = q.Intents[:1]
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic rollback") }
	if _, e := commitAction(r, br, q); e == nil {
		t.Fatal("rollback ignored")
	}
	r.Store.BeforeCommit = nil
	if _, e := commitAction(r, br, q); e != nil {
		t.Fatal("same task retry after known rollback", e)
	}
}
