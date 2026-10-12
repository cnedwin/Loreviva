package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"testing"
	"time"
)

type candidateCutoffOwner interface {
	CutoffDraftCandidate(context.Context, domain.ProductScope, string) (store.HarnessCutoffResult, error)
}

func TestProductCandidateUnsentRepairDoesNotRetainSupplierRisk(t *testing.T) {
	ctx := context.Background()
	r, l, _ := invalidCandidate(t)
	id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	extra, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, extra); e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, l.TaskID)
	if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalBillItem(ctx, id, "original-charge"); e != nil {
		t.Fatal(e)
	}
	var unresolved bool
	var settled, released int
	e = r.Store.DB.QueryRow("SELECT unresolved,(SELECT COUNT(*) FROM reservations WHERE task_id=? AND purpose=? AND status='SETTLED'),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND purpose=? AND status='RELEASED') FROM harness_risk_cases WHERE task_id=?", l.TaskID, id.AttemptID, l.TaskID, extra.AttemptID, l.TaskID).Scan(&unresolved, &settled, &released)
	if e != nil || unresolved || settled != 2 || released != 2 {
		t.Fatal("unsent repair invented cost obligation", unresolved, settled, released, e)
	}
	if send, e := r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker"); e == nil {
		if _, e = r.Store.StartSend(ctx, send, send.AttemptID); e == nil {
			t.Fatal("retired repair sent")
		}
	}
}

func TestProductCandidateCutoffOriginalOutcomes(t *testing.T) {
	for _, mode := range []string{"AUTHORIZED_UNSENT", "TWO_SENT", "COMMITTED_ACK", "ROLLED_BACK_ACK", "CUTOFF_ACK", "CUTOFF_ROLLBACK_ACK", "INFLIGHT_COMMIT"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			if mode == "AUTHORIZED_UNSENT" {
				r, l := candidateDispatchFixture(t)
				d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
					t.Fatal(e)
				}
				expireCutoff(t, r.Store.DB, l.TaskID)
				if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
					t.Fatal(e)
				}
				var released int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='RELEASED'", l.TaskID).Scan(&released); e != nil || released != 5 {
					t.Fatal(released, e)
				}
				if send, e := r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker"); e == nil {
					if _, e = r.Store.StartSend(ctx, send, send.AttemptID); e == nil {
						t.Fatal("retired authorized attempt sent")
					}
				}
				return
			}
			r, l, _ := validCandidateBusiness(t, mode == "TWO_SENT")
			if mode == "COMMITTED_ACK" || mode == "ROLLED_BACK_ACK" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					var e error
					if mode == "COMMITTED_ACK" {
						e = tx.Commit()
					} else {
						e = tx.Rollback()
					}
					if e != nil {
						return e
					}
					return errors.New("synthetic candidate business ACK lost")
				}
				_, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker")
				r.Store.Commit = nil
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			}
			if mode == "INFLIGHT_COMMIT" {
				entered, release := make(chan struct{}), make(chan struct{})
				result := make(chan error, 1)
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error { close(entered); <-release; return tx.Commit() }
				go func() { _, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); result <- e }()
				<-entered
				cutoff := make(chan error, 1)
				go func() {
					out, e := r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
					if e == nil && out.State != "COMMITTED" {
						e = errors.New("inflight commit lost")
					}
					cutoff <- e
				}()
				select {
				case e := <-cutoff:
					t.Fatal("cutoff bypassed real original locks", e)
				case <-time.After(100 * time.Millisecond):
				}
				close(release)
				if e := <-result; e != nil {
					t.Fatal(e)
				}
				r.Store.Commit = nil
				if e := <-cutoff; e != nil {
					t.Fatal(e)
				}
				return
			}
			expireCutoff(t, r.Store.DB, l.TaskID)
			if mode == "CUTOFF_ACK" || mode == "CUTOFF_ROLLBACK_ACK" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					var e error
					if mode == "CUTOFF_ROLLBACK_ACK" {
						e = tx.Rollback()
					} else {
						e = tx.Commit()
					}
					if e != nil {
						return e
					}
					return errors.New("synthetic cutoff ACK lost")
				}
			}
			out, e := r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
			r.Store.Commit = nil
			if mode == "CUTOFF_ACK" || mode == "CUTOFF_ROLLBACK_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				if mode == "CUTOFF_ROLLBACK_ACK" {
					var proofs, held int
					if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?),COUNT(*) FROM reservations WHERE task_id=? AND status='HELD'", l.TaskID, l.TaskID).Scan(&proofs, &held); e != nil || proofs != 0 || held != 5 {
						t.Fatal("rollback cutoff partial effects", proofs, held, e)
					}
				}
				out, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
			}
			want := "CUTOFF"
			if mode == "COMMITTED_ACK" {
				want = "COMMITTED"
			}
			if e != nil || out.State != want {
				t.Fatal(out, e)
			}
			var customer, held, services int
			e = r.Store.DB.QueryRow("SELECT SUM(kind IN ('customer','project') AND status='RELEASED'),SUM(kind IN ('supplier_global','supplier_tenant') AND status='HELD'),(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM reservations WHERE task_id=?", l.TaskID, l.TaskID).Scan(&customer, &held, &services)
			n := 2
			if mode == "TWO_SENT" {
				n = 4
			}
			if mode == "COMMITTED_ACK" {
				if e != nil || customer != 0 || held != 0 || services != 1 {
					t.Fatal(customer, held, services, e)
				}
			} else {
				if e != nil || customer != 2 || held != n || services != 0 {
					t.Fatal(customer, held, services, e)
				}
			}
		})
	}
}

func TestProductCandidateCutoffUsesTimeAfterRealTaskLock(t *testing.T) {
	ctx := context.Background()
	r, l, _, _ := productSent(t)
	if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 179 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 59 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", l.TaskID); e != nil {
		t.Fatal(e)
	}
	blocker, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	var task string
	if e = blocker.QueryRow("SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE", l.TaskID).Scan(&task); e != nil {
		t.Fatal(e)
	}
	result := make(chan error, 1)
	go func() {
		out, e := r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
		if e == nil && out.State != "CUTOFF" {
			e = errors.New("incorrect cutoff")
		}
		result <- e
	}()
	select {
	case e := <-result:
		t.Fatal("cutoff decided before lock", e)
	case <-time.After(1200 * time.Millisecond):
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	if e = <-result; e != nil {
		t.Fatal("deadline not rechecked after wait", e)
	}
}

func TestProductCandidateCommittedLateDebtIsIndependent(t *testing.T) {
	ctx := context.Background()
	r, l, _ := validCandidateBusiness(t, true)
	if _, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	var draft string
	if e := r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	prepared := []store.DispatchRequest{}
	for _, key := range []string{"late-already-authorized", "late-not-authorized"} {
		q := generated.CandidateRequest{ProductID: generated.Identity(l.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: "coffee-world", SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
		accepted, e := r.Store.AcceptDraftCandidate(ctx, l.ProductScope, draft, key, q)
		if e != nil {
			t.Fatal(e)
		}
		next, e := r.Store.ClaimRegistered(ctx, "debt-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE"}})
		if e != nil || next.TaskID != string(accepted.TaskID) {
			t.Fatal(next, e)
		}
		d, e := r.Store.PrepareDraftCandidateDispatch(ctx, next.TaskLease, "debt-worker")
		if e != nil {
			t.Fatal(e)
		}
		prepared = append(prepared, d)
	}
	if _, e := r.Store.AuthorizeDispatch(ctx, prepared[0]); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	done, failed, e := r.Store.RunOriginalBills(ctx)
	if e != nil || done != 4 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	r.Worker.Turns.Fault = harness.MockLateConflictingBillItem
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunOriginalBills(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var debt, risk, state string
	var services, lines, blocks, conflicts int
	e = r.Store.DB.QueryRow("SELECT CAST(l.debt_atoms AS CHAR),CAST(r.cost_atoms AS CHAR),e.state,(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?),(SELECT COUNT(*) FROM harness_account_blocks),(SELECT COUNT(*) FROM harness_bill_conflicts WHERE task_id=?) FROM harness_supplier_liabilities l JOIN harness_risk_cases r USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE l.task_id=? AND l.attempt_id=?", l.TaskID, l.TaskID, l.TaskID, l.TaskID, id.AttemptID).Scan(&debt, &risk, &state, &services, &lines, &blocks, &conflicts)
	if e != nil || debt != "3" || risk != "3" || state != "COMMITTED" || services != 1 || lines != 2 || blocks != 2 || conflicts != 1 {
		t.Fatal(debt, risk, state, services, lines, blocks, conflicts, e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, prepared[1]); e == nil {
		t.Fatal("Product authorization ignored supplier debt")
	}
	d := prepared[0]
	d.CommandKey = content.Identity(d.TaskID, "candidate-send")
	d.CommitKey = d.CommandKey
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
		t.Fatal("Product StartSend ignored supplier debt")
	}
	var global, tenant, negative int
	if e = r.Store.DB.QueryRow("SELECT SUM(kind='supplier_global' AND scope_key='GLOBAL' AND tenant_id IS NULL),SUM(kind='supplier_tenant' AND tenant_id=?),SUM(balance<held) FROM budget_accounts", l.TenantID).Scan(&global, &tenant, &negative); e != nil || global != 1 || tenant != 1 || negative != 0 {
		t.Fatal(global, tenant, negative, e)
	}
	if _, e = r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
		t.Fatal("late debt erased real committed candidate", e)
	}
}

func TestProductCandidateCutoffLockTimeoutHasDurableRetry(t *testing.T) {
	ctx := context.Background()
	r, l, _, _ := productSent(t)
	expireCutoff(t, r.Store.DB, l.TaskID)
	blocker, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	var task string
	if e = blocker.QueryRow("SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE", l.TaskID).Scan(&task); e != nil {
		t.Fatal(e)
	}
	bounded, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	start := time.Now()
	done, failed, e := r.Store.RunHarnessCutoffs(bounded)
	elapsed := time.Since(start)
	if e != nil || done != 0 || failed != 1 || elapsed > 5*time.Second {
		t.Fatal("item including failed continuation exceeded budget", done, failed, elapsed, e)
	}
	var failures int
	var doneFlag bool
	if e = r.Store.DB.QueryRow("SELECT failures,done FROM harness_cutoff_work WHERE task_id=?", l.TaskID).Scan(&failures, &doneFlag); e != nil || failures != 1 || doneFlag {
		t.Fatal(failures, doneFlag, e)
	}
	// The failed waiting statement must have rolled back the whole owning Tx.
	probeCtx, probeCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer probeCancel()
	probe, e := r.Store.DB.BeginTx(probeCtx, nil)
	if e != nil {
		t.Fatal(e)
	}
	var tenant string
	e = probe.QueryRowContext(probeCtx, "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE", l.TenantID).Scan(&tenant)
	probe.Rollback()
	if e != nil {
		t.Fatal("failed item retained preceding locks", e)
	}
	if e = blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_cutoff_work SET next_run_at=UTC_TIMESTAMP(6) WHERE task_id=?", l.TaskID); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal("original failed item not retried", done, failed, e)
	}
}

func TestProductCandidateSettlementCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"SERVICE_DISABLED", "SERVICE_MISSING", "INSTALLATION", "BINDING", "POST_JOURNAL", "WRITER"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, l, _, sent := productSent(t)
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, sent.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
			if e != nil {
				t.Fatal(e)
			}
			expireCutoff(t, r.Store.DB, l.TaskID)
			if mode == "POST_JOURNAL" {
				if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
					t.Fatal(e)
				}
				r.Store.ProviderEvidence = disableSettlementAfterBillRead{r.Local.Mock, r.Store.DB}
			} else {
				switch mode {
				case "SERVICE_DISABLED":
					_, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=0")
				case "SERVICE_MISSING":
					_, e = r.Store.DB.Exec("DELETE FROM harness_settlement_bindings WHERE task_id=?", l.TaskID)
					if e == nil {
						_, e = r.Store.DB.Exec("DELETE FROM harness_settlement_services")
					}
				case "INSTALLATION":
					_, e = r.Store.DB.Exec("UPDATE memory_installation SET generation=generation+1")
				case "BINDING":
					_, e = r.Store.DB.Exec("UPDATE harness_settlement_bindings SET policy='not-original' WHERE task_id=?", l.TaskID)
				case "WRITER":
					r.Store.SettlementAuthority = nil
				}
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e == nil {
					t.Fatal("invalid cutoff authority")
				}
			}
			if _, e = r.Store.RecordOriginalBillItem(ctx, id, "original-charge"); e == nil {
				t.Fatal("invalid bill authority")
			}
			var items int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?", l.TaskID).Scan(&items); e != nil || items != 0 {
				t.Fatal(items, e)
			}
			if mode == "SERVICE_DISABLED" {
				local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
				if e != nil {
					t.Fatal(e)
				}
				defer local.Participant.Close(ctx)
				var enabled bool
				if e = r.Store.DB.QueryRow("SELECT enabled FROM harness_settlement_services").Scan(&enabled); e != nil || enabled {
					t.Fatal("ordinary open reenabled service", enabled, e)
				}
			} else if mode == "SERVICE_MISSING" || mode == "INSTALLATION" {
				if _, e = app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath); e == nil {
					t.Fatal("ordinary open reseeded missing or stale authority")
				}
			}
		})
	}
}

func TestProductCandidateCanceledResetDiscardsDedicatedConnection(t *testing.T) {
	ctx := context.Background()
	r, l, _, _ := productSent(t)
	expireCutoff(t, r.Store.DB, l.TaskID)
	r.Store.DB.SetMaxOpenConns(1)
	r.Store.DB.SetMaxIdleConns(1)
	var before, after uint64
	if e := r.Store.DB.QueryRow("SELECT CONNECTION_ID()").Scan(&before); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("SET SESSION innodb_lock_wait_timeout=19"); e != nil {
		t.Fatal(e)
	}
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error { e := tx.Commit(); cancel(); return e }
	out, e := r.Store.CutoffDraftCandidate(canceled, l.ProductScope, l.TaskID)
	r.Store.Commit = nil
	if e != nil || out.State != "CUTOFF" {
		t.Fatal(out, e)
	}
	var timeout int
	if e = r.Store.DB.QueryRow("SELECT CONNECTION_ID(),@@SESSION.innodb_lock_wait_timeout").Scan(&after, &timeout); e != nil || after == before || timeout == 4 || timeout == 19 {
		t.Fatal("failed session restore returned polluted connection", before, after, timeout, e)
	}
}

func TestProductCandidateCutoffInterruptedContinuationIsNotSkipped(t *testing.T) {
	for _, mode := range []string{"EXHAUSTED_CONTEXT", "WORK_ROW_WAIT"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, l, _, _ := productSent(t)
			expireCutoff(t, r.Store.DB, l.TaskID)
			blocker, e := r.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			query := "SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE"
			if mode == "WORK_ROW_WAIT" {
				query = "SELECT task_id FROM harness_cutoff_work WHERE task_id=? FOR UPDATE"
			}
			var task string
			if e = blocker.QueryRow(query, l.TaskID).Scan(&task); e != nil {
				t.Fatal(e)
			}
			duration := 8 * time.Second
			if mode == "EXHAUSTED_CONTEXT" {
				duration = 60 * time.Millisecond
			}
			bounded, cancel := context.WithTimeout(ctx, duration)
			defer cancel()
			start := time.Now()
			done, _, e := r.Store.RunHarnessCutoffs(bounded)
			if e == nil || done != 0 || time.Since(start) > 5500*time.Millisecond {
				t.Fatal("interrupted item boundary", done, time.Since(start), e)
			}
			if e = blocker.Rollback(); e != nil {
				t.Fatal(e)
			}
			var complete bool
			var proofs, held int
			if e = r.Store.DB.QueryRow("SELECT done,(SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD') FROM harness_cutoff_work WHERE task_id=?", l.TaskID, l.TaskID, l.TaskID).Scan(&complete, &proofs, &held); e != nil || complete || proofs != 0 || held != 5 {
				t.Fatal("partial cutoff or skipped work", complete, proofs, held, e)
			}
			done, failed, e := r.Store.RunHarnessCutoffs(ctx)
			if e != nil || done != 1 || failed != 0 {
				t.Fatal("unchanged original due item lost", done, failed, e)
			}
		})
	}
}

func TestProductCandidateBillScanAckAndSessionRestoration(t *testing.T) {
	for _, mode := range []string{"SCAN_ACK", "ITEM_ACK", "SESSION"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, l, _, sent := productSent(t)
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, sent.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			expireCutoff(t, r.Store.DB, l.TaskID)
			if _, e := r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
				t.Fatal(e)
			}
			commits := 0
			if mode == "SESSION" {
				r.Store.DB.SetMaxOpenConns(1)
				r.Store.DB.SetMaxIdleConns(1)
				if _, e := r.Store.DB.Exec("SET SESSION innodb_lock_wait_timeout=19"); e != nil {
					t.Fatal(e)
				}
			} else {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					commits++
					if e := tx.Commit(); e != nil {
						return e
					}
					if (mode == "SCAN_ACK" && commits == 1) || (mode == "ITEM_ACK" && commits == 2) {
						return errors.New("synthetic original bill scan/item ACK lost")
					}
					return nil
				}
			}
			done, failed, e := r.Store.RunOriginalBills(ctx)
			r.Store.Commit = nil
			if mode == "SCAN_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				done, failed, e = r.Store.RunOriginalBills(ctx)
			}
			if e != nil || done != 1 || failed != 0 {
				t.Fatal(done, failed, e)
			}
			done, failed, e = r.Store.RunOriginalBills(ctx)
			if e != nil || done != 0 || failed != 0 {
				t.Fatal("original cursor/receipt redelivered", done, failed, e)
			}
			var matches int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_work w JOIN harness_bill_items i ON i.receipt_key=w.receipt_key AND i.item_hash=UNHEX(w.item_hash) WHERE w.done=1 AND i.task_id=?", l.TaskID).Scan(&matches); e != nil || matches != 1 {
				t.Fatal(matches, e)
			}
			if mode == "SESSION" {
				var timeout int
				if e = r.Store.DB.QueryRow("SELECT @@SESSION.innodb_lock_wait_timeout").Scan(&timeout); e != nil || timeout != 19 {
					t.Fatal(timeout, e)
				}
			}
		})
	}
}

func TestProductCandidateActualCutoffPageIsBounded(t *testing.T) {
	ctx := context.Background()
	r, l := candidateDispatchFixture(t)
	var draft string
	if e := r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 100; n++ {
		q := generated.CandidateRequest{ProductID: generated.Identity(l.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: "coffee-world", SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
		if _, e := r.Store.AcceptDraftCandidate(ctx, l.ProductScope, draft, fmt.Sprintf("page-candidate-%03d", n), q); e != nil {
			t.Fatal(n, e)
		}
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 181 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 181 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 181 SECOND)"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_cutoff_work SET next_run_at=UTC_TIMESTAMP(6)"); e != nil {
		t.Fatal(e)
	}
	done, failed, e := r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 100 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	done, failed, e = r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var receipts, released, world int
	e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts WHERE operation='HARNESS_CUTOFF' AND scope_kind='PRODUCT' AND world_instance_id IS NULL),SUM(status='RELEASED'),SUM(kind='world') FROM reservations").Scan(&receipts, &released, &world)
	if e != nil || receipts != 101 || released != 303 || world != 0 {
		t.Fatal(receipts, released, world, e)
	}
}

func TestProductCandidateActualBackgroundSettlementWorker(t *testing.T) {
	ctx := context.Background()
	r, l, _, sent := productSent(t)
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, sent.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, l.TaskID)
	if e := r.Worker.Start(ctx); e != nil {
		t.Fatal(e)
	}
	defer r.Worker.Stop(ctx)
	deadline := time.Now().Add(8 * time.Second)
	for {
		var bills, cutoff int
		e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?),(SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?)", l.TaskID, l.TaskID).Scan(&bills, &cutoff)
		if e != nil {
			t.Fatal(e)
		}
		if bills == 1 && cutoff == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registered accounting worker did not finish original Product work", bills, cutoff)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if e := r.Worker.Stop(ctx); e != nil {
		t.Fatal(e)
	}
	var calls, services int
	var state string
	if e := r.Store.DB.QueryRow("SELECT t.calls_used,e.state,(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID, l.TaskID).Scan(&calls, &state, &services); e != nil || calls != 1 || state != "CUTOFF" || services != 0 {
		t.Fatal(calls, state, services, e)
	}
}

func TestProductCandidateUnknownCutoffKeepsOriginalSupplier(t *testing.T) {
	ctx := context.Background()
	r, l := candidateDispatchFixture(t)
	d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(l.TaskID, "candidate-send")
	d.CommitKey = d.CommandKey
	sent, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	// A known marker with no Generate remains genuinely UNKNOWN, not zero cost.
	before := journalBytes(t, r)
	owner, ok := any(r.Store).(candidateCutoffOwner)
	if !ok {
		t.Fatal("actual Product cutoff owning path unavailable")
	}
	if _, e = owner.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e == nil {
		t.Fatal("early cutoff")
	}
	expireCutoff(t, r.Store.DB, l.TaskID)
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET enabled=0 WHERE subject_id=?", l.SubjectID); e != nil {
		t.Fatal(e)
	}
	out, e := owner.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
	if e != nil || out.State != "CUTOFF" {
		t.Fatal(out, e)
	}
	var customer, supplier, risk, business, world, receipt int
	e = r.Store.DB.QueryRow("SELECT SUM(kind IN ('customer','project') AND status='RELEASED'),SUM(kind IN ('supplier_global','supplier_tenant') AND status='HELD'),SUM(kind='platform_risk' AND status='RISK'),(SELECT COUNT(*) FROM harness_candidate_commits WHERE task_id=?),SUM(kind='world'),(SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND operation='HARNESS_CUTOFF' AND scope_kind='PRODUCT' AND world_instance_id IS NULL) FROM reservations WHERE task_id=?", l.TaskID, l.TaskID, l.TaskID).Scan(&customer, &supplier, &risk, &business, &world, &receipt)
	if e != nil || customer != 2 || supplier != 2 || risk != 1 || business != 0 || world != 0 || receipt != 1 {
		t.Fatal(customer, supplier, risk, business, world, receipt, e)
	}
	again, e := owner.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID)
	if e != nil || again != out {
		t.Fatal(again, e)
	}
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, sent.OriginalSend()); e == nil {
		t.Fatal("retired opaque permit sent")
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cutoff touched J/provider")
	}
}

func TestProductCandidateCutoffAndBillActualScanners(t *testing.T) {
	for _, mode := range []string{"CUTOFF_SCAN", "LATE_BILL", "BILL_SCAN", "SOURCE_DELETE"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, l, _, sent := productSent(t)
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, sent.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "SOURCE_DELETE" {
				var draft string
				if e = r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
					t.Fatal(e)
				}
				impact, e := r.Content.Impact(ctx, l.ProductScope, draft, 1, "DELETE", "cost-source-impact")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Content.Command(ctx, l.ProductScope, draft, 1, "DELETE", string(impact.ReportID), "", "", "cost-source-delete"); e != nil {
					t.Fatal(e)
				}
				var upper uint64
				var cutoff time.Time
				if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 24 HOUR) FROM file_staging").Scan(&upper, &cutoff); e != nil {
					t.Fatal(e)
				}
				page, e := r.Files.CleanupPage(ctx, l.ProductScope, cutoff, 0, upper)
				if e != nil || page.Removed != 1 {
					t.Fatal("actual source/body purge", page, e)
				}
			}
			expireCutoff(t, r.Store.DB, l.TaskID)
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action IN ('content.assist','content.read','asset.read')"); e != nil {
				t.Fatal(e)
			}
			if mode == "CUTOFF_SCAN" {
				done, failed, e := r.Store.RunHarnessCutoffs(ctx)
				if e != nil || done != 1 || failed != 0 {
					t.Fatal("actual Product cutoff scan", done, failed, e)
				}
				return
			}
			if _, e = r.Store.CutoffDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
				t.Fatal(e)
			}
			before := journalBytes(t, r)
			if mode == "LATE_BILL" {
				receipt, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
				if e != nil {
					t.Fatal("actual Product original bill", e)
				}
				again, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
				if e != nil || again != receipt {
					t.Fatal(again, e)
				}
			} else {
				done, failed, e := r.Store.RunOriginalBills(ctx)
				if e != nil || done != 1 || failed != 0 {
					t.Fatal("actual Product original bill scan", done, failed, e)
				}
			}
			var supplier, customer, service, items int
			e = r.Store.DB.QueryRow("SELECT SUM(kind IN ('supplier_global','supplier_tenant') AND status='SETTLED'),SUM(kind IN ('customer','project') AND status='RELEASED'),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?) FROM reservations WHERE task_id=?", l.TaskID, l.TaskID, l.TaskID).Scan(&supplier, &customer, &service, &items)
			if e != nil || supplier != 2 || customer != 2 || service != 0 || items != 1 {
				t.Fatal(supplier, customer, service, items, e)
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("settlement mutated original J")
			}
			if _, e = r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID); e == nil {
				t.Fatal("cost granted body")
			}
		})
	}
}
