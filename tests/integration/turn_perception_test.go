package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func TestTurnCanonicalContainsActualBoundedWorldPerception(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "perception-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "perception-turn")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "perception-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "perception-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(accepted.TurnID, "send")
	d.CommitKey = d.CommandKey
	marker, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	_, material, e := marker.OriginalSend().Take(ctx, r.Store)
	if e != nil {
		t.Fatal(e)
	}
	var prompt struct{ World domain.WorldPerception }
	if e = json.Unmarshal(material.Canonical, &prompt); e != nil {
		t.Fatal(e)
	}
	if prompt.World.ActorID != request.PrimaryActorID || len(prompt.World.Actors) != 2 || len(prompt.World.Items) != 1 {
		t.Fatal("actual generated input has no authoritative visible world", prompt.World)
	}
	var actual world.ItemState
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if json.Unmarshal(raw, &actual) != nil {
		t.Fatal("invalid actual item")
	}
	if prompt.World.Items[0].ID != actual.ID || prompt.World.Items[0].CustodianID != actual.CustodianID {
		t.Fatal("not actual visible resource identities", prompt.World.Items)
	}
}

func TestPerceptionUsesRealSceneVisibilityAndHidesReferences(t *testing.T) {
	for _, scenario := range []string{"actor-scoped", "cross-scene", "over-cap"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, s, seed := runtimeFixtureWithConsent(t, true)
			base, e := r.Content.GetRelease(ctx, s, seed)
			if e != nil {
				t.Fatal(e)
			}
			in := base.Input
			for n, raw := range in.Objects {
				var header struct {
					Kind string `json:"kind"`
				}
				if e = json.Unmarshal(raw, &header); e != nil {
					t.Fatal(e)
				}
				if header.Kind == "SCENE" && scenario != "over-cap" {
					var scene generated.SceneDefinition
					if e = generated.Decode("SceneDefinition", raw, &scene); e != nil {
						t.Fatal(e)
					}
					if scenario == "actor-scoped" {
						scene.Visibility = "ACTOR_SCOPED"
						in.Objects[n], e = json.Marshal(scene)
					} else {
						scene.ObjectID = "hidden-room"
						scene.Name = "private name never in World"
						b, err := json.Marshal(scene)
						e = err
						in.Objects = append(in.Objects, b)
					}
					if e != nil {
						t.Fatal(e)
					}
				}
				if scenario == "over-cap" && header.Kind == "PROP" {
					var prop generated.PropDefinition
					if e = generated.Decode("PropDefinition", raw, &prop); e != nil {
						t.Fatal(e)
					}
					for i := 0; i < 32; i++ {
						prop.RuleRefs = []generated.DefinitionRef{}
						prop.ObjectID = generated.Identity(fmt.Sprintf("extra-prop-%d", i))
						b, err := json.Marshal(prop)
						if err != nil {
							t.Fatal(err)
						}
						in.Objects = append(in.Objects, b)
					}
				}
				if scenario == "cross-scene" && header.Kind == "WORLD" {
					var w generated.WorldDefinition
					if e = generated.Decode("WorldDefinition", raw, &w); e != nil {
						t.Fatal(e)
					}
					w.InitialActorSetups[1].SceneRef = json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"hidden-room"}`)
					in.Objects[n], e = json.Marshal(w)
					if e != nil {
						t.Fatal(e)
					}
				}
			}
			draft, e := r.Content.Create(ctx, s, "WORLD", "", in, "scene-draft")
			if e != nil {
				t.Fatal(e)
			}
			check, e := r.Content.CheckPublish(ctx, s, string(draft.DraftID), 1, []string{"MOCK_DEMO"}, "scene-check")
			if e != nil || !check.Passed {
				t.Fatal(check, e)
			}
			rel, e := r.Content.Publish(ctx, s, string(draft.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "scene-release")
			if e != nil {
				t.Fatal(e)
			}
			instance := runtimeControl(t, r, createRuntimeWorld(t, r, string(rel.ReleaseID), "scene-world"), "START")
			scope := domain.Scope{ProductScope: s, WorldInstanceID: instance.Scope.WorldInstanceID}
			var raw []byte
			var init world.Initialization
			if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(raw, &init); e != nil {
				t.Fatal(e)
			}
			actor := init.Actors[0].ID
			counts := func() string {
				t.Helper()
				var out string
				if e := r.Store.DB.QueryRow("SELECT CONCAT((SELECT COUNT(*) FROM harness_quotes),':',(SELECT COUNT(*) FROM tasks),':',(SELECT COUNT(*) FROM harness_turns),':',(SELECT COUNT(*) FROM harness_envelopes),':',(SELECT COUNT(*) FROM attempts),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts),':',(SELECT COALESCE(SUM(calls_used),0) FROM tasks))").Scan(&out); e != nil {
					t.Fatal(e)
				}
				return out
			}
			before, originalJ := counts(), journalBytes(t, r)
			q, e := r.Turns.QuoteTurn(ctx, scope, generated.QuoteRequest{Scope: instance.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}, "scene-quote")
			if scenario == "over-cap" {
				if len(init.Items) != 33 || !errors.Is(e, store.ErrDenied) || q.QuoteID != "" {
					t.Fatal("33 initialized resources must be rejected before quote", len(init.Items), e)
				}
				if counts() != before || !bytes.Equal(originalJ, journalBytes(t, r)) {
					t.Fatal("over-cap quote changed intake, budgets, receipts or journal")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			a, e := r.Turns.SubmitTurn(ctx, scope, generated.TurnRequest{PrimaryActorID: actor, QuoteID: q.QuoteID, ExpectedVersions: generated.ExpectedVersions{WorldRevision: instance.Versions.WorldRevision, ControlVersion: instance.Versions.ControlVersion}, Input: json.RawMessage(`{"kind":"TEXT","text":"Hello."}`), RequestedAudience: "PRIVATE_RELATIONSHIP"}, "scene-turn")
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			var original domain.OriginalContextSnapshot
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=?", a.TurnID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(raw, &original); e != nil {
				t.Fatal(e)
			}
			if len(original.World.Actors) != 1 || original.World.Actors[0].ID != actor || len(original.World.Items) != 0 {
				t.Fatal("scene visibility/custodian reference leaked", original.World)
			}
			if bytes.Contains(raw, []byte("private name never in World")) {
				t.Fatal("authoring scene text copied")
			}
		})
	}
}

func TestPerceptionReleaseBridgeDoesNotAdmitCallerLocks(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, q, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, q, "lock-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	a, e := turns.SubmitTurn(ctx, scope, request, "lock-turn")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "lock-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: a.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "lock-worker")
	if e != nil {
		t.Fatal(e)
	}
	for _, k := range []store.LockKey{content.Key(scope.ProductScope, "content_release", "caller-release"), content.Key(scope.ProductScope, "content_project", "caller-project"), content.Key(scope.ProductScope, "content_check", "caller-check")} {
		br := d.BarrierRequest
		br.Locks = append(append([]store.LockKey{}, br.Locks...), k)
		if _, e = r.Store.WithBarrier(ctx, br, func(tx store.Tx) error { return nil }); !errors.Is(e, store.ErrDenied) {
			t.Fatal("caller lock scope was widened", k, e)
		}
	}
	// The exact owning plan still works after each rejected transaction.
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
}

func actionResultFixture(t *testing.T, faults ...harness.MockFault) (*app.Runtime, domain.Scope, string, world.ItemState) {
	t.Helper()
	ctx := context.Background()
	r, scope, turns, q, request := actualTurnFixture(t, true)
	var raw []byte
	var item world.ItemState
	if e := r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(raw, &item); e != nil {
		t.Fatal(e)
	}
	request.PrimaryActorID = item.CustodianID
	q.PrimaryActorID = &request.PrimaryActorID
	request.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
	q.OutputUpperBound = "4000"
	quote, e := turns.QuoteTurn(ctx, scope, q, "action-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "action-turn")
	if e != nil {
		t.Fatal(e)
	}
	if len(faults) > 0 {
		r.Worker.Turns.Fault = faults[0]
	}
	if e = runOriginalPrefix(ctx, r); e != nil {
		t.Fatal(e)
	}
	return r, scope, accepted.TurnID, item
}

func TestActualCandidateRejectsUngovernedNoteAndWrongMemoryDirection(t *testing.T) {
	for _, fault := range []harness.MockFault{harness.MockUngovernedActionNote, harness.MockWrongRelationshipMemory} {
		t.Run(fmt.Sprint(fault), func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t, fault)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrDenied) {
				t.Fatal("untrusted private-note/direction candidate approved", e)
			}
			var count int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_candidate_approvals WHERE task_id=?", task).Scan(&count); e != nil || count != 0 {
				t.Fatal("invalid candidate retained approval", count, e)
			}
		})
	}
}

func TestActualTurnActionAndMemoryJointCommit(t *testing.T) {
	ctx := context.Background()
	r, scope, task, item := actionResultFixture(t)
	var raw []byte
	var e error
	turns := r.Turns
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id=?", task).Scan(&count); e != nil || count != 1 {
		t.Fatal("no real joint action", count, e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? AND item_id=?", scope.WorldInstanceID, item.ID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var after world.ItemState
	if e = json.Unmarshal(raw, &after); e != nil || after.HolderID == "" || after.State != "held" {
		t.Fatal("item did not move", e)
	}
	view, e := turns.GetTurn(ctx, scope, task)
	if e != nil {
		t.Fatal("terminal action+memory body unavailable", e)
	}
	if len(view.ApprovedPackets) != 1 {
		t.Fatal("no approved packet")
	}
	var committed domain.CandidateCommit
	if e = r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", task).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &committed); e != nil || len(committed.MemoryIDs) != 1 || len(committed.EventIDs) != 1 {
		t.Fatal("missing joint memory/event", committed, e)
	}
}

func TestActionMemoryCommitRollbackAndLostAck(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "committed-ack-lost"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, item := actionResultFixture(t)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal(e)
			}
			var before []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&before); e != nil {
				t.Fatal(e)
			}
			failure := errors.New("synthetic action-memory transaction fault")
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var events, services, posts, outbox int
				if e := tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM events WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?),(SELECT COUNT(*) FROM outbox WHERE task_id=?)", task, task, task, task).Scan(&events, &services, &posts, &outbox); e != nil {
					return e
				}
				if services == 0 {
					return nil
				} // New local prepare/outcome transactions carry no world writes.
				if events != 1 || services != 1 || posts != 6 || outbox != 1 {
					t.Errorf("fault did not follow actual writes %d/%d/%d/%d", events, services, posts, outbox)
				}
				var raw []byte
				var a memory.Aggregate
				var previous memory.Aggregate
				if e := tx.QueryRowContext(ctx, "SELECT document FROM memory_aggregates WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
					return e
				}
				if json.Unmarshal(raw, &a) != nil || json.Unmarshal(before, &previous) != nil || len(a.Nodes) != len(previous.Nodes)+1 {
					t.Error("joint proposal absent at fault boundary")
				}
				if !unknown {
					return failure
				}
				return nil
			}
			if unknown {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					var services int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
						return e
					}
					if services == 0 {
						return tx.Commit()
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					return failure
				}
			}
			_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if e == nil || unknown && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("missing transport/rollback failure", e)
			}
			if !unknown {
				for _, table := range []string{"events", "approved_results", "service_events", "postings", "outbox", "harness_turn_commits", "harness_service_lines"} {
					var count int
					if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", task).Scan(&count); e != nil || count != 0 {
						t.Fatal("partial write survived", table, count, e)
					}
				}
				var after []byte
				if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&after); e != nil || !bytes.Equal(before, after) {
					t.Fatal("memory survived rollback", e)
				}
				if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=? AND item_id=?", scope.WorldInstanceID, item.ID).Scan(&after); e != nil {
					t.Fatal(e)
				}
				var restored world.ItemState
				if json.Unmarshal(after, &restored) != nil || restored.HolderID != "" || restored.State != item.State {
					t.Fatal("item survived rollback")
				}
			} else {
				if receipt, e := r.Store.QueryOriginalTurnCommit(ctx, scope, task); e != nil || !receipt.Committed {
					t.Fatal("original commit knowledge", e)
				}
			}
			if receipt, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil || !receipt.Committed {
				t.Fatal("original retry/receipt failed", e)
			}
			if _, _, e = r.Files.ReadApprovedTurn(ctx, scope, task); e != nil {
				t.Fatal("terminal result after memory graph extension", e)
			}
			var calls, count int
			if e = r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM tasks WHERE task_id=?", task, task).Scan(&calls, &count); e != nil || calls != 1 || count != 1 {
				t.Fatal("repeated purchase/service", calls, count, e)
			}
		})
	}
}

func TestOriginalWorldSnapshotSurvivesActualRelatedAction(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	before, _, e := readOriginalResult(ctx, r.Files, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	var snapshotBefore []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=?", task).Scan(&snapshotBefore); e != nil {
		t.Fatal(e)
	}
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	br, q := actionTask(t, r, scope.ProductScope, instance, "competing-real-action")
	if _, e = commitAction(r, br, q); e != nil {
		t.Fatal("actual competing action", e)
	}
	after, _, e := readOriginalResult(ctx, r.Files, lease, worker)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("actual prior bytes lost when relevant world values changed", e)
	}
	var snapshotAfter []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=?", task).Scan(&snapshotAfter); e != nil || !bytes.Equal(snapshotBefore, snapshotAfter) {
		t.Fatal("original World rewritten", e)
	}
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
		t.Fatal("missing actual relevant-resource conflict classification", e)
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_candidate_approvals WHERE task_id=?", task).Scan(&count); e != nil || count != 0 {
		t.Fatal("conflicted candidate approved", count, e)
	}
}

func TestApprovedOriginalWorldConflictDoesNotCommitOrBuy(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	var approvalBefore []byte
	if e := r.Store.DB.QueryRow("SELECT document FROM harness_candidate_approvals WHERE task_id=?", task).Scan(&approvalBefore); e != nil {
		t.Fatal(e)
	}
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	br, q := actionTask(t, r, scope.ProductScope, instance, "approved-competing-action")
	if _, e = commitAction(r, br, q); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
		t.Fatal("stale resource reached local commit", e)
	}
	if _, e = r.Store.QueryOriginalTurnCommit(ctx, scope, task); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("conflicted original has world receipt", e)
	}
	var after []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM harness_candidate_approvals WHERE task_id=?", task).Scan(&after); e != nil || !bytes.Equal(approvalBefore, after) {
		t.Fatal("old approval changed", e)
	}
	var calls, steps, events, services int
	if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,(SELECT COUNT(*) FROM events WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task).Scan(&calls, &steps, &events, &services); e != nil || calls != 1 || steps != 9 || events != 0 || services != 0 {
		t.Fatal("conflict changed world or budget", calls, steps, events, services, e)
	}
}
