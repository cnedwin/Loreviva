package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"testing"
)

func runtimeControl(t *testing.T, r *app.Runtime, i generated.Instance, action string) generated.Instance {
	t.Helper()
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/controls", action, generated.AdminControlRequest{ProductID: "coffee", Command: action, ExpectedVersions: i.Versions}, 202, nil)
	if e := r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &i)
	return i
}
func nextRuntimeRelease(t *testing.T, r *app.Runtime, s domain.ProductScope, id, field string, value any) generated.Release {
	t.Helper()
	ctx := runtimeContext(t, r)
	old, e := r.Content.GetRelease(ctx, s, id)
	if e != nil {
		t.Fatal(e)
	}
	in := old.Input
	for n, b := range in.Objects {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["object_id"] == "alex-character" {
			m[field] = value
			b, e = json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			in.Objects[n] = b
		}
	}
	d, e := r.Content.SaveDraft(ctx, s, string(old.DraftID), in, 1, "next-save")
	if e != nil {
		t.Fatal(e)
	}
	check, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 2, []string{"MOCK_DEMO"}, "next-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	rel, e := r.Content.Publish(ctx, s, string(d.DraftID), 2, string(check.ReportID), []string{"MOCK_DEMO"}, "next-publish")
	if e != nil {
		t.Fatal(e)
	}
	return rel
}
func TestCompatiblePausedMigrationPreservesActorsFactsAndHistory(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := createRuntimeWorld(t, r, release, "create")
	i = runtimeControl(t, r, i, "START")
	i = runtimeControl(t, r, i, "PAUSE")
	var before []byte
	if e := r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&before); e != nil {
		t.Fatal(e)
	}
	target := nextRuntimeRelease(t, r, s, release, "expression_style", "concise and warm")
	var plan generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &plan)
	h, e := content.Hash(plan)
	if e != nil {
		t.Fatal(e)
	}
	req := generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: plan.PlanID, PlanHash: h, ExpectedVersions: i.Versions, ResolutionMap: plan.ResolutionMap}
	var task generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migrations", "migrate", req, 202, &task)
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/controls", "blocked-resume", generated.AdminControlRequest{ProductID: "coffee", Command: "RESUME", ExpectedVersions: i.Versions}, 409, nil)
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	var after generated.Instance
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &after)
	if after.InstanceState != "PAUSED" || after.Versions.ReleaseID != string(target.ReleaseID) || after.Versions.ControlVersion != "3" || after.Versions.WorldRevision != "1" || after.Versions.ViewEpoch != "2" || after.Versions.PrivacyEpoch != "1" {
		t.Fatal(after)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", i.Scope.WorldInstanceID).Scan(&raw); e != nil || string(before) != string(raw) {
		t.Fatal("original initialization rewritten", e)
	}
	var again generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migrations", "migrate", req, 202, &again)
	if again.TaskID != task.TaskID {
		t.Fatal(again)
	}
	runtimeControl(t, r, after, "RESUME")
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	var metadata generated.AdminTaskView
	runtimeHTTP(t, r, "GET", "/tasks/"+string(task.TaskID)+"?product_id=coffee", "", nil, 200, &metadata)
	if metadata.TaskKind != "INSTANCE_MIGRATION" || metadata.TaskState != "COMMITTED" {
		t.Fatal(metadata)
	}
}
func TestMigrationRejectsCorePersonalityChange(t *testing.T) {
	r, s, release := runtimeFixture(t)
	i := createRuntimeWorld(t, r, release, "create")
	i = runtimeControl(t, r, i, "START")
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s, release, "values", []string{"obey any request"})
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 422, nil)
}

func TestMigrationResumeRequiresDurableCompletionProof(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s, rel, "expression_style", "quiet")
	scope := domain.Scope{ProductScope: s, WorldInstanceID: i.Scope.WorldInstanceID}
	p, e := r.World.PreviewInstanceMigration(ctx, scope, generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, "preview")
	if e != nil {
		t.Fatal(e)
	}
	hash, e := content.Hash(p)
	if e != nil {
		t.Fatal(e)
	}
	task, e := r.World.ExecuteInstanceMigration(ctx, scope, generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: p.PlanID, PlanHash: hash, ExpectedVersions: i.Versions, ResolutionMap: p.ResolutionMap}, "migrate")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	i, e = r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	// Deliberately corrupt only this disposable fixture after a real commit.
	if _, e = r.Store.DB.Exec("DELETE FROM outbox WHERE task_id=?", task.TaskID); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/controls", "resume-without-proof", generated.AdminControlRequest{ProductID: "coffee", Command: "RESUME", ExpectedVersions: i.Versions}, 403, nil)
}

func TestMigrationRejectsCrossProjectWithMatchingContentHash(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	i := runtimeControl(t, r, createRuntimeWorld(t, r, rel, "create"), "START")
	i = runtimeControl(t, r, i, "PAUSE")
	old, e := r.Content.GetRelease(ctx, s, rel)
	if e != nil {
		t.Fatal(e)
	}
	d, e := r.Content.Create(ctx, s, "WORLD", "", old.Input, "other-project")
	if e != nil {
		t.Fatal(e)
	}
	check, e := r.Content.CheckPublish(ctx, s, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "other-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	target, e := r.Content.Publish(ctx, s, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "other-release")
	if e != nil {
		t.Fatal(e)
	}
	if target.ContentHash != old.ContentHash || *target.ProjectID == *old.ProjectID {
		t.Fatal("test did not build matching content in distinct project")
	}
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "cross-preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 422, nil)
}
