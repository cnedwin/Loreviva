package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func candidateHTTPFixture(t *testing.T) (*app.Runtime, string, generated.CandidateRequest) {
	t.Helper()
	r, scope, release := runtimeFixtureWithConsent(t, false)
	p, e := r.Content.GetRelease(context.Background(), scope, release)
	if e != nil {
		t.Fatal(e)
	}
	d, e := r.Content.Create(context.Background(), scope, "WORLD", "", p.Input, "http-candidate-draft")
	if e != nil {
		t.Fatal(e)
	}
	return r, string(d.DraftID), generated.CandidateRequest{ProductID: "coffee", ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: p.Input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
}

func TestCandidateHTTPCommittedReadIndependentOfExecution(t *testing.T) {
	for _, mode := range []string{"READ_ONLY", "TASK_ONLY", "DISABLED", "FAKE_ASSET", "NO_COMMIT", "CLOSING"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, req := candidateHTTPFixture(t)
			var receipt generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "history", req, 202, &receipt)
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			var view generated.AdminTaskView
			runtimeHTTP(t, r, "GET", "/tasks/"+string(receipt.TaskID)+"?product_id=coffee", "", nil, 200, &view)
			asset := string(*view.ResultAssetID)
			read := func() *httptest.ResponseRecorder {
				q := httptest.NewRequest("GET", "/world/v1/assets/"+asset+"/content?product_id=coffee", nil)
				q.Header.Set("Authorization", "Bearer "+runtimeToken)
				w := httptest.NewRecorder()
				r.Handler().ServeHTTP(w, q)
				return w
			}
			before := read()
			if before.Code != 200 {
				t.Fatal(before.Code)
			}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			wantTask, wantBody := 200, 200
			switch mode {
			case "TASK_ONLY":
				if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action IN ('content.read','asset.read')"); e != nil {
					t.Fatal(e)
				}
				wantBody = 404
			case "DISABLED":
				if _, e := r.Store.DB.Exec("UPDATE subject_governance SET enabled=0 WHERE subject_id='creator'"); e != nil {
					t.Fatal(e)
				}
				wantTask, wantBody = 401, 404
			case "FAKE_ASSET":
				asset = "missing-asset"
				wantBody = 404
			case "NO_COMMIT":
				if _, e := r.Store.DB.Exec("DELETE FROM commit_receipts WHERE commit_key=?", content.Identity(string(receipt.TaskID), "candidate-commit")); e != nil {
					t.Fatal(e)
				}
				wantTask, wantBody = 404, 404
			case "CLOSING":
				if e := r.Local.Participant.Close(ctx); e != nil {
					t.Fatal(e)
				}
				wantTask, wantBody = 503, 503
			}
			runtimeHTTP(t, r, "GET", "/tasks/"+string(receipt.TaskID)+"?product_id=coffee", "", nil, wantTask, nil)
			after := read()
			if after.Code != wantBody {
				t.Fatal(mode, after.Code, after.Body.String())
			}
			if wantBody == 200 && !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
				t.Fatal("history bytes changed")
			}
			if mode == "READ_ONLY" {
				runtimeHTTP(t, r, "GET", "/assets/"+asset+"?product_id=coffee", "", nil, 200, nil)
				runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "new-purchase", req, 401, nil)
			}
		})
	}
}

func TestCandidateHTTPRepairRestartKeepsOriginalDependency(t *testing.T) {
	for _, mode := range []string{"APPROVAL_ACK", "EXTRA_MARKER_ACK", "ORIGINAL_DELETED", "OLD_FENCE", "REFUSAL"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, req := candidateHTTPFixture(t)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			if mode == "REFUSAL" {
				r.Worker.Turns.Fault = harness.MockRefuseOriginalResult
			}
			var receipt generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "repair-original", req, 202, &receipt)
			task := string(receipt.TaskID)
			if mode != "REFUSAL" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					var n int
					query := "SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=?"
					if mode == "EXTRA_MARKER_ACK" {
						query = "SELECT COUNT(*) FROM attempts WHERE task_id=? AND slot=1 AND send_started_at IS NOT NULL"
					}
					if e := tx.QueryRow(query, task).Scan(&n); e != nil {
						return e
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					if n > 0 {
						return errors.New("original repair ACK lost")
					}
					return nil
				}
			}
			if e := r.Worker.Once(ctx); e == nil {
				t.Fatal("interrupted repair/refusal completed")
			}
			r.Store.Commit = nil
			if mode == "REFUSAL" {
				var calls, extra int
				if e := r.Store.DB.QueryRow("SELECT t.calls_used,(SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=t.task_id) FROM tasks t WHERE task_id=?", task).Scan(&calls, &extra); e != nil || calls != 1 || extra != 0 {
					t.Fatal(calls, extra, e)
				}
				return
			}
			var snapshot, bodyhash []byte
			if e := r.Store.DB.QueryRow("SELECT a.document,v.body_hash FROM harness_candidate_attempts a JOIN harness_original_validations v USING(tenant_id,product_id,task_id,attempt_id) WHERE a.task_id=?", task).Scan(&snapshot, &bodyhash); e != nil && mode != "EXTRA_MARKER_ACK" {
				t.Fatal(e)
			}
			before := journalBytes(t, r)
			if mode == "ORIGINAL_DELETED" {
				var key string
				if e := r.Store.DB.QueryRow("SELECT storage_key FROM harness_candidate_results WHERE task_id=?", task).Scan(&key); e != nil {
					t.Fatal(e)
				}
				if e := os.Remove(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, key+".part")); e != nil {
					t.Fatal(e)
				}
			}
			var oldFence uint64
			if e := r.Store.DB.QueryRow("SELECT fence FROM tasks WHERE task_id=?", task).Scan(&oldFence); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=UTC_TIMESTAMP(6) WHERE task_id=?", task); e != nil {
				t.Fatal(e)
			}
			r2 := app.NewRuntime(r.Store, r.Files, r.Auth)
			r2.BindLocal(r.Local)
			var err error
			if mode == "OLD_FENCE" {
				fresh, e := r.Store.ClaimRegistered(ctx, "m1-file-cleanup", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE", TaskID: task}})
				if e != nil {
					t.Fatal(e)
				}
				old := fresh.TaskLease
				old.Fence = oldFence
				d, e := r.Store.PrepareDraftCandidateSend(ctx, old, "m1-file-cleanup")
				if e != nil {
					t.Fatal("read original metadata", e)
				}
				if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
					t.Fatal("old fence actually sent")
				}
				err = r2.Worker.Turns.ExecuteDraftCandidate(ctx, fresh.TaskLease, "m1-file-cleanup")
			} else {
				err = r2.Worker.Once(ctx)
			}
			deny := mode == "ORIGINAL_DELETED" || mode == "EXTRA_MARKER_ACK"
			if deny && err == nil || !deny && err != nil {
				t.Fatal("repair resume", mode, err)
			}
			if deny && !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("denied repair purchased")
			}
			var afterSnapshot, afterHash []byte
			if e := r.Store.DB.QueryRow("SELECT a.document,v.body_hash FROM harness_candidate_attempts a JOIN harness_original_validations v USING(tenant_id,product_id,task_id,attempt_id) WHERE a.task_id=? AND a.attempt_id=?", task, content.Identity(task, "candidate-attempt", "0")).Scan(&afterSnapshot, &afterHash); e != nil || !bytes.Equal(snapshot, afterSnapshot) || !bytes.Equal(bodyhash, afterHash) {
				t.Fatal("original immutable evidence changed", e)
			}
			var calls, holds, commits int
			if e := r.Store.DB.QueryRow("SELECT t.calls_used,(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_candidate_commits WHERE task_id=t.task_id) FROM tasks t WHERE task_id=?", task).Scan(&calls, &holds, &commits); e != nil {
				t.Fatal(e)
			}
			wantCalls, wantHolds, wantCommit := 2, 7, 1
			if deny {
				wantCommit = 0
			}
			if mode == "ORIGINAL_DELETED" {
				wantCalls, wantHolds = 1, 5
			}
			if calls != wantCalls || holds != wantHolds || commits != wantCommit {
				t.Fatal(calls, holds, commits)
			}
		})
	}
}

func TestCandidateHTTPRequiredModulesUseRealHandler(t *testing.T) {
	ctx := context.Background()
	r, scope, release := runtimeFixtureWithConsent(t, false)
	p, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	in := p.Input
	for _, o := range in.Objects {
		var m map[string]any
		if json.Unmarshal(o, &m) != nil {
			t.Fatal("fixture")
		}
		if m["kind"] == "ROLE" {
			m["object_id"] = "candidate-agent"
			m["implementation_kind"] = "AGENT"
			m["duty_ids"] = []string{"NARRATIVE"}
			m["generation"] = map[string]any{"mode": "INHERIT", "required_capabilities": []string{}, "allowed_external_uses": []string{"NONE"}}
			raw, _ := json.Marshal(m)
			in.Objects = append(in.Objects, raw)
			break
		}
	}
	k := map[string]any{"object_id": "candidate-knowledge", "kind": "KNOWLEDGE", "name": "Synthetic knowledge", "description": "bounded synthetic text", "tags": []string{}, "rights": map[string]any{"origin_kind": "SYNTHETIC", "source_label": "synthetic", "license": "Apache-2.0", "allowed_uses": []string{"EDIT", "COPY", "PUBLISH"}}, "audience_policy": "PRODUCT_READERS", "version": "1", "source_refs": []string{}, "dependency_refs": []any{}, "binding_refs": []any{}, "knowledge_type": "NOTE", "text": "The shop is open."}
	raw, _ := json.Marshal(k)
	in.Objects = append(in.Objects, raw)
	d, e := r.Content.Create(ctx, scope, "WORLD", "", in, "modules-draft")
	if e != nil {
		t.Fatal("legal module fixture", e)
	}
	for _, tc := range []struct{ module, object, field string }{{"AUTHORING", "coffee-world", "background"}, {"INTERACTION", "alex-character", "example_lines"}, {"PROACTIVE", "candidate-agent", "instruction_template"}, {"REHEARSAL", "candidate-agent", "instruction_template"}, {"EVALUATION", "candidate-agent", "instruction_template"}, {"KNOWLEDGE_EXTRACTION", "candidate-knowledge", "text"}} {
		t.Run(tc.module, func(t *testing.T) {
			request := generated.CandidateRequest{ProductID: "coffee", ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: tc.module, ObjectID: generated.Identity(tc.object), SelectedFields: []string{tc.field}, ApprovedCostUnits: "0"}
			var receipt generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", "/content-drafts/"+string(d.DraftID)+"/candidates", "module-"+tc.module, request, 202, &receipt)
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			b, e := r.Files.ReadApprovedDraftCandidate(ctx, scope, string(receipt.TaskID))
			if e != nil || !bytes.Contains(b, []byte(tc.field)) {
				t.Fatal(string(b), e)
			}
			var calls, lines int
			if e = r.Store.DB.QueryRow("SELECT t.calls_used,(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=t.task_id) FROM tasks t WHERE task_id=?", string(receipt.TaskID)).Scan(&calls, &lines); e != nil || calls != 1 || lines != 1 {
				t.Fatal(calls, lines, e)
			}
		})
	}
	var caps generated.CreatorCapabilities
	runtimeHTTP(t, r, "GET", "/capabilities?product_id=coffee", "", nil, 200, &caps)
	found := false
	for _, job := range caps.SupportedJobs {
		found = found || job == "CANDIDATE"
	}
	if !found {
		t.Fatal("actual handler omitted from support", caps.SupportedJobs, caps.UnimplementedJobs)
	}
	for _, job := range caps.UnimplementedJobs {
		if job == "CANDIDATE" {
			t.Fatal("real handler still advertised unavailable")
		}
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_candidate_commits").Scan(&count); e != nil || count != 6 {
		t.Fatal(count, e)
	}
}

func TestCandidateHTTPIntakeAndReadBoundaries(t *testing.T) {
	for _, mode := range []string{"INTAKE_ACK", "STALE_PENDING", "UNSUPPORTED", "BODY_READ_REVOKED", "FAKE_COMMIT", "SOURCE_DELETE"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, request := candidateHTTPFixture(t)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			path := "/content-drafts/" + draft + "/candidates"
			if mode == "UNSUPPORTED" {
				request.ModuleID = "INTERACTION"
				runtimeHTTP(t, r, "POST", path, "original", request, 422, nil)
				var n int
				if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_candidates").Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
				return
			}
			if mode == "INTAKE_ACK" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("intake ACK lost")
				}
				runtimeHTTP(t, r, "POST", path, "original", request, 409, nil)
				r.Store.Commit = nil
			}
			var receipt generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", path, "original", request, 202, &receipt)
			if mode == "STALE_PENDING" {
				d, e := r.Content.GetDraft(ctx, scope, draft, "")
				if e != nil {
					t.Fatal(e)
				}
				d.Input.Description = "new draft description"
				if _, e = r.Content.SaveDraft(ctx, scope, draft, d.Input, 1, "save-pending"); e != nil {
					t.Fatal(e)
				}
				if e = r.Worker.Once(ctx); e == nil {
					t.Fatal("stale pending basis generated")
				}
				var calls int
				if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", string(receipt.TaskID)).Scan(&calls); e != nil || calls != 0 {
					t.Fatal(calls, e)
				}
				return
			}
			if e := r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			var view generated.AdminTaskView
			runtimeHTTP(t, r, "GET", "/tasks/"+string(receipt.TaskID)+"?product_id=coffee", "", nil, 200, &view)
			if view.ResultAssetID == nil {
				t.Fatal(view)
			}
			if mode == "INTAKE_ACK" {
				var n int
				if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_candidates").Scan(&n); e != nil || n != 1 {
					t.Fatal(n, e)
				}
				return
			}
			if mode == "FAKE_COMMIT" {
				if _, e := r.Store.DB.Exec("DELETE FROM commit_receipts WHERE commit_key=?", content.Identity(string(receipt.TaskID), "candidate-commit")); e != nil {
					t.Fatal(e)
				}
				// S60's fixed metadata reader hides an object whose original
				// business receipt is missing; it cannot return a result reference.
				runtimeHTTP(t, r, "GET", "/tasks/"+string(receipt.TaskID)+"?product_id=coffee", "", nil, 404, nil)
				return
			}
			if mode == "SOURCE_DELETE" {
				impact, e := r.Content.Impact(ctx, scope, draft, 1, "DELETE", "source-impact")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Content.Command(ctx, scope, draft, 1, "DELETE", string(impact.ReportID), "", "", "delete-source"); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='asset.read'"); e != nil {
					t.Fatal(e)
				}
			}
			// Task metadata remains an original committed fact; current body rights
			// are separate and must not be recovered through the task receipt.
			runtimeHTTP(t, r, "GET", "/tasks/"+string(receipt.TaskID)+"?product_id=coffee", "", nil, 200, &view)
			q := httptest.NewRequest("GET", "/world/v1/assets/"+string(*view.ResultAssetID)+"/content?product_id=coffee", nil)
			q.Header.Set("Authorization", "Bearer "+runtimeToken)
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, q)
			if w.Code != 404 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestCandidateHTTPOriginalRecoveryAndCancellation(t *testing.T) {
	for _, mode := range []string{"CANCEL_BEFORE", "CANCEL_MARKER", "REPLY_RESTART", "MARKER_ACK", "BUSINESS_ACK", "BUSINESS_ROLLBACK_ACK", "CUTOFF_VIEW", "ASSIST_WITHDRAW", "HANDLER_MISSING"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, request := candidateHTTPFixture(t)
			path := "/content-drafts/" + draft + "/candidates"
			if mode == "HANDLER_MISSING" {
				r.Worker.Turns = nil
				runtimeHTTP(t, r, "POST", path, "original", request, 503, nil)
				var n int
				if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_candidates").Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
				return
			}
			var accepted generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", path, "original", request, 202, &accepted)
			task := string(accepted.TaskID)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			if mode == "CANCEL_BEFORE" {
				runtimeHTTP(t, r, "POST", "/tasks/"+task+"/cancel", "cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "stop original"}, 202, nil)
				if e := r.Worker.Once(ctx); !errors.Is(e, store.ErrNoTask) {
					t.Fatal(e)
				}
				var calls int
				if e := r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", task).Scan(&calls); e != nil || calls != 0 {
					t.Fatal(calls, e)
				}
				return
			}
			if mode == "ASSIST_WITHDRAW" {
				if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
					t.Fatal(e)
				}
				if e := r.Worker.Once(ctx); e == nil {
					t.Fatal("withdrawn generation allowed")
				}
				var calls int
				if e := r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", task).Scan(&calls); e != nil || calls != 0 {
					t.Fatal(calls, e)
				}
				return
			}
			if mode == "REPLY_RESTART" {
				r.Worker.Turns.Fault = harness.MockDropOriginalReply
			}
			if mode == "CUTOFF_VIEW" {
				expireCutoff(t, r.Store.DB, task)
				if _, e := r.Store.CutoffDraftCandidate(ctx, scope, task); e != nil {
					t.Fatal(e)
				}
				var view generated.AdminTaskView
				runtimeHTTP(t, r, "GET", "/tasks/"+task+"?product_id=coffee", "", nil, 200, &view)
				if view.TaskState != "FAILED" || view.ResultAssetID != nil {
					t.Fatal("cutoff projection", view)
				}
				return
			}
			if mode != "REPLY_RESTART" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					query := "SELECT COUNT(*) FROM harness_original_queries WHERE task_id=?"
					if mode == "BUSINESS_ACK" || mode == "BUSINESS_ROLLBACK_ACK" {
						query = "SELECT COUNT(*) FROM harness_candidate_commits WHERE task_id=?"
					}
					var n int
					if e := tx.QueryRow(query, task).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return tx.Commit()
					}
					if mode == "BUSINESS_ROLLBACK_ACK" {
						if e := tx.Rollback(); e != nil {
							return e
						}
					} else {
						if e := tx.Commit(); e != nil {
							return e
						}
					}
					return errors.New("original ACK lost")
				}
			}
			err := r.Worker.Once(ctx)
			if err == nil {
				t.Fatal("expected interrupted original", mode)
			}
			r.Store.Commit = nil
			before := journalBytes(t, r)
			if mode == "CANCEL_MARKER" {
				runtimeHTTP(t, r, "POST", "/tasks/"+task+"/cancel", "cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "stop in flight"}, 202, nil)
				id, e := r.Store.CandidateMockIdentity(ctx, scope, task)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=UTC_TIMESTAMP(6) WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
				q, e := r.Store.BeginOriginalQuery(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				m, e := r.Store.RecordOriginalQuery(ctx, q)
				if e != nil || m.State == "SUCCEEDED" {
					t.Fatal(m, e)
				}
				if e = r.Worker.Once(ctx); !errors.Is(e, store.ErrNoTask) {
					t.Fatal(e)
				}
				if !bytes.Equal(before, journalBytes(t, r)) {
					t.Fatal("cancel caused Generate")
				}
				return
			}
			if mode == "BUSINESS_ACK" {
				var view generated.AdminTaskView
				runtimeHTTP(t, r, "GET", "/tasks/"+task+"?product_id=coffee", "", nil, 200, &view)
				if view.TaskState != "COMMITTED" || view.ResultAssetID == nil {
					t.Fatal(view)
				}
				runtimeHTTP(t, r, "POST", "/tasks/"+task+"/cancel", "cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "already committed"}, 409, nil)
				if e := r.Worker.Once(ctx); !errors.Is(e, store.ErrNoTask) {
					t.Fatal(e)
				}
				return
			}
			// Reconstruct actual runtime against the same Local/DB/Files, then use a
			// genuine lease takeover. Only expiry timing is advanced by the fixture.
			r2 := app.NewRuntime(r.Store, r.Files, r.Auth)
			r2.BindLocal(r.Local)
			if _, e := r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("UPDATE harness_original_queries SET next_query_at=UTC_TIMESTAMP(6) WHERE task_id=?", task); e != nil {
				t.Fatal(e)
			}
			err = r2.Worker.Once(ctx)
			if mode == "MARKER_ACK" {
				if err == nil {
					t.Fatal("unsent known marker invented completion")
				}
			} else if err != nil {
				t.Fatal("resume original", err)
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("restart repurchased original")
			}
			var calls, holds, queries, commits int
			if e := r.Store.DB.QueryRow("SELECT t.calls_used,(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),(SELECT queries_used FROM harness_original_queries WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_candidate_commits WHERE task_id=t.task_id) FROM tasks t WHERE task_id=?", task).Scan(&calls, &holds, &queries, &commits); e != nil {
				t.Fatal(e)
			}
			want := 1
			if mode == "MARKER_ACK" {
				want = 0
			}
			if calls != 1 || holds != 5 || queries > 1 || commits != want {
				t.Fatal(calls, holds, queries, commits)
			}
		})
	}
}

func TestAdminHTTPDoesNotOwnWorldTurn(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, scope, qr, "admin-scope-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, scope, request, "admin-scope-turn")
	if e != nil {
		t.Fatal(e)
	}
	id := string(accepted.TurnID)
	snapshot := func() string {
		var v string
		if e := r.Store.DB.QueryRow("SELECT JSON_OBJECT('state',t.state,'cancelled',t.cancelled,'calls',t.calls_used,'steps',e.steps_used,'fsm',(SELECT fsm_state FROM harness_turns WHERE task_id=t.task_id),'attempts',(SELECT COUNT(*) FROM attempts WHERE task_id=t.task_id),'holds',(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),'receipts',(SELECT COUNT(*) FROM commit_receipts)) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", id).Scan(&v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	before := snapshot()
	t.Run("READ", func(t *testing.T) {
		runtimeHTTP(t, r, "GET", "/tasks/"+id+"?product_id=coffee", "", nil, 404, nil)
	})
	t.Run("CANCEL", func(t *testing.T) {
		runtimeHTTP(t, r, "POST", "/tasks/"+id+"/cancel", "admin-scope-cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "wrong task family"}, 404, nil)
	})
	t.Run("TASK_LOCK_RECHECK", func(t *testing.T) {
		_, e := content.Command(ctx, r.Store, scope.ProductScope, "cancelAdminTask", "admin-scope-direct", id, "AdminCancelReceipt", generated.AdminCancelRequest{ProductID: "coffee", Reason: "wrong family"}, []store.LockKey{{Rank: 50, IDs: []string{scope.TenantID, scope.ProductID, id}}}, func(tx store.Tx) (any, error) { return nil, tx.CancelRegisteredTask(ctx, id) })
		if !errors.Is(e, store.ErrDenied) {
			t.Fatal("task lock did not reject TURN", e)
		}
	})
	if after := snapshot(); before != after {
		t.Fatal("rejection mutated TURN", before, after)
	}
	// A real WORLD-linked control remains a legal administrative task.
	var controlID string
	if e = r.Store.DB.QueryRow("SELECT task_id FROM tasks WHERE world_instance_id=? AND task_kind='CONTROL' ORDER BY task_id LIMIT 1", scope.WorldInstanceID).Scan(&controlID); e != nil {
		t.Fatal(e)
	}
	var control generated.AdminTaskView
	runtimeHTTP(t, r, "GET", "/tasks/"+controlID+"?product_id=coffee", "", nil, 200, &control)
	if control.TaskKind != "INSTANCE_CONTROL" {
		t.Fatal(control)
	}
	var state string
	var cancelled, receipts int
	if e = r.Store.DB.QueryRow("SELECT state,cancelled,(SELECT COUNT(*) FROM commit_receipts WHERE command_key='admin-scope-cancel') FROM tasks WHERE task_id=?", id).Scan(&state, &cancelled, &receipts); e != nil {
		t.Fatal(e)
	}
	if state != "ACCEPTED" || cancelled != 0 || receipts != 0 {
		t.Fatal(state, cancelled, receipts)
	}
}

func TestCandidateHTTPRegisteredWorkerAndAsset(t *testing.T) {
	for _, repair := range []bool{false, true} {
		name := "ORIGINAL"
		if repair {
			name = "REPAIR"
		}
		t.Run(name, func(t *testing.T) {
			r, draft, request := candidateHTTPFixture(t)
			if repair {
				r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			}
			var receipt, replay generated.AdminTaskReceipt
			path := "/content-drafts/" + draft + "/candidates"
			runtimeHTTP(t, r, "POST", path, "http-original", request, 202, &receipt)
			runtimeHTTP(t, r, "POST", path, "http-original", request, 202, &replay)
			if receipt.TaskID != replay.TaskID {
				t.Fatal("intake replay changed original", receipt, replay)
			}
			changed := request
			changed.SelectedFields = []string{"background"}
			runtimeHTTP(t, r, "POST", path, "http-original", changed, 409, nil)
			if e := r.Worker.Once(context.Background()); e != nil {
				t.Fatal("registered generation worker", e)
			}
			var task generated.AdminTaskView
			runtimeHTTP(t, r, "GET", "/tasks/"+string(receipt.TaskID)+"?product_id=coffee", "", nil, 200, &task)
			if task.TaskKind != "CANDIDATE" || task.TaskState != "COMMITTED" || task.ResultAssetID == nil {
				t.Fatal("real result missing", task)
			}
			q := httptest.NewRequest("GET", "/world/v1/assets/"+string(*task.ResultAssetID)+"/content?product_id=coffee", nil)
			q.Header.Set("Authorization", "Bearer "+runtimeToken)
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, q)
			if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"fields"`)) {
				t.Fatal("authorized actual bytes", w.Code, w.Body.String())
			}
			var fields struct {
				Fields map[string]json.RawMessage `json:"fields"`
			}
			if json.Unmarshal(w.Body.Bytes(), &fields) != nil || len(fields.Fields) != 1 {
				t.Fatal(w.Body.String())
			}
			var calls, steps, holds, commits, services, lines, revision, worlds int
			e := r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_candidate_commits WHERE task_id=t.task_id),(SELECT COUNT(*) FROM service_events WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=t.task_id),(SELECT current_revision FROM content_drafts WHERE draft_id=?),(SELECT COUNT(*) FROM runtime_instances) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", draft, string(receipt.TaskID)).Scan(&calls, &steps, &holds, &commits, &services, &lines, &revision, &worlds)
			wantCalls, wantSteps, wantHolds := 1, 9, 5
			if repair {
				wantCalls, wantSteps, wantHolds = 2, 14, 7
			}
			if e != nil || calls != wantCalls || steps != wantSteps || holds != wantHolds || commits != 1 || services != 1 || lines != wantCalls || revision != 1 || worlds != 0 {
				t.Fatal(calls, steps, holds, commits, services, lines, revision, worlds, e)
			}
		})
	}
}
