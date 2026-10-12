package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
)

func TestNonfinalSupplierRiskCannotBeReleasedAtCommitFinish(t *testing.T) {
	for _, mode := range []string{"RISK", "SUPPLIER", "PURPOSE", "AMOUNT", "POSTING"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, req := candidateHTTPFixture(t)
			var a generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "nonfinal-finish", req, 202, &a)
			r.Worker.Turns.Fault = harness.MockCostFinalityPending
			injected := false
			r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
				var n int
				if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM service_events WHERE task_id=?", a.TaskID).Scan(&n); e != nil {
					return e
				}
				if n == 0 {
					return nil
				}
				injected = true
				query := "UPDATE reservations SET status='RELEASED' WHERE task_id=? AND kind='platform_risk'"
				switch mode {
				case "SUPPLIER":
					query = "UPDATE reservations SET status='SETTLED' WHERE task_id=? AND kind='supplier_global'"
				case "PURPOSE":
					query = "UPDATE reservations SET purpose='other-attempt' WHERE task_id=? AND kind='supplier_tenant'"
				case "AMOUNT":
					query = "UPDATE reservations SET amount=1 WHERE task_id=? AND kind='supplier_tenant'"
				case "POSTING":
					query = "UPDATE postings SET amount=1 WHERE task_id=? AND kind='customer'"
				}
				_, e := tx.ExecContext(c, query, a.TaskID)
				return e
			}
			e := r.Worker.Once(ctx)
			r.Store.BeforeCommit = nil
			if !injected || e == nil {
				t.Fatal("original obligation changed at business finish", mode, injected, e)
			}
			var count int
			for _, table := range []string{"service_events", "harness_service_lines", "harness_candidate_commits", "postings"} {
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", a.TaskID).Scan(&count); e != nil || count != 0 {
					t.Fatal("partial business survived", table, count, e)
				}
			}
		})
	}
}

func TestKnownUnitsNonfinalSupplierCommitsOrdinaryNarrative(t *testing.T) {
	for _, kind := range []string{"TURN", "CANDIDATE"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var r *app.Runtime
			var task string
			if kind == "TURN" {
				var scope domain.Scope
				var turns *harness.TurnService
				var qr generated.QuoteRequest
				var req generated.TurnRequest
				r, scope, turns, qr, req = actualTurnFixture(t, true)
				quote, e := turns.QuoteTurn(ctx, scope, qr, "nonfinal-quote")
				if e != nil {
					t.Fatal(e)
				}
				req.QuoteID = quote.QuoteID
				a, e := turns.SubmitTurn(ctx, scope, req, "nonfinal-turn")
				if e != nil {
					t.Fatal(e)
				}
				task = string(a.TurnID)
			} else {
				var draft string
				var req generated.CandidateRequest
				r, draft, req = candidateHTTPFixture(t)
				var a generated.AdminTaskReceipt
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "nonfinal-candidate", req, 202, &a)
				task = string(a.TaskID)
			}
			r.Worker.Turns.Fault = harness.MockCostFinalityPending
			workerErr := r.Worker.Once(ctx)
			var raw []byte
			var meta domain.MockResultMetadata
			if e := r.Store.DB.QueryRow("SELECT metadata FROM harness_original_queries WHERE task_id=?", task).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			if e := json.Unmarshal(raw, &meta); e != nil || !meta.UsageKnown || meta.NoFurtherCost {
				t.Fatal("not actual known-unit nonfinal result", meta.UsageKnown, meta.NoFurtherCost, e)
			}
			if workerErr != nil {
				t.Fatal("known narrative blocked on supplier finality", workerErr)
			}
			var taskState, serviceState string
			var calls, steps, held, settled, lines int
			if e := r.Store.DB.QueryRow("SELECT t.state,s.status,e.calls_allocated,e.steps_used,(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD'),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='SETTLED'),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?) FROM tasks t JOIN service_events s USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task, task).Scan(&taskState, &serviceState, &calls, &steps, &held, &settled, &lines); e != nil {
				t.Fatal(e)
			}
			wantSettled := 2
			if kind == "TURN" {
				wantSettled = 3
			}
			if taskState != "COMMITTED" || serviceState != "COMMITTED" || calls != 1 || steps != 9 || held != 3 || settled != wantSettled || lines != 1 {
				t.Fatal("narrative/service/supplier axes conflated", taskState, serviceState, calls, steps, held, settled, lines)
			}
			r.Worker.Turns.Fault = harness.MockLateFinalZeroBill
			if e := r.Worker.Turns.DeliverLateMockBills(ctx, meta.Identity); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.RecordOriginalBillItem(ctx, meta.Identity, "late-final"); e != nil {
				t.Fatal(e)
			}
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD'", task).Scan(&held); e != nil || held != 0 {
				t.Fatal("actual final bill retained unknown obligation", held, e)
			}
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&lines); e != nil || lines != 1 {
				t.Fatal("second parent service", lines, e)
			}
		})
	}
}

func TestKnownUnitsNonfinalRepairKeepsBothSupplierObligations(t *testing.T) {
	for _, kind := range []string{"TURN", "CANDIDATE"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var r *app.Runtime
			var task string
			if kind == "TURN" {
				actual, scope, turns, qr, req := actualTurnFixture(t, true)
				r = actual
				q, e := turns.QuoteTurn(ctx, scope, qr, "nonfinal-repair")
				if e != nil {
					t.Fatal(e)
				}
				req.QuoteID = q.QuoteID
				a, e := turns.SubmitTurn(ctx, scope, req, "nonfinal-repair")
				if e != nil {
					t.Fatal(e)
				}
				task = string(a.TurnID)
			} else {
				actual, draft, req := candidateHTTPFixture(t)
				r = actual
				var a generated.AdminTaskReceipt
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "nonfinal-repair", req, 202, &a)
				task = string(a.TaskID)
			}
			r.Worker.Turns.Fault = harness.MockFault(20)
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal("known-unit nonfinal repair did not commit", e)
			}
			var calls, steps, services, lines, held int
			if e := r.Store.DB.QueryRow("SELECT calls_allocated,steps_used,(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD') FROM harness_envelopes WHERE task_id=?", task, task, task, task).Scan(&calls, &steps, &services, &lines, &held); e != nil || calls != 2 || steps != 14 || services != 1 || lines != 2 || held != 5 {
				t.Fatal("two original obligations lost", calls, steps, services, lines, held, e)
			}
			rows, e := r.Store.DB.Query("SELECT metadata FROM harness_original_queries WHERE task_id=? ORDER BY attempt_id", task)
			if e != nil {
				t.Fatal(e)
			}
			var ids []domain.MockRequestIdentity
			for rows.Next() {
				var raw []byte
				var m domain.MockResultMetadata
				if e = rows.Scan(&raw); e != nil {
					t.Fatal(e)
				}
				if e = json.Unmarshal(raw, &m); e != nil || !m.UsageKnown || m.NoFurtherCost {
					t.Fatal("not independent known units and unknown finality", e)
				}
				ids = append(ids, m.Identity)
			}
			e = rows.Err()
			rows.Close()
			if e != nil || len(ids) != 2 {
				t.Fatal(len(ids), e)
			}
			for i, id := range ids {
				r.Worker.Turns.Fault = harness.MockLateFinalZeroBill
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.RecordOriginalBillItem(ctx, id, "late-final"); e != nil {
					t.Fatal(e)
				}
				want := 3
				if i == 1 {
					want = 0
				}
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD'", task).Scan(&held); e != nil || held != want {
					t.Fatal("another attempt risk released", i, held, want, e)
				}
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil || services != 1 {
				t.Fatal(services, e)
			}
		})
	}
}
