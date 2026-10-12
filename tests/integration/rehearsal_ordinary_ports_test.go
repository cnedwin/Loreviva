package integration

import (
	"context"

	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func TestRehearsalOrdinaryPortsUseActualIsolation(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, q, request := actualTurnFixture(t, true)
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	release, e := r.Content.GetRelease(ctx, scope.ProductScope, instance.Versions.ReleaseID)
	if e != nil {
		t.Fatal(e)
	}
	draft, e := r.Content.Create(ctx, scope.ProductScope, "WORLD", "", release.Input, "isolation-draft")
	if e != nil {
		t.Fatal(e)
	}
	var req generated.RehearsalRequest
	if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":"hello"}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope.ProductScope, string(draft.DraftID), "isolation", req)
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "isolation", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, lease.TaskLease, "isolation"); e != nil {
		t.Fatal(e)
	}
	child, e := r.Store.StartRehearsalChild(ctx, lease.TaskLease, "isolation")
	if e != nil {
		t.Fatal(e)
	}
	var isolated, actor string
	if e = r.Store.DB.QueryRow("SELECT world_instance_id,actor_id FROM harness_turns WHERE task_id=?", child.TurnID).Scan(&isolated, &actor); e != nil {
		t.Fatal(e)
	}
	iso := domain.Scope{ProductScope: scope.ProductScope, WorldInstanceID: isolated}
	count := func() string {
		var v string
		if e := r.Store.DB.QueryRow("SELECT CONCAT((SELECT COUNT(*) FROM tasks),':',(SELECT COUNT(*) FROM attempts),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts),':',(SELECT SUM(calls_used) FROM tasks),':',(SELECT SUM(steps_used) FROM harness_envelopes))").Scan(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	t.Run("GET_LIST_CONTROL", func(t *testing.T) {
		before := count()
		if _, e = r.World.GetInstance(ctx, iso); !errors.Is(e, content.ErrNotFound) {
			t.Fatal("isolated GetInstance", e)
		}
		page, e := r.World.ListInstances(ctx, scope.ProductScope, 100, "")
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, v := range page.Items {
			if v.Scope.WorldInstanceID == isolated {
				t.Fatal("isolated listed")
			}
			found = found || v.Scope.WorldInstanceID == scope.WorldInstanceID
		}
		if !found {
			t.Fatal("ordinary missing")
		}
		if _, e = r.World.Control(ctx, iso, world.ControlCommand{Key: "isolation-control", Request: generated.AdminControlRequest{ProductID: "coffee", Command: "PAUSE", ExpectedVersions: instance.Versions}}); !errors.Is(e, content.ErrNotFound) {
			t.Fatal("isolated control", e)
		}
		if count() != before {
			t.Fatal("isolated writes")
		}
	})
	// Real separate ordinary world supplies a compatible preview/execute positive.
	target := nextRuntimeRelease(t, r, scope.ProductScope, instance.Versions.ReleaseID, "expression_style", "safe migration")
	m := createRuntimeWorld(t, r, instance.Versions.ReleaseID, "migration-positive")
	for _, action := range []string{"START", "PAUSE"} {
		runtimeHTTP(t, r, "POST", "/instances/"+m.Scope.WorldInstanceID+"/controls", "migration-"+action, generated.AdminControlRequest{ProductID: "coffee", Command: action, ExpectedVersions: m.Versions}, 202, nil)
		if e = r.Worker.Once(ctx); e != nil {
			t.Fatal(e)
		}
		runtimeHTTP(t, r, "GET", "/instances/"+m.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &m)
	}
	ms := domain.Scope{ProductScope: scope.ProductScope, WorldInstanceID: m.Scope.WorldInstanceID}
	preview := generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: m.Versions}
	plan, e := r.World.PreviewInstanceMigration(ctx, ms, preview, "isolation-migrate-preview")
	if e != nil {
		t.Fatal(e)
	}
	hash, _ := content.Hash(plan)
	execute := generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: plan.PlanID, PlanHash: hash, ExpectedVersions: m.Versions, ResolutionMap: plan.ResolutionMap}
	t.Run("MIGRATION", func(t *testing.T) {
		before := count()
		if _, e = r.World.PreviewInstanceMigration(ctx, iso, preview, "isolated-preview"); !errors.Is(e, content.ErrNotFound) {
			t.Fatal(e)
		}
		if _, e = r.World.ExecuteInstanceMigration(ctx, iso, execute, "isolated-execute"); !errors.Is(e, content.ErrNotFound) {
			t.Fatal(e)
		}
		if count() != before {
			t.Fatal("migration denial wrote")
		}
	})
	if _, e = r.World.ExecuteInstanceMigration(ctx, ms, execute, "ordinary-execute"); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	quote, e := turns.QuoteTurn(ctx, scope, q, "ordinary-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "ordinary-turn")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = turns.GetTurn(ctx, scope, accepted.TurnID); e != nil {
		t.Fatal(e)
	}
	t.Run("READ_TURN", func(t *testing.T) {
		before := count()
		if _, e = r.Store.ReadTurn(ctx, iso, child.TurnID); !errors.Is(e, store.ErrDenied) {
			t.Fatal("ordinary metadata exposed child", e)
		}
		if _, e = turns.GetTurn(ctx, iso, child.TurnID); !errors.Is(e, store.ErrDenied) {
			t.Fatal("ordinary turn exposed child", e)
		}
		if count() != before {
			t.Fatal("read wrote")
		}
	})
	t.Run("QUOTE_SUBMIT", func(t *testing.T) {
		before := count()
		other := q
		other.Scope.WorldInstanceID = isolated
		other.PrimaryActorID = &actor
		if _, e = turns.QuoteTurn(ctx, iso, other, "isolated-quote"); !errors.Is(e, store.ErrDenied) {
			t.Fatal("isolated quote named gate", e)
		}
		bad := request
		bad.PrimaryActorID = actor
		bad.Input = json.RawMessage(`{"kind":"TEXT","text":"hello"}`)
		if _, e = turns.SubmitTurn(ctx, iso, bad, "isolated-turn"); !errors.Is(e, store.ErrDenied) {
			t.Fatal("isolated submit named gate", e)
		}
		if count() != before {
			t.Fatal("isolated procurement wrote")
		}
	})
	t.Run("CLAIM", func(t *testing.T) {
		before := count()
		if _, e = r.Store.ClaimRegistered(ctx, "ordinary", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "REHEARSAL_TURN", TaskID: child.TurnID}}); !errors.Is(e, store.ErrNoTask) {
			t.Fatal(e)
		}
		if count() != before {
			t.Fatal("isolated claim wrote")
		}
		if _, e = r.Store.ClaimRegistered(ctx, "ordinary", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}}); e != nil {
			t.Fatal("ordinary claim", e)
		}
	})
	t.Run("ORIGINAL_REPLAY", func(t *testing.T) {
		hash, e := store.TurnCommandHash(scope, request)
		if e != nil {
			t.Fatal(e)
		}
		if original, e := r.Store.OriginalTurn(ctx, scope, "ordinary-turn", hash); e != nil || original.TurnID != accepted.TurnID {
			t.Fatal("ordinary original receipt", e)
		}
		var key string
		var originalHash []byte
		if e = r.Store.DB.QueryRow("SELECT original_key,command_hash FROM harness_turns WHERE task_id=?", child.TurnID).Scan(&key, &originalHash); e != nil {
			t.Fatal(e)
		}
		var h [32]byte
		copy(h[:], originalHash)
		before := count()
		if _, e = r.Store.OriginalTurn(ctx, iso, key, h); !errors.Is(e, store.ErrDenied) {
			t.Fatal("exact original child receipt escaped", e)
		}
		if count() != before {
			t.Fatal("replay wrote")
		}
	})
	t.Run("TASK_LIST", func(t *testing.T) {
		before := count()
		page, e := r.Store.ListAdminTaskMetadata(ctx, scope.ProductScope, store.AdminTaskSelection{Limit: 100})
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, v := range page.Items {
			if v.ID == child.TurnID || v.World == isolated {
				t.Fatal("child listed")
			}
			found = found || v.ID == accepted.TurnID
		}
		if !found {
			t.Fatal("ordinary turn not listed")
		}
		if count() != before {
			t.Fatal("task list wrote")
		}
	})
	t.Run("GENERIC_CLAIM", func(t *testing.T) {
		before := count()
		if _, e = r.Store.ClaimTask(ctx, "generic", time.Now(), time.Minute); !errors.Is(e, store.ErrNoTask) {
			t.Fatal("generic claimed isolation", e)
		}
		if before != count() {
			t.Fatal("generic denial wrote")
		}
		quote, e := turns.QuoteTurn(ctx, scope, q, "ordinary-quote-two")
		if e != nil {
			t.Fatal(e)
		}
		second := request
		second.QuoteID = quote.QuoteID
		other, e := turns.SubmitTurn(ctx, scope, second, "ordinary-two")
		if e != nil {
			t.Fatal(e)
		}
		got, e := r.Store.ClaimTask(ctx, "generic", time.Now(), time.Minute)
		if e != nil || got.TaskID != other.TurnID {
			t.Fatal("generic ordinary positive", e)
		}
	})

}
