package integration

import (
	"bytes"
	"context"
	"loreviva.local/loreviva/internal/harness"
	"testing"
)

func TestTurnWorkerProducesOriginalGovernedResult(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "worker-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "worker-turn")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal("registered TURN worker did not run original Generate", e)
	}
	var state string
	if e = r.Store.DB.QueryRow("SELECT state FROM attempts WHERE task_id=?", accepted.TurnID).Scan(&state); e != nil || state != "SUCCEEDED" {
		t.Fatal("no actual original completed provider result", state, e)
	}
	var task string
	if e = r.Store.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", accepted.TurnID).Scan(&task); e != nil || task != "COMMITTED" {
		t.Fatal("worker did not finish program-approved world commit", task, e)
	}
}

func TestOrdinaryWorkerCommitsOrRepairsProgramCandidate(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "real-structure-repair"}[repair], func(t *testing.T) {
			ctx := context.Background()
			r, scope, turns, qr, request := actualTurnFixture(t, true)
			quote, e := turns.QuoteTurn(ctx, scope, qr, "automatic-quote")
			if e != nil {
				t.Fatal(e)
			}
			request.QuoteID = quote.QuoteID
			accepted, e := turns.SubmitTurn(ctx, scope, request, "automatic-turn")
			if e != nil {
				t.Fatal(e)
			}
			if repair {
				r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			}
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal("ordinary worker did not complete", e)
			}
			var state string
			var calls, steps, service int
			if e = r.Store.DB.QueryRow("SELECT t.state,t.calls_used,e.steps_used,(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", accepted.TurnID, accepted.TurnID).Scan(&state, &calls, &steps, &service); e != nil {
				t.Fatal(e)
			}
			wantCalls, wantSteps := 1, 9
			if repair {
				wantCalls, wantSteps = 2, 14
			}
			if state != "COMMITTED" || calls != wantCalls || steps != wantSteps || service != 1 {
				t.Fatal("worker stopped before actual local completion", state, calls, steps, service)
			}
			if body, _, e := r.Files.ReadApprovedTurn(ctx, scope, accepted.TurnID); e != nil || len(body) == 0 {
				t.Fatal("worker result unavailable", e)
			}
		})
	}
}

func TestWorkerAutomaticallyRecomputesOnlyActualConflict(t *testing.T) {
	for _, related := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated-revision-local-rebase", true: "actual-related-action-recompute"}[related], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t)
			var original []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=?", task).Scan(&original); e != nil {
				t.Fatal(e)
			}
			before := journalBytes(t, r)
			if related {
				instance, e := r.World.GetInstance(ctx, scope)
				if e != nil {
					t.Fatal(e)
				}
				br, q := actionTask(t, r, scope.ProductScope, instance, "automatic-competing-action")
				if _, e = commitAction(r, br, q); e != nil {
					t.Fatal(e)
				}
			} else {
				// A synthetic unrelated revision change has no referenced resource delta.
				if _, e := r.Store.DB.Exec("UPDATE world_controls SET world_revision=world_revision+1 WHERE world_instance_id=?", scope.WorldInstanceID); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
				t.Fatal(e)
			}
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal("actual worker did not finish", e)
			}
			var state string
			var calls, steps, recoveries, services int
			if e := r.Store.DB.QueryRow("SELECT t.state,t.calls_used,e.steps_used,(SELECT COUNT(*) FROM harness_recovery_approvals WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task).Scan(&state, &calls, &steps, &recoveries, &services); e != nil {
				t.Fatal(e)
			}
			wc, ws, wr := 1, 9, 0
			if related {
				wc, ws, wr = 2, 16, 1
			}
			if state != "COMMITTED" || calls != wc || steps != ws || recoveries != wr || services != 1 {
				t.Fatal(state, calls, steps, recoveries, services)
			}
			var after []byte
			if e := r.Store.DB.QueryRow("SELECT c.document FROM harness_attempt_contexts c JOIN attempts a USING(tenant_id,product_id,attempt_id) WHERE c.task_id=? AND a.slot=0", task).Scan(&after); e != nil || !bytes.Equal(original, after) {
				t.Fatal("original snapshot changed", e)
			}
			if !related && !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("unrelated revision caused provider purchase")
			}
			if body, _, e := r.Files.ReadApprovedTurn(ctx, scope, task); e != nil || len(body) == 0 {
				t.Fatal("terminal result unavailable", e)
			}
		})
	}
}
