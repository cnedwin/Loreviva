package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
	"testing"
	"time"
)

func createPolicyInput(t *testing.T, mode string) func(*generated.DraftInput) {
	return func(input *generated.DraftInput) {
		t.Helper()
		rights := map[string]any{"origin_kind": "OWNED", "source_label": "new fixture original authored note", "license": "CC0-1.0", "allowed_uses": []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}}
		note := map[string]any{"kind": "KNOWLEDGE", "knowledge_type": "NOTE", "object_id": "s66-note", "version": "1", "name": "S66 note", "description": "new synthetic owned fixture", "tags": []string{}, "audience_policy": "PRODUCT_READERS", "rights": rights, "source_refs": []string{}, "binding_refs": []any{}, "dependency_refs": []any{}, "text": "The fixture library opens at sunrise."}
		switch mode {
		case "editor":
			note["audience_policy"] = "PRODUCT_EDITORS"
		case "licensed":
			rights["origin_kind"] = "LICENSED"
		case "builtin":
			rights["origin_kind"] = "BUILTIN"
		case "no_copy":
			rights["allowed_uses"] = []string{"EDIT", "PUBLISH", "REDISTRIBUTE"}
		case "no_redistribute":
			rights["allowed_uses"] = []string{"EDIT", "COPY", "PUBLISH"}
		case "source_ref":
			note["source_refs"] = []string{"unsupported-authored-source"}
		case "expires":
			rights["expires_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		case "source_location":
			note["source_location"] = "synthetic-local-source"
		case "rights_location":
			rights["source_location"] = "synthetic-local-source"
		}
		if mode == "origin_equal" || mode == "origin_different" {
			parent := map[string]any{}
			for k, v := range note {
				parent[k] = v
			}
			parent["object_id"] = "s66-note-parent"
			b, _ := json.Marshal(parent)
			input.Objects = append(input.Objects, b)
			note["origin_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-note-parent"}
			if mode == "origin_different" {
				note["text"] = "Different copied text"
			}
		}
		b, e := json.Marshal(note)
		if e != nil {
			t.Fatal(e)
		}
		input.Objects = append(input.Objects, b)
		for i, raw := range input.Objects {
			var o map[string]any
			_ = json.Unmarshal(raw, &o)
			if o["kind"] == "WORLD" && o["object_id"] == string(input.RootObjectID) {
				setups := o["initial_actor_setups"].([]any)
				setups[0].(map[string]any)["knowledge_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-note"}}
				input.Objects[i], _ = json.Marshal(o)
			}
		}
	}
}
func TestIntegrationCreateInitialSourcePolicy(t *testing.T) {
	for _, mode := range []string{"owned", "origin_equal", "editor", "licensed", "builtin", "no_copy", "no_redistribute", "source_ref", "expires", "source_location", "rights_location", "origin_different"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "expires" {
				integrationExpiringPublicationRejected(t)
				return
			}
			r, _, ctx, req, _ := integrationCreationFixtureInput(t, createPolicyInput(t, mode), time.Hour)
			out, e := r.World.CreateIntegrationInstance(ctx, "s66-policy", req)
			if mode != "owned" && mode != "origin_equal" {
				var fields *content.FieldErrors
				if !errors.As(e, &fields) {
					t.Fatal("not owning source policy denial", e)
				}
				id := content.Identity("demo-local", "coffee", "s66-service", "createInstance", "s66-policy")
				if integrationWorldRows(t, r.Store, id) != 0 {
					t.Fatal("partial private import")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			var b []byte
			var ns string
			if e = r.Store.DB.QueryRow("SELECT data_subject_id,document FROM memory_aggregates WHERE world_instance_id=?", out.Scope.WorldInstanceID).Scan(&ns, &b); e != nil {
				t.Fatal(e)
			}
			if ns != "s66-player" {
				t.Fatal("wrong private namespace")
			}
			var a memory.Aggregate
			if json.Unmarshal(b, &a) != nil {
				t.Fatal("aggregate")
			}
			found := false
			for _, src := range a.Sources {
				if src.Body == "The fixture library opens at sunrise." {
					found = true
				}
				if src.Audience != memory.Private || src.Domain != memory.ActorBelief || len(src.Uses) != 2 || src.Uses[0] != memory.PrivateResponse || src.Uses[1] != memory.Relationship {
					t.Fatal("source/use broadening")
				}
			}
			if !found {
				t.Fatal("published note not imported")
			}
			if strings.Contains(string(b), "s66-service") {
				t.Fatal("service private material copied")
			}
			var init struct {
				IntegrationInitialLineage *store.IntegrationInitialLineage
			}
			if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", out.Scope.WorldInstanceID).Scan(&b); e != nil {
				t.Fatal(e)
			}
			if json.Unmarshal(b, &init) != nil || init.IntegrationInitialLineage == nil || init.IntegrationInitialLineage.Player != "s66-player" {
				t.Fatal("lineage missing")
			}
			chain := false
			for _, v := range init.IntegrationInitialLineage.Sources {
				if v.Kind == "BELIEF" && len(v.Origins) >= 4 {
					chain = true
				}
			}
			if mode == "origin_equal" && !chain {
				t.Fatal("origin edge omitted")
			}
		})
	}
}
func integrationExpiringPublicationRejected(t *testing.T) {
	r, scope, release := runtimeFixture(t)
	ctx := runtimeContext(t, r)
	rel, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	input := rel.Input
	createPolicyInput(t, "expires")(&input)
	draft, e := r.Content.Create(ctx, scope, "WORLD", "", input, "s66-expiring-draft")
	if e != nil {
		t.Fatal(e)
	}
	report, e := r.Content.CheckPublish(ctx, scope, string(draft.DraftID), 1, []string{"MOCK_DEMO"}, "s66-expiring-check")
	if e != nil || report.Passed {
		t.Fatal("expiring publication must reject before registration", e)
	}
	raw, _ := json.Marshal(report)
	if !strings.Contains(string(raw), "EXPIRING_SOURCE") {
		t.Fatal("wrong upstream denial", string(raw))
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_world_participants").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
	t.Log("real publisher EXPIRING_SOURCE denial; registration and CREATE intentionally NOT_REACHED")
}
func TestIntegrationCreateFinalNaturalExpiry(t *testing.T) {
	r, _, ctx, req, cmd := integrationCreationFixtureInput(t, nil, 8*time.Second)
	var now time.Time
	if e := r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		t.Fatal(e)
	}
	if cmd.Player.ExpiresAt.Sub(now) < 3*time.Second {
		t.Fatal("PRECONDITION_FAIL: insufficient actual original expiry")
	}
	calls := 0
	r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
		calls++
		for {
			var utc time.Time
			if e := tx.QueryRowContext(c, "SELECT UTC_TIMESTAMP(6)").Scan(&utc); e != nil {
				return e
			}
			if utc.After(cmd.Player.ExpiresAt) {
				return nil
			}
			select {
			case <-c.Done():
				return c.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	_, e := r.World.CreateIntegrationInstance(ctx, "s66-expiry", req)
	r.Store.BeforeCommit = nil
	if e == nil || calls != 1 {
		t.Fatal("final expiry not enforced", calls, e)
	}
	id := content.Identity("demo-local", "coffee", "s66-service", "createInstance", "s66-expiry")
	if integrationWorldRows(t, r.Store, id) != 0 {
		t.Fatal("expired transaction committed")
	}
}
func TestIntegrationCreateCloseDrain(t *testing.T) {
	r, _, ctx, req, _ := integrationCreationFixture(t)
	p, e := r.Store.PrepareIntegrationCreation(ctx, "s66-close", req)
	if e != nil {
		t.Fatal(e)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { close(entered); <-resume; return nil }
	result := make(chan error, 1)
	go func() { _, e := r.Store.RunIntegrationCreation(ctx, p); result <- e }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owning Tx not entered")
	}
	if e = r.Local.Coordinator.BeginClose(context.Background(), "s66-close"); e != nil {
		close(resume)
		t.Fatal(e)
	}
	if _, e = r.Store.PrepareIntegrationCreation(ctx, "s66-after-close", req); e == nil {
		close(resume)
		t.Fatal("Close admitted new plan")
	}
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	e = r.Local.Coordinator.Drain(short, "s66-close")
	cancel()
	if e == nil {
		close(resume)
		t.Fatal("Drain ignored entered Tx")
	}
	close(resume)
	e = <-result
	r.Store.BeforeCommit = nil
	// A completed admitted Tx may commit, but its new readback admission after
	// CLOSING must fail. This is not evidence that the committed rows rolled back.
	if e == nil {
		t.Fatal("post-Close readback unexpectedly admitted")
	}
	if e = r.Local.Coordinator.Drain(context.Background(), "s66-close"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RunIntegrationCreation(ctx, p); e == nil {
		t.Fatal("retained plan reenabled authority")
	}
}
