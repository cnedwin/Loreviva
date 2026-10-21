package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

// These controls reach the original owning API and its actual COMMIT before
// checking the missing delivery joint; fixture or compile errors are not RED.
func TestIntegrationDeliveryControlJoint(t *testing.T) {
	r, _, service, request, _ := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
	w, e := r.World.CreateIntegrationInstance(service, "delivery-control-world", request)
	if e != nil {
		t.Fatal(e)
	}
	a, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "delivery-control-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	if e != nil {
		t.Fatal(e)
	}
	l, e := r.Store.ClaimRegistered(context.Background(), "delivery-control-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: a.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.World.Process(context.Background(), l, "delivery-control-worker"); e != nil {
		t.Fatal(e)
	}
	var state string
	var committed int
	if e = r.Store.DB.QueryRow("SELECT state,(SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND operation='INSTANCE_CONTROL' AND phase='COMMIT') FROM tasks WHERE task_id=?", a.TaskID, a.TaskID).Scan(&state, &committed); e != nil || state != "COMMITTED" || committed != 1 {
		t.Fatal("original CONTROL COMMIT control", state, committed, e)
	}
	var h, updates, notices, outbox uint64
	e = r.Store.DB.QueryRow(`SELECT update_seq,(SELECT COUNT(*) FROM delivery_updates WHERE task_ref=? AND kind='CONTROL'),(SELECT COUNT(*) FROM integration_notifications WHERE world_instance_id=?),(SELECT COUNT(*) FROM outbox o JOIN integration_notifications n ON n.outbox_ref=o.record_id WHERE n.world_instance_id=?) FROM delivery_streams WHERE world_instance_id=?`, a.TaskID, w.Scope.WorldInstanceID, w.Scope.WorldInstanceID, w.Scope.WorldInstanceID).Scan(&h, &updates, &notices, &outbox)
	if e != nil {
		t.Fatal(e)
	}
	if h != 1 || updates != 1 || notices != 1 || outbox != 1 {
		t.Fatalf("original CONTROL committed but delivery joint absent: H=%d update=%d notice=%d outbox=%d", h, updates, notices, outbox)
	}
	// A completed worker lease is not a new execution capability. Replay the
	// original owning request, which resolves its retained task and receipt.
	replay, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "delivery-control-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
	if e != nil || replay.TaskID != a.TaskID {
		t.Fatal("original request replay", e)
	}
	var after uint64
	if e = r.Store.DB.QueryRow("SELECT update_seq FROM delivery_streams WHERE world_instance_id=?", w.Scope.WorldInstanceID).Scan(&after); e != nil || after != h {
		t.Fatal("replay changed H", after, e)
	}
}

// Every injected mutation must affect the newly written row inside the owning
// transaction. A database rejection or a hook that was never reached is not a
// successful final-authority negative.
func TestIntegrationDeliveryJointFinalRollback(t *testing.T) {
	for _, mode := range []string{"control", "H", "update", "notice", "outbox", "receipt", "forced-error"} {
		t.Run(mode, func(t *testing.T) {
			r, _, service, request, _ := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
			w, e := r.World.CreateIntegrationInstance(service, "delivery-rollback-world", request)
			if e != nil {
				t.Fatal(e)
			}
			a, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "delivery-rollback-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
			if e != nil {
				t.Fatal(e)
			}
			l, e := r.Store.ClaimRegistered(context.Background(), "delivery-rollback-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: a.TaskID}})
			if e != nil {
				t.Fatal(e)
			}
			hit := 0
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var present int
				if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM delivery_updates WHERE task_ref=?", a.TaskID).Scan(&present); e != nil {
					return e
				}
				if present == 0 {
					return nil
				}
				hit++
				if mode == "control" {
					return nil
				}
				if mode == "forced-error" {
					return errors.New("owned delivery rollback fault")
				}
				query, arg := "", a.TaskID
				switch mode {
				case "H":
					query = "UPDATE delivery_streams SET update_seq=update_seq+1 WHERE world_instance_id=?"
					arg = w.Scope.WorldInstanceID
				case "update":
					query = "UPDATE delivery_updates SET resource_revision=2 WHERE task_ref=?"
				case "notice":
					query = "UPDATE integration_notifications SET initial_revision=2 WHERE world_instance_id=?"
					arg = w.Scope.WorldInstanceID
				case "outbox":
					query = "UPDATE outbox o JOIN integration_notifications n ON n.tenant_id=o.tenant_id AND n.product_id=o.product_id AND n.outbox_ref=o.record_id SET o.revision=2 WHERE o.task_id=?"
				case "receipt":
					query = "UPDATE commit_receipts SET operation='ACTION_BUNDLE' WHERE task_id=? AND operation='INSTANCE_CONTROL' AND phase='COMMIT'"
				}
				res, e := tx.ExecContext(ctx, query, arg)
				if e != nil {
					t.Errorf("fault injection rejected: %v", e)
					return e
				}
				n, e := res.RowsAffected()
				if e != nil || n != 1 {
					t.Errorf("fault did not affect exactly one row: %d %v", n, e)
					return errors.New("invalid fault precondition")
				}
				return nil
			}
			e = r.World.Process(context.Background(), l, "delivery-rollback-worker")
			r.Store.BeforeCommit = nil
			if hit != 1 {
				t.Fatalf("owning joint hook reached %d times", hit)
			}
			if mode == "control" && e != nil {
				t.Fatal("legal joint control", e)
			}
			if mode != "control" && e == nil {
				t.Fatal("mutated joint committed")
			}
			var h, updates, notices, outbox, receipts, results int
			e = r.Store.DB.QueryRow(`SELECT update_seq,(SELECT COUNT(*) FROM delivery_updates WHERE task_ref=?),(SELECT COUNT(*) FROM integration_notifications WHERE world_instance_id=?),(SELECT COUNT(*) FROM outbox o JOIN integration_notifications n ON n.tenant_id=o.tenant_id AND n.product_id=o.product_id AND n.outbox_ref=o.record_id WHERE o.task_id=?),(SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND operation='INSTANCE_CONTROL' AND phase='COMMIT'),(SELECT COUNT(*) FROM approved_results WHERE task_id=?) FROM delivery_streams WHERE world_instance_id=?`, a.TaskID, w.Scope.WorldInstanceID, a.TaskID, a.TaskID, a.TaskID, w.Scope.WorldInstanceID).Scan(&h, &updates, &notices, &outbox, &receipts, &results)
			if e != nil {
				t.Fatal(e)
			}
			want := 0
			if mode == "control" {
				want = 1
			}
			if h != want || updates != want || notices != want || outbox != want || receipts != want || results != want {
				t.Fatalf("non-atomic joint: H/update/notice/outbox/receipt/result=%d/%d/%d/%d/%d/%d want %d", h, updates, notices, outbox, receipts, results, want)
			}
		})
	}
	for _, mode := range []string{"foreign-store", "proxy-unit", "wrong-task"} {
		t.Run(mode, func(t *testing.T) {
			r, _, service, request, _ := integrationCreationFixtureActions(t, nil, time.Hour, []string{"READ", "TURN", "MEMORY_VIEW"})
			w, e := r.World.CreateIntegrationInstance(service, "delivery-sealed-world", request)
			if e != nil {
				t.Fatal(e)
			}
			a, e := r.World.ControlIntegrationInstance(service, w.Scope.WorldInstanceID, "delivery-sealed-start", generated.ControlRequest{Command: "START", ExpectedControlVersion: "0"})
			if e != nil {
				t.Fatal(e)
			}
			worker := "delivery-sealed-worker"
			lease, e := r.Store.ClaimRegistered(context.Background(), worker, time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "CONTROL", Operation: "INSTANCE_CONTROL", TaskID: a.TaskID}})
			if e != nil {
				t.Fatal(e)
			}
			ctx, e := r.Store.EnterControlTaskIssuer(context.Background(), lease, worker)
			if e != nil {
				t.Fatal("actual original issuer", e)
			}
			var raw []byte
			var p struct {
				Scope          domain.Scope
				Versions       domain.Versions
				Status, Action string
			}
			if e = r.Store.DB.QueryRow("SELECT document FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(raw, &p); e != nil {
				t.Fatal(e)
			}
			gate, e := content.Gate(ctx, r.Store, p.Scope.ProductScope)
			if e != nil {
				t.Fatal(e)
			}
			id := content.Identity(lease.TaskID, "result")
			keys := []store.LockKey{}
			for _, kind := range []string{"events", "approved_results", "outbox"} {
				keys = append(keys, store.LockKey{Rank: 110, Kind: kind, IDs: []string{p.Scope.TenantID, p.Scope.ProductID, id}})
			}
			hit := 0
			stop := errors.New("owned sealed-input rejection")
			_, e = r.Store.WithBarrier(ctx, store.BarrierRequest{ProductScope: p.Scope.ProductScope, WorldScope: &p.Scope, Versions: &p.Versions, OwnerSubjectID: lease.OwnerSubjectID, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, Purpose: store.ControlPurpose, ExpectedWorldStatus: p.Status, Runtime: &store.RuntimeBinding{Action: p.Action}, Operation: lease.Operation, CommandKey: lease.TaskID, CommitKey: id, GateVersions: gate, PayloadHash: sha256.Sum256(raw), Locks: keys}, func(tx store.Tx) error {
				hit++
				input := store.DeliveryCommit{Scope: p.Scope, Kind: "CONTROL", TaskID: lease.TaskID, ResultID: id, ReceiptKey: id, EventIDs: []string{id}, Lineage: []string{p.Scope.WorldInstanceID}, Revision: 1}
				owner := r.Store
				if mode == "foreign-store" {
					owner = store.New(r.Store.DB)
				}
				if mode == "proxy-unit" {
					tx = &deliveryForeignUnit{tx}
				}
				if mode == "wrong-task" {
					input.TaskID = "another-task"
				}
				if e := owner.AppendIntegrationResultTx(ctx, tx, input); !errors.Is(e, store.ErrDenied) {
					t.Errorf("sealed %s did not reject: %v", mode, e)
				}
				return stop
			})
			if hit != 1 || !errors.Is(e, stop) {
				t.Fatal("original issuer/transaction not reached", hit, e)
			}
			var h, updates, notices, receipts int
			if e = r.Store.DB.QueryRow(`SELECT update_seq,(SELECT COUNT(*) FROM delivery_updates WHERE task_ref=?),(SELECT COUNT(*) FROM integration_notifications WHERE world_instance_id=?),(SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND phase='COMMIT') FROM delivery_streams WHERE world_instance_id=?`, a.TaskID, w.Scope.WorldInstanceID, a.TaskID, w.Scope.WorldInstanceID).Scan(&h, &updates, &notices, &receipts); e != nil || h != 0 || updates != 0 || notices != 0 || receipts != 0 {
				t.Fatal("rejected unit left writes", h, updates, notices, receipts, e)
			}
		})
	}
}

// Embedding the public Tx methods does not preserve the private concrete unit.
type deliveryForeignUnit struct{ store.Tx }

func TestIntegrationDeliveryTurnJoint(t *testing.T) {
	for _, action := range []bool{false, true} {
		t.Run(map[bool]string{false: "dialogue-only", true: "action"}[action], func(t *testing.T) {
			f := integrationFixSetup(t, true)
			r := f.r
			var raw []byte
			var item world.ItemState
			if e := r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? LIMIT 1", f.world.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			if json.Unmarshal(raw, &item) != nil {
				t.Fatal("actual item")
			}
			f.input.PrimaryActorID = item.CustodianID
			if action {
				f.input.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
			}
			q, e := r.Store.QuoteIntegrationTurn(f.service, "delivery-turn-quote", generated.QuoteRequest{Scope: f.world, ServiceKind: "TURN_TEXT", PrimaryActorID: &f.input.PrimaryActorID, InputUpperBound: "16000", OutputUpperBound: "4000", PurchaseResponsibility: "MOCK"})
			if e != nil {
				t.Fatal(e)
			}
			f.input.QuoteID = q.QuoteID
			a, e := r.Store.AcceptIntegrationTurn(f.client, f.world.WorldInstanceID, "delivery-turn-intake", f.input)
			if e != nil {
				t.Fatal(e)
			}
			l, e := r.Store.ClaimRegistered(context.Background(), "delivery-turn-worker", time.Now().UTC(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Worker.Turns.Execute(context.Background(), l.TaskLease, "delivery-turn-worker"); e != nil {
				t.Fatal("original Mock/approval/COMMIT", e)
			}
			var committed int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turn_commits c JOIN commit_receipts r ON r.commit_key=c.receipt_key WHERE c.task_id=? AND r.phase='COMMIT' AND r.operation='HARNESS_TURN_COMMIT'", a.TurnID).Scan(&committed); e != nil || committed != 1 {
				t.Fatal("original TURN COMMIT control", committed, e)
			}
			var h, updates, notices uint64
			e = r.Store.DB.QueryRow(`SELECT d.update_seq,(SELECT COUNT(*) FROM delivery_updates WHERE task_ref=? AND kind='TURN'),(SELECT COUNT(*) FROM integration_notifications WHERE world_instance_id=d.world_instance_id) FROM delivery_streams d JOIN tasks t USING(tenant_id,product_id,world_instance_id) WHERE t.task_id=?`, a.TurnID, a.TurnID).Scan(&h, &updates, &notices)
			if e != nil {
				t.Fatal(e)
			}
			if h != 2 || updates != 1 || notices != 2 {
				t.Fatalf("original TURN committed but delivery joint absent: H=%d update=%d notice=%d", h, updates, notices)
			}
			var events, calls, serviceEvents int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM events WHERE task_id=?),calls_used,(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM tasks WHERE task_id=?", a.TurnID, a.TurnID, a.TurnID).Scan(&events, &calls, &serviceEvents); e != nil {
				t.Fatal(e)
			}
			want := 0
			if action {
				want = 1
			}
			if events != want || calls != 1 || serviceEvents != 1 {
				t.Fatal("actual action/dialogue or original accounting", events, calls, serviceEvents)
			}
		})
	}
}

func TestIntegrationDeliveryProducerIsolation(t *testing.T) {
	check := func(t *testing.T, s *store.Store) {
		t.Helper()
		var streams, updates, notices int
		if e := s.DB.QueryRow("SELECT (SELECT COUNT(*) FROM delivery_streams),(SELECT COUNT(*) FROM delivery_updates),(SELECT COUNT(*) FROM integration_notifications)").Scan(&streams, &updates, &notices); e != nil || streams != 0 || updates != 0 || notices != 0 {
			t.Fatal("non-integration producer created customer delivery", streams, updates, notices, e)
		}
	}
	t.Run("admin-action", func(t *testing.T) {
		r, s, release := runtimeFixture(t)
		i := runtimeControl(t, r, createRuntimeWorld(t, r, release, "isolated-create"), "START")
		br, q := actionTask(t, r, s, i, "isolated-action")
		var result world.ActionResult
		_, e := r.Store.WithBarrier(context.Background(), br, func(tx store.Tx) error {
			input := store.DeliveryCommit{Scope: q.Scope, Kind: "TURN", TaskID: q.TaskID, ResultID: q.ResultID, ReceiptKey: br.CommitKey, Revision: 1}
			// A real UoW for another operation is still not the sealed joint unit.
			if r.Store.AppendIntegrationResultTx(context.Background(), tx, input) == nil || store.New(r.Store.DB).AppendIntegrationResultTx(context.Background(), tx, input) == nil {
				return errors.New("foreign Store/operation gained delivery")
			}
			var err error
			result, err = world.ApplyActions(context.Background(), tx, q)
			return err
		})
		if e != nil {
			t.Fatal("real non-integration action", e)
		}
		_ = result
		check(t, r.Store)
	})
	t.Run("migration", func(t *testing.T) {
		// A generic action-test task is not a complete Harness accounting chain.
		// Use the actual ordinary Admin migration prerequisite in its own world.
		r, s, release := runtimeFixture(t)
		current := runtimeControl(t, r, createRuntimeWorld(t, r, release, "isolated-migration-create"), "START")
		current = runtimeControl(t, r, current, "PAUSE")
		target := nextRuntimeRelease(t, r, s, release, "expression_style", "delivery isolation")
		var plan generated.MigrationPlan
		runtimeHTTP(t, r, "POST", "/instances/"+current.Scope.WorldInstanceID+"/migration-previews", "isolated-preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: current.Versions}, 201, &plan)
		h, e := content.Hash(plan)
		if e != nil {
			t.Fatal(e)
		}
		runtimeHTTP(t, r, "POST", "/instances/"+current.Scope.WorldInstanceID+"/migrations", "isolated-migrate", generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: plan.PlanID, PlanHash: h, ExpectedVersions: current.Versions, ResolutionMap: plan.ResolutionMap}, 202, nil)
		if e = r.Worker.Once(context.Background()); e != nil {
			t.Fatal(e)
		}
		check(t, r.Store)
	})
	t.Run("rehearsal", func(t *testing.T) {
		r, draft, _ := candidateHTTPFixture(t)
		ctx := runtimeContext(t, r)
		scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
		d, e := r.Content.GetDraft(ctx, scope, draft, "")
		if e != nil {
			t.Fatal(e)
		}
		var req generated.RehearsalRequest
		if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":"Pass the sign and remember my request."}],"max_cost_units":"0","assertions":["ISOLATED","RIGHTS"]}`), &req); e != nil {
			t.Fatal(e)
		}
		req.ExpectedDraftRevision = d.DraftRevision
		a, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "isolated-rehearsal", req)
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Worker.Once(ctx); e != nil {
			t.Fatal(e)
		}
		if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(a.TaskID)); e != nil {
			t.Fatal("real committed rehearsal", e)
		}
		check(t, r.Store)
	})
}
