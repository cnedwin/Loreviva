package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/generated"
)

// The shared dedicated-connection primitive does not make the three owning
// work loops equivalent. Exercise cutoff and bill delivery separately.
func TestOriginalSettlementWholePassAndFailedWork(t *testing.T) {
	for _, mode := range []string{"CUTOFF", "BILL"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, req := candidateHTTPFixture(t)
			var tasks []string
			for n := 0; n < 8; n++ {
				key := fmt.Sprintf("slow-%s-%d", mode, n)
				if mode == "BILL" {
					id := addPendingCandidateMeter(t, r, draft, req, key, false)
					tasks = append(tasks, id.TaskID)
				} else {
					var accepted generated.AdminTaskReceipt
					runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", key, req, 202, &accepted)
					tasks = append(tasks, string(accepted.TaskID))
					expireCutoff(t, r.Store.DB, string(accepted.TaskID))
				}
			}
			blocker, e := r.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer blocker.Rollback()
			for _, task := range tasks {
				var id string
				if e = blocker.QueryRow("SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE", task).Scan(&id); e != nil {
					t.Fatal(e)
				}
			}
			run := r.Store.RunHarnessCutoffs
			work := "harness_cutoff_work"
			if mode == "BILL" {
				run, work = r.Store.RunOriginalBills, "harness_bill_work"
			}
			start := time.Now()
			done, failed, runErr := run(ctx)
			elapsed := time.Since(start)
			t.Log("actual whole pass including cleanup and failed-row recording", elapsed, done, failed, runErr)
			if elapsed > 31*time.Second || done != 0 || failed < 1 || !errors.Is(runErr, context.DeadlineExceeded) {
				t.Fatal("whole owning pass bound", done, failed, elapsed, runErr)
			}
			// A failure-record item may exhaust its own five seconds and return
			// before the whole-pass limit. Do not require it to wait thirty seconds.
			if mode == "BILL" && (elapsed < 28*time.Second || failed < 7) {
				t.Fatal("bill fixture did not reach whole-pass boundary", elapsed, failed)
			}
			if e = blocker.Rollback(); e != nil {
				t.Fatal(e)
			}
			var total, complete, untouched, financial int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*),SUM(done),SUM(failures=0) FROM "+work).Scan(&total, &complete, &untouched); e != nil || total != 8 || complete != 0 || untouched < 1 {
				t.Fatal("unpersisted failure skipped original work", total, complete, untouched, e)
			}
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_cutoffs)+(SELECT COUNT(*) FROM harness_bill_items)").Scan(&financial); e != nil || financial != 0 {
				t.Fatal("partial financial proof", financial, e)
			}
			if _, e = r.Store.DB.Exec("UPDATE " + work + " SET next_run_at=UTC_TIMESTAMP(6)"); e != nil {
				t.Fatal(e)
			}
			if mode == "CUTOFF" {
				// Actual owning transactions stay open while their commit callback
				// models slow local acknowledgment. Two commits per original item
				// include its work record in the same thirty-second pass budget.
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					timer := time.NewTimer(2 * time.Second)
					defer timer.Stop()
					select {
					case <-c.Done():
						return c.Err()
					case <-timer.C:
						return tx.Commit()
					}
				}
				t.Cleanup(func() { r.Store.Commit = nil })
				start = time.Now()
				done, failed, runErr = run(ctx)
				elapsed = time.Since(start)
				r.Store.Commit = nil
				t.Log("actual cutoff pass with slow owning commits and work records", elapsed, done, failed, runErr)
				if elapsed < 28*time.Second || elapsed > 31*time.Second || runErr == nil || done < 6 {
					t.Fatal("cutoff whole-pass boundary", elapsed, done, failed, runErr)
				}
				if e = r.Store.DB.QueryRow("SELECT SUM(done) FROM harness_cutoff_work").Scan(&complete); e != nil || complete >= 8 {
					t.Fatal("unfinished work falsely confirmed", complete, e)
				}
			}
			done, failed, e = run(ctx)
			if e != nil || done != 8-complete || failed != 0 {
				t.Fatal("original work not recovered", done, failed, e)
			}
			if e = r.Store.DB.QueryRow("SELECT SUM(done) FROM " + work).Scan(&complete); e != nil || complete != 8 {
				t.Fatal(complete, e)
			}
			done, failed, e = run(ctx)
			if e != nil || done != 0 || failed != 0 {
				t.Fatal("original queue replay duplicated", done, failed, e)
			}
		})
	}
}
