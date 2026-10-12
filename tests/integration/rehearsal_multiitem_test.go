package integration

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"testing"
)

// One eligible item has a real positive/negative pair; another eligible item
// has only its custodian. The actual persisted report must not collapse the
// incomplete program to PASS merely because another item was checked.
func TestRehearsalActualMultiItemReportPreservesNotRun(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	ref := func(id string) map[string]any { return map[string]any{"kind": "LOCAL_DRAFT", "object_id": id} }
	var extra []json.RawMessage
	for i, raw := range d.Input.Objects {
		var o map[string]any
		if e = json.Unmarshal(raw, &o); e != nil {
			t.Fatal(e)
		}
		switch o["object_id"] {
		case "coffee-world":
			actors := o["initial_actor_setups"].([]any)
			cloneBytes, _ := json.Marshal(actors[0])
			var alone map[string]any
			_ = json.Unmarshal(cloneBytes, &alone)
			alone["slot_id"] = "alone"
			alone["scene_ref"] = ref("alone-scene")
			o["initial_actor_setups"] = append(actors, alone)
		case "cafe-scene":
			var other map[string]any
			_ = json.Unmarshal(raw, &other)
			other["object_id"] = "alone-scene"
			other["child_refs"] = []any{ref("alone-sign")}
			other["adjacent_scene_refs"] = []any{}
			x, _ := json.Marshal(other)
			extra = append(extra, x)
			o["adjacent_scene_refs"] = []any{ref("alone-scene")}
		case "wooden-sign":
			var other map[string]any
			_ = json.Unmarshal(raw, &other)
			other["object_id"] = "alone-sign"
			other["custodian_slot"] = "alone"
			other["scene_ref"] = ref("alone-scene")
			other["rule_refs"] = []any{ref("alone-custody")}
			x, _ := json.Marshal(other)
			extra = append(extra, x)
		case "sign-custody":
			var other map[string]any
			_ = json.Unmarshal(raw, &other)
			other["object_id"] = "alone-custody"
			other["targets"] = []any{ref("alone-sign")}
			other["condition"] = map[string]any{"op": "ACTOR_SLOT_EQUALS", "slot_id": "alone"}
			x, _ := json.Marshal(other)
			extra = append(extra, x)
		}
		d.Input.Objects[i], _ = json.Marshal(o)
	}
	d.Input.Objects = append(d.Input.Objects, extra...)
	saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 1, "actual-multi-item")
	if e != nil {
		t.Fatal(e)
	}
	var req generated.RehearsalRequest
	if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED","SIGN_CUSTODY","RIGHTS","DUTIES"]}`), &req); e != nil {
		t.Fatal(e)
	}
	req.ExpectedDraftRevision = saved.DraftRevision
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "actual-multi-item-report", req)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	raw, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil {
		t.Fatal(e)
	}
	var document struct {
		Assertions []domain.RehearsalAssertion `json:"assertions"`
	}
	if e = json.Unmarshal(raw, &document); e != nil {
		t.Fatal(e)
	}
	if len(document.Assertions) != 4 {
		t.Fatal(document)
	}
	for i, a := range document.Assertions {
		want := "PASS"
		if i == 1 {
			want = "NOT_RUN"
		}
		if a.Kind != req.Assertions[i] || a.State != want {
			t.Fatal(document)
		}
	}
	var actors, items, events int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM actors),(SELECT COUNT(*) FROM items),(SELECT COUNT(*) FROM runtime_events)").Scan(&actors, &items, &events); e != nil || actors != 3 || items != 2 || events != 0 {
		t.Fatal(actors, items, events, e)
	}
}
