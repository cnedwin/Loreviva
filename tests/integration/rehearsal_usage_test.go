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
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestRehearsalUnknownUsageOriginalMetadata(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "actual-unknown-usage", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockUsagePending
	workerErr := r.Worker.Once(ctx)
	var task, world string
	if e = r.Store.DB.QueryRow("SELECT child_task_id,world_instance_id FROM harness_rehearsal_children WHERE root_task_id=?", root.TaskID).Scan(&task, &world); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: world}, task)
	if e != nil {
		t.Fatal(e)
	}
	original, e := r.Local.Mock.OriginalMetadata(ctx, id)
	if e != nil || original.State != "SUCCEEDED" || !original.BodyAvailable || original.UsageKnown || original.NoFurtherCost || original.InputUnits != 0 || original.OutputUnits != 0 {
		t.Fatal(original, e)
	}
	var raw []byte
	var recorded domain.MockResultMetadata
	if e = r.Store.DB.QueryRow("SELECT metadata FROM harness_original_queries WHERE task_id=?", task).Scan(&raw); e != nil || json.Unmarshal(raw, &recorded) != nil || recorded != original {
		t.Fatal("complete narrative with unknown usage was not recorded", workerErr, string(raw), e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.DB.Exec("UPDATE harness_rehearsal_children SET started_at=started_at-INTERVAL 181 SECOND,execute_by=execute_by-INTERVAL 181 SECOND WHERE child_task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action IN ('content.rehearse','content.read','asset.read')"); e != nil {
		t.Fatal(e)
	}
	q, e := r.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	cost, e := r.Store.RecordNecessaryCostQuery(ctx, q)
	if e != nil || cost.UsageKnown || cost.NoFurtherCost || cost.State != "SUCCEEDED" {
		t.Fatal("necessary query rejected actual unknown measurement", cost, e)
	}
	var lines, services, held, calls int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM reservations WHERE status='HELD'),calls_allocated FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&lines, &services, &held, &calls); e != nil || lines != 0 || services != 1 || held != 5 || calls != 1 {
		t.Fatal("unknown measurement became charge or lost original holds", lines, services, held, calls, e)
	}
	r.Worker.Turns.Fault = harness.MockLateFinalZeroBill
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET next_cost_query_at=UTC_TIMESTAMP(6) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_necessary_cost_records SET began_at=began_at-INTERVAL 31 SECOND,next_query_at=next_query_at-INTERVAL 31 SECOND WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	q, e = r.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	cost, e = r.Store.RecordNecessaryCostQuery(ctx, q)
	if e != nil || cost.UsageKnown || !cost.NoFurtherCost || cost.InputUnits != 0 || cost.OutputUnits != 0 {
		t.Fatal("real final cost incorrectly implied known usage", cost, e)
	}
	latest, e := r.Local.Mock.OriginalMetadata(ctx, id)
	if e != nil || latest != original {
		t.Fatal("late metadata altered original result", e)
	}
}

func TestOriginalUnknownUsageHasIndependentMeasuredEvidence(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "usage-evidence")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "usage-evidence")
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockUsagePending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, scope, string(accepted.TurnID))
	if e != nil {
		t.Fatal(e)
	}
	original, e := r.Local.Mock.OriginalMetadata(ctx, id)
	if e != nil || original.UsageKnown {
		t.Fatal(original, e)
	}
	reader, ok := any(r.Local.Mock).(interface {
		ReadOriginalUsageEvidence(context.Context, domain.MockRequestIdentity) (domain.OriginalUsageEvidence, error)
	})
	if !ok {
		t.Fatal("actual independent original usage reader missing")
	}
	if proof, e := reader.ReadOriginalUsageEvidence(ctx, id); e == nil {
		proof.Close()
		t.Fatal("unpublished measurement leaked")
	}
	r.Worker.Turns.Fault = harness.MockFault(19)
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	proof, e := reader.ReadOriginalUsageEvidence(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	m := proof.Measurement()
	proof.Close()
	if m.Identity != id || m.InputUnits == 0 || m.OutputUnits == 0 || m.Provider != "r0-mock" || m.Account != "r0-mock-supplier" {
		t.Fatal(m)
	}
	latest, e := r.Local.Mock.OriginalMetadata(ctx, id)
	if e != nil || latest != original {
		t.Fatal("measurement rewrote original result", e)
	}
	reconciler, ok := any(r.Store).(interface {
		ReconcileOriginalServiceMetering(context.Context, domain.MockRequestIdentity) (string, error)
	})
	if !ok {
		t.Fatal("fixed original service reconciliation missing")
	}
	key, e := reconciler.ReconcileOriginalServiceMetering(ctx, id)
	if e != nil || key == "" {
		t.Fatal(key, e)
	}
	again, e := reconciler.ReconcileOriginalServiceMetering(ctx, id)
	if e != nil || again != key {
		t.Fatal("original usage replay", again, e)
	}
	view, e := turns.GetTurn(ctx, scope, string(accepted.TurnID))
	if e != nil || view.Task.TaskState != "COMMITTED" || len(view.ApprovedPackets) != 1 || view.Billing.ServiceSettlementState != "SETTLED" || view.Attempts[0].UsageState != "AUTHORITATIVE" {
		t.Fatal(view, e)
	}
	var services, lines, held, calls, steps int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM reservations WHERE status='HELD'),calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", accepted.TurnID).Scan(&services, &lines, &held, &calls, &steps); e != nil || services != 1 || lines != 1 || held != 3 || calls != 1 || steps != 9 {
		t.Fatal(services, lines, held, calls, steps, e)
	}
}

func TestCandidateUnknownUsageBindsPendingService(t *testing.T) {
	r, draft, request := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	var accepted generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "pending-meter", request, 202, &accepted)
	r.Worker.Turns.Fault = harness.MockUsagePendingRepair
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal("known candidate blocked by unknown measurement", e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	if _, e := r.Store.QueryDraftCandidateCommit(ctx, scope, string(accepted.TaskID)); e != nil {
		t.Fatal(e)
	}
	var view generated.AdminTaskView
	runtimeHTTP(t, r, "GET", "/tasks/"+string(accepted.TaskID)+"?product_id=coffee", "", nil, 200, &view)
	if view.ResultAssetID == nil {
		t.Fatal(view)
	}
	original, e := r.Files.ReadApprovedDraftCandidate(ctx, scope, string(accepted.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	after, e := r.Files.ReadApprovedDraftCandidate(ctx, scope, string(accepted.TaskID))
	if e != nil || !bytes.Equal(original, after) {
		t.Fatal("pending-meter genuine history", e)
	}
	var status string
	var lines, held, calls, steps, unknown int
	if e := r.Store.DB.QueryRow("SELECT s.status,(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM reservations WHERE status='HELD'),e.calls_allocated,e.steps_used,(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN' AND input_units IS NULL AND output_units IS NULL) FROM service_events s JOIN harness_envelopes e ON e.tenant_id=s.tenant_id AND e.product_id=s.product_id AND e.task_id=s.task_id WHERE s.task_id=?", accepted.TaskID).Scan(&status, &lines, &held, &calls, &steps, &unknown); e != nil || status != "PENDING" || lines != 0 || held != 7 || calls != 2 || steps != 14 || unknown != 2 {
		t.Fatal(status, lines, held, calls, steps, unknown, e)
	}
}

func TestOriginalMeteringRepairFamiliesAndAtomicRecovery(t *testing.T) {
	for _, mode := range []string{"CANDIDATE", "REHEARSAL", "FINISH_SERVICE", "ACK_COMMIT", "ACK_ROLLBACK"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, request := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var task string
			if mode == "REHEARSAL" {
				var req generated.RehearsalRequest
				if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
					t.Fatal(e)
				}
				root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "usage-family", req)
				if e != nil {
					t.Fatal(e)
				}
				task = string(root.TaskID)
			} else {
				var accepted generated.AdminTaskReceipt
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "usage-family", request, 202, &accepted)
				task = string(accepted.TaskID)
			}
			r.Worker.Turns.Fault = harness.MockUsagePendingRepair
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			read := func() ([]byte, error) {
				if mode == "REHEARSAL" {
					return r.Files.ReadCommittedRehearsalReport(ctx, scope, task)
				}
				return r.Files.ReadApprovedDraftCandidate(ctx, scope, task)
			}
			before, e := read()
			if e != nil {
				t.Fatal(e)
			}
			rows, e := r.Store.DB.Query("SELECT q.request_identity FROM harness_original_queries q JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) ORDER BY a.slot")
			if e != nil {
				t.Fatal(e)
			}
			var ids []domain.MockRequestIdentity
			for rows.Next() {
				var raw []byte
				var id domain.MockRequestIdentity
				if e = rows.Scan(&raw); e != nil {
					t.Fatal(e)
				}
				if e = json.Unmarshal(raw, &id); e != nil {
					t.Fatal(e)
				}
				ids = append(ids, id)
			}
			rows.Close()
			if len(ids) != 2 {
				t.Fatal(len(ids))
			}
			injected := false
			for i, id := range ids {
				if i == 1 && mode == "REHEARSAL" {
					r2 := app.NewRuntime(r.Store, r.Files, r.Auth)
					r2.BindLocal(r.Local)
					r = r2
				}
				r.Worker.Turns.Fault = harness.MockFault(19)
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
				if i == 0 && mode == "FINISH_SERVICE" {
					r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
						injected = true
						_, e := tx.ExecContext(c, "UPDATE harness_settlement_services SET enabled=0")
						return e
					}
				}
				if i == 0 && (mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK") {
					r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
						injected = true
						if mode == "ACK_COMMIT" {
							if e := tx.Commit(); e != nil {
								return e
							}
						} else {
							if e := tx.Rollback(); e != nil {
								return e
							}
						}
						return errors.New("original usage ACK lost")
					}
				}
				key, err := r.Store.ReconcileOriginalServiceMetering(ctx, id)
				r.Store.BeforeCommit = nil
				r.Store.Commit = nil
				if i == 0 && (mode == "FINISH_SERVICE" || mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK") {
					if !injected || err == nil {
						t.Fatal(injected, err)
					}
					if mode != "FINISH_SERVICE" && !errors.Is(err, store.ErrCommitUnknown) {
						t.Fatal(err)
					}
					if mode != "ACK_COMMIT" {
						var n int
						if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_attempt_metering WHERE evidence_receipt IS NOT NULL").Scan(&n); e != nil || n != 0 {
							t.Fatal("partial usage after rollback", n, e)
						}
					}
					key, err = r.Store.ReconcileOriginalServiceMetering(ctx, id)
				}
				if err != nil || key == "" {
					t.Fatal(key, err)
				}
				var status string
				var lines int
				if e = r.Store.DB.QueryRow("SELECT s.status,(SELECT COUNT(*) FROM harness_service_lines WHERE service_id=s.record_id) FROM service_events s WHERE s.task_id=?", task).Scan(&status, &lines); e != nil {
					t.Fatal(e)
				}
				want := "PENDING"
				if i == 1 {
					want = "COMMITTED"
				}
				if status != want || lines != i+1 {
					t.Fatal("partial units treated as total", status, lines)
				}
				after, e := read()
				if e != nil || !bytes.Equal(before, after) {
					t.Fatal("real metering invalidated immutable history", i, e)
				}
			}
			var services, calls, held int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),calls_allocated,(SELECT COUNT(*) FROM reservations WHERE status='HELD') FROM harness_envelopes WHERE task_id=?", task).Scan(&services, &calls, &held); e != nil || services != 1 || calls != 2 || held != 5 {
				t.Fatal(services, calls, held, e)
			}
		})
	}
}

func TestCommittedPendingMeterCutoffKeepsNarrative(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "pending-cutoff")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "pending-cutoff")
	if e != nil {
		t.Fatal(e)
	}
	task := string(accepted.TurnID)
	r.Worker.Turns.Fault = harness.MockUsagePending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=accepted_at-INTERVAL 181 SECOND,execute_by=execute_by-INTERVAL 181 SECOND,settle_by=settle_by-INTERVAL 181 SECOND WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	out, e := r.Store.CutoffHarnessTask(ctx, scope, task)
	if e != nil || out.State != "COMMITTED" {
		t.Fatal(out, e)
	}
	var released, risk, held int
	if e = r.Store.DB.QueryRow("SELECT SUM(status='RELEASED'),SUM(status='RISK'),SUM(status='HELD') FROM reservations WHERE task_id=?", task).Scan(&released, &risk, &held); e != nil || released != 3 || risk != 1 || held != 2 {
		t.Fatal("committed pending customer reserve retained", released, risk, held, e)
	}
	view, e := turns.GetTurn(ctx, scope, task)
	if e != nil || view.Task.TaskState != "COMMITTED" || len(view.ApprovedPackets) != 1 || view.Billing.ServiceSettlementState != "RISK_TRANSFERRED" {
		t.Fatal(view, e)
	}
	r.Worker.Turns.Fault = harness.MockFault(19)
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ReconcileOriginalServiceMetering(ctx, id); e != nil {
		t.Fatal(e)
	}
	view, e = turns.GetTurn(ctx, scope, task)
	if e != nil || view.Billing.ServiceSettlementState != "RISK_TRANSFERRED" || len(view.ApprovedPackets) != 1 {
		t.Fatal(view, e)
	}
	var services, settled int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM reservations WHERE purpose='PARENT' AND status='SETTLED')").Scan(&services, &settled); e != nil || services != 1 || settled != 0 {
		t.Fatal("late units recouped customer", services, settled, e)
	}
}

func TestOriginalUsageAutomaticFailedRetryAndRestart(t *testing.T) {
	ctx := context.Background()
	r, draft, req := candidateHTTPFixture(t)
	var accepted generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "meter-auto", req, 202, &accepted)
	r.Worker.Turns.Fault = harness.MockUsagePendingRepair
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	runner, ok := any(r.Store).(interface {
		RunOriginalServiceMetering(context.Context) (int, int, error)
	})
	if !ok {
		t.Fatal("registered original usage delivery absent")
	}
	done, failed, e := runner.RunOriginalServiceMetering(ctx)
	if e != nil || done != 0 || failed != 2 {
		var count, due, offset int
		if qe := r.Store.DB.QueryRow("SELECT COUNT(*),SUM(meter_next_run_at<=UTC_TIMESTAMP(6)),TIMESTAMPDIFF(SECOND,UTC_TIMESTAMP(6),CURRENT_TIMESTAMP(6)) FROM harness_attempt_metering").Scan(&count, &due, &offset); qe == nil {
			t.Log("meter work count/due/session offset seconds", count, due, offset)
		} else {
			t.Log(qe)
		}
		t.Fatal(done, failed, e)
	}
	var failures int
	if e = r.Store.DB.QueryRow("SELECT SUM(meter_failures) FROM harness_attempt_metering").Scan(&failures); e != nil || failures != 2 {
		t.Fatal(failures, e)
	}
	rows, e := r.Store.DB.Query("SELECT request_identity FROM harness_original_queries")
	if e != nil {
		t.Fatal(e)
	}
	var ids []domain.MockRequestIdentity
	for rows.Next() {
		var raw []byte
		var id domain.MockRequestIdentity
		if e = rows.Scan(&raw); e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(raw, &id); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		r.Worker.Turns.Fault = harness.MockFault(19)
		if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
			t.Fatal(e)
		}
	}
	r2 := app.NewRuntime(r.Store, r.Files, r.Auth)
	r2.BindLocal(r.Local)
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action IN ('content.assist','content.read','asset.read')"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_attempt_metering SET meter_next_run_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND"); e != nil {
		t.Fatal(e)
	}
	runner = any(r2.Store).(interface {
		RunOriginalServiceMetering(context.Context) (int, int, error)
	})
	done, failed, e = runner.RunOriginalServiceMetering(ctx)
	if e != nil || done != 2 || failed != 0 {
		t.Fatal(done, failed, e)
	}
	done, failed, e = runner.RunOriginalServiceMetering(ctx)
	if e != nil || done != 0 || failed != 0 {
		t.Fatal("duplicate meter batch", done, failed, e)
	}
	var status string
	var lines, calls, steps int
	if e = r.Store.DB.QueryRow("SELECT s.status,(SELECT COUNT(*) FROM harness_service_lines),e.calls_allocated,e.steps_used FROM service_events s JOIN harness_envelopes e ON e.tenant_id=s.tenant_id AND e.product_id=s.product_id AND e.task_id=s.task_id WHERE s.task_id=?", accepted.TaskID).Scan(&status, &lines, &calls, &steps); e != nil || status != "COMMITTED" || lines != 2 || calls != 2 || steps != 14 {
		t.Fatal(status, lines, calls, steps, e)
	}
}

func TestFinalSupplierDoesNotSettleUnknownCustomerUnits(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "final-supplier-pending-units")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "final-supplier-pending-units")
	if e != nil {
		t.Fatal(e)
	}
	task := string(accepted.TurnID)
	r.Worker.Turns.Fault = harness.MockUsagePending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateFinalZeroBill
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalBillItem(ctx, id, "late-final"); e != nil {
		t.Fatal(e)
	}
	var state string
	var held int
	if e = r.Store.DB.QueryRow("SELECT s.status,(SELECT COUNT(*) FROM reservations WHERE status='HELD') FROM service_events s WHERE task_id=?", task).Scan(&state, &held); e != nil || state != "PENDING" || held != 3 {
		t.Fatal(state, held, e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	view, e := turns.GetTurn(ctx, scope, task)
	if e != nil || view.Task.TaskState != "COMMITTED" || view.Billing.ServiceSettlementState != "ADJUSTED" || view.Attempts[0].UsageState != "UNKNOWN" || len(view.ApprovedPackets) != 1 {
		t.Fatal(view, e)
	}
}

func TestCommittedProductMeterCutoffAndLateUnits(t *testing.T) {
	for _, mode := range []string{"CANDIDATE", "REHEARSAL", "FINISH_SERVICE", "ACK_COMMIT", "ACK_ROLLBACK"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, request := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var task string
			if mode == "CANDIDATE" {
				var receipt generated.AdminTaskReceipt
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "late-units", request, 202, &receipt)
				task = string(receipt.TaskID)
			} else {
				var req generated.RehearsalRequest
				if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
					t.Fatal(e)
				}
				root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "late-units", req)
				if e != nil {
					t.Fatal(e)
				}
				task = string(root.TaskID)
			}
			r.Worker.Turns.Fault = harness.MockUsagePendingRepair
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			read := func() ([]byte, error) {
				if mode == "CANDIDATE" {
					return r.Files.ReadApprovedDraftCandidate(ctx, scope, task)
				}
				return r.Files.ReadCommittedRehearsalReport(ctx, scope, task)
			}
			before, e := read()
			if e != nil {
				t.Fatal(e)
			}
			expireCutoff(t, r.Store.DB, task)
			injected := false
			if mode == "FINISH_SERVICE" {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					injected = true
					_, e := tx.ExecContext(c, "UPDATE harness_settlement_services SET enabled=0")
					return e
				}
			}
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					injected = true
					if mode == "ACK_COMMIT" {
						if e := tx.Commit(); e != nil {
							return e
						}
					} else {
						if e := tx.Rollback(); e != nil {
							return e
						}
					}
					return errors.New("pending committed cutoff ACK lost")
				}
			}
			cut := func() (store.HarnessCutoffResult, error) {
				if mode == "CANDIDATE" {
					return r.Store.CutoffDraftCandidate(ctx, scope, task)
				}
				return r.Store.CutoffDraftRehearsal(ctx, scope, task)
			}
			out, e := cut()
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if mode == "FINISH_SERVICE" || mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				if !injected || e == nil {
					t.Fatal(injected, e)
				}
				if mode != "FINISH_SERVICE" && !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				if mode != "ACK_COMMIT" {
					var released int
					if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_service_metering WHERE state='RELEASED'").Scan(&released); e != nil || released != 0 {
						t.Fatal("partial cutoff", released, e)
					}
				}
				out, e = cut()
			}
			if e != nil || out.State != "COMMITTED" {
				t.Fatal(out, e)
			}
			done, failed, e := r.Store.RunHarnessCutoffs(ctx)
			if e != nil || failed != 0 || done != 1 {
				t.Fatal(done, failed, e)
			}
			rows, e := r.Store.DB.Query("SELECT request_identity FROM harness_original_queries ORDER BY BINARY attempt_id")
			if e != nil {
				t.Fatal(e)
			}
			var ids []domain.MockRequestIdentity
			for rows.Next() {
				var raw []byte
				var id domain.MockRequestIdentity
				if e = rows.Scan(&raw); e != nil {
					t.Fatal(e)
				}
				if e = json.Unmarshal(raw, &id); e != nil {
					t.Fatal(e)
				}
				ids = append(ids, id)
			}
			rows.Close()
			if len(ids) != 2 {
				t.Fatal(ids)
			}
			for _, id := range ids {
				r.Worker.Turns.Fault = harness.MockFault(19)
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.ReconcileOriginalServiceMetering(ctx, id); e != nil {
					t.Fatal(e)
				}
			}
			after, e := read()
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("late units lost immutable report/candidate", e)
			}
			var services, lines, released, risk, held, calls int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM reservations WHERE status='RELEASED'),(SELECT COUNT(*) FROM reservations WHERE status='RISK'),(SELECT COUNT(*) FROM reservations WHERE status='HELD'),calls_allocated FROM harness_envelopes WHERE task_id=?", task).Scan(&services, &lines, &released, &risk, &held, &calls); e != nil || services != 1 || lines != 2 || released != 2 || risk != 1 || held != 4 || calls != 2 {
				t.Fatal(services, lines, released, risk, held, calls, e)
			}
		})
	}
}

func TestPendingMeterReportAtomicOutcomes(t *testing.T) {
	for _, mode := range []string{"FINISH_METER", "FINISH_HOLD", "ACK_COMMIT", "ACK_ROLLBACK", "MIXED"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "pending-atomic", req)
			if e != nil {
				t.Fatal(e)
			}
			r.Worker.Turns.Fault = harness.MockUsagePendingRepair
			injected := false
			if mode == "MIXED" {
				r.Worker.Turns.Fault = harness.MockUsagePending
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_turn_commits").Scan(&n); e != nil {
						return e
					}
					if n == 1 {
						r.Worker.Turns.Fault = 0
					}
					return nil
				}
			}
			if mode == "FINISH_METER" || mode == "FINISH_HOLD" {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_service_metering").Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return nil
					}
					injected = true
					if mode == "FINISH_HOLD" {
						_, e := tx.ExecContext(c, "UPDATE reservations SET status='RELEASED' WHERE task_id=? AND purpose='PARENT'", root.TaskID)
						return e
					}
					_, e := tx.ExecContext(c, "UPDATE harness_attempt_metering SET original_hash=UNHEX(REPEAT('ab',32))")
					return e
				}
			}
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_service_metering").Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return tx.Commit()
					}
					injected = true
					if mode == "ACK_COMMIT" {
						if e := tx.Commit(); e != nil {
							return e
						}
					} else {
						if e := tx.Rollback(); e != nil {
							return e
						}
					}
					return errors.New("pending meter original commit ACK lost")
				}
			}
			e = r.Worker.Once(ctx)
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if mode == "MIXED" {
				if e != nil {
					t.Fatal(e)
				}
			} else {
				if !injected || e == nil {
					t.Fatal(mode, injected, e)
				}
				if mode != "FINISH_METER" && mode != "FINISH_HOLD" && !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			}
			if mode == "FINISH_METER" || mode == "FINISH_HOLD" || mode == "ACK_ROLLBACK" {
				var services, reports int
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_rehearsal_reports WHERE state='COMMITTED')").Scan(&services, &reports); e != nil || services != 0 || reports != 0 {
					t.Fatal("partial pending joint transaction", services, reports, e)
				}
				if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id=?", root.TaskID); e != nil {
					t.Fatal(e)
				}
				if e = r.Worker.Once(ctx); e != nil {
					t.Fatal("original report retry", e)
				}
			}
			if _, e = r.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID)); e != nil {
				t.Fatal(e)
			}
			original, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
			if e != nil {
				t.Fatal(e)
			}
			var services, lines, unknown, calls, steps, held int
			var state string
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN'),e.calls_allocated,e.steps_used,(SELECT COUNT(*) FROM reservations WHERE status='HELD'),m.state FROM harness_envelopes e JOIN harness_service_metering m USING(tenant_id,product_id,task_id) WHERE e.task_id=?", root.TaskID).Scan(&services, &lines, &unknown, &calls, &steps, &held, &state); e != nil {
				t.Fatal(e)
			}
			wantCalls, wantSteps, wantUnknown, wantLines, wantHeld := 4, 30, 4, 0, 11
			if mode == "MIXED" {
				wantCalls, wantSteps, wantUnknown, wantLines, wantHeld = 2, 20, 1, 1, 5
			}
			if services != 1 || lines != wantLines || unknown != wantUnknown || calls != wantCalls || steps != wantSteps || held != wantHeld || state != "PENDING_VERIFICATION" {
				t.Fatal(services, lines, unknown, calls, steps, held, state)
			}
			if _, e = r.Store.DB.Exec("UPDATE harness_service_metering SET original_hash=UNHEX(REPEAT('cd',32)) WHERE task_id=?", root.TaskID); e != nil {
				t.Fatal(e)
			}
			if body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil || len(body) > 0 {
				t.Fatal("forged pending service authorized historical bytes", len(original), e)
			}
		})
	}
}

func TestRehearsalUnknownUsageStillCommitsKnownReport(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "pending-meter-report", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockUsagePending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal("known narrative was blocked by missing measured units", e)
	}
	if _, e = r.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID)); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e != nil {
		t.Fatal(e)
	}
	var state, meter string
	var services, lines, held, unknown, calls, steps int
	if e = r.Store.DB.QueryRow("SELECT s.status,m.state,(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM reservations WHERE status='HELD'),(SELECT COUNT(*) FROM harness_attempt_metering WHERE usage_state='UNKNOWN' AND input_units IS NULL AND output_units IS NULL),e.calls_allocated,e.steps_used FROM service_events s JOIN harness_service_metering m ON m.tenant_id=s.tenant_id AND m.product_id=s.product_id AND m.service_id=s.record_id JOIN harness_envelopes e ON e.tenant_id=m.tenant_id AND e.product_id=m.product_id AND e.task_id=m.task_id WHERE s.task_id=?", root.TaskID).Scan(&state, &meter, &services, &lines, &held, &unknown, &calls, &steps); e != nil || state != "PENDING" || meter != "PENDING_VERIFICATION" || services != 1 || lines != 0 || held != 5 || unknown != 1 || calls != 1 || steps != 11 {
		t.Fatal(state, meter, services, lines, held, unknown, calls, steps, e)
	}
}

func TestTurnUnknownUsageBindsPendingService(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "unknown-units")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "unknown-units")
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockUsagePending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var status string
	var lines, held int
	if e = r.Store.DB.QueryRow("SELECT status,(SELECT COUNT(*) FROM harness_service_lines),(SELECT COUNT(*) FROM reservations WHERE status='HELD') FROM service_events WHERE task_id=?", accepted.TurnID).Scan(&status, &lines, &held); e != nil || status != "PENDING" || lines != 0 || held != 6 {
		t.Fatal("unknown original TURN units were settled", status, lines, held, e)
	}
	view, e := turns.GetTurn(ctx, scope, string(accepted.TurnID))
	if e != nil || view.Task.TaskState != "COMMITTED" || len(view.ApprovedPackets) != 1 || view.Billing.ServiceSettlementState != "PENDING_VERIFICATION" || len(view.Attempts) != 1 || view.Attempts[0].UsageState != "UNKNOWN" {
		t.Fatal("known packet projected false measured settlement", view, e)
	}
}

func TestOrdinaryTurnUnknownUsageRepairKeepsOneService(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "unknown-repair")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "unknown-repair")
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockUsagePendingRepair
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	view, e := turns.GetTurn(ctx, scope, string(accepted.TurnID))
	if e != nil || view.Task.TaskState != "COMMITTED" || len(view.ApprovedPackets) != 1 || len(view.Attempts) != 2 || view.Attempts[0].UsageState != "UNKNOWN" || view.Attempts[1].UsageState != "UNKNOWN" || view.Billing.ServiceSettlementState != "PENDING_VERIFICATION" {
		t.Fatal(view, e)
	}
	var services, lines, calls, steps, held int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_service_lines),calls_allocated,steps_used,(SELECT COUNT(*) FROM reservations WHERE status='HELD') FROM harness_envelopes WHERE task_id=?", accepted.TurnID).Scan(&services, &lines, &calls, &steps, &held); e != nil || services != 1 || lines != 0 || calls != 2 || steps != 14 || held != 8 {
		t.Fatal(services, lines, calls, steps, held, e)
	}
}
