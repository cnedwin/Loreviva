package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestRehearsalChildOriginalBillUsesParentRisk(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "child-late-original", req)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	before, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	var task, world string
	if e = r.Store.DB.QueryRow("SELECT child_task_id,world_instance_id FROM harness_rehearsal_children WHERE root_task_id=?", root.TaskID).Scan(&task, &world); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: world}, task)
	if e != nil {
		t.Fatal(e)
	}
	injected := false
	r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
		injected = true
		_, e := tx.ExecContext(c, "UPDATE harness_settlement_services SET enabled=0")
		return e
	}
	_, failed := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	r.Store.BeforeCommit = nil
	if !injected || failed == nil {
		t.Fatal("bill committed despite current settlement service withdrawal", failed)
	}
	var partial int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?", task).Scan(&partial); e != nil || partial != 0 {
		t.Fatal("partial bill on rollback", partial, e)
	}
	r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(c, "UPDATE harness_rehearsal_children SET input_hash=UNHEX(REPEAT('ad',32)) WHERE child_task_id=?", task)
		return e
	}
	_, failed = r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	r.Store.BeforeCommit = nil
	if failed == nil {
		t.Fatal("bill finisher accepted mutated original family binding")
	}
	first, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	if e != nil {
		t.Fatal("real child original bill blocked by ordinary parent assumption", e)
	}
	again, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	if e != nil || again != first {
		t.Fatal(again, e)
	}
	after, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("immutable report lost after original cost", e)
	}
	var services, risks, childRisks, holds int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_risk_cases WHERE task_id=?),(SELECT COUNT(*) FROM harness_risk_cases WHERE task_id=?),(SELECT COUNT(*) FROM reservations)", root.TaskID, task).Scan(&services, &risks, &childRisks, &holds); e != nil || services != 1 || risks != 1 || childRisks != 0 || holds != 5 {
		t.Fatal(services, risks, childRisks, holds, e)
	}
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	for _, item := range []string{"late-a", "late-b", "late-a"} {
		if _, e = r.Store.RecordOriginalBillItem(ctx, id, item); e != nil {
			t.Fatal(item, e)
		}
	}
	var cost, debt string
	var items, blocks int
	if e = r.Store.DB.QueryRow("SELECT CAST(cost_atoms AS CHAR),(SELECT CAST(debt_atoms AS CHAR) FROM harness_supplier_liabilities WHERE attempt_id=?),(SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?),(SELECT COUNT(*) FROM harness_account_blocks) FROM harness_risk_cases WHERE task_id=?", id.AttemptID, task, root.TaskID).Scan(&cost, &debt, &items, &blocks); e != nil || cost != "3" || debt != "3" || items != 3 || blocks != 2 {
		t.Fatal(cost, debt, items, blocks, e)
	}
	after, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("late debt rewrote or invalidated original report", e)
	}
}

func TestRehearsalBillsAllAttemptsAndCutoffRestart(t *testing.T) {
	for _, mode := range []string{"REPORT_REPAIR", "CUTOFF_REPLY_UNKNOWN"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, _ := candidateHTTPFixture(t)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "bill-family-restart", req)
			if e != nil {
				t.Fatal(e)
			}
			count, services := 4, 1
			r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			if mode == "CUTOFF_REPLY_UNKNOWN" {
				r.Worker.Turns.Fault = harness.MockDropOriginalReply
				count = 1
				services = 0
			}
			e = r.Worker.Once(ctx)
			var original []byte
			if mode == "REPORT_REPAIR" {
				if e != nil {
					t.Fatal(e)
				}
				original, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
				if e != nil {
					t.Fatal(e)
				}
			} else {
				if !errors.Is(e, harness.ErrMockReplyLost) {
					t.Fatal(e)
				}
				expireCutoff(t, r.Store.DB, string(root.TaskID))
				// N=2 fixes execute=150s and settle=210s; expire the full
				// original formula, rather than using the N=1 helper alone.
				if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=accepted_at-INTERVAL 60 SECOND,execute_by=execute_by-INTERVAL 60 SECOND,settle_by=settle_by-INTERVAL 60 SECOND WHERE task_id=?", root.TaskID); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.CutoffDraftRehearsal(ctx, scope, string(root.TaskID)); e != nil {
					t.Fatal(e)
				}
			}
			rows, e := r.Store.DB.Query("SELECT a.task_id,c.world_instance_id,a.attempt_id FROM attempts a JOIN harness_rehearsal_children c ON c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.child_task_id=a.task_id WHERE c.root_task_id=? ORDER BY c.input_index,a.slot", root.TaskID)
			if e != nil {
				t.Fatal(e)
			}
			type ref struct{ task, world, attempt string }
			refs := []ref{}
			for rows.Next() {
				var x ref
				if e = rows.Scan(&x.task, &x.world, &x.attempt); e != nil {
					t.Fatal(e)
				}
				refs = append(refs, x)
			}
			rows.Close()
			if len(refs) != count {
				t.Fatal(len(refs), count)
			}
			ids := []domain.MockRequestIdentity{}
			for _, x := range refs {
				id, e := r.Store.AttemptMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: x.world}, x.task, x.attempt)
				if e != nil {
					t.Fatal(e)
				}
				ids = append(ids, id)
			}
			if mode == "CUTOFF_REPLY_UNKNOWN" {
				// The root is closed. Expire the child's unchanged duration as
				// well, without inventing a narrative outcome or a new attempt.
				expireCutoff(t, r.Store.DB, refs[0].task)
				if _, e = r.Store.DB.Exec("UPDATE harness_rehearsal_children SET started_at=started_at-INTERVAL 181 SECOND,execute_by=execute_by-INTERVAL 181 SECOND WHERE child_task_id=?", refs[0].task); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action IN ('content.rehearse','content.read','asset.read')"); e != nil {
					t.Fatal(e)
				}
				var beforeCalls, beforeSteps int
				if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&beforeCalls, &beforeSteps); e != nil {
					t.Fatal(e)
				}
				q, e := r.Store.BeginNecessaryCostQuery(ctx, ids[0])
				if e != nil {
					t.Fatal("closed root original cost query", e)
				}
				metadata, e := r.Store.RecordNecessaryCostQuery(ctx, q)
				if e != nil || !metadata.UsageKnown {
					t.Fatal(metadata, e)
				}
				var afterCalls, afterSteps, reports, worldCommits int
				if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used,(SELECT COUNT(*) FROM harness_rehearsal_reports),(SELECT COUNT(*) FROM harness_candidate_commits) FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&afterCalls, &afterSteps, &reports, &worldCommits); e != nil || beforeCalls != afterCalls || beforeSteps != afterSteps || reports != 0 || worldCommits != 0 {
					t.Fatal("cost query resumed narrative", beforeCalls, afterCalls, beforeSteps, afterSteps, reports, worldCommits, e)
				}
			}
			prefix := journalBytes(t, r)
			r.Worker.Turns.Fault = harness.MockLateTwoBillItems
			for _, id := range ids {
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
			}
			if !bytes.HasPrefix(journalBytes(t, r), prefix) {
				t.Fatal("late append rewrote original identity/results")
			}
			r2 := app.NewRuntime(r.Store, r.Files, r.Auth)
			r2.BindLocal(r.Local)
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action='content.rehearse'"); e != nil {
				t.Fatal(e)
			}
			done, failed, e := r2.Store.RunOriginalBills(ctx)
			if e != nil || done != 3*count || failed != 0 {
				t.Fatal("restart actual old-child append discovery", done, failed, e)
			}
			done, failed, e = r2.Store.RunOriginalBills(ctx)
			if e != nil || done != 0 || failed != 0 {
				t.Fatal("cursor replay", done, failed, e)
			}
			r2.Worker.Turns.Fault = harness.MockLateConflictingBillItem
			if e = r2.Worker.Turns.DeliverLateMockBills(ctx, ids[0]); e != nil {
				t.Fatal(e)
			}
			done, failed, e = r2.Store.RunOriginalBills(ctx)
			if e != nil || done != 1 || failed != 0 {
				t.Fatal("exact conflicting version receipt", done, failed, e)
			}
			conflict, e := r2.Store.RecordOriginalBillItem(ctx, ids[0], "late-a")
			if !errors.Is(e, store.ErrConflict) || !conflict.Conflict {
				t.Fatal(conflict, e)
			}
			var total string
			var parentRisk, childRisk, actualService, actualHolds, items, conflicts, blocks int
			if e = r.Store.DB.QueryRow("SELECT CAST(cost_atoms AS CHAR),(SELECT COUNT(*) FROM harness_risk_cases WHERE task_id=?),(SELECT COUNT(*) FROM harness_risk_cases WHERE task_id<>?),(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM reservations),(SELECT COUNT(*) FROM harness_bill_items),(SELECT COUNT(*) FROM harness_bill_conflicts),(SELECT COUNT(*) FROM harness_account_blocks) FROM harness_risk_cases WHERE task_id=?", root.TaskID, root.TaskID, root.TaskID).Scan(&total, &parentRisk, &childRisk, &actualService, &actualHolds, &items, &conflicts, &blocks); e != nil || total != fmt.Sprint(3*count) || parentRisk != 1 || childRisk != 0 || actualService != services || actualHolds != 3+2*count || items != 3*count || conflicts != 1 || blocks != 2 {
				t.Fatal(total, parentRisk, childRisk, actualService, actualHolds, items, conflicts, blocks, e)
			}
			if mode == "REPORT_REPAIR" {
				after, e := r2.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
				if e != nil || !bytes.Equal(original, after) {
					t.Fatal("four original bodies/report changed after debt and conflict", e)
				}
			}
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action IN ('content.read','asset.read')"); e != nil {
				t.Fatal(e)
			}
			if _, e = r2.Store.RecordOriginalBillItem(ctx, ids[0], "late-b"); e != nil {
				t.Fatal("cost path borrowed revoked body permission", e)
			}
			if _, e = r2.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
				t.Fatal("cost path restored report permission")
			}
		})
	}
}

func TestRehearsalRiskWaitsForEveryChildFinalBill(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "every-child-final", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockCostFinalityPending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	original, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	rows, e := r.Store.DB.Query("SELECT child_task_id,world_instance_id FROM harness_rehearsal_children WHERE root_task_id=? ORDER BY input_index", root.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	type childRef struct{ task, world string }
	var refs []childRef
	for rows.Next() {
		var x childRef
		if e = rows.Scan(&x.task, &x.world); e != nil {
			t.Fatal(e)
		}
		refs = append(refs, x)
	}
	rows.Close()
	if len(refs) != 2 {
		t.Fatal(refs)
	}
	r.Worker.Turns.Fault = harness.MockLateFinalZeroBill
	for i, x := range refs {
		id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: x.world}, x.task)
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
			t.Fatal(e)
		}
		done, failed, e := r.Store.RunOriginalBills(ctx)
		expected := 3
		if i == 1 {
			expected = 1
		}
		if e != nil || done != expected || failed != 0 {
			t.Fatal(i, done, failed, e)
		}
		var unresolved, held, services, calls, steps int
		if e = r.Store.DB.QueryRow("SELECT unresolved,(SELECT COUNT(*) FROM reservations WHERE status IN ('HELD','RISK')),(SELECT COUNT(*) FROM service_events),(SELECT calls_allocated FROM harness_envelopes WHERE task_id=?),(SELECT steps_used FROM harness_envelopes WHERE task_id=?) FROM harness_risk_cases WHERE task_id=?", root.TaskID, root.TaskID, root.TaskID).Scan(&unresolved, &held, &services, &calls, &steps); e != nil {
			t.Fatal(e)
		}
		wantUnresolved, wantHeld := 1, 3
		if i == 1 {
			wantUnresolved, wantHeld = 0, 0
		}
		if unresolved != wantUnresolved || held != wantHeld || services != 1 || calls != 2 || steps != 20 {
			t.Fatal(i, unresolved, held, services, calls, steps)
		}
		after, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
		if e != nil || !bytes.Equal(original, after) {
			t.Fatal("finality changed report", i, e)
		}
	}
}

func TestRehearsalFinalBillChangesAccountingNotReport(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "final-bill-report", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockCostFinalityPending
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	before, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	var task, world string
	if e = r.Store.DB.QueryRow("SELECT child_task_id,world_instance_id FROM harness_rehearsal_children WHERE root_task_id=?", root.TaskID).Scan(&task, &world); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: world}, task)
	if e != nil {
		t.Fatal(e)
	}
	original, e := r.Local.Mock.OriginalMetadata(ctx, id)
	if e != nil || original.NoFurtherCost {
		t.Fatal(original, e)
	}
	r.Worker.Turns.Fault = harness.MockFault(16)
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal("actual late final bill unavailable", e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.DB.Exec("UPDATE harness_rehearsal_children SET started_at=started_at-INTERVAL 181 SECOND,execute_by=execute_by-INTERVAL 181 SECOND WHERE child_task_id=?", task); e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.BeginNecessaryCostQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	cost, e := r.Store.RecordNecessaryCostQuery(ctx, lease)
	if e != nil || !cost.UsageKnown || !cost.NoFurtherCost || cost.CostAtoms != 0 {
		t.Fatal("actual final metadata", cost, e)
	}
	done, failed, e := r.Store.RunOriginalBills(ctx)
	if e != nil || done != 2 || failed != 0 {
		t.Fatal("actual append scanner", done, failed, e)
	}
	after, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("finality changed immutable historical report", e)
	}
	latest, e := r.Local.Mock.OriginalMetadata(ctx, id)
	if e != nil || latest != original {
		t.Fatal("late bill rewrote original result evidence", e)
	}
	var risk, held, services, calls, steps int
	if e = r.Store.DB.QueryRow("SELECT (SELECT SUM(unresolved) FROM harness_risk_cases WHERE task_id=?),(SELECT COUNT(*) FROM reservations WHERE status IN ('HELD','RISK')),(SELECT COUNT(*) FROM service_events),calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID, root.TaskID).Scan(&risk, &held, &services, &calls, &steps); e != nil || risk != 0 || held != 0 || services != 1 || calls != 1 || steps != 11 {
		t.Fatal(risk, held, services, calls, steps, e)
	}
}
