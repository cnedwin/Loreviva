package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminTaskListHTTPCursorCurrentScopeAndRestart(t *testing.T) {
	r, draft, req := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	s := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	for _, key := range []string{"list-a", "list-b", "list-c"} {
		if _, e := r.Store.AcceptDraftCandidate(ctx, s, draft, key, req); e != nil {
			t.Fatal(e)
		}
	}
	read := func(runtime *app.Runtime, path string) (int, generated.AdminTaskPage) {
		t.Helper()
		q := httptest.NewRequest("GET", "/world/v1/admin/tasks?"+path, nil)
		q.Header.Set("Authorization", "Bearer "+runtimeToken)
		w := httptest.NewRecorder()
		runtime.Handler().ServeHTTP(w, q)
		var page generated.AdminTaskPage
		if w.Code == 200 {
			if e := generated.Decode("AdminTaskPage", w.Body.Bytes(), &page); e != nil {
				t.Fatal(e)
			}
		}
		return w.Code, page
	}
	code, p := read(r, "product_id=coffee&limit=1")
	if code != 200 || len(p.Items) != 1 || p.NextCursor == nil {
		t.Fatal(code, p)
	}
	first := *p.NextCursor
	last := ""
	seen := map[string]bool{}
	for {
		var item generated.AdminTaskListAdminItem
		if e := json.Unmarshal(p.Items[0], &item); e != nil {
			t.Fatal(e)
		}
		id := string(item.TaskID)
		if seen[id] || id <= last {
			t.Fatal("nonmonotonic visible unchanged page", id, last)
		}
		seen[id] = true
		last = id
		if p.NextCursor == nil {
			break
		}
		code, p = read(r, "product_id=coffee&limit=1&cursor="+url.QueryEscape(*p.NextCursor))
		if code != 200 || len(p.Items) != 1 {
			t.Fatal(code, p)
		}
	}
	if len(seen) != 3 {
		t.Fatal(seen)
	}
	for _, query := range []string{"product_id=coffee&limit=2&cursor=" + first, "product_id=coffee&limit=1&cursor=" + first + "x", "product_id=coffee&task_id=task&cursor=" + first, "product_id=coffee&limit=0", "product_id=coffee&limit=101", "product_id=coffee&limit=1&limit=1", "product_id=coffee&unknown=1"} {
		if code, _ = read(r, query); code != 400 {
			t.Fatal(query, code)
		}
	}
	if code, _ = read(r, "product_id=other-product"); code != 404 {
		t.Fatal("cross product", code)
	}
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET view_epoch=view_epoch+1 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if code, _ = read(r, "product_id=coffee&limit=1&cursor="+first); code != 400 {
		t.Fatal("old view cursor", code)
	}
	code, p = read(r, "product_id=coffee&limit=1")
	if code != 200 || p.NextCursor == nil {
		t.Fatal(code, p)
	}
	rebuilt := app.NewRuntime(r.Store, r.Files, r.Auth)
	rebuilt.BindLocal(r.Local)
	if code, _ = read(rebuilt, "product_id=coffee&limit=1&cursor="+*p.NextCursor); code != 400 {
		t.Fatal("old runtime cursor", code)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='task.read'")
		return e
	}
	if code, p = read(rebuilt, "product_id=coffee&limit=1"); code != 403 || p.NextCursor != nil {
		t.Fatal("finish revoked read emitted page", code, p)
	}
	r.Store.BeforeCommit = nil
	if code, _ = read(rebuilt, "product_id=coffee&limit=1"); code != 200 {
		t.Fatal("read transaction did not rollback", code)
	}
}

func TestGenerationResolutionHTTPMalformedAndCurrentRead(t *testing.T) {
	r, draft, req := candidateHTTPFixture(t)
	base := map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "object_id": req.ObjectID, "module_id": "AUTHORING"}
	call := func(body map[string]any) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		q := httptest.NewRequest("POST", "/world/v1/admin/content-drafts/"+draft+"/generation-resolution", strings.NewReader(string(raw)))
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set("Authorization", "Bearer "+runtimeToken)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		return w.Code
	}
	if code := call(base); code != 200 {
		t.Fatal(code)
	}
	base["prompt"] = "unallocated"
	if code := call(base); code != 400 {
		t.Fatal(code)
	}
	delete(base, "prompt")
	base["expected_draft_revision"] = "2"
	if code := call(base); code != 409 {
		t.Fatal(code)
	}
	base["expected_draft_revision"] = "1"
	base["module_id"] = "MEDIA"
	if code := call(base); code != 200 {
		t.Fatal("unsupported metadata must be honest response", code)
	}
	if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.read'"); e != nil {
		t.Fatal(e)
	}
	if code := call(base); code != 403 {
		t.Fatal("current read revoked", code)
	}
}

func TestRegisteredAdminDetailReadWithoutProcurement(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "PENDING"
		if terminal {
			name = "COMMITTED"
		}
		t.Run(name, func(t *testing.T) {
			r, draft, req := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			var receipt generated.AdminTaskReceipt
			runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "detail", req, 202, &receipt)
			if terminal {
				if e := r.Worker.Once(ctx); e != nil {
					t.Fatal(e)
				}
			}
			var beforeCalls, beforeSteps, beforeReceipts, beforeHolds int
			if e := r.Store.DB.QueryRow("SELECT calls_used,(SELECT steps_used FROM harness_envelopes WHERE task_id=t.task_id),(SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM reservations) FROM tasks t WHERE task_id=?", receipt.TaskID).Scan(&beforeCalls, &beforeSteps, &beforeReceipts, &beforeHolds); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action<>'task.read'"); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ReadAdminTaskMetadata(ctx, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, string(receipt.TaskID)); e != nil {
				t.Fatal("fixed detail", e)
			}
			var page generated.AdminTaskPage
			runtimeHTTP(t, r, "GET", "/tasks?product_id=coffee&task_id="+string(receipt.TaskID), "", nil, 200, &page)
			if len(page.Items) != 1 {
				t.Fatal(page)
			}
			var item generated.AdminTaskListAdminItem
			if e := json.Unmarshal(page.Items[0], &item); e != nil {
				t.Fatal(e)
			}
			q := httptest.NewRequest("GET", item.QueryURL, nil)
			q.Header.Set("Authorization", "Bearer "+runtimeToken)
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, q)
			if w.Code != 200 {
				t.Fatalf("task.read directory link requires procurement: %d %s", w.Code, w.Body.String())
			}
			var view generated.AdminTaskView
			if e := generated.Decode("AdminTaskView", w.Body.Bytes(), &view); e != nil {
				t.Fatal(e)
			}
			if view.TaskID != receipt.TaskID || view.TaskKind != "CANDIDATE" || (terminal && (view.TaskState != "COMMITTED" || view.ResultAssetID == nil)) {
				t.Fatal(view)
			}
			runtimeHTTP(t, r, "POST", "/tasks/"+string(receipt.TaskID)+"/cancel", "deny-cancel", generated.AdminCancelRequest{ProductID: "coffee"}, 401, nil)
			runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "deny-generation", req, 401, nil)
			if terminal {
				q = httptest.NewRequest("GET", "/world/v1/assets/"+string(*view.ResultAssetID)+"/content?product_id=coffee", nil)
				q.Header.Set("Authorization", "Bearer "+runtimeToken)
				w = httptest.NewRecorder()
				r.Handler().ServeHTTP(w, q)
				if w.Code != 404 {
					t.Fatal("metadata grants body", w.Code)
				}
			}
			var calls, steps, receipts, holds int
			if e := r.Store.DB.QueryRow("SELECT calls_used,(SELECT steps_used FROM harness_envelopes WHERE task_id=t.task_id),(SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM reservations) FROM tasks t WHERE task_id=?", receipt.TaskID).Scan(&calls, &steps, &receipts, &holds); e != nil {
				t.Fatal(e)
			}
			if calls != beforeCalls || steps != beforeSteps || receipts != beforeReceipts || holds != beforeHolds {
				t.Fatal("read mutated", calls, steps, receipts, holds)
			}
		})
	}
}

func TestRegisteredAdminDetailCurrentProofAndInfrastructure(t *testing.T) {
	r, draft, req := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	var receipt generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "proof-detail", req, 202, &receipt)
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	id := string(receipt.TaskID)
	cases := []struct {
		name, sql string
		code      int
	}{
		{"TASK_READ", "DELETE FROM operator_actions WHERE credential_id='owner' AND action='task.read'", 403},
		{"SUBJECT", "UPDATE subject_governance SET enabled=0 WHERE subject_id='creator'", 403},
		{"PRIVACY", "UPDATE subject_governance SET privacy_epoch=privacy_epoch+1 WHERE subject_id='creator'", 403},
		{"VIEW", "UPDATE subject_governance SET view_epoch=view_epoch+1 WHERE subject_id='creator'", 403},
		{"REGISTRATION", "UPDATE registered_jobs SET operation='REHEARSAL' WHERE task_id=?", 404},
		{"INTAKE", "UPDATE commit_receipts SET payload_hash=REPEAT('x',32) WHERE operation='CANDIDATE_INTAKE'", 404},
		{"COMMIT", "UPDATE commit_receipts SET payload_hash=REPEAT('x',32) WHERE operation='CANDIDATE_COMMIT'", 404},
		{"NO_COMMIT", "DELETE FROM commit_receipts WHERE operation='CANDIDATE_COMMIT'", 404},
		{"ASSET", "UPDATE assets SET storage_key=REPEAT('0',32) WHERE asset_id=(SELECT asset_id FROM harness_candidate_commits WHERE task_id=?)", 404},
		{"ASSET_DOCUMENT", "UPDATE assets SET document='malformed' WHERE asset_id=(SELECT asset_id FROM harness_candidate_commits WHERE task_id=?)", 404},
		{"SQL_FAILURE", "SELECT no_such_column FROM tasks WHERE task_id=?", 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var e error
				if strings.Contains(c.sql, "?") {
					_, e = tx.ExecContext(ctx, c.sql, id)
				} else {
					_, e = tx.ExecContext(ctx, c.sql)
				}
				return e
			}
			runtimeHTTP(t, r, "GET", "/tasks/"+id+"?product_id=coffee", "", nil, c.code, nil)
			r.Store.BeforeCommit = nil
			runtimeHTTP(t, r, "GET", "/tasks/"+id+"?product_id=coffee", "", nil, 200, nil)
		})
	}
	runtimeHTTP(t, r, "GET", "/tasks/"+id+"?product_id=other", "", nil, 404, nil)
	if e := r.Local.Participant.Close(ctx); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/tasks/"+id+"?product_id=coffee", "", nil, 503, nil)
}

func TestRegisteredAdminLegacyIntakeProof(t *testing.T) {
	r, s, release := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	instance := createRuntimeWorld(t, r, release, "legacy-instance")
	var control generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+instance.Scope.WorldInstanceID+"/controls", "legacy-start", generated.AdminControlRequest{ProductID: "coffee", Command: "START", ExpectedVersions: instance.Versions}, 202, &control)
	cleanup, e := app.QueueCleanup(ctx, r.Store, s, "legacy-cleanup")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	for _, c := range []struct {
		name, op string
		task     generated.AdminTaskReceipt
	}{{"CONTROL", "controlAdminInstance", control}, {"CLEANUP", "queueFileCleanup", cleanup}} {
		t.Run(c.name, func(t *testing.T) {
			path := "/tasks/" + string(c.task.TaskID) + "?product_id=coffee"
			runtimeHTTP(t, r, "GET", path, "", nil, 200, nil)
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, "DELETE FROM commit_receipts WHERE operation=?", c.op)
				return e
			}
			runtimeHTTP(t, r, "GET", path, "", nil, 404, nil)
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				_, e := tx.ExecContext(ctx, "UPDATE commit_receipts SET payload_hash=REPEAT('x',32) WHERE operation=?", c.op)
				return e
			}
			runtimeHTTP(t, r, "GET", path, "", nil, 404, nil)
			r.Store.BeforeCommit = nil
			runtimeHTTP(t, r, "GET", path, "", nil, 200, nil)
		})
	}
}
