package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func TestRehearsalIsolatedInitialization(t *testing.T) {
	for _, mode := range []string{"NORMAL", "ROLLBACK", "COMMIT_ACK", "OLD_FENCE", "INPUT_TAMPER", "FINISH_REVOKE", "EXPIRED"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":"Mia is the explicit synthetic responder."}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "isolation", req)
			if e != nil {
				t.Fatal(e)
			}
			lease, e := r.Store.ClaimRegistered(ctx, "rehearsal-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
			if e != nil {
				t.Fatal(e)
			}
			if mode == "ROLLBACK" {
				r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("isolation rollback") }
			}
			if mode == "COMMIT_ACK" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("isolation ACK lost")
				}
			}
			if mode == "INPUT_TAMPER" {
				if _, e = r.Store.DB.Exec("UPDATE harness_rehearsal_input_sources SET document=JSON_SET(document,'$.inputs[0].text','tampered synthetic input') WHERE task_id=?", root.TaskID); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "FINISH_REVOKE" {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
					return e
				}
			}
			if mode == "EXPIRED" {
				if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 91 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 91 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 91 SECOND) WHERE task_id=?", root.TaskID); e != nil {
					t.Fatal(e)
				}
			}
			old := lease.TaskLease
			if mode == "OLD_FENCE" {
				lease.Fence++
			}
			_, e = r.Store.InitializeDraftRehearsal(ctx, lease.TaskLease, "rehearsal-worker")
			if mode == "INPUT_TAMPER" || mode == "FINISH_REVOKE" || mode == "EXPIRED" {
				if e == nil {
					t.Fatal("invalid current init accepted", mode)
				}
				r.Store.BeforeCommit = nil
				for _, table := range []string{"harness_rehearsal_worlds", "runtime_instances", "actors", "items", "memory_aggregates"} {
					var n int
					if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil || n != 0 {
						t.Fatal("partial initialization", table, n, e)
					}
				}
				return
			}
			if mode == "ROLLBACK" || mode == "OLD_FENCE" {
				if e == nil {
					t.Fatal("closed initialization permitted", mode)
				}
				r.Store.BeforeCommit = nil
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_rehearsal_worlds").Scan(&n); e != nil || n != 0 {
					t.Fatal(n, e)
				}
				lease.TaskLease = old
				_, e = r.Store.InitializeDraftRehearsal(ctx, lease.TaskLease, "rehearsal-worker")
			}
			if mode == "COMMIT_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				r.Store.Commit = nil
				_, e = r.Store.InitializeDraftRehearsal(ctx, lease.TaskLease, "rehearsal-worker")
			}
			if e != nil {
				t.Fatal("initialize actual isolated world", e)
			}
			world := content.Identity(string(root.TaskID), "rehearsal-world")
			version := content.Identity(string(root.TaskID), "rehearsal-version")
			var actors, items, published, consent, steps int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM actors WHERE world_instance_id=?", world).Scan(&actors); e != nil || actors != 2 {
				t.Fatal(actors, e)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM items WHERE world_instance_id=?", world).Scan(&items); e != nil || items != 1 {
				t.Fatal(items, e)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM content_releases WHERE release_id=?", version).Scan(&published); e != nil || published != 0 {
				t.Fatal("fake published release", published, e)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&consent); e != nil || consent != 0 {
				t.Fatal(consent, e)
			}
			var raw []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", world).Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var aggregate memory.Aggregate
			if e = json.Unmarshal(raw, &aggregate); e != nil {
				t.Fatal(e)
			}
			if len(aggregate.Sources) != 4 || len(aggregate.Acquisitions) != 4 {
				t.Fatal("INITIAL graph", len(aggregate.Sources), len(aggregate.Acquisitions))
			}
			for _, source := range aggregate.Sources {
				if source.Domain != memory.Sandbox {
					t.Fatal("source is not synthetic", source.Domain)
				}
			}
			for _, a := range aggregate.Acquisitions {
				if a.Kind != memory.Initial || a.EvidenceID != content.Identity(string(root.TaskID), "rehearsal-init") {
					t.Fatal("fake creation evidence", a)
				}
			}
			if _, e = r.World.GetInstance(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: world}); e == nil {
				t.Fatal("ordinary instance read exposed isolation")
			}
			page, e := r.World.ListInstances(ctx, scope, 20, "")
			if e != nil || len(page.Items) != 0 {
				t.Fatal("ordinary instance page leaked isolation", page, e)
			}
			if _, e = r.Content.GetRelease(ctx, scope, version); e == nil {
				t.Fatal("ordinary publication lookup exposed internal version")
			}
			if _, e = r.Store.InitializeDraftRehearsal(ctx, lease.TaskLease, "rehearsal-worker"); e != nil {
				t.Fatal("init replay", e)
			}
			if e = r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&steps); e != nil || steps != 1 {
				t.Fatal("init step reset/duplicate", steps, e)
			}
		})
	}
}

func TestRehearsalRootIntake(t *testing.T) {
	for _, mode := range []string{"NORMAL20", "ROLLBACK", "COMMIT_ACK", "SOURCE_DELETE"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":"synthetic source only"}],"max_cost_units":"0","assertions":["ISOLATED","SIGN_CUSTODY"]}`), &req); e != nil {
				t.Fatal(e)
			}
			if mode == "NORMAL20" {
				for len(req.Inputs) < 20 {
					req.Inputs = append(req.Inputs, req.Inputs[0])
				}
			}
			if mode == "ROLLBACK" {
				r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return errors.New("rollback before root receipt") }
			}
			if mode == "COMMIT_ACK" {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("original root commit ACK lost")
				}
			}
			out, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "root", req)
			if mode == "ROLLBACK" {
				if e == nil {
					t.Fatal("root rollback failed")
				}
				r.Store.BeforeCommit = nil
				for _, table := range []string{"harness_admin_rehearsals", "harness_rehearsal_input_sources", "harness_envelopes", "reservations"} {
					var n int
					if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil || n != 0 {
						t.Fatal(table, n, e)
					}
				}
				out, e = r.Store.AcceptDraftRehearsal(ctx, scope, draft, "root", req)
			} else if mode == "COMMIT_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				r.Store.Commit = nil
				out, e = r.Store.AcceptDraftRehearsal(ctx, scope, draft, "root", req)
			}
			if e != nil {
				t.Fatal("typed root intake", e)
			}
			var calls, steps, seconds, settle, allocated, used, holds, taskCalls, consent int
			if e = r.Store.DB.QueryRow("SELECT max_calls,max_steps,TIMESTAMPDIFF(SECOND,accepted_at,execute_by),TIMESTAMPDIFF(SECOND,accepted_at,settle_by),calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", out.TaskID).Scan(&calls, &steps, &seconds, &settle, &allocated, &used); e != nil {
				t.Fatal(e)
			}
			n := len(req.Inputs)
			if calls != 2*n || steps != 2+20*n || seconds != 30+60*n || settle != 90+60*n || allocated != 0 || used != 0 {
				t.Fatal(calls, steps, seconds, settle, allocated, used)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=?", out.TaskID).Scan(&holds); e != nil || holds != 3 {
				t.Fatal("exact three parent holds", holds, e)
			}
			if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", out.TaskID).Scan(&taskCalls); e != nil || taskCalls != 0 {
				t.Fatal(taskCalls, e)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&consent); e != nil || consent != 0 {
				t.Fatal(consent, e)
			}
			if mode == "SOURCE_DELETE" {
				impact, e := r.Content.Impact(ctx, scope, draft, 1, "DELETE", "delete-preview")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Content.Command(ctx, scope, draft, 1, "DELETE", string(impact.ReportID), "", "", "delete-root"); e != nil {
					t.Fatal(e)
				}
				var left int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_rehearsal_input_sources WHERE task_id=?", out.TaskID).Scan(&left); e != nil || left != 0 {
					t.Fatal("governed input was not purged", left, e)
				}
			}
			replay, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "root", req)
			if e != nil || replay.TaskID != out.TaskID {
				t.Fatal("original metadata replay", replay, e)
			}
			req.Inputs[0].Text = "different original payload"
			if _, e = r.Store.AcceptDraftRehearsal(ctx, scope, draft, "root", req); !errors.Is(e, store.ErrConflict) {
				t.Fatal("same key different payload", e)
			}
		})
	}
}

func TestRehearsalCurrentDraftQualification(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	var world generated.WorldDefinition
	for _, raw := range d.Input.Objects {
		var h struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &h) == nil && h.Kind == "WORLD" {
			if e = generated.Decode("WorldDefinition", raw, &world); e != nil {
				t.Fatal(e)
			}
		}
	}
	var request generated.RehearsalRequest
	b, _ := json.Marshal(map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "mode": "MOCK", "fixture": "COFFEE", "max_cost_units": "0", "inputs": []any{map[string]string{"actor_slot": string(world.InitialActorSetups[0].SlotID), "text": ""}}, "assertions": []string{"ISOLATED", "ISOLATED"}})
	if e = generated.Decode("RehearsalRequest", b, &request); e != nil {
		t.Fatal(e)
	}
	var before, after int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts").Scan(&before); e != nil {
		t.Fatal(e)
	}
	material, e := r.Content.InspectRehearsal(ctx, scope, draft, request)
	if e != nil {
		t.Fatal("real owned draft, empty text and duplicate assertions are legal", e)
	}
	if material.Draft.DraftID != d.DraftID || material.Draft.ContentHash != d.ContentHash || len(material.Sources) < 2 || len(material.SourceHash) != 64 || len(material.RouteHash) != 64 {
		t.Fatal(material)
	}
	request.Inputs = nil
	if _, e = r.Content.InspectRehearsal(ctx, scope, draft, request); e == nil {
		t.Fatal("zero inputs accepted")
	}
	if e = generated.Decode("RehearsalRequest", b, &request); e != nil {
		t.Fatal(e)
	}
	request.Inputs[0].ActorSlot = "unrelated-slot"
	if _, e = r.Content.InspectRehearsal(ctx, scope, draft, request); e == nil {
		t.Fatal("unknown actor slot accepted")
	}
	if e = generated.Decode("RehearsalRequest", b, &request); e != nil {
		t.Fatal(e)
	}
	first := request.Inputs[0]
	for len(request.Inputs) < 20 {
		request.Inputs = append(request.Inputs, first)
	}
	if _, e = r.Content.InspectRehearsal(ctx, scope, draft, request); e != nil {
		t.Fatal("twenty inputs rejected", e)
	}
	request.Inputs = append(request.Inputs, first)
	if _, e = r.Content.InspectRehearsal(ctx, scope, draft, request); e == nil {
		t.Fatal("twenty one inputs accepted")
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts").Scan(&after); e != nil || before != after {
		t.Fatal("qualification wrote a receipt", before, after, e)
	}
	var consent, worlds int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&consent); e != nil || consent != 0 {
		t.Fatal("synthetic qualification granted private generation", consent, e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM runtime_instances").Scan(&worlds); e != nil || worlds != 0 {
		t.Fatal("pure qualification created a world", worlds, e)
	}
}

func TestRehearsalAuthoringBasisAndRights(t *testing.T) {
	for _, mode := range []string{"EDIT_ONLY", "EXPIRED_SOURCE", "SOURCE_CYCLE", "ROUTE_NONE", "NO_REHEARSE", "CURRENT_BASIS"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			d, e := r.Content.GetDraft(ctx, scope, draft, "")
			if e != nil {
				t.Fatal(e)
			}
			var request generated.RehearsalRequest
			if e = generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"2","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &request); e != nil {
				t.Fatal(e)
			}
			for i, raw := range d.Input.Objects {
				var object map[string]any
				if e = json.Unmarshal(raw, &object); e != nil {
					t.Fatal(e)
				}
				if mode == "EDIT_ONLY" {
					rights := object["rights"].(map[string]any)
					rights["allowed_uses"] = []string{"EDIT"}
					rights["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				}
				if object["object_id"] == "coffee-world" {
					object["description"] = "actual changed requested draft basis"
					if mode == "ROUTE_NONE" {
						object["generation"] = map[string]any{"mode": "NONE", "required_capabilities": []any{}, "allowed_external_uses": []string{"NONE"}}
					}
					if mode == "EXPIRED_SOURCE" {
						object["rights"].(map[string]any)["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
					}
				}
				if mode == "SOURCE_CYCLE" && object["object_id"] == "alex-character" {
					object["dependency_refs"] = []any{map[string]string{"kind": "LOCAL_DRAFT", "object_id": "mia-character"}}
				}
				if mode == "SOURCE_CYCLE" && object["object_id"] == "mia-character" {
					object["dependency_refs"] = []any{map[string]string{"kind": "LOCAL_DRAFT", "object_id": "alex-character"}}
				}
				d.Input.Objects[i], e = json.Marshal(object)
				if e != nil {
					t.Fatal(e)
				}
			}
			saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 1, "actual-basis")
			if e != nil {
				t.Fatal("valid editable draft fixture", e)
			}
			if mode == "NO_REHEARSE" {
				if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'"); e != nil {
					t.Fatal(e)
				}
			}
			m, e := r.Content.InspectRehearsal(ctx, scope, draft, request)
			if mode == "CURRENT_BASIS" || mode == "EDIT_ONLY" {
				if e != nil {
					t.Fatal("current authoring basis qualified", e)
				}
				if m.Draft.ContentHash != saved.ContentHash || m.Draft.DraftRevision != "2" {
					t.Fatal("global coffee basis substituted", m.Draft.DraftRevision)
				}
				request.ExpectedDraftRevision = "1"
				if _, e = r.Content.InspectRehearsal(ctx, scope, draft, request); e == nil {
					t.Fatal("old revision accepted")
				}
			} else if e == nil {
				t.Fatal("unqualified authoring basis accepted", mode)
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_rehearsals").Scan(&n); e != nil || n != 0 {
				t.Fatal("pure qualification accepted task", n, e)
			}
		})
	}
}

func TestRehearsalHTTPFrozenIsolation(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t) // real owner consent remains false
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(runtimeContext(t, r), scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	var slot string
	for _, raw := range d.Input.Objects {
		var h struct {
			Kind    string `json:"kind"`
			Initial []struct {
				Slot string `json:"slot_id"`
			} `json:"initial_actor_setups"`
		}
		if e = json.Unmarshal(raw, &h); e != nil {
			t.Fatal(e)
		}
		if h.Kind == "WORLD" && len(h.Initial) > 0 {
			slot = h.Initial[0].Slot
		}
	}
	if slot == "" {
		t.Fatal("fixture lacks actual actor slot")
	}
	body := map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "mode": "MOCK", "fixture": "COFFEE", "inputs": []any{map[string]string{"actor_slot": slot, "text": "Hello in this isolated rehearsal."}}, "max_cost_units": "0", "assertions": []string{"ISOLATED", "RIGHTS", "DUTIES", "SIGN_CUSTODY"}}
	var accepted, replay generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/rehearsals", "original-rehearsal", body, 202, &accepted)
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/rehearsals", "original-rehearsal", body, 202, &replay)
	if accepted.TaskID != replay.TaskID || accepted.TaskKind != "REHEARSAL" {
		t.Fatal(accepted, replay)
	}
	if e = r.Worker.Once(context.Background()); e != nil {
		t.Fatal("registered rehearsal worker", e)
	}
	var view generated.AdminTaskView
	runtimeHTTP(t, r, "GET", "/tasks/"+string(accepted.TaskID)+"?product_id=coffee", "", nil, 200, &view)
	if view.TaskKind != "REHEARSAL" || view.TaskState != "COMMITTED" || view.ResultAssetID == nil {
		t.Fatal(view)
	}
	var consent int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&consent); e != nil || consent != 0 {
		t.Fatal("rehearsal fabricated private consent", consent, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action<>'task.read'"); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/tasks/"+string(accepted.TaskID)+"?product_id=coffee", "", nil, 200, &view)
	var child string
	if e = r.Store.DB.QueryRow("SELECT child_task_id FROM harness_rehearsal_children WHERE root_task_id=?", accepted.TaskID).Scan(&child); e != nil {
		t.Fatal(e)
	}
	runtimeHTTP(t, r, "GET", "/tasks/"+child+"?product_id=coffee", "", nil, 404, nil)
	var page generated.AdminTaskPage
	runtimeHTTP(t, r, "GET", "/tasks?product_id=coffee&task_id="+child, "", nil, 200, &page)
	if len(page.Items) != 0 {
		t.Fatal("isolated child escaped safe list", page)
	}
}
