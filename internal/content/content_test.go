package content_test

import (
	"context"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"os"
	"testing"
)

func fixture(t *testing.T) (*content.Service, domain.ProductScope) {
	t.Helper()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{"INSERT INTO tenant_gates VALUES('demo-local',1,1)", "INSERT INTO product_gates VALUES('demo-local','coffee',1,1)", "INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('demo-local','coffee','creator',1,1,1,1)"} {
		if _, e := s.DB.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	return content.New(s), domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
}
func coffee(t *testing.T) generated.DraftInput {
	t.Helper()
	b, e := os.ReadFile("../../examples/coffee/definitions.json")
	if e != nil {
		t.Fatal(e)
	}
	var input generated.DraftInput
	if e = generated.Decode("DraftInput", b, &input); e != nil {
		t.Fatal(e)
	}
	return input
}
func TestReleaseImmutableAndOldInstancePinned(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	in := coffee(t)
	d, e := s.Create(ctx, scope, "WORLD", "", in, "create")
	if e != nil {
		t.Fatal(e)
	}
	check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	r, e := s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "publish")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.DB.Exec("INSERT INTO world_controls VALUES(?,?,?, ?,0,1,1,1,'CREATED')", scope.TenantID, scope.ProductID, "old-instance", r.ReleaseID); e != nil {
		t.Fatal(e)
	}
	in.Name = "Revised cafe"
	next, e := s.SaveDraft(ctx, scope, string(d.DraftID), in, 1, "save")
	if e != nil || next.DraftRevision != "2" {
		t.Fatal(next, e)
	}
	again, e := s.SaveDraft(ctx, scope, string(d.DraftID), in, 1, "save")
	if e != nil || again.DraftRevision != "2" {
		t.Fatal("replay compared stale revision before original result", again, e)
	}
	old, e := s.GetRelease(ctx, scope, string(r.ReleaseID))
	if e != nil || old.ContentHash != r.ContentHash || old.Input.Name == in.Name {
		t.Fatal("release mutated", old, e)
	}
	var pinned string
	if e = s.Store.DB.QueryRow("SELECT release_id FROM world_controls WHERE world_instance_id='old-instance'").Scan(&pinned); e != nil || pinned != string(r.ReleaseID) {
		t.Fatal("old runtime pin mutated", e)
	}
	_, e = s.SaveDraft(ctx, scope, string(d.DraftID), in, 1, "stale")
	var conflict *content.Conflict
	if !errors.As(e, &conflict) || conflict.Current != "2" {
		t.Fatal("expected field conflict", e)
	}
	in.Name = "different"
	if _, e = s.SaveDraft(ctx, scope, string(d.DraftID), in, 1, "save"); !errors.Is(e, store.ErrConflict) {
		t.Fatal("samekey differentpayload accepted", e)
	}
}
func TestCopyExcludesRuntimeIdentityAndSecrets(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	in := coffee(t)
	d, e := s.Create(ctx, scope, "WORLD", "", in, "create")
	if e != nil {
		t.Fatal(e)
	}
	report, e := s.Impact(ctx, scope, string(d.DraftID), 1, "COPY", "impact")
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.Command(ctx, scope, string(d.DraftID), 1, "COPY", string(report.ReportID), "0", "Copy", "copy")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(result)
	var copy generated.CopyReceipt
	if e = generated.Decode("CopyReceipt", b, &copy); e != nil {
		t.Fatal(e)
	}
	if copy.Draft.DraftID == d.DraftID || copy.Draft.ProjectID == d.ProjectID || len(copy.Draft.Input.Objects) != len(in.Objects) {
		t.Fatal("copy not independent static definition", copy)
	}
	forbidden := []byte(`{"name":"bad","description":"","tags":[],"root_object_id":"x","objects":[],"dependency_locks":[],"module_agent_bindings":[],"skill_bindings":[],"player_id":"secret"}`)
	if generated.Validate("DraftInput", forbidden) == nil {
		t.Fatal("runtime identity bypass")
	}
}
func TestCoffeeSeedIdempotent(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	a, e := s.SeedCoffee(ctx, scope, "coffee-v1")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.SeedCoffee(ctx, scope, "coffee-v1")
	if e != nil || a != b {
		t.Fatal("duplicate seed", a, b, e)
	}
	var n int
	if e = s.Store.DB.QueryRow("SELECT COUNT(*) FROM content_drafts").Scan(&n); e != nil || n != 1 {
		t.Fatal("seed duplicate drafts", n, e)
	}
	if e = s.Store.DB.QueryRow("SELECT COUNT(*) FROM world_controls").Scan(&n); e != nil || n != 0 {
		t.Fatal("seed created runtime", n, e)
	}
}
func TestDutyCoverageEditablePresetAndWrongScope(t *testing.T) {
	s, scope := fixture(t)
	ctx := context.Background()
	in := coffee(t)
	d, e := s.Create(ctx, scope, "WORLD", "", in, "create")
	if e != nil {
		t.Fatal(e)
	}
	wrong := scope
	wrong.ProductID = "other"
	if _, e = s.GetDraft(ctx, wrong, string(d.DraftID), ""); !errors.Is(e, content.ErrNotFound) {
		t.Fatal("wrong product disclosed", e)
	}
	in.Objects = in.Objects[:len(in.Objects)-1]
	next, e := s.SaveDraft(ctx, scope, string(d.DraftID), in, 1, "remove-duty")
	if e != nil {
		t.Fatal(e)
	}
	report, e := s.CheckPublish(ctx, scope, string(d.DraftID), 2, []string{"MOCK_DEMO"}, "blocked")
	if e != nil || report.Passed || len(report.Findings) == 0 {
		t.Fatal("missing duties allowed", next, report, e)
	}
	if _, e = s.Publish(ctx, scope, string(d.DraftID), 2, string(report.ReportID), []string{"MOCK_DEMO"}, "blocked-publish"); e == nil {
		t.Fatal("failed report published")
	}
}
