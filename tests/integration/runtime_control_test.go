package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"net/http/httptest"
	"testing"
)

const runtimeToken = "synthetic-runtime-test-token-at-least32"

func TestRuntimeCreationReplayChecksMigratedCurrentRelease(t *testing.T) {
	r, s, release := runtimeFixture(t)
	original := createRuntimeWorld(t, r, release, "create")
	i := runtimeControl(t, r, original, "START")
	i = runtimeControl(t, r, i, "PAUSE")
	target := nextRuntimeRelease(t, r, s, release, "expression_style", "safe presentation")
	var plan generated.MigrationPlan
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migration-previews", "preview", generated.MigrationPreviewRequest{ProductID: "coffee", TargetReleaseID: target.ReleaseID, ExpectedVersions: i.Versions}, 201, &plan)
	h, e := content.Hash(plan)
	if e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/migrations", "migrate", generated.MigrationExecuteRequest{ProductID: "coffee", PlanID: plan.PlanID, PlanHash: h, ExpectedVersions: i.Versions, ResolutionMap: plan.ResolutionMap}, 202, nil)
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	if again := createRuntimeWorld(t, r, release, "create"); again != original {
		t.Fatal("original recovery changed", again)
	}
	if _, e = r.Store.DB.Exec("UPDATE releases SET available=0 WHERE release_id=?", target.ReleaseID); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "POST", "/instances", "create", generated.AdminCreateInstanceRequest{ProductID: "coffee", ReleaseID: release, Mode: "PRIVATE", InitialParticipants: []string{}, ClientCapabilities: []string{"TEXT"}}, 403, nil)
	if _, e = r.Store.DB.Exec("UPDATE releases SET available=1 WHERE release_id=?", target.ReleaseID); e != nil {
		t.Fatal(e)
	}
	if again := createRuntimeWorld(t, r, release, "create"); again != original {
		t.Fatal("recovered original differs")
	}
}

func runtimeFixture(t *testing.T) (*app.Runtime, domain.ProductScope, string) {
	return runtimeFixtureWithConsent(t, false)
}
func runtimeFixtureWithConsent(t *testing.T, optIn bool) (*app.Runtime, domain.ProductScope, string) {
	t.Helper()
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	privateRoot := t.TempDir()
	independentRoot := t.TempDir()
	if e := app.InitializeLocal(ctx, s, independentRoot, privateRoot); e != nil {
		t.Fatal(e)
	}
	if e := maintenance.BootstrapSyntheticWithConsent(ctx, s, scope, "owner", runtimeToken, optIn); e != nil {
		t.Fatal(e)
	}
	f, e := files.New(s, privateRoot)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	r := app.NewRuntime(s, f, auth.New(s, scope, "owner", runtimeToken))
	local, e := app.OpenLocal(ctx, s, independentRoot, privateRoot)
	if e != nil {
		t.Fatal(e)
	}
	r.BindLocal(local)
	r.Memory.Ledger = local.Ledger
	t.Cleanup(func() {
		if e := local.Participant.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	seed, e := content.New(s).SeedCoffee(ctx, scope, "coffee-v1")
	if e != nil {
		t.Fatal(e)
	}
	return r, scope, seed.ReleaseID
}
func runtimeHTTP(t *testing.T, r *app.Runtime, method, path, key string, in any, want int, out any) {
	t.Helper()
	b, e := json.Marshal(in)
	if e != nil {
		t.Fatal(e)
	}
	q := httptest.NewRequest(method, "/world/v1/admin"+path, bytes.NewReader(b))
	q.Header.Set("Authorization", "Bearer "+runtimeToken)
	q.Header.Set("Content-Type", "application/json")
	q.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, q)
	if w.Code != want {
		t.Fatalf("%s %s: expected %d got %d: %s", method, path, want, w.Code, w.Body.String())
	}
	if out != nil {
		if e = json.Unmarshal(w.Body.Bytes(), out); e != nil {
			t.Fatal(e)
		}
	}
}
func createRuntimeWorld(t *testing.T, r *app.Runtime, release, key string) generated.Instance {
	t.Helper()
	var i generated.Instance
	runtimeHTTP(t, r, "POST", "/instances", key, generated.AdminCreateInstanceRequest{ProductID: "coffee", ReleaseID: release, Mode: "PRIVATE", InitialParticipants: []string{}, ClientCapabilities: []string{"TEXT"}}, 201, &i)
	return i
}
func TestRuntimeCreatesActualInitialWorldAndReplays(t *testing.T) {
	r, _, release := runtimeFixture(t)
	a := createRuntimeWorld(t, r, release, "create")
	b := createRuntimeWorld(t, r, release, "create")
	if a != b || a.InstanceState != "CREATED" || a.Versions.ControlVersion != "0" || a.Versions.WorldRevision != "0" || a.Versions.PrivacyEpoch != "1" || a.Versions.ViewEpoch != "1" {
		t.Fatal("creation/replay differs", a, b)
	}
	var actors generated.AdminActorPage
	runtimeHTTP(t, r, "GET", "/instances/"+a.Scope.WorldInstanceID+"/actors?product_id=coffee", "", nil, 200, &actors)
	if len(actors.Items) != 2 || actors.Items[0].ActorInstanceID == actors.Items[1].ActorInstanceID {
		t.Fatal("actors not materialized", actors)
	}
	var count, taskCount int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM world_controls").Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE operation='createAdminInstance' AND task_id IS NOT NULL").Scan(&taskCount); e != nil || taskCount != 0 {
		t.Fatal(taskCount, e)
	}
}
func TestRuntimeControlsAreDurableLegalAndReplayable(t *testing.T) {
	r, _, release := runtimeFixture(t)
	i := createRuntimeWorld(t, r, release, "create")
	path := "/instances/" + i.Scope.WorldInstanceID + "/controls"
	var task generated.AdminTaskReceipt
	in := generated.AdminControlRequest{ProductID: "coffee", Command: "START", ExpectedVersions: i.Versions}
	runtimeHTTP(t, r, "POST", path, "start", in, 202, &task)
	if task.TaskKind != "INSTANCE_CONTROL" {
		t.Fatal(task)
	}
	var still generated.Instance
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &still)
	if still.InstanceState != "CREATED" {
		t.Fatal("202 executed inline", still)
	}
	if e := r.Worker.Once(context.Background()); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 200, &still)
	if still.InstanceState != "RUNNING" || still.Versions.ControlVersion != "1" {
		t.Fatal(still)
	}
	var replay generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", path, "start", in, 202, &replay)
	if replay.TaskID != task.TaskID {
		t.Fatal("replayed new task", replay)
	}
	in.ExpectedVersions = still.Versions
	runtimeHTTP(t, r, "POST", path, "illegal", in, 409, nil)
}

func TestRuntimeEndedCannotRestartAndCreatedCanEnd(t *testing.T) {
	r, _, release := runtimeFixture(t)
	i := createRuntimeWorld(t, r, release, "created-end")
	i = runtimeControl(t, r, i, "END")
	if i.InstanceState != "ENDED" || i.Versions.ControlVersion != "1" {
		t.Fatal(i)
	}
	for _, action := range []string{"START", "RESUME", "PAUSE", "END"} {
		runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/controls", "new-"+action, generated.AdminControlRequest{ProductID: "coffee", Command: action, ExpectedVersions: i.Versions}, 409, nil)
	}
	var before int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&before); e != nil || before != 1 {
		t.Fatal("illegal commands made events", before, e)
	}
}

func TestRuntimeReadAndReplayRespectCurrentOwnerAndPermissions(t *testing.T) {
	r, s, rel := runtimeFixture(t)
	i := createRuntimeWorld(t, r, rel, "create")
	other := s
	other.SubjectID = "someone-else"
	if _, e := r.World.GetInstance(context.Background(), domain.Scope{ProductScope: other, WorldInstanceID: i.Scope.WorldInstanceID}); e == nil {
		t.Fatal("other owner read instance")
	}
	if _, e := r.Store.DB.Exec("UPDATE subject_governance SET can_control=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "POST", "/instances/"+i.Scope.WorldInstanceID+"/controls", "start", generated.AdminControlRequest{ProductID: "coffee", Command: "START", ExpectedVersions: i.Versions}, 403, nil)
	if _, e := r.Store.DB.Exec("UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/instances/"+i.Scope.WorldInstanceID+"?product_id=coffee", "", nil, 401, nil)
}

func TestRuntimeAdvertisesRegisteredJobs(t *testing.T) {
	r, _, _ := runtimeFixture(t)
	var cap generated.CreatorCapabilities
	runtimeHTTP(t, r, "GET", "/capabilities?product_id=coffee", "", nil, 200, &cap)
	for _, kind := range []generated.AdminTaskKind{"INSTANCE_CONTROL", "INSTANCE_MIGRATION"} {
		found := false
		for _, got := range cap.SupportedJobs {
			if got == kind {
				found = true
			}
		}
		for _, got := range cap.UnimplementedJobs {
			if got == kind {
				t.Fatal("implemented runtime job still advertised unavailable", kind)
			}
		}
		if !found {
			t.Fatal("registered runtime job absent", kind)
		}
	}
}
