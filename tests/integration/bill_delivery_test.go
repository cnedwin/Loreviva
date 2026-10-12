package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"testing"
	"time"
)

func TestAutomaticOriginalBillsFindNewItemsOnOldAttempt(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	done, failed, e := r.Store.RunOriginalBills(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal("automatic original invoice unavailable", done, failed, e)
	}
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateConflictingBillItem
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunOriginalBills(ctx)
	if e != nil || done != 3 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var items, conflicts, services int
	var debt string
	e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?),(SELECT COUNT(*) FROM harness_bill_conflicts WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),CAST(debt_atoms AS CHAR) FROM harness_supplier_liabilities WHERE task_id=?", task, task, task, task).Scan(&items, &conflicts, &services, &debt)
	if e != nil || items != 3 || conflicts != 1 || services != 0 || debt != "3" {
		t.Fatal(items, conflicts, services, debt, e)
	}
	done, failed, e = r.Store.RunOriginalBills(ctx)
	if e != nil || done != 0 || failed != 0 {
		t.Fatal("original durable replay", done, failed, e)
	}
}

func TestAutomaticBillsScanACKUnknownAndRestart(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic scan ACK lost")
	}
	if _, _, e := r.Store.RunOriginalBills(ctx); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	r.Store.Commit = nil
	if e := r.Local.Participant.Close(ctx); e != nil {
		t.Fatal(e)
	}
	local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
	if e != nil {
		t.Fatal(e)
	}
	defer local.Participant.Close(ctx)
	recovered := app.NewRuntime(r.Store, r.Files, r.Auth)
	recovered.BindLocal(local)
	done, failed, e := recovered.Store.RunOriginalBills(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var pages, items int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_bill_scan_receipts),(SELECT COUNT(*) FROM harness_bill_items)").Scan(&pages, &items); e != nil || pages != 1 || items != 1 {
		t.Fatal(pages, items, e)
	}
}
func TestAutomaticBillsRetryRealWorldLockTimeout(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	blocker, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	var id string
	if e = blocker.QueryRow("SELECT world_instance_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", scope.TenantID, scope.ProductID, scope.WorldInstanceID).Scan(&id); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	done, failed, e := r.Store.RunOriginalBills(ctx)
	elapsed := time.Since(start)
	t.Logf("actual original bill item including failed continuation: %s", elapsed)
	if e != nil || done != 0 || failed != 1 || elapsed < 4*time.Second || elapsed > 5*time.Second+250*time.Millisecond {
		rows, diagnostic := r.Store.DB.Query("SELECT STATE FROM information_schema.PROCESSLIST WHERE DB=DATABASE() AND INFO LIKE 'SELECT world_instance_id FROM world_controls%'")
		if diagnostic == nil {
			for rows.Next() {
				var state string
				rows.Scan(&state)
				t.Log("own original world SELECT server state after client timeout:", state)
			}
			rows.Close()
		}
		t.Fatal(done, failed, elapsed, e)
	}
	var failures int
	var backoff bool
	if e = r.Store.DB.QueryRow("SELECT failures,next_run_at>UTC_TIMESTAMP(6) FROM harness_bill_work").Scan(&failures, &backoff); e != nil || failures != 1 || !backoff {
		t.Fatal(failures, backoff, e)
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_bill_work SET next_run_at=UTC_TIMESTAMP(6)"); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunOriginalBills(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
}
func TestAutomaticBillsAuthorityAndOrdinaryOpenNeverReseed(t *testing.T) {
	for _, mode := range []string{"disabled", "missing-service", "missing-cursor", "old-generation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, true)
			expireCutoff(t, r.Store.DB, task)
			if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
				t.Fatal(e)
			}
			var e error
			switch mode {
			case "disabled":
				_, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=0")
			case "missing-service":
				_, e = r.Store.DB.Exec("DELETE FROM harness_settlement_bindings")
				if e == nil {
					_, e = r.Store.DB.Exec("DELETE FROM harness_settlement_services")
				}
			case "missing-cursor":
				_, e = r.Store.DB.Exec("DELETE FROM harness_bill_cursor")
			case "old-generation":
				_, e = r.Store.DB.Exec("UPDATE memory_installation SET generation=generation+1")
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, _, e = r.Store.RunOriginalBills(ctx); e == nil {
				t.Fatal("current authority missing yet scanned")
			}
			if mode == "disabled" {
				local, openErr := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer local.Participant.Close(ctx)
				var enabled bool
				if e = r.Store.DB.QueryRow("SELECT enabled FROM harness_settlement_services").Scan(&enabled); e != nil || enabled {
					t.Fatal("ordinary open reenabled disabled service", enabled, e)
				}
			}
			if mode != "disabled" {
				if _, e = app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath); e == nil {
					t.Fatal("ordinary open repaired missing trusted identity")
				}
			}
			var work int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_work").Scan(&work); e != nil || work != 0 {
				t.Fatal(work, e)
			}
			if mode == "missing-service" {
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_settlement_services").Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
			}
		})
	}
}
func TestAutomaticBillWorkerActuallyDelivers(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	if e := r.Worker.Start(ctx); e != nil {
		t.Fatal(e)
	}
	defer r.Worker.Stop(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var done int
		if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_work WHERE done=1").Scan(&done); e != nil {
			t.Fatal(e)
		}
		if done == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual worker never delivered original bill")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if e := r.Worker.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	var calls int
	var state string
	if e := r.Store.DB.QueryRow("SELECT t.calls_used,e.state FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task).Scan(&calls, &state); e != nil || calls != 1 || state != "CUTOFF" {
		t.Fatal(calls, state, e)
	}
}

func TestAuthorizedUnsentCutoffReleasesBothSupplierHolds(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	q, e := turns.QuoteTurn(ctx, scope, qr, "unsent-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = q.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "unsent-turn")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "unsent-test", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN"}})
	if e != nil {
		t.Fatal(e)
	}
	d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "unsent-test")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, accepted.TurnID)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, accepted.TurnID); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(d.TaskID, "send")
	d.CommitKey = d.CommandKey
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
		t.Fatal("retired authorized send accepted")
	}
	var released, markers int
	var unresolved bool
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='RELEASED'),(SELECT COUNT(*) FROM attempts WHERE task_id=? AND marker_commit_key IS NOT NULL),unresolved FROM harness_risk_cases WHERE task_id=?", d.TaskID, d.TaskID, d.TaskID).Scan(&released, &markers, &unresolved); e != nil || released != 6 || markers != 0 || unresolved {
		t.Fatal(released, markers, unresolved, e)
	}
}
func TestOldOpaqueSendPermitCannotGenerateAfterCutoff(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	q, e := turns.QuoteTurn(ctx, scope, qr, "permit-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = q.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "permit-turn")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "permit-test", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN"}})
	if e != nil {
		t.Fatal(e)
	}
	d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "permit-test")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(d.TaskID, "send")
	d.CommitKey = d.CommandKey
	auth, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	expireCutoff(t, r.Store.DB, accepted.TurnID)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, accepted.TurnID); e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.Generate(ctx, auth.OriginalSend()); e == nil {
		t.Fatal("expired opaque permit generated")
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("old opaque permit dispatched to independent provider after cutoff")
	}
}

func TestAutomaticBillScanPageLimitPreservesEveryJournalPosition(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	for n := 0; n < 52; n++ {
		if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
			t.Fatal(e)
		}
	}
	done, failed, e := r.Store.RunOriginalBills(ctx)
	if e != nil || done != 98 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var position int
	if e = r.Store.DB.QueryRow("SELECT sequence_number FROM harness_bill_cursor").Scan(&position); e != nil || position != 100 {
		t.Fatal(position, e)
	}
	done, failed, e = r.Store.RunOriginalBills(ctx)
	if e != nil || done != 7 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var completed, items int
	var debt string
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_bill_work WHERE done=1),(SELECT COUNT(*) FROM harness_bill_items),CAST(debt_atoms AS CHAR) FROM harness_supplier_liabilities WHERE task_id=?", task).Scan(&completed, &items, &debt); e != nil || completed != 105 || items != 3 || debt != "3" {
		t.Fatal(completed, items, debt, e)
	}
}
func TestCutoffReadTurnUsesProvenNarrativeAndHistoricalServiceDisposition(t *testing.T) {
	for _, mode := range []string{"sent", "authorized-unsent"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, scope, turns, qr, request := actualTurnFixture(t, true)
			q, e := turns.QuoteTurn(ctx, scope, qr, "view-quote")
			if e != nil {
				t.Fatal(e)
			}
			request.QuoteID = q.QuoteID
			a, e := turns.SubmitTurn(ctx, scope, request, "view-turn")
			if e != nil {
				t.Fatal(e)
			}
			if mode == "sent" {
				if e = runOriginalPrefix(ctx, r); e != nil {
					t.Fatal(e)
				}
			} else {
				l, e := r.Store.ClaimRegistered(ctx, "view-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN"}})
				if e != nil {
					t.Fatal(e)
				}
				d, e := turns.PrepareOriginalTurn(ctx, l.TaskLease, "view-worker")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
					t.Fatal(e)
				}
			}
			expireCutoff(t, r.Store.DB, a.TurnID)
			if _, e = r.Store.CutoffHarnessTask(ctx, scope, a.TurnID); e != nil {
				t.Fatal(e)
			}
			expected := "RISK_TRANSFERRED"
			if mode != "sent" {
				expected = "ADJUSTED"
			}
			view, e := turns.GetTurn(ctx, scope, a.TurnID)
			if e != nil || view.Task.TaskState != "FAILED" || view.Billing.ServiceSettlementState != expected || len(view.ApprovedPackets) != 0 {
				t.Fatal(view.Task, view.Billing, e)
			}
			if mode == "sent" {
				if _, _, e = r.Store.RunOriginalBills(ctx); e != nil {
					t.Fatal(e)
				}
				view, e = turns.GetTurn(ctx, scope, a.TurnID)
				if e != nil || view.Billing.ServiceSettlementState != "RISK_TRANSFERRED" {
					t.Fatal("invoice final erased historical customer risk transfer", view.Billing, e)
				}
			}
		})
	}
}

func TestCommittedTurnLateDebtKeepsOriginalCustomerSettlementProjection(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	if done, failed, e := r.Store.RunOriginalBills(ctx); e != nil || done != 3 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	view, e := r.Turns.GetTurn(ctx, scope, task)
	if e != nil || view.Task.TaskState != "COMMITTED" || view.Billing.ServiceSettlementState != "SETTLED" || len(view.ApprovedPackets) != 1 {
		t.Fatal(view.Task, view.Billing, e)
	}
}

func TestAutomaticBillItemUnknownCommitResolvesExactVersionReceipt(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	n := 0
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		n++
		if e := tx.Commit(); e != nil {
			return e
		}
		if n == 2 {
			return errors.New("synthetic original bill ACK lost")
		}
		return nil
	}
	done, failed, e := r.Store.RunOriginalBills(ctx)
	r.Store.Commit = nil
	if e != nil || done != 1 || failed != 0 || n != 3 {
		t.Fatal(done, failed, n, e)
	}
	done, failed, e = r.Store.RunOriginalBills(ctx)
	if e != nil || done != 0 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var matches int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_work w JOIN harness_bill_items i ON i.receipt_key=w.receipt_key AND i.item_hash=UNHEX(w.item_hash) WHERE w.done=1").Scan(&matches); e != nil || matches != 1 {
		t.Fatal(matches, e)
	}
}

func TestBillBackgroundConnectionRestoresOriginalSessionTimeout(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	r.Store.DB.SetMaxOpenConns(1)
	r.Store.DB.SetMaxIdleConns(1)
	if _, e := r.Store.DB.Exec("SET SESSION innodb_lock_wait_timeout=19"); e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	if done, failed, e := r.Store.RunOriginalBills(ctx); e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var restored int
	if e := r.Store.DB.QueryRow("SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&restored); e != nil || restored != 19 {
		t.Fatal("dedicated connection leaked session setting", restored, e)
	}
}
