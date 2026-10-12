package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"testing"
)

func TestGenerationResolutionProgramOwnersAreUnsupported(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	found := 0
	for _, raw := range d.Input.Objects {
		var object map[string]any
		if e = json.Unmarshal(raw, &object); e != nil {
			t.Fatal(e)
		}
		if object["implementation_kind"] != "PROGRAM" {
			continue
		}
		found++
		out, err := r.Content.InspectDraftGeneration(ctx, scope, draft, 1, object["object_id"].(string), "AUTHORING")
		if err != nil || len(out.Fields) != 0 || out.State == "RESOLVED" || out.RouteHash != "" {
			t.Fatal("program owner advertised generated fields", object["kind"], out.Fields, out.State, err)
		}
	}
	if found == 0 {
		t.Fatal("fixture has no actual program owner")
	}
}

func TestGenerationResolutionIsCurrentReadNotProcurement(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	counts := func() string {
		t.Helper()
		var n string
		if e := r.Store.DB.QueryRow("SELECT CONCAT_WS(',',(SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM reservations),(SELECT COUNT(*) FROM harness_quotes),(SELECT COUNT(*) FROM world_projects),(SELECT COUNT(*) FROM service_events))").Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	before := counts()
	out, e := r.Content.InspectDraftGeneration(ctx, scope, draft, 1, string(d.Input.RootObjectID), "AUTHORING")
	if e != nil || out.State != "RESOLVED" || out.RouteHash == "" || len(out.Fields) == 0 {
		t.Fatal("current metadata read wrongly requires procurement", out.State, e)
	}
	request := generated.CandidateRequest{ProductID: "coffee", ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: d.Input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, draft, "no-procurement", request); e == nil {
		t.Fatal("resolution granted procurement")
	}
	if counts() != before {
		t.Fatal("read or denied purchase wrote work")
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.read'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.InspectDraftGeneration(ctx, scope, draft, 1, string(d.Input.RootObjectID), "AUTHORING"); e == nil {
		t.Fatal("read withdrawal bypass")
	}
}

func TestGenerationResolutionAndTaskListActualHTTP(t *testing.T) {
	ctx := context.Background()
	r, draft, req := candidateHTTPFixture(t)
	s := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	accepted, e := r.Store.AcceptDraftCandidate(ctx, s, draft, "http-metadata", req)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	var resolved generated.DraftGenerationResolution
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/generation-resolution", "", generated.DraftGenerationResolutionRequest{ProductID: "coffee", ExpectedDraftRevision: "1", ObjectID: req.ObjectID, ModuleID: "AUTHORING"}, 200, &resolved)
	if e = generated.Validate("DraftGenerationResolution", resolved); e != nil {
		t.Fatal(e)
	}
	var page generated.AdminTaskPage
	runtimeHTTP(t, r, "GET", "/tasks?product_id=coffee&task_id="+string(accepted.TaskID), "", nil, 200, &page)
	if len(page.Items) != 1 {
		t.Fatal(page)
	}
	var item generated.AdminTaskListAdminItem
	if e = json.Unmarshal(page.Items[0], &item); e != nil {
		t.Fatal(e)
	}
	var view generated.AdminTaskView
	// The S60 linked detail independently proves safe metadata,
	// and grants neither procurement nor body permission.
	if item.TaskID != accepted.TaskID || item.Family != "ADMIN" {
		t.Fatal(item)
	}
	runtimeHTTP(t, r, "GET", "/tasks/"+string(item.TaskID)+"?product_id=coffee", "", nil, 200, &view)
}
