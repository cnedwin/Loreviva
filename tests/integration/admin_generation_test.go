package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sync"
	"testing"
)

func TestCandidateDraftQualificationUsesCurrentOwnerRouteAndPureRead(t *testing.T) {
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	var receipts, tasks, quotes int
	count := func() (int, int, int) {
		t.Helper()
		var a, b, c int
		if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM harness_quotes)").Scan(&a, &b, &c); e != nil {
			t.Fatal(e)
		}
		return a, b, c
	}
	receipts, tasks, quotes = count()
	got, e := r.Content.InspectDraftGeneration(ctx, scope, string(published.DraftID), 1, string(published.Input.RootObjectID), "AUTHORING")
	if e != nil {
		t.Fatalf("current explicitly authorized draft must resolve without private runtime consent: %v", e)
	}
	if got.State != "RESOLVED" || got.RouteHash == "" || got.Settings.ModelID == nil || len(got.Fields) == 0 {
		t.Fatal(got)
	}
	a, b, c := count()
	if a != receipts || b != tasks || c != quotes {
		t.Fatal("pure inspection wrote durable work", a, b, c)
	}
	if _, e = r.Content.InspectDraftGeneration(ctx, scope, string(published.DraftID), 2, string(published.Input.RootObjectID), "AUTHORING"); e == nil {
		t.Fatal("stale revision allowed")
	}
	other := scope
	other.SubjectID = "another-owner"
	if _, e = r.Content.InspectDraftGeneration(ctx, other, string(published.DraftID), 1, string(published.Input.RootObjectID), "AUTHORING"); e == nil {
		t.Fatal("another owner read")
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.InspectDraftGeneration(ctx, scope, string(published.DraftID), 1, string(published.Input.RootObjectID), "AUTHORING"); e != nil {
		t.Fatal("independent current content read denied", e)
	}
	request := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: published.Input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, string(published.DraftID), "inspection-not-purchase", request); e == nil {
		t.Fatal("content read silently authorized generation")
	}
}

func TestCandidateSameKeyCompetitionUsesOneOriginalBudget(t *testing.T) {
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	request := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: published.Input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var receipts [2]generated.AdminTaskReceipt
	var errs [2]error
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			receipts[i], errs[i] = r.Store.AcceptDraftCandidate(ctx, scope, string(published.DraftID), "same-candidate", request)
		}(i)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || receipts[0].TaskID != receipts[1].TaskID {
		t.Fatal(receipts, errs)
	}
	var tasks, holds, quotes int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_admin_candidates),(SELECT COUNT(*) FROM reservations),(SELECT COUNT(*) FROM harness_quotes)").Scan(&tasks, &holds, &quotes); e != nil || tasks != 1 || holds != 3 || quotes != 1 {
		t.Fatal(tasks, holds, quotes, e)
	}
}

func TestCandidateIntakeWholeTransactionUnknownAndWorkspaceCollision(t *testing.T) {
	for _, mode := range []string{"ROLLBACK", "ACK_UNKNOWN", "PROJECT_COLLISION", "EXISTING_PROJECT", "WITHDRAW_BEFORE_COMMIT"} {
		t.Run(mode, func(t *testing.T) {
			r, scope, release := runtimeFixtureWithConsent(t, false)
			ctx := runtimeContext(t, r)
			published, e := r.Content.GetRelease(ctx, scope, release)
			if e != nil {
				t.Fatal(e)
			}
			input := published.Input
			if mode != "EXISTING_PROJECT" {
				for _, raw := range input.Objects {
					var v map[string]any
					_ = json.Unmarshal(raw, &v)
					if v["kind"] == "CHARACTER" {
						input.RootObjectID = generated.Identity(v["object_id"].(string))
						break
					}
				}
			}
			draftID := string(published.DraftID)
			if mode != "EXISTING_PROJECT" {
				d, e := r.Content.Create(ctx, scope, "CHARACTER", "", input, "standalone")
				if e != nil {
					t.Fatal(e)
				}
				draftID = string(d.DraftID)
			}
			project := content.Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "R0_AUTHORING_PROJECT")
			if mode == "PROJECT_COLLISION" {
				if _, e = r.Store.DB.Exec("INSERT INTO world_projects VALUES(?,?,?,?,'unrelated existing project')", scope.TenantID, scope.ProductID, project, scope.SubjectID); e != nil {
					t.Fatal(e)
				}
			}
			request := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
			if mode == "ROLLBACK" {
				r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("synthetic intake rollback") }
			}
			if mode == "WITHDRAW_BEFORE_COMMIT" {
				r.Store.BeforeCommit = func(ctx context.Context, _ *sql.Tx) error {
					_, e := r.Store.DB.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'")
					return e
				}
			}
			if mode == "ACK_UNKNOWN" {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic intake lost ACK")
				}
			}
			receipt, e := r.Store.AcceptDraftCandidate(ctx, scope, draftID, "candidate-atomic", request)
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			var tasks, quotes, holds, envelopes, projects int
			if err := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_admin_candidates),(SELECT COUNT(*) FROM harness_quotes),(SELECT COUNT(*) FROM reservations),(SELECT COUNT(*) FROM harness_envelopes),(SELECT COUNT(*) FROM harness_authoring_projects)").Scan(&tasks, &quotes, &holds, &envelopes, &projects); err != nil {
				t.Fatal(err)
			}
			if mode == "ROLLBACK" || mode == "PROJECT_COLLISION" || mode == "WITHDRAW_BEFORE_COMMIT" {
				if e == nil || tasks != 0 || quotes != 0 || holds != 0 || envelopes != 0 || projects != 0 {
					t.Fatal("partial product intake", tasks, quotes, holds, envelopes, projects, e)
				}
				return
			}
			if mode == "ACK_UNKNOWN" && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("unknown not preserved", e)
			}
			if mode == "EXISTING_PROJECT" && e != nil {
				t.Fatal(e)
			}
			wantProjects := 1
			if mode == "EXISTING_PROJECT" {
				wantProjects = 0
			}
			if tasks != 1 || quotes != 1 || holds != 3 || envelopes != 1 || projects != wantProjects {
				t.Fatal(tasks, quotes, holds, envelopes, projects)
			}
			replay, e := r.Store.AcceptDraftCandidate(ctx, scope, draftID, "candidate-atomic", request)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "EXISTING_PROJECT" && replay.TaskID != receipt.TaskID {
				t.Fatal("original identity changed")
			}
			var accountProject string
			if e = r.Store.DB.QueryRow("SELECT project_id FROM tasks WHERE task_id=?", replay.TaskID).Scan(&accountProject); e != nil {
				t.Fatal(e)
			}
			if mode == "EXISTING_PROJECT" {
				d, e := r.Content.GetDraft(ctx, scope, draftID, "")
				if e != nil || d.ProjectID == nil || accountProject != string(*d.ProjectID) {
					t.Fatal("actual project replaced", d.ProjectID, e)
				}
			}
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AcceptDraftCandidate(ctx, scope, draftID, "candidate-atomic", request); e == nil {
				t.Fatal("replay ignored current action")
			}
		})
	}
}

func TestCandidateIntakeAtomicZeroBudgetAndOriginalReplay(t *testing.T) {
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	input := published.Input
	for _, raw := range input.Objects {
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		if v["kind"] == "CHARACTER" {
			input.RootObjectID = generated.Identity(v["object_id"].(string))
			break
		}
	}
	draft, e := r.Content.Create(ctx, scope, "CHARACTER", "", input, "standalone-character")
	if e != nil || draft.ProjectID != nil {
		t.Fatal("real unassigned authoring resource", draft.ProjectID, e)
	}
	request := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: input.RootObjectID, SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	receipt, e := r.Store.AcceptDraftCandidate(ctx, scope, string(draft.DraftID), "candidate-original", request)
	if e != nil {
		t.Fatal(e)
	}
	if receipt.TaskKind != "CANDIDATE" || receipt.TaskState != "ACCEPTED" {
		t.Fatal(receipt)
	}
	var holds, calls, steps, maxCalls, maxSteps, execute, settle int
	var project string
	var world bool
	e = r.Store.DB.QueryRow("SELECT t.project_id,t.world_instance_id IS NOT NULL,(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),h.calls_allocated,h.steps_used,h.max_calls,h.max_steps,TIMESTAMPDIFF(SECOND,h.accepted_at,h.execute_by),TIMESTAMPDIFF(SECOND,h.accepted_at,h.settle_by) FROM tasks t JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.task_id=?", receipt.TaskID).Scan(&project, &world, &holds, &calls, &steps, &maxCalls, &maxSteps, &execute, &settle)
	if e != nil || world || holds != 3 || calls != 0 || steps != 0 || maxCalls != 2 || maxSteps != 20 || execute != 120 || settle != 180 {
		t.Fatal(project, world, holds, calls, steps, maxCalls, maxSteps, execute, settle, e)
	}
	if project != content.Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "R0_AUTHORING_PROJECT") {
		t.Fatal("caller-free workspace project identity", project)
	}
	var name, owner string
	if e = r.Store.DB.QueryRow("SELECT name,owner_subject_id FROM world_projects WHERE project_id=?", project).Scan(&name, &owner); e != nil || owner != scope.SubjectID || name != "R0 workspace authoring budget" {
		t.Fatal(name, owner, e)
	}
	in := input
	in.Name = "subsequent user edit"
	if _, e = r.Content.SaveDraft(ctx, scope, string(draft.DraftID), in, 1, "after-candidate"); e != nil {
		t.Fatal(e)
	}
	replay, e := r.Store.AcceptDraftCandidate(ctx, scope, string(draft.DraftID), "candidate-original", request)
	if e != nil || replay.TaskID != receipt.TaskID {
		t.Fatal("original replay after source revision", replay, e)
	}
	request.SelectedFields = []string{"background"}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, string(draft.DraftID), "candidate-original", request); !errors.Is(e, store.ErrConflict) {
		t.Fatal("different original payload", e)
	}
}

func TestCandidateCanonicalReadsOnlySupportedSelectedFields(t *testing.T) {
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	input := published.Input
	for i, raw := range input.Objects {
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		if v["object_id"] == string(input.RootObjectID) {
			v["description"] = "selected synthetic description"
			v["background"] = "UNSELECTED_PRIVATE_TEXT_MUST_NOT_ENTER_PROMPT"
			input.Objects[i], _ = json.Marshal(v)
		}
	}
	d, e := r.Content.SaveDraft(ctx, scope, string(published.DraftID), input, 1, "candidate-source")
	if e != nil {
		t.Fatal(e)
	}
	selection := domain.DraftCandidateSelection{DraftID: string(d.DraftID), Revision: 2, ObjectID: string(input.RootObjectID), ModuleID: "AUTHORING", Fields: []string{"description"}}
	read := func(sel domain.DraftCandidateSelection) (domain.DraftCandidateMaterial, error) {
		keys, e := r.Content.PlanCandidateLocks(ctx, scope, sel)
		if e != nil {
			return domain.DraftCandidateMaterial{}, e
		}
		g, e := content.Gate(ctx, r.Store, scope)
		if e != nil {
			return domain.DraftCandidateMaterial{}, e
		}
		var material domain.DraftCandidateMaterial
		sentinel := errors.New("actual read only end")
		_, e = r.Store.WithCommandBarrier(ctx, store.CommandBarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "CANDIDATE_PREPARE", CommandKey: "selected-fields", CommitKey: content.Identity("selected-fields"), GateVersions: g, PayloadHash: sha256.Sum256([]byte("selected-fields")), Locks: keys}, func(tx store.Tx) error {
			material, e = r.Content.BuildCandidateTx(ctx, tx, scope, sel)
			if e != nil {
				return e
			}
			return sentinel
		})
		if errors.Is(e, sentinel) {
			e = nil
		}
		return material, e
	}
	m, e := read(selection)
	if e != nil {
		t.Fatal(e)
	}
	var prompt map[string]any
	if e = json.Unmarshal(m.Canonical, &prompt); e != nil || prompt["Settings"] == nil {
		t.Fatal("actual effective settings absent from canonical request", e)
	}
	if !bytes.Contains(m.Canonical, []byte("selected synthetic description")) || bytes.Contains(m.Canonical, []byte("UNSELECTED_PRIVATE")) {
		t.Fatal("selected field projection failed", string(m.Canonical))
	}
	m2, e := read(selection)
	if e != nil || !bytes.Equal(m.Canonical, m2.Canonical) || m.RequestHash != m2.RequestHash {
		t.Fatal("stable input drift", e)
	}
	for _, fields := range [][]string{{}, {"description", "description"}, {"generation"}, {"../biography"}, {"name"}} {
		selection.Fields = fields
		if _, e := read(selection); e == nil {
			t.Fatal("unsupported field accepted", fields)
		}
	}
}
