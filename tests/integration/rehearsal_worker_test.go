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

func TestRehearsalWorkerOriginalRecovery(t *testing.T) {
	for _, mode := range []string{"REPLY_LOST", "REPORT_ACK_COMMIT", "REPORT_ACK_ROLLBACK"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "worker-recovery", req)
			if e != nil {
				t.Fatal(e)
			}
			injected := false
			if mode == "REPLY_LOST" {
				r.Worker.Turns.Fault = harness.MockDropOriginalReply
			} else {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_rehearsal_reports WHERE task_id=? AND state='COMMITTED'", root.TaskID).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return tx.Commit()
					}
					injected = true
					if mode == "REPORT_ACK_COMMIT" {
						if e := tx.Commit(); e != nil {
							return e
						}
					} else {
						if e := tx.Rollback(); e != nil {
							return e
						}
					}
					return errors.New("original report ACK lost")
				}
			}
			e = r.Worker.Once(ctx)
			r.Store.Commit = nil
			if mode == "REPLY_LOST" {
				if !errors.Is(e, harness.ErrMockReplyLost) {
					t.Fatal("original reply ambiguity", e)
				}
			} else if !injected || !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("original report ambiguity", e)
			}
			before := journalBytes(t, r)
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id=? OR task_id IN (SELECT child_task_id FROM harness_rehearsal_children WHERE root_task_id=?)", root.TaskID, root.TaskID); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=UTC_TIMESTAMP(6)"); e != nil {
				t.Fatal(e)
			}
			r2 := app.NewRuntime(r.Store, r.Files, r.Auth)
			r2.BindLocal(r.Local)
			if mode != "REPORT_ACK_COMMIT" {
				if e = r2.Worker.Once(ctx); e != nil {
					t.Fatal("reconstructed owning worker", e)
				}
			}
			if _, e = r2.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID)); e != nil {
				t.Fatal("original report receipt", e)
			}
			if _, e = r2.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e != nil {
				t.Fatal("original report body", e)
			}
			if !bytes.Equal(before, journalBytes(t, r2)) {
				t.Fatal("restart generated a second original request")
			}
			var calls, services, next, own int
			if e = r.Store.DB.QueryRow("SELECT e.calls_allocated,a.next_input,a.own_steps,(SELECT COUNT(*) FROM service_events) FROM harness_envelopes e JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE e.task_id=?", root.TaskID).Scan(&calls, &next, &own, &services); e != nil {
				t.Fatal(e)
			}
			if calls != 1 || next != 1 || own != 2 || services != 1 {
				t.Fatal("original aggregate was reset", calls, next, own, services)
			}
		})
	}
}

func TestRehearsalRegisteredWorker(t *testing.T) {
	for _, repair := range []bool{false, true} {
		name := "NORMAL"
		if repair {
			name = "REPAIR"
		}
		t.Run(name, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED","RIGHTS","DUTIES","SIGN_CUSTODY"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "registered-rehearsal", req)
			if e != nil {
				t.Fatal(e)
			}
			if repair {
				r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			}
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal("actual registered rehearsal worker", e)
			}
			if _, e = r.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID)); e != nil {
				t.Fatal("actual root report receipt", e)
			}
			body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
			if e != nil || len(body) == 0 {
				t.Fatal("actual report bytes", e)
			}
			var calls, steps, own, next, services, consent, commits int
			if e = r.Store.DB.QueryRow("SELECT e.calls_allocated,e.steps_used,a.own_steps,a.next_input FROM harness_envelopes e JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE e.task_id=?", root.TaskID).Scan(&calls, &steps, &own, &next); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'),(SELECT COUNT(*) FROM harness_turn_commits)").Scan(&services, &consent, &commits); e != nil {
				t.Fatal(e)
			}
			wantCalls, wantSteps := 2, 20
			if repair {
				wantCalls, wantSteps = 4, 30
			}
			if calls != wantCalls || steps != wantSteps || own != 2 || next != 2 || services != 1 || consent != 0 || commits != 2 {
				t.Fatal("original aggregate facts", calls, steps, own, next, services, consent, commits)
			}
			var view generated.AdminTaskView
			runtimeHTTP(t, r, "GET", "/tasks/"+string(root.TaskID)+"?product_id=coffee", "", nil, 200, &view)
			if view.TaskState != "COMMITTED" || view.TaskKind != "REHEARSAL" || view.ResultAssetID == nil {
				t.Fatal("actual safe report task projection", view)
			}
			if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action IN ('content.read','asset.read','content.rehearse')"); e != nil {
				t.Fatal(e)
			}
			runtimeHTTP(t, r, "GET", "/tasks/"+string(root.TaskID)+"?product_id=coffee", "", nil, 200, &view)
			if view.ResultAssetID == nil {
				t.Fatal("metadata incorrectly required body permission")
			}
			if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
				t.Fatal("task metadata granted body")
			}
		})
	}
}

func TestRehearsalKnownRefusalProducesHonestCompleteReport(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED","RIGHTS"]}`), &req); e != nil {
		t.Fatal(e)
	}
	accepted, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "honest-refusal", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockRefuseOriginalResult
	if e = r.Worker.Once(ctx); e != nil {
		var code string
		var next, calls int
		if inspect := r.Store.DB.QueryRow("SELECT v.validation_code,a.next_input,e.calls_allocated FROM harness_original_validations v JOIN harness_rehearsal_children c ON c.child_task_id=v.task_id JOIN harness_admin_rehearsals a ON a.task_id=c.root_task_id JOIN harness_envelopes e ON e.task_id=a.task_id WHERE a.task_id=?", accepted.TaskID).Scan(&code, &next, &calls); inspect != nil {
			t.Fatal("original rejection classification missing", inspect, e)
		}
		t.Log("actual immutable validation", code, "root next", next, "original calls", calls)
		t.Fatal("known refusal prevented complete honest report", e)
	}
	body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(accepted.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	var report struct {
		Inputs []struct {
			State string `json:"state"`
		} `json:"inputs"`
	}
	if e = json.Unmarshal(body, &report); e != nil || len(report.Inputs) != 2 {
		t.Fatal(string(body), e)
	}
	for _, input := range report.Inputs {
		if input.State != "REFUSED" {
			t.Fatal("refusal claimed as committed narrative", input.State)
		}
	}
	var calls, next, services, worldCommits, events int
	if e = r.Store.DB.QueryRow("SELECT e.calls_allocated,a.next_input,(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_turn_commits),(SELECT COUNT(*) FROM runtime_events) FROM harness_envelopes e JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE e.task_id=?", accepted.TaskID).Scan(&calls, &next, &services, &worldCommits, &events); e != nil || calls != 2 || next != 2 || services != 1 || worldCommits != 0 || events != 0 {
		t.Fatal(calls, next, services, worldCommits, events, e)
	}
}

func TestRehearsalFinalStructureRejectionProducesHonestCompleteReport(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED","RIGHTS"]}`), &req); e != nil {
		t.Fatal(e)
	}
	accepted, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "honest-final-rejection", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedEveryResult
	if e = r.Worker.Once(ctx); e != nil {
		var code string
		var next, calls int
		if inspect := r.Store.DB.QueryRow("SELECT v.validation_code,a.next_input,e.calls_allocated FROM harness_original_validations v JOIN harness_rehearsal_children c ON c.child_task_id=v.task_id JOIN harness_admin_rehearsals a ON a.task_id=c.root_task_id JOIN harness_envelopes e ON e.task_id=a.task_id WHERE a.task_id=?", accepted.TaskID).Scan(&code, &next, &calls); inspect != nil {
			t.Fatal("original rejection classification missing", inspect, e)
		}
		t.Log("actual immutable validation", code, "root next", next, "original calls", calls)
		t.Fatal("known refusal prevented complete honest report", e)
	}
	body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(accepted.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	var report struct {
		Inputs []struct {
			State string `json:"state"`
		} `json:"inputs"`
	}
	if e = json.Unmarshal(body, &report); e != nil || len(report.Inputs) != 2 {
		t.Fatal(string(body), e)
	}
	for _, input := range report.Inputs {
		if input.State != "FAILED" {
			t.Fatal("refusal claimed as committed narrative", input.State)
		}
	}
	var calls, next, services, worldCommits, events int
	if e = r.Store.DB.QueryRow("SELECT e.calls_allocated,a.next_input,(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_turn_commits),(SELECT COUNT(*) FROM runtime_events) FROM harness_envelopes e JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE e.task_id=?", accepted.TaskID).Scan(&calls, &next, &services, &worldCommits, &events); e != nil || calls != 4 || next != 2 || services != 1 || worldCommits != 0 || events != 0 {
		t.Fatal(calls, next, services, worldCommits, events, e)
	}
}
