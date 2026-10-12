package integration

import (
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
	"sync"
	"testing"
	"time"
)

func TestTurnActualIntakeOriginalReplay(t *testing.T) {
	ctx := context.Background()
	r, s, rel := runtimeFixtureWithConsent(t, true)
	instance := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "intake-world"), "START")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: instance.Scope.WorldInstanceID}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var initial world.Initialization
	if e := json.Unmarshal(raw, &initial); e != nil {
		t.Fatal(e)
	}
	actor := initial.Actors[0].ID
	turns := harness.NewTurnService(r.Store)
	quote, e := turns.QuoteTurn(ctx, scope, generated.QuoteRequest{Scope: instance.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}, "original-quote")
	if e != nil {
		t.Fatal("actual zero quote unavailable", e)
	}
	out, _ := json.Marshal(quote)
	var decoded generated.Quote
	if e = generated.Decode("Quote", out, &decoded); e != nil {
		t.Fatal("quote violates sole schema", e)
	}
	request := generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: instance.Versions.WorldRevision, ControlVersion: instance.Versions.ControlVersion}, PrimaryActorID: actor, QuoteID: quote.QuoteID, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"May I borrow the sign?"}`)}
	accepted, e := turns.SubmitTurn(ctx, scope, request, "original-turn")
	if e != nil {
		t.Fatal("actual TURN acceptance unavailable", e)
	}
	out, _ = json.Marshal(accepted)
	var receipt generated.TurnAccepted
	if e = generated.Decode("TurnAccepted", out, &receipt); e != nil {
		t.Fatal(e)
	}
	var acceptedAt, executeBy, settleBy time.Time
	var holds, tasks int
	if e = r.Store.DB.QueryRow("SELECT accepted_at,execute_by,settle_by FROM harness_envelopes WHERE task_id=?", accepted.TurnID).Scan(&acceptedAt, &executeBy, &settleBy); e != nil {
		t.Fatal(e)
	}
	if executeBy.Sub(acceptedAt) != 120*time.Second || settleBy.Sub(acceptedAt) != 180*time.Second {
		t.Fatal("actual accepted deadlines wrong")
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=?", accepted.TurnID).Scan(&holds); e != nil || holds != 4 {
		t.Fatal("zero price skipped parent holds", holds, e)
	}
	var source string
	if e = r.Store.DB.QueryRow("SELECT input_source_id FROM harness_turns WHERE task_id=?", accepted.TurnID).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='creator'", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var graph memory.Aggregate
	if e = json.Unmarshal(raw, &graph); e != nil {
		t.Fatal(e)
	}
	input, ok := graph.Sources[source]
	if !ok || input.Audience != memory.Private || input.Domain != memory.PlayerStatement {
		t.Fatal("input not governed private statement", input)
	}
	told := false
	for _, a := range graph.Acquisitions {
		if a.SourceID == source && a.ActorID == actor && a.Kind == memory.Told && a.EvidenceID == accepted.TurnID {
			told = true
		}
	}
	if !told {
		t.Fatal("input did not establish actual actor TOLD")
	}
	lease, e := r.Store.ClaimRegistered(ctx, "original-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	dispatch, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "original-worker")
	if e != nil {
		t.Fatal("original task preparation unavailable", e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, dispatch); e != nil {
		t.Fatal("actual authorization", e)
	}
	dispatch.CommandKey = content.Identity(accepted.TurnID, "send")
	dispatch.CommitKey = dispatch.CommandKey
	marker, e := r.Store.StartSend(ctx, dispatch, dispatch.AttemptID)
	if e != nil || !marker.CanSend || !marker.Receipt.Committed {
		t.Fatal("known original marker", marker, e)
	}
	replay, e := r.Store.StartSend(ctx, dispatch, dispatch.AttemptID)
	if e != nil || replay.CanSend || !replay.QueryOriginal {
		t.Fatal("original marker allowed a second send", replay, e)
	}
	var step int
	var state string
	if e = r.Store.DB.QueryRow("SELECT e.steps_used,h.fsm_state FROM harness_envelopes e JOIN harness_turns h ON h.task_id=e.task_id WHERE e.task_id=?", accepted.TurnID).Scan(&step, &state); e != nil || step != 3 || state != "SEND_MARKED" {
		t.Fatal("durable FSM did not advance exactly three steps", step, state, e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=?", accepted.TurnID).Scan(&holds); e != nil || holds != 6 {
		t.Fatal("per attempt two supplier caps absent", holds, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", accepted.TurnID); e != nil {
		t.Fatal(e)
	}
	replacement, e := r.Store.ClaimRegistered(ctx, "replacement-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil || replacement.Fence <= lease.Fence {
		t.Fatal(replacement, e)
	}
	if _, e = r.Store.StartSend(ctx, dispatch, dispatch.AttemptID); e == nil {
		t.Fatal("old fence replayed send")
	}
	againDispatch, e := turns.PrepareOriginalTurn(ctx, replacement.TaskLease, "replacement-worker")
	if e != nil || againDispatch.AttemptID != dispatch.AttemptID || againDispatch.RequestKey != dispatch.RequestKey || againDispatch.RequestHash != dispatch.RequestHash {
		t.Fatal("takeover changed original purchase", e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, againDispatch); e != nil {
		t.Fatal(e)
	}
	againDispatch.CommandKey = dispatch.CommandKey
	againDispatch.CommitKey = dispatch.CommitKey
	replay, e = r.Store.StartSend(ctx, againDispatch, againDispatch.AttemptID)
	if e != nil || replay.CanSend || !replay.QueryOriginal {
		t.Fatal("takeover reset original marker", replay, e)
	}
	var calls int
	if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", accepted.TurnID).Scan(&calls); e != nil || calls != 1 {
		t.Fatal("takeover reset budget", calls, e)
	}
	if e = r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", accepted.TurnID).Scan(&step); e != nil || step != 3 {
		t.Fatal("takeover allocated another step", step, e)
	}
	// The original result lookup precedes original quote expiry and old versions.
	if _, e = r.Store.DB.Exec("UPDATE harness_quotes SET created_at=DATE_SUB(created_at,INTERVAL 61 SECOND),expires_at=DATE_SUB(expires_at,INTERVAL 61 SECOND) WHERE quote_id=?", quote.QuoteID); e != nil {
		t.Fatal(e)
	}
	runtimeControl(t, r, instance, "PAUSE")
	again, e := turns.SubmitTurn(ctx, scope, request, "original-turn")
	if e != nil || again != accepted {
		t.Fatal("lost acceptance reply did not recover original", again, e)
	}
	changed := request
	changed.Input = json.RawMessage(`{"kind":"TEXT","text":"different"}`)
	if _, e = turns.SubmitTurn(ctx, scope, changed, "original-turn"); e == nil {
		t.Fatal("different complete payload reused key")
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turns").Scan(&tasks); e != nil || tasks != 1 {
		t.Fatal("replay created another task", tasks, e)
	}
	view, e := turns.GetTurn(ctx, scope, accepted.TurnID)
	if e != nil || view.Task.TaskID != accepted.TurnID || view.Billing.ReservationSettleBy == nil {
		t.Fatal(view, e)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].AttemptID != dispatch.AttemptID || view.Attempts[0].UsageState != "UNKNOWN" {
		t.Fatal("attempt state omitted or unknown usage invented", view.Attempts)
	}
	out, _ = json.Marshal(view)
	var checked generated.TurnView
	if e = generated.Decode("TurnView", out, &checked); e != nil {
		t.Fatal("view violates sole schema", e)
	}
}

func TestTurnDeletedInputCannotBind(t *testing.T) {
	ctx := context.Background()
	r, s, rel := runtimeFixtureWithConsent(t, true)
	instance := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "deleted-input-world"), "START")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: instance.Scope.WorldInstanceID}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var initial world.Initialization
	if e := json.Unmarshal(raw, &initial); e != nil {
		t.Fatal(e)
	}
	actor := initial.Actors[0].ID
	turns := harness.NewTurnService(r.Store)
	quote, e := turns.QuoteTurn(ctx, scope, generated.QuoteRequest{Scope: instance.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}, "quote")
	if e != nil {
		t.Fatal(e)
	}
	request := generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: instance.Versions.WorldRevision, ControlVersion: instance.Versions.ControlVersion}, PrimaryActorID: actor, QuoteID: quote.QuoteID, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"Unique input to delete."}`)}
	accepted, e := turns.SubmitTurn(ctx, scope, request, "turn")
	if e != nil {
		t.Fatal(e)
	}
	var source string
	if e = r.Store.DB.QueryRow("SELECT input_source_id FROM harness_turns WHERE task_id=?", accepted.TurnID).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"memory.govern", "memory.source.govern"} {
		if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = r.Memory.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: actor, PlayerID: s.SubjectID}, memory.MemoryGovernanceCommand{Key: "delete-input", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{source}, Expected: map[string]uint64{source: 1}}}); e != nil {
		t.Fatal(e)
	}
	again, e := turns.SubmitTurn(ctx, scope, request, "turn")
	if e != nil || again != accepted {
		t.Fatal("metadata replay failed", e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
	if e != nil {
		t.Fatal(e)
	}
	var v domain.Versions
	if e = r.Store.DB.QueryRow("SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch); e != nil {
		t.Fatal(e)
	}
	g, e := content.Gate(ctx, r.Store, s)
	if e != nil {
		t.Fatal(e)
	}
	selection := domain.GenerationSelection{ActorID: actor, DataSubjectID: s.SubjectID, TargetKind: "PLAYER", TargetID: s.SubjectID}
	locks := memory.GenerationContextLocks(memory.ActorGenerationRequest{Scope: scope, ActorID: actor, DataSubjectID: s.SubjectID, TargetKind: "PLAYER", TargetID: s.SubjectID})
	br := store.BarrierRequest{ProductScope: s, WorldScope: &scope, Versions: &v, OwnerSubjectID: s.SubjectID, TaskID: accepted.TurnID, WorkerID: "worker", Fence: lease.Fence, GateVersions: g, Operation: "HARNESS_CONTEXT", CommandKey: "bind", CommitKey: "bind", Locks: locks}
	if _, e = r.Store.InitializeHarnessContext(ctx, br, selection); e == nil {
		t.Fatal("deleted original input silently omitted from bound generation request")
	}
	var bindings int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_context_bindings WHERE task_id=?", accepted.TurnID).Scan(&bindings); e != nil || bindings != 0 {
		t.Fatal("failed binding persisted", bindings, e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='creator'", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var graph memory.Aggregate
	if e = json.Unmarshal(raw, &graph); e != nil {
		t.Fatal(e)
	}
	if graph.Sources[source].Body != "" {
		t.Fatal("replay restored deleted input")
	}
}

func actualTurnFixture(t *testing.T, optIn bool) (*app.Runtime, domain.Scope, *harness.TurnService, generated.QuoteRequest, generated.TurnRequest) {
	t.Helper()
	r, s, rel := runtimeFixtureWithConsent(t, optIn)
	instance := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "bounded-world"), "START")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: instance.Scope.WorldInstanceID}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var initial world.Initialization
	if e := json.Unmarshal(raw, &initial); e != nil {
		t.Fatal(e)
	}
	actor := initial.Actors[0].ID
	return r, scope, harness.NewTurnService(r.Store), generated.QuoteRequest{Scope: instance.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}, generated.TurnRequest{ExpectedVersions: generated.ExpectedVersions{WorldRevision: instance.Versions.WorldRevision, ControlVersion: instance.Versions.ControlVersion}, PrimaryActorID: actor, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"Hello."}`)}
}
func TestTurnIntakeBoundaries(t *testing.T) {
	for _, scenario := range []string{"default-consent", "rollback", "commit-unknown", "concurrent-original", "shared-quote", "stale-versions", "wrong-actor", "expired-quote", "assembled-input-cap", "steps-cap", "execution-deadline"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			r, scope, turns, quoteRequest, request := actualTurnFixture(t, scenario != "default-consent")
			if scenario == "assembled-input-cap" {
				quoteRequest.InputUpperBound = "80"
			}
			quote, e := turns.QuoteTurn(ctx, scope, quoteRequest, "quote")
			if scenario == "default-consent" {
				if e == nil {
					t.Fatal("default false consent allowed first quote")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			request.QuoteID = quote.QuoteID
			if scenario == "expired-quote" {
				if _, e = r.Store.DB.Exec("UPDATE harness_quotes SET created_at=DATE_SUB(created_at,INTERVAL 61 SECOND),expires_at=DATE_SUB(expires_at,INTERVAL 61 SECOND) WHERE quote_id=?", quote.QuoteID); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "stale-versions" {
				request.ExpectedVersions.ControlVersion = "0"
			}
			if scenario == "wrong-actor" {
				request.PrimaryActorID = "foreign-actor"
			}
			if scenario == "concurrent-original" || scenario == "shared-quote" {
				var group sync.WaitGroup
				results := make(chan generated.TurnAccepted, 6)
				failures := make(chan error, 6)
				for n := 0; n < 6; n++ {
					group.Add(1)
					go func(n int) {
						defer group.Done()
						key := "original"
						if scenario == "shared-quote" {
							key = fmt.Sprintf("competing-%d", n)
						}
						out, e := turns.SubmitTurn(ctx, scope, request, key)
						if e != nil {
							failures <- e
						} else {
							results <- out
						}
					}(n)
				}
				group.Wait()
				close(results)
				close(failures)
				count := 0
				var first generated.TurnAccepted
				for a := range results {
					if count > 0 && a != first {
						t.Fatal("split original task")
					}
					first = a
					count++
				}
				if scenario == "concurrent-original" && count != 6 || scenario == "shared-quote" && count != 1 {
					t.Fatal("wrong acceptance count", count, len(failures))
				}
				var holds, tasks int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turns").Scan(&tasks); e != nil || tasks != 1 {
					t.Fatal(tasks, e)
				}
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=?", first.TurnID).Scan(&holds); e != nil || holds != 4 {
					t.Fatal(holds, e)
				}
				return
			}
			var originalGraph []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='creator'", scope.WorldInstanceID).Scan(&originalGraph); e != nil {
				t.Fatal(e)
			}
			if scenario == "rollback" {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					var count int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_turns").Scan(&count); e != nil {
						return e
					}
					if count > 0 {
						return errors.New("injected after complete accepted input/holds before receipt commit")
					}
					return nil
				}
			}
			if scenario == "commit-unknown" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("lost original acceptance reply")
				}
			}
			accepted, e := turns.SubmitTurn(ctx, scope, request, "turn")
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if scenario == "rollback" || scenario == "stale-versions" || scenario == "wrong-actor" || scenario == "expired-quote" {
				if e == nil {
					t.Fatal("invalid acceptance succeeded")
				}
				var tasks, accounts, holds int
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_turns),(SELECT COUNT(*) FROM budget_accounts),(SELECT COUNT(*) FROM reservations)").Scan(&tasks, &accounts, &holds); e != nil || tasks != 0 || accounts != 0 || holds != 0 {
					t.Fatal("acceptance leaked persisted effects", tasks, accounts, holds, e)
				}
				var after []byte
				if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id='creator'", scope.WorldInstanceID).Scan(&after); e != nil || string(after) != string(originalGraph) {
					t.Fatal("acceptance leaked governed input", e)
				}
				return
			}
			if scenario == "commit-unknown" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				accepted, e = turns.SubmitTurn(ctx, scope, request, "turn")
				if e != nil || accepted.TurnID == "" {
					t.Fatal("original acceptance not recovered", e)
				}
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turns").Scan(&n); e != nil || n != 1 {
					t.Fatal(n, e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if scenario == "execution-deadline" {
				if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 121 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 121 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 121 SECOND) WHERE task_id=?", accepted.TurnID); e != nil {
					t.Fatal(e)
				}
			}
			lease, e := r.Store.ClaimRegistered(ctx, "bounded-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: accepted.TurnID}})
			if e != nil {
				t.Fatal(e)
			}
			d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "bounded-worker")
			if scenario == "assembled-input-cap" || scenario == "execution-deadline" {
				if e == nil {
					t.Fatal("invalid assembled/deadline context bound")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET steps_used=max_steps WHERE task_id=?", accepted.TurnID); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e == nil {
				t.Fatal("full step budget authorized")
			}
			var attempts, calls int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM attempts WHERE task_id=?),calls_used FROM tasks WHERE task_id=?", accepted.TurnID, accepted.TurnID).Scan(&attempts, &calls); e != nil || attempts != 0 || calls != 0 {
				t.Fatal("step denial leaked purchase", attempts, calls, e)
			}
		})
	}
}

func TestTurnQuoteLatestAndActualDeadlines(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "near-expiry")
	if e != nil {
		t.Fatal(e)
	}
	expiry, e := time.Parse(time.RFC3339Nano, quote.ExpiresAt)
	if e != nil {
		t.Fatal(e)
	}
	latest, e := time.Parse(time.RFC3339Nano, quote.ReservationSettleBy)
	if e != nil || latest.Sub(expiry) != 180*time.Second {
		t.Fatal("quote latest bound is wrong", e)
	}
	// Wait real wall time, without moving persisted timestamps or using a process
	// clock as the authority for accepted/deadline values. MySQL owns both values.
	time.Sleep(55 * time.Second)
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "near-expiry-turn")
	if e != nil {
		t.Fatal(e)
	}
	var at, execute, settle time.Time
	if e = r.Store.DB.QueryRow("SELECT accepted_at,execute_by,settle_by FROM harness_envelopes WHERE task_id=?", accepted.TurnID).Scan(&at, &execute, &settle); e != nil {
		t.Fatal(e)
	}
	if expiry.Sub(at) > 6*time.Second || !at.Before(expiry) || execute.Sub(at) != 120*time.Second || settle.Sub(at) != 180*time.Second || !settle.Before(latest) {
		t.Fatal("acceptance extended actual envelope", at, execute, settle, latest)
	}
	view, e := turns.GetTurn(ctx, scope, accepted.TurnID)
	if e != nil || view.Billing.ReservationSettleBy == nil || *view.Billing.ReservationSettleBy != settle.Format(time.RFC3339Nano) {
		t.Fatal("GetTurn returned quote upper bound", e)
	}
}
