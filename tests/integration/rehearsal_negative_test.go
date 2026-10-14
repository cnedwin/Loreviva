package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
)

func TestRehearsalNegativeHistoryAndUnknown(t *testing.T) {
	for _, mode := range []string{"FINAL_STRUCTURE_HISTORY", "UNKNOWN_USAGE", "UNKNOWN"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			var root generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/rehearsals", "negative-history", req, 202, &root)
			var e error
			r.Worker.Turns.Fault = harness.MockMalformedEveryResult
			if mode == "UNKNOWN_USAGE" {
				r.Worker.Turns.Fault = harness.MockUsagePendingRefusal
			}
			if mode == "UNKNOWN" {
				r.Worker.Turns.Fault = harness.MockAcceptedOutcomeUnknown
			}
			e = r.Worker.Once(ctx)
			if mode == "UNKNOWN" {
				if e == nil {
					t.Fatal("unknown pretended complete")
				}
				before := journalBytes(t, r)
				if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND"); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=UTC_TIMESTAMP(6)"); e != nil {
					t.Fatal(e)
				}
				again := app.NewRuntime(r.Store, r.Files, r.Auth)
				again.BindLocal(r.Local)
				if e = again.Worker.Once(ctx); e == nil {
					t.Fatal("unknown restart advanced")
				}
				var next, calls, negative, world, svc int
				if e = r.Store.DB.QueryRow("SELECT a.next_input,e.calls_allocated,(SELECT COUNT(*) FROM harness_rehearsal_negative_outcomes),(SELECT COUNT(*) FROM harness_turn_commits),(SELECT COUNT(*) FROM service_events) FROM harness_admin_rehearsals a JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE a.task_id=?", root.TaskID).Scan(&next, &calls, &negative, &world, &svc); e != nil || next != 0 || calls != 1 || negative != 0 || world != 0 || svc != 0 {
					t.Fatal(next, calls, negative, world, svc, e)
				}
				if !bytes.Equal(before, journalBytes(t, r)) {
					t.Fatal("unknown restart generated new provider work")
				}
				runtimeHTTP(t, again, "POST", "/tasks/"+string(root.TaskID)+"/cancel", "unknown-http-cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "stop unknown rehearsal"}, 202, nil)
				expireCutoff(t, r.Store.DB, string(root.TaskID))
				if _, e = again.Store.CutoffDraftRehearsal(ctx, scope, string(root.TaskID)); e != nil {
					t.Fatal("HTTP unknown cancellation settlement", e)
				}
				var sentHeld, rootCutoff int
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM reservations WHERE kind IN ('supplier_global','supplier_tenant') AND status='HELD'),(SELECT COUNT(*) FROM harness_envelopes WHERE task_id=? AND state='CUTOFF'),(SELECT COUNT(*) FROM harness_rehearsal_negative_outcomes),(SELECT COUNT(*) FROM harness_turn_commits),(SELECT COUNT(*) FROM service_events)", root.TaskID).Scan(&sentHeld, &rootCutoff, &negative, &world, &svc); e != nil || sentHeld != 2 || rootCutoff != 1 || negative != 0 || world != 0 || svc != 0 {
					t.Fatal("unknown cancellation fabricated outcome or cleared supplier", sentHeld, rootCutoff, negative, world, svc, e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
			if e != nil {
				t.Fatal(e)
			}
			if mode == "UNKNOWN_USAGE" {
				var status string
				var lines, unknown, svc int
				if e = r.Store.DB.QueryRow("SELECT status,(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN' AND input_units IS NULL AND output_units IS NULL),(SELECT COUNT(*) FROM service_events) FROM service_events").Scan(&status, &lines, &unknown, &svc); e != nil || status != "PENDING" || lines != 0 || unknown != 1 || svc != 1 {
					t.Fatal("negative metering inferred numeric usage", status, lines, unknown, svc, e)
				}
				if !bytes.Contains(body, []byte("REFUSED")) {
					t.Fatal(string(body))
				}
				var raw []byte
				if e = r.Store.DB.QueryRow("SELECT request_identity FROM harness_original_queries").Scan(&raw); e != nil {
					t.Fatal(e)
				}
				var id domain.MockRequestIdentity
				if e = json.Unmarshal(raw, &id); e != nil {
					t.Fatal(e)
				}
				r.Worker.Turns.Fault = harness.MockFault(19)
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.ReconcileOriginalServiceMetering(ctx, id); e != nil {
					t.Fatal("negative original metering", e)
				}
				r.Worker.Turns.Fault = harness.MockLateFinalZeroBill
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
				if _, _, e = r.Store.RunOriginalBills(ctx); e != nil {
					t.Fatal("negative original final cost", e)
				}
				got, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
				if e != nil || !bytes.Equal(body, got) {
					t.Fatal("cost changed historical negative report", e)
				}
				if e = r.Store.DB.QueryRow("SELECT status,(SELECT COUNT(*) FROM service_events) FROM service_events").Scan(&status, &svc); e != nil || status != "COMMITTED" || svc != 1 {
					t.Fatal(status, svc, e)
				}
				return
			}
			var child string
			var raw []byte
			if e = r.Store.DB.QueryRow("SELECT task_id,document FROM harness_rehearsal_negative_outcomes WHERE root_task_id=?", root.TaskID).Scan(&child, &raw); e != nil {
				t.Fatal(e)
			}
			var n domain.RehearsalNegativeOutcome
			if e = json.Unmarshal(raw, &n); e != nil || n.OriginalDependency == nil {
				t.Fatal(e)
			}
			for _, change := range []struct{ name, apply, restore string }{
				{"NEGATIVE_RECEIPT", "UPDATE commit_receipts SET operation='HARNESS_TURN_COMMIT' WHERE task_id=? AND operation='REHEARSAL_CHILD_REJECT'", "UPDATE commit_receipts SET operation='REHEARSAL_CHILD_REJECT' WHERE task_id=? AND operation='HARNESS_TURN_COMMIT'"},
				{"VALIDATION", "UPDATE harness_original_validations SET validation_code='REFUSAL' WHERE task_id=? AND attempt_id IN (SELECT attempt_id FROM attempts WHERE slot=1)", "UPDATE harness_original_validations SET validation_code='STRUCTURE_INVALID' WHERE task_id=? AND attempt_id IN (SELECT attempt_id FROM attempts WHERE slot=1)"},
			} {
				t.Run(change.name, func(t *testing.T) {
					if _, e := r.Store.DB.Exec(change.apply, child); e != nil {
						t.Fatal(e)
					}
					defer r.Store.DB.Exec(change.restore, child)
					if _, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
						t.Fatal("negative historical proof bypass")
					}
				})
			}
			for _, meta := range []domain.MockResultMetadata{*n.OriginalDependency, n.Metadata} {
				t.Run(meta.Identity.AttemptID, func(t *testing.T) {
					path := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, meta.BodyStorage+".part")
					original, e := os.ReadFile(path)
					if e != nil {
						t.Fatal(e)
					}
					if e = os.WriteFile(path, []byte("corrupt actual original"), 0600); e != nil {
						t.Fatal(e)
					}
					defer os.WriteFile(path, original, 0600)
					if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
						t.Fatal("negative body dependency missing")
					}
				})
			}
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='asset.read'"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
				t.Fatal("negative history ignored current read")
			}
			if _, e = r.Store.DB.Exec("INSERT INTO operator_actions VALUES('owner','asset.read')"); e != nil {
				t.Fatal(e)
			}
			got, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
			if e != nil || !bytes.Equal(got, body) {
				t.Fatal("original negative report changed", e)
			}
		})
	}
}

func TestRehearsalNegativeAtomicRecovery(t *testing.T) {
	for _, mode := range []string{"CUTOFF", "MIXED", "COMMIT_ACK", "ROLLBACK_ACK", "SOURCE", "REHEARSE", "FENCE", "RECEIPT"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED","RIGHTS"]}`), &req); e != nil {
				t.Fatal(e)
			}
			accepted, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "negative-atomic", req)
			if e != nil {
				t.Fatal(e)
			}
			r.Worker.Turns.Fault = harness.MockRefuseOriginalResult
			injected := false
			negative := func(c context.Context, tx *sql.Tx) (bool, error) {
				var n int
				e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_rehearsal_negative_outcomes WHERE root_task_id=?", accepted.TaskID).Scan(&n)
				return n > 0, e
			}
			if mode == "CUTOFF" || mode == "MIXED" || mode == "COMMIT_ACK" || mode == "ROLLBACK_ACK" {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					hit, e := negative(c, tx)
					if e != nil {
						return e
					}
					if !hit || injected {
						return tx.Commit()
					}
					injected = true
					if mode == "ROLLBACK_ACK" {
						if e = tx.Rollback(); e != nil {
							return e
						}
					} else {
						if e = tx.Commit(); e != nil {
							return e
						}
					}
					if mode == "MIXED" {
						r.Worker.Turns.Fault = 0
						return nil
					}
					return errors.New("negative original ACK lost")
				}
			} else {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					hit, e := negative(c, tx)
					if e != nil || !hit {
						return e
					}
					injected = true
					switch mode {
					case "REHEARSE":
						_, e = tx.ExecContext(c, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
					case "FENCE":
						_, e = tx.ExecContext(c, "UPDATE tasks SET fence=fence+1 WHERE task_id IN (SELECT child_task_id FROM harness_rehearsal_children WHERE root_task_id=?)", accepted.TaskID)
					case "RECEIPT":
						_, e = tx.ExecContext(c, "UPDATE commit_receipts SET payload_hash=REPEAT(CHAR(0),32) WHERE operation='REHEARSAL_CHILD_REJECT'")
					case "SOURCE":
						var world, source string
						var raw []byte
						if e = tx.QueryRowContext(c, "SELECT world_instance_id,input_source_id FROM harness_rehearsal_children WHERE root_task_id=? AND input_index=0", accepted.TaskID).Scan(&world, &source); e != nil {
							return e
						}
						if e = tx.QueryRowContext(c, "SELECT document FROM memory_aggregates WHERE world_instance_id=?", world).Scan(&raw); e != nil {
							return e
						}
						var a memory.Aggregate
						if e = json.Unmarshal(raw, &a); e != nil {
							return e
						}
						delete(a.Sources, source)
						raw, e = json.Marshal(a)
						if e != nil {
							return e
						}
						_, e = tx.ExecContext(c, "UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", raw, world)
					}
					return e
				}
			}
			e = r.Worker.Once(ctx)
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			if !injected {
				t.Fatal("negative owning commit not reached", e)
			}
			var next, rows, calls, services, worlds int
			check := func() {
				t.Helper()
				if x := r.Store.DB.QueryRow("SELECT a.next_input,e.calls_allocated,(SELECT COUNT(*) FROM harness_rehearsal_negative_outcomes),(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_turn_commits) FROM harness_admin_rehearsals a JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE a.task_id=?", accepted.TaskID).Scan(&next, &calls, &rows, &services, &worlds); x != nil {
					t.Fatal(x)
				}
			}
			check()
			if mode == "MIXED" {
				if e != nil || next != 2 || rows != 1 || calls != 2 || services != 1 || worlds != 1 {
					t.Fatal("mixed actual outcomes", next, rows, calls, services, worlds, e)
				}
				body, x := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(accepted.TaskID))
				if x != nil {
					t.Fatal(x)
				}
				var report struct {
					Inputs []struct {
						State string `json:"state"`
					} `json:"inputs"`
				}
				if x = json.Unmarshal(body, &report); x != nil || len(report.Inputs) != 2 || report.Inputs[0].State != "REFUSED" || report.Inputs[1].State != "COMMITTED" {
					t.Fatal(string(body), x)
				}
				return
			}
			if mode == "CUTOFF" || mode == "COMMIT_ACK" || mode == "ROLLBACK_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal("ACK outcome", e)
				}
			} else if e == nil {
				t.Fatal("current negative finish accepted mutation")
			}
			want := 0
			if mode == "COMMIT_ACK" || mode == "CUTOFF" {
				want = 1
			}
			if next != want || rows != want || calls != 1 || services != 0 || worlds != 0 {
				t.Fatal("partial or inferred negative outcome", next, rows, calls, services, worlds, e)
			}
			if mode == "CUTOFF" {
				expireCutoff(t, r.Store.DB, string(accepted.TaskID))
				if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=accepted_at-INTERVAL 60 SECOND,execute_by=execute_by-INTERVAL 60 SECOND,settle_by=settle_by-INTERVAL 60 SECOND WHERE task_id=?", accepted.TaskID); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.CutoffDraftRehearsal(ctx, scope, string(accepted.TaskID)); e != nil {
					t.Fatal("negative known outcome cutoff", e)
				}
				var state string
				if e = r.Store.DB.QueryRow("SELECT state FROM harness_envelopes WHERE task_id IN (SELECT child_task_id FROM harness_rehearsal_children WHERE root_task_id=?)", accepted.TaskID).Scan(&state); e != nil || state != "ACCOUNTING_ONLY" {
					t.Fatal("negative original retirement changed", state, e)
				}
				check()
				if rows != 1 || next != 1 || calls != 1 || services != 0 || worlds != 0 {
					t.Fatal("cutoff rewrote negative history", rows, next, calls, services, worlds)
				}
				return
			}
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id=? OR task_id IN (SELECT child_task_id FROM harness_rehearsal_children WHERE root_task_id=?)", accepted.TaskID, accepted.TaskID); e != nil {
				t.Fatal(e)
			}
			again := app.NewRuntime(r.Store, r.Files, r.Auth)
			again.BindLocal(r.Local)
			again.Worker.Turns.Fault = harness.MockRefuseOriginalResult
			if e = again.Worker.Once(ctx); e != nil {
				t.Fatal("original negative recovery", e)
			}
			check()
			if next != 2 || rows != 2 || calls != 2 || services != 1 || worlds != 0 {
				t.Fatal("recovery repeated purchase or world effect", next, rows, calls, services, worlds)
			}
			if _, e = again.Files.ReadCommittedRehearsalReport(ctx, scope, string(accepted.TaskID)); e != nil {
				t.Fatal("actual terminal negative report", e)
			}
		})
	}
}
