package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func TestActualDeadlineReleasesCustomerButRetainsOriginalSupplierRisk(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e == nil {
		t.Fatal("cutoff before original deadline")
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 181 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 181 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 181 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET enabled=0 WHERE subject_id=?", scope.SubjectID); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	out, e := r.Store.CutoffHarnessTask(ctx, scope, task)
	if e != nil {
		t.Fatal("actual cutoff unavailable", e)
	}
	if out.State != "CUTOFF" || out.RiskCaseID == "" || out.ReceiptKey == "" {
		t.Fatal(out)
	}
	var released, supplier, risk, commits int
	if e = r.Store.DB.QueryRow("SELECT SUM(kind IN ('customer','project','world') AND status='RELEASED'),SUM(kind IN ('supplier_global','supplier_tenant') AND status='HELD'),SUM(kind='platform_risk' AND status='RISK'),(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?) FROM reservations WHERE task_id=?", task, task).Scan(&released, &supplier, &risk, &commits); e != nil || released != 3 || supplier != 2 || risk != 1 || commits != 0 {
		t.Fatal(released, supplier, risk, commits, e)
	}
	again, e := r.Store.CutoffHarnessTask(ctx, scope, task)
	if e != nil || again != out {
		t.Fatal("cutoff original replay", again, e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("cutoff queried/generated provider")
	}
}

func expireCutoff(t *testing.T, db *sql.DB, task string) {
	t.Helper()
	if _, e := db.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 181 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 181 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 181 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("UPDATE harness_cutoff_work SET next_run_at=UTC_TIMESTAMP(6) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
}
func TestCutoffProvesNoWorldCommitWithoutInventingPendingRollback(t *testing.T) {
	for _, mode := range []string{"prepare-approval", "prepare-world", "world-rollback-ACK-unknown"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, false)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if mode != "prepare-approval" {
				if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
					t.Fatal(e)
				}
			}
			r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
				if mode == "world-rollback-ACK-unknown" {
					var services int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
						return e
					}
					if services == 0 {
						return tx.Commit()
					}
					if e := tx.Rollback(); e != nil {
						return e
					}
				} else {
					if e := tx.Commit(); e != nil {
						return e
					}
				}
				return errors.New("synthetic exact local ACK lost")
			}
			var e error
			if mode == "prepare-approval" {
				e = r.Store.ApproveOriginalTurn(ctx, lease, worker)
			} else {
				_, e = r.Store.CommitOriginalTurn(ctx, lease, worker)
			}
			r.Store.Commit = nil
			if !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal(e)
			}
			expireCutoff(t, r.Store.DB, task)
			before := journalBytes(t, r)
			out, e := r.Store.CutoffHarnessTask(ctx, scope, task)
			if e != nil || out.State != "CUTOFF" {
				t.Fatal(out, e)
			}
			var pending, released, commits, generation int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=? AND state='PENDING'),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND kind IN ('customer','project','world') AND status='RELEASED'),(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),narrative_generation FROM harness_envelopes WHERE task_id=?", task, task, task, task).Scan(&pending, &released, &commits, &generation); e != nil || pending != 1 || released != 3 || commits != 0 || generation != 2 {
				t.Fatal(pending, released, commits, generation, e)
			}
			if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); e == nil {
				t.Fatal("retired original permit resumed world commit")
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("cutoff touched provider")
			}
		})
	}
}
func TestCutoffAckUnknownAndConcurrentReplayHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic cutoff ACK lost")
	}
	original, e := r.Store.CutoffHarnessTask(ctx, scope, task)
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	results := make(chan store.HarnessCutoffResult, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { v, e := r.Store.CutoffHarnessTask(ctx, scope, task); results <- v; errs <- e }()
	}
	for i := 0; i < 2; i++ {
		if v := <-results; v != original {
			t.Fatal(v, original)
		}
		if e = <-errs; e != nil {
			t.Fatal(e)
		}
	}
	var n, g int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM postings WHERE task_id=?),narrative_generation FROM harness_envelopes WHERE task_id=?", task, task).Scan(&n, &g); e != nil || n != 4 || g != 2 {
		t.Fatal(n, g, e)
	}
}
func TestCutoffWaitsForRealWorldCommitAndPreservesItsSettlement(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	worldDone := make(chan error, 1)
	r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			return tx.Commit()
		}
		close(entered)
		<-release
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic actual committed world ACK lost")
	}
	go func() { _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); worldDone <- e }()
	<-entered
	cutoffDone := make(chan error, 1)
	go func() {
		v, e := r.Store.CutoffHarnessTask(ctx, scope, task)
		if e == nil && v.State != "COMMITTED" {
			e = errors.New("wrong winner")
		}
		cutoffDone <- e
	}()
	select {
	case e := <-cutoffDone:
		t.Fatal("cutoff did not wait for original world transaction", e)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if e := <-worldDone; !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	if e := <-cutoffDone; e != nil {
		t.Fatal(e)
	}
	r.Store.Commit = nil
	var risks, service, postings int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?)", task, task, task).Scan(&risks, &service, &postings); e != nil || risks != 0 || service != 1 || postings != 6 {
		t.Fatal(risks, service, postings, e)
	}
}
func TestCutoffQueueRetainsFailureThenRetriesOriginal(t *testing.T) {
	ctx := context.Background()
	r, _, task, _ := originalResultFixture(t, true)
	expireCutoff(t, r.Store.DB, task)
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic local cutoff rollback") }
	done, failed, e := r.Store.RunHarnessCutoffs(ctx)
	r.Store.BeforeCommit = nil
	if e != nil || done != 0 || failed != 1 {
		t.Fatal(done, failed, e)
	}
	var failures, complete, risks int
	var retry bool
	if e = r.Store.DB.QueryRow("SELECT failures,done,next_run_at>UTC_TIMESTAMP(6),(SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?) FROM harness_cutoff_work WHERE task_id=?", task, task).Scan(&failures, &complete, &retry, &risks); e != nil || failures != 1 || complete != 0 || !retry || risks != 0 {
		t.Fatal(failures, complete, retry, risks, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_cutoff_work SET next_run_at=UTC_TIMESTAMP(6) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
}

func TestCutoffCurrentServiceAndInstallationAreRequired(t *testing.T) {
	for _, mode := range []string{"disabled", "missing-binding", "old-installation-generation"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, true)
			expireCutoff(t, r.Store.DB, task)
			q := "UPDATE harness_settlement_services SET enabled=0"
			if mode == "missing-binding" {
				q = "DELETE FROM harness_settlement_bindings"
			}
			if mode == "old-installation-generation" {
				q = "UPDATE memory_installation SET generation=generation+1"
			}
			if _, e := r.Store.DB.Exec(q); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.CutoffHarnessTask(ctx, scope, task); e == nil {
				t.Fatal("missing current service permitted settlement")
			}
			var holds, proof int
			if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD'),(SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?)", task, task).Scan(&holds, &proof); e != nil || holds != 6 || proof != 0 {
				t.Fatal(holds, proof, e)
			}
		})
	}
}
func TestCutoffUsesTimeAfterWaitingForWorldLock(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	blocker, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	var world string
	if e = blocker.QueryRow("SELECT world_instance_id FROM world_controls WHERE world_instance_id=? FOR UPDATE", scope.WorldInstanceID).Scan(&world); e != nil {
		t.Fatal(e)
	}
	// Frozen intervals remain 120/180; only the synthetic accepted clock is shifted.
	if _, e = blocker.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 300000 MICROSECOND),INTERVAL 180 SECOND),execute_by=DATE_SUB(DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 300000 MICROSECOND),INTERVAL 60 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 300000 MICROSECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := r.Store.CutoffHarnessTask(ctx, scope, task); done <- e }()
	select {
	case e = <-done:
		t.Fatal("cutoff bypassed actual world lock", e)
	case <-time.After(450 * time.Millisecond):
	}
	if e = blocker.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	var valid bool
	if e = r.Store.DB.QueryRow("SELECT c.settled_at>=e.settle_by AND TIMESTAMPDIFF(SECOND,e.accepted_at,e.settle_by)=180 FROM harness_cutoffs c JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE task_id=?", task).Scan(&valid); e != nil || !valid {
		t.Fatal(valid, e)
	}
}
func TestManagedWorldCommitRepeatsDeadlineImmediatelyBeforeCommit(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 2 SECOND),INTERVAL 120 SECOND),execute_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 2 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 62 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&n); e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
		_, e := tx.ExecContext(ctx, "DO SLEEP(2.1)")
		return e
	}
	_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
	r.Store.BeforeCommit = nil
	if e == nil {
		t.Fatal("deadline crossing committed narrative")
	}
	var commits, service, holds int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD')", task, task, task).Scan(&commits, &service, &holds); e != nil || commits != 0 || service != 0 || holds != 6 {
		t.Fatal(commits, service, holds, e)
	}
}

func TestCutoffParentDoesNotRetireAnotherLiveTurnInSameWorld(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	submit := func(key string) string {
		q, e := turns.QuoteTurn(ctx, scope, qr, key+"-q")
		if e != nil {
			t.Fatal(e)
		}
		request.QuoteID = q.QuoteID
		a, e := turns.SubmitTurn(ctx, scope, request, key)
		if e != nil {
			t.Fatal(e)
		}
		return a.TurnID
	}
	first, second := submit("parent-cutoff"), submit("other-live-parent")
	if e := runOriginalPrefix(ctx, r); e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, first)
	if _, e := r.Store.CutoffHarnessTask(ctx, scope, first); e != nil {
		t.Fatal(e)
	}
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal("independent valid parent retired", e)
	}
	var state string
	var generation, calls, steps int
	if e := r.Store.DB.QueryRow("SELECT state,narrative_generation,calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", second).Scan(&state, &generation, &calls, &steps); e != nil || state != "COMMITTED" || generation != 1 || calls != 1 || steps != 9 {
		t.Fatal(state, generation, calls, steps, e)
	}
}
func TestSettlementServiceClosureBlocksBothNewSendBoundaries(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	q, e := turns.QuoteTurn(ctx, scope, qr, "settle-service-q")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = q.QuoteID
	a, e := turns.SubmitTurn(ctx, scope, request, "settle-service-turn")
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "service-gates", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN"}})
	if e != nil || lease.TaskID != a.TurnID {
		t.Fatal(e)
	}
	d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "service-gates")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e == nil {
		t.Fatal("disabled settlement obligations bought a call")
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=1"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(d.TaskID, "send")
	d.CommitKey = d.CommandKey
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
		t.Fatal("disabled settlement obligations sent a call")
	}
	var markers int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=? AND send_started_at IS NOT NULL", a.TurnID).Scan(&markers); e != nil || markers != 0 {
		t.Fatal(markers, e)
	}
}

func TestCutoffPassProcessesOnlyOneHundredRealAcceptedParents(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	for i := 0; i < 101; i++ {
		key := fmt.Sprintf("cutoff-page-%03d", i)
		q, e := turns.QuoteTurn(ctx, scope, qr, key+"-q")
		if e != nil {
			t.Fatal(i, e)
		}
		request.QuoteID = q.QuoteID
		if _, e = turns.SubmitTurn(ctx, scope, request, key); e != nil {
			t.Fatal(i, e)
		}
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 181 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 181 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 181 SECOND)"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE harness_cutoff_work SET next_run_at=UTC_TIMESTAMP(6)"); e != nil {
		t.Fatal(e)
	}
	started := time.Now()
	done, failed, e := r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 100 || failed != 0 || time.Since(started) > 30*time.Second {
		t.Fatal(done, failed, e, time.Since(started))
	}
	done, failed, e = r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	var finished, attempts, held, unresolved int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_cutoff_work WHERE done=1),(SELECT COUNT(*) FROM attempts),(SELECT COUNT(*) FROM reservations WHERE status<>'RELEASED'),(SELECT COUNT(*) FROM harness_risk_cases WHERE unresolved=1)").Scan(&finished, &attempts, &held, &unresolved); e != nil || finished != 101 || attempts != 0 || held != 0 || unresolved != 0 {
		t.Fatal(finished, attempts, held, unresolved, e)
	}
}
