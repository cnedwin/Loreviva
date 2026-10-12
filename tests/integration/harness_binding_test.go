package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
	"testing"
	"time"
)

func TestHarnessBoundContextDispatchSendCommit(t *testing.T) {
	for _, boundary := range []string{"positive", "authorize", "send", "commit", "route", "quoted-route", "restart", "missing-adapter", "missing-binding", "source-authorize", "source-send", "source-commit", "takeover", "send-ack-unknown", "commit-rollback", "expired-authorize", "expired-send", "expired-commit"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			r, scope, turns, qr, request := actualTurnFixture(t, true)
			// Real accepted Turn/input/TOLD/quote/holds are essential dependencies of
			// the current owner. A generic Harness envelope is not a Turn fixture.
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var item world.ItemState
			if e := json.Unmarshal(raw, &item); e != nil {
				t.Fatal(e)
			}
			request.PrimaryActorID = item.CustodianID
			qr.PrimaryActorID = &request.PrimaryActorID
			qr.OutputUpperBound = "4000"
			request.Input = json.RawMessage(`{"kind":"TEXT","text":"Pass the sign and remember my request."}`)
			quote, e := turns.QuoteTurn(ctx, scope, qr, "binding-quote")
			if e != nil {
				t.Fatal(e)
			}
			request.QuoteID = quote.QuoteID
			accepted, e := turns.SubmitTurn(ctx, scope, request, "binding-turn")
			if e != nil {
				t.Fatal(e)
			}
			task := accepted.TurnID
			worker := "binding-worker"
			claimed, e := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: task}})
			if e != nil {
				t.Fatal(e)
			}
			lease := claimed.TaskLease
			if boundary == "quoted-route" {
				if e = r.Store.DB.QueryRow("SELECT document FROM harness_quotes WHERE quote_id=?", quote.QuoteID).Scan(&raw); e != nil {
					t.Fatal(e)
				}
				var original store.HarnessQuote
				if e = json.Unmarshal(raw, &original); e != nil {
					t.Fatal(e)
				}
				original.RouteHash = [32]byte{99}
				raw, e = json.Marshal(original)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.DB.Exec("UPDATE harness_quotes SET document=? WHERE quote_id=?", raw, quote.QuoteID); e != nil {
					t.Fatal(e)
				}
				if _, e = turns.PrepareOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrConflict) {
					t.Fatal("quote route mismatch", e)
				}
				return
			}
			d, e := turns.PrepareOriginalTurn(ctx, lease, worker)
			if e != nil {
				t.Fatal("actual original binding", e)
			}
			var saved []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_context_bindings WHERE task_id=?", task).Scan(&saved); e != nil {
				t.Fatal(e)
			}
			var binding domain.GenerationBinding
			if e = json.Unmarshal(saved, &binding); e != nil {
				t.Fatal(e)
			}
			if binding.InputSourceID == "" || binding.TurnCommandHash == "" {
				t.Fatal("missing real input/command binding")
			}
			if strings.Contains(string(saved), "Body") || strings.Contains(string(saved), "OriginalCharacter") {
				t.Fatal("binding contains private plaintext")
			}
			if boundary == "restart" {
				r.Store.GenerationContext = memory.New(r.Store, r.Memory.Ledger)
			}
			if boundary == "missing-adapter" {
				r.Store.GenerationContext = nil
			}
			if boundary == "missing-binding" {
				if _, e = r.Store.DB.Exec("DELETE FROM harness_context_bindings WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
			}
			if boundary == "route" {
				d.Route = "foreign-owner-route"
			}
			withdraw := func() {
				if _, e := r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
					t.Fatal(e)
				}
			}
			expire := func() {
				if _, e := r.Store.DB.Exec("UPDATE memory_subject_permissions SET expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE action='memory.generation.use'"); e != nil {
					t.Fatal(e)
				}
			}
			deleteSource := func() {
				if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id='creator'"); e != nil {
					t.Fatal(e)
				}
				for _, action := range []string{"memory.govern", "memory.source.govern"} {
					if _, e := r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
						t.Fatal(e)
					}
				}
				if _, e := r.Memory.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: binding.ActorID, PlayerID: scope.SubjectID}, memory.MemoryGovernanceCommand{Key: "withdraw-original-source", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{binding.InputSourceID}, Expected: map[string]uint64{binding.InputSourceID: 1}}}); e != nil {
					t.Fatal(e)
				}
				// Current outer versions avoid treating stale request epochs as the
				// intended original source/binding denial.
				var v domain.Versions
				if e := r.Store.DB.QueryRow("SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch); e != nil {
					t.Fatal(e)
				}
				d.Versions = &v
				g, e := content.Gate(ctx, r.Store, scope.ProductScope)
				if e != nil {
					t.Fatal(e)
				}
				d.GateVersions = g
			}
			mutate := func(stage string) {
				switch boundary {
				case stage:
					withdraw()
				case "source-" + stage:
					deleteSource()
				case "expired-" + stage:
					expire()
				}
			}
			expectDenied := func(e error) {
				t.Helper()
				if !errors.Is(e, store.ErrDenied) && !errors.Is(e, store.ErrConflict) && !errors.Is(e, memory.ErrInvalid) {
					t.Fatalf("expected current binding/authority denial, got %T %v", e, e)
				}
			}
			stateCounts := func() string {
				t.Helper()
				var v string
				if e := r.Store.DB.QueryRow("SELECT CONCAT(calls_used,':',(SELECT steps_used FROM harness_envelopes h WHERE h.tenant_id=tasks.tenant_id AND h.product_id=tasks.product_id AND h.task_id=tasks.task_id),':',(SELECT COUNT(*) FROM attempts WHERE task_id=?),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts),':',(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),':',(SELECT COUNT(*) FROM events WHERE task_id=?),':',(SELECT COUNT(*) FROM service_events WHERE task_id=?)) FROM tasks WHERE task_id=?", task, task, task, task, task).Scan(&v); e != nil {
					t.Fatal(e)
				}
				return v
			}
			unchanged := func(v string, j []byte) {
				t.Helper()
				if stateCounts() != v || !bytes.Equal(j, journalBytes(t, r)) {
					t.Fatal("denial changed counters, holds, receipt, business rows or journal")
				}
			}
			mutate("authorize")
			deniedState, deniedJournal := stateCounts(), journalBytes(t, r)
			_, e = r.Store.AuthorizeDispatch(ctx, d)
			if boundary == "authorize" || boundary == "route" || boundary == "missing-adapter" || boundary == "missing-binding" || boundary == "source-authorize" || boundary == "expired-authorize" {
				expectDenied(e)
				unchanged(deniedState, deniedJournal)
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
					t.Fatal("denied authorize created attempt", n, e)
				}
				return
			}
			if e != nil {
				t.Fatal("authorize", e)
			}
			if boundary == "takeover" {
				if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
				next, e := r.Store.ClaimRegistered(ctx, "replacement-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN", TaskID: task}})
				if e != nil || next.Fence <= lease.Fence {
					t.Fatal(next, e)
				}
				old := d
				old.CommandKey = content.Identity(task, "send")
				old.CommitKey = old.CommandKey
				if _, e = r.Store.StartSend(ctx, old, old.AttemptID); !errors.Is(e, store.ErrDenied) {
					t.Fatal("old fence", e)
				}
				worker = "replacement-worker"
				lease = next.TaskLease
				d, e = turns.PrepareOriginalTurn(ctx, lease, worker)
				if e != nil {
					t.Fatal(e)
				}
			}
			mutate("send")
			deniedState, deniedJournal = stateCounts(), journalBytes(t, r)
			d.CommandKey = content.Identity(task, "send")
			d.CommitKey = d.CommandKey
			if boundary == "send-ack-unknown" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic lost marker COMMIT reply")
				}
			}
			sent, e := r.Store.StartSend(ctx, d, d.AttemptID)
			if boundary == "send-ack-unknown" {
				r.Store.Commit = nil
				if !errors.Is(e, store.ErrCommitUnknown) || sent.CanSend {
					t.Fatal("unknown marker granted send", sent, e)
				}
				replay, e := r.Store.StartSend(ctx, d, d.AttemptID)
				if e != nil || replay.CanSend || !replay.QueryOriginal {
					t.Fatal("original query-only recovery", replay, e)
				}
				var calls, attempts int
				if e = r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM attempts WHERE task_id=?) FROM tasks WHERE task_id=?", task, task).Scan(&calls, &attempts); e != nil || calls != 1 || attempts != 1 {
					t.Fatal(calls, attempts, e)
				}
				return
			}
			if boundary == "send" || boundary == "source-send" || boundary == "expired-send" {
				expectDenied(e)
				unchanged(deniedState, deniedJournal)
				return
			}
			if e != nil || !sent.CanSend || sent.OriginalSend() == nil {
				t.Fatal("actual send", sent, e)
			}
			if e = r.Worker.Turns.Generate(ctx, sent.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.ObserveOriginalReply(ctx, sent.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.ValidateOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal(e)
			}
			mutate("commit")
			deniedState, deniedJournal = stateCounts(), journalBytes(t, r)
			before := journalBytes(t, r)
			if boundary == "commit-rollback" {
				injected := false
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?", task).Scan(&n); e != nil {
						return e
					}
					if n > 0 {
						injected = true
						return errors.New("synthetic joint commit rollback")
					}
					return nil
				}
				if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); e == nil || !injected {
					t.Fatal("joint rollback not reached", injected, e)
				}
				r.Store.BeforeCommit = nil
				var n int
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM events WHERE task_id=?)+(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?)+(SELECT COUNT(*) FROM service_events WHERE task_id=?)", task, task, task).Scan(&n); e != nil || n != 0 {
					t.Fatal("rollback leaked joint writes", n, e)
				}
			}
			_, e = r.Store.CommitOriginalTurn(ctx, lease, worker)
			if boundary == "commit" || boundary == "source-commit" || boundary == "expired-commit" {
				expectDenied(e)
				unchanged(deniedState, deniedJournal)
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
				return
			}
			if e != nil {
				t.Fatal("actual original joint commit", e)
			}
			var events, calls int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM events WHERE task_id=?),calls_used FROM tasks WHERE task_id=?", task, task).Scan(&events, &calls); e != nil || events != 1 || calls != 1 {
				t.Fatal("real action and one original call", events, calls, e)
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("local commit repeated provider call")
			}
		})
	}
}
