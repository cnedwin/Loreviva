package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

func TestOnlineSkillPinnedDefiningBindingBeforeProcurement(t *testing.T) {
	for _, mode := range []string{"VALID", "SCHEMA", "CONSUMER_AUTH"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			schema := "GenerationMaterial"
			if mode == "SCHEMA" {
				schema = "TurnRequest"
			}
			r, draft, req := skillRehearsalFixture(t, schema)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			d, e := r.Content.GetDraft(ctx, scope, draft, "")
			if e != nil {
				t.Fatal(e)
			}
			check, e := r.Content.CheckPublish(ctx, scope, draft, 2, []string{"MOCK_DEMO"}, "pinned-skill-check")
			if e != nil || !check.Passed {
				t.Fatal(check, e)
			}
			rel, e := r.Content.Publish(ctx, scope, draft, 2, string(check.ReportID), []string{"MOCK_DEMO"}, "pinned-skill-publish")
			if e != nil {
				t.Fatal(e)
			}
			var role string
			for _, b := range d.Input.ModuleAgentBindings {
				if b.ModuleID == "REHEARSAL" {
					var ref struct {
						ObjectID string `json:"object_id"`
					}
					json.Unmarshal(b.RoleRef, &ref)
					role = ref.ObjectID
				}
			}
			var hash, version string
			for _, raw := range d.Input.Objects {
				var obj map[string]any
				json.Unmarshal(raw, &obj)
				if obj["object_id"] == role {
					hash, e = content.Hash(json.RawMessage(raw))
					version = obj["version"].(string)
					if e != nil {
						t.Fatal(e)
					}
				}
			}
			if hash == "" {
				t.Fatal("actual ROLE missing")
			}
			ref, _ := json.Marshal(map[string]any{"kind": "PINNED_DEFINITION", "release_id": rel.ReleaseID, "object_id": role, "version": version, "content_hash": hash})
			d.Input.DependencyLocks = append(d.Input.DependencyLocks, generated.DependencyLock{ReleaseID: rel.ReleaseID, ObjectID: generated.Identity(role), Version: generated.ExactUnsigned(version), ContentHash: hash})
			for i, b := range d.Input.ModuleAgentBindings {
				if b.ModuleID == "REHEARSAL" {
					d.Input.ModuleAgentBindings[i].RoleRef = ref
				}
			}
			d.Input.SkillBindings = []generated.SkillBinding{}
			consumer, e := r.Content.Create(ctx, scope, "WORLD", "", d.Input, "pinned-skill-consumer")
			if e != nil {
				t.Fatal(e)
			}
			req.ExpectedDraftRevision = consumer.DraftRevision
			var before, after int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&before); e != nil {
				t.Fatal(e)
			}
			if mode == "CONSUMER_AUTH" {
				if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'"); e != nil {
					t.Fatal(e)
				}
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, string(consumer.DraftID), "pinned-skill-rehearse", req)
			if mode == "SCHEMA" || mode == "CONSUMER_AUTH" {
				expected := content.ErrBlocked
				if mode == "CONSUMER_AUTH" {
					expected = store.ErrDenied
				}
				if !errors.Is(e, expected) {
					t.Fatal("incompatible defining-release Skill procured", root, e)
				}
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&after); e != nil || after != before {
					t.Fatal("rejected preflight changed holds", before, after, e)
				}
				var n int
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_rehearsals").Scan(&n); e != nil || n != 0 {
					t.Fatal("rejected preflight intake", n, e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			var calls, steps, inv int
			if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used,(SELECT COUNT(*) FROM harness_skill_invocations) FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&calls, &steps, &inv); e != nil || calls != 1 || steps != 13 || inv != 1 {
				t.Fatal("actual pinned defining binding", calls, steps, inv, e)
			}
		})
	}
}

func TestGenerativeSkillEditingCandidateUsesExplicitModuleRoute(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := skillRehearsalFixture(t, "GenerationMaterial")
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	request := generated.CandidateRequest{ProductID: "coffee", ExpectedDraftRevision: d.DraftRevision, Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: "online-text", SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
	// NONE has no implicit fallback. The actual module override is a separate
	// editing producer, not a new generator for the online method.
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, draft, "skill-editor-none", request); !errors.Is(e, content.ErrBlocked) {
		t.Fatal("NONE editing route", e)
	}
	var role generated.DefinitionRef
	for _, b := range d.Input.ModuleAgentBindings {
		if b.ModuleID == "REHEARSAL" {
			role = b.RoleRef
		}
	}
	connection, model := generated.Identity("mock-local"), generated.Identity("synthetic-text-v1")
	d.Input.ModuleAgentBindings = append(d.Input.ModuleAgentBindings, generated.ModuleAgentBinding{BindingID: "skill-editor", OwnerObjectID: "online-text", ModuleID: "AUTHORING", RoleRef: role, Enabled: true, Generation: generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: &connection, ModelID: &model, RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}})
	saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 2, "skill-editor-save")
	if e != nil {
		t.Fatal(e)
	}
	request.ExpectedDraftRevision = saved.DraftRevision
	var resolution generated.DraftGenerationResolution
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/generation-resolution", "", generated.DraftGenerationResolutionRequest{ProductID: "coffee", ExpectedDraftRevision: saved.DraftRevision, ObjectID: "online-text", ModuleID: "AUTHORING"}, 200, &resolution)
	var receipt generated.AdminTaskReceipt
	runtimeHTTP(t, r, "POST", "/content-drafts/"+draft+"/candidates", "skill-editor-candidate", request, 202, &receipt)
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	b, e := r.Files.ReadApprovedDraftCandidate(ctx, scope, string(receipt.TaskID))
	if e != nil || !bytes.Contains(b, []byte("description")) {
		t.Fatal("real editing candidate", e)
	}
	var calls, steps, inv, commits int
	if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used,(SELECT COUNT(*) FROM harness_skill_invocations),(SELECT COUNT(*) FROM commit_receipts WHERE operation='CANDIDATE_COMMIT') FROM harness_envelopes WHERE task_id=?", receipt.TaskID).Scan(&calls, &steps, &inv, &commits); e != nil || calls != 1 || steps != 9 || inv != 0 || commits != 1 {
		t.Fatal("editing is not an online invocation", calls, steps, inv, commits, e)
	}
	request.SelectedFields = []string{"tool_ids"}
	if _, e = r.Store.AcceptDraftCandidate(ctx, scope, draft, "skill-editor-unsupported", request); !errors.Is(e, content.ErrBlocked) {
		t.Fatal("unsupported field fallback", e)
	}
	after, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil || after.DraftRevision != saved.DraftRevision {
		t.Fatal("auto Save", e)
	}
}

func TestOnlineSkillActualDeclarationAndSourcesBeforePurchase(t *testing.T) {
	for _, mode := range []string{"VALID", "FAILURE_POLICY", "TOOLS", "DEPTH", "CYCLE", "USE", "EXPIRY", "ROUTE"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, req := skillRehearsalFixture(t, "GenerationMaterial")
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			d, e := r.Content.GetDraft(ctx, scope, draft, "")
			if e != nil {
				t.Fatal(e)
			}
			for i, raw := range d.Input.Objects {
				var obj map[string]any
				json.Unmarshal(raw, &obj)
				if obj["object_id"] != "online-text" {
					continue
				}
				switch mode {
				case "FAILURE_POLICY":
					obj["failure_policy"] = "RETURN_CANDIDATE"
				case "TOOLS":
					obj["tool_ids"] = []string{"TEXT"}
				case "DEPTH":
					obj["max_depth"] = 0
				case "CYCLE":
					obj["dependency_refs"] = []any{map[string]any{"kind": "LOCAL_DRAFT", "object_id": "online-text"}}
				case "USE":
					obj["rights"].(map[string]any)["allowed_uses"] = []string{"PUBLISH"}
				case "EXPIRY":
					obj["rights"].(map[string]any)["expires_at"] = "2000-01-01T00:00:00Z"
				}
				d.Input.Objects[i], _ = json.Marshal(obj)
			}
			if mode == "ROUTE" {
				for i, b := range d.Input.ModuleAgentBindings {
					if b.ModuleID == "REHEARSAL" {
						d.Input.ModuleAgentBindings[i].Generation = generated.GenerationSettings{Mode: "NONE", RequiredCapabilities: []generated.Identity{}, AllowedExternalUses: []string{"NONE"}}
					}
				}
			}
			saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 2, "skill-qualification-save")
			if e != nil {
				t.Fatal("real schema-valid saved declaration", e)
			}
			req.ExpectedDraftRevision = saved.DraftRevision
			var tasks, holds int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM reservations)").Scan(&tasks, &holds); e != nil {
				t.Fatal(e)
			}
			_, e = r.Store.AcceptDraftRehearsal(ctx, scope, draft, "skill-qualification-intake", req)
			if mode == "VALID" {
				if e != nil {
					t.Fatal("positive declaration", e)
				}
				return
			}
			var fields *content.FieldErrors
			if !errors.Is(e, content.ErrBlocked) && !(mode == "CYCLE" && errors.As(e, &fields)) {
				t.Fatal("specific skill declaration/source gate", mode, e)
			}
			var afterTasks, afterHolds int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM reservations)").Scan(&afterTasks, &afterHolds); e != nil || tasks != afterTasks || holds != afterHolds {
				t.Fatal("rejected declaration purchased", tasks, afterTasks, holds, afterHolds, e)
			}
		})
	}
}
