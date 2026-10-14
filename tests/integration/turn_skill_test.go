package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"testing"
	"time"
)

func skillRehearsalFixture(t *testing.T, inputSchema string, ordinary ...bool) (*app.Runtime, string, generated.RehearsalRequest) {
	t.Helper()
	ctx := context.Background()
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var r *app.Runtime
	var draft string
	if len(ordinary) > 0 && ordinary[0] {
		var release string
		r, _, release = runtimeFixtureWithConsent(t, true)
		ctx = runtimeContext(t, r)
		p, e := r.Content.GetRelease(ctx, scope, release)
		if e != nil {
			t.Fatal(e)
		}
		d, e := r.Content.Create(ctx, scope, "WORLD", "", p.Input, "skill-draft")
		if e != nil {
			t.Fatal(e)
		}
		draft = string(d.DraftID)
	} else {
		r, draft, _ = candidateHTTPFixture(t)
	}
	saved := attachOnlineTextSkill(t, r, draft, inputSchema)
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"2","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	req.ExpectedDraftRevision = saved
	return r, draft, req
}

func attachOnlineTextSkill(t *testing.T, r *app.Runtime, draft, inputSchema string) generated.ExactUnsigned {
	t.Helper()
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	var role string
	for _, b := range d.Input.ModuleAgentBindings {
		if b.Enabled && b.ModuleID == "REHEARSAL" && b.OwnerObjectID == d.Input.RootObjectID {
			var ref struct {
				ObjectID string `json:"object_id"`
			}
			if json.Unmarshal(b.RoleRef, &ref) != nil {
				t.Fatal("role ref")
			}
			role = ref.ObjectID
		}
	}
	if role == "" {
		for _, raw := range d.Input.Objects {
			var head struct {
				Kind     string `json:"kind"`
				ObjectID string `json:"object_id"`
			}
			json.Unmarshal(raw, &head)
			if head.Kind == "ROLE" {
				role = head.ObjectID
				break
			}
		}
		if role == "" {
			t.Fatal("actual role missing")
		}
		roleRef, _ := json.Marshal(map[string]string{"kind": "LOCAL_DRAFT", "object_id": role})
		connection, model := generated.Identity("mock-local"), generated.Identity("synthetic-text-v1")
		d.Input.ModuleAgentBindings = append(d.Input.ModuleAgentBindings, generated.ModuleAgentBinding{BindingID: "online-rehearsal-role", OwnerObjectID: d.Input.RootObjectID, ModuleID: "REHEARSAL", RoleRef: roleRef, Enabled: true, Generation: generated.GenerationSettings{Mode: "OVERRIDE", ConnectionRef: &connection, ModelID: &model, RequiredCapabilities: []generated.Identity{"TEXT"}, AllowedExternalUses: []string{"NONE"}}})
	}
	raw := []byte(`{"object_id":"online-text","kind":"SKILL","name":"Original text dispatch","description":"","tags":[],"rights":{"origin_kind":"SYNTHETIC","source_label":"owned fixture","license":"Apache-2.0","allowed_uses":["EDIT","PUBLISH"]},"audience_policy":"PRODUCT_EDITORS","version":"1","source_refs":[],"dependency_refs":[],"builtin_original":false,"implementation_kind":"GENERATIVE","input_schema_id":"GenerationMaterial","output_schema_id":"DispatchAuthorization","registered_capability_ids":["TEXT"],"tool_ids":[],"conflicting_refs":[],"failure_policy":"BLOCK","max_depth":1,"max_duration_ms":"60000","max_cost_units":"0","synthetic_examples":[],"generation":{"mode":"NONE","required_capabilities":[],"allowed_external_uses":["NONE"]}}`)
	var skill map[string]any
	json.Unmarshal(raw, &skill)
	skill["description"] = "Use the original actor voice with the current shared budget."
	skill["input_schema_id"] = inputSchema
	raw, _ = json.Marshal(skill)
	d.Input.Objects = append(d.Input.Objects, raw)
	ref := generated.DefinitionRef([]byte(`{"kind":"LOCAL_DRAFT","object_id":"online-text"}`))
	for n, raw := range d.Input.Objects {
		var obj map[string]any
		json.Unmarshal(raw, &obj)
		if obj["object_id"] == role {
			obj["skill_refs"] = []generated.DefinitionRef{ref}
			d.Input.Objects[n], _ = json.Marshal(obj)
		}
	}
	d.Input.SkillBindings = append(d.Input.SkillBindings, generated.SkillBinding{BindingID: "online-text-binding", BudgetOwner: "PARENT", Enabled: true, Order: 0, RoleObjectID: generated.Identity(role), SkillRef: ref, Trigger: "TURN"})
	revision, e := strconv.ParseUint(string(d.DraftRevision), 10, 64)
	if e != nil {
		t.Fatal(e)
	}
	saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, revision, "skill-basis")
	if e != nil {
		t.Fatal(e)
	}
	return saved.DraftRevision
}

func TestOnlineSkillActualPublishedTurn(t *testing.T) {
	r, draft, _ := skillRehearsalFixture(t, "GenerationMaterial", true)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	var binding generated.ModuleAgentBinding
	for _, b := range d.Input.ModuleAgentBindings {
		if b.ModuleID == "REHEARSAL" && b.Enabled {
			binding = b
		}
	}
	for i, b := range d.Input.ModuleAgentBindings {
		if b.ModuleID == "INTERACTION" {
			d.Input.ModuleAgentBindings[i].RoleRef = binding.RoleRef
			d.Input.ModuleAgentBindings[i].Enabled = true
			d.Input.ModuleAgentBindings[i].Generation = binding.Generation
		}
	}
	for _, raw := range d.Input.Objects {
		var obj struct {
			ObjectID string `json:"object_id"`
			Kind     string `json:"kind"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			t.Fatal("object")
		}
		if obj.Kind != "CHARACTER" {
			continue
		}
		found := false
		for _, b := range d.Input.ModuleAgentBindings {
			found = found || b.ModuleID == "INTERACTION" && string(b.OwnerObjectID) == obj.ObjectID
		}
		if !found {
			b := binding
			b.BindingID = generated.Identity(obj.ObjectID + "-online-role")
			b.ModuleID = "INTERACTION"
			b.OwnerObjectID = generated.Identity(obj.ObjectID)
			d.Input.ModuleAgentBindings = append(d.Input.ModuleAgentBindings, b)
		}
	}
	if _, e = r.Content.SaveDraft(ctx, scope, draft, d.Input, 2, "ordinary-skill-save"); e != nil {
		t.Fatal(e)
	}
	check, e := r.Content.CheckPublish(ctx, scope, draft, 3, []string{"MOCK_DEMO"}, "ordinary-skill-check")
	if e != nil || !check.Passed {
		t.Fatal(check, e)
	}
	release, e := r.Content.Publish(ctx, scope, draft, 3, string(check.ReportID), []string{"MOCK_DEMO"}, "ordinary-skill-publish")
	if e != nil {
		t.Fatal(e)
	}
	i := runtimeControl(t, r, createRuntimeWorld(t, r, string(release.ReleaseID), "ordinary-skill-world"), "START")
	ws := domain.Scope{ProductScope: scope, WorldInstanceID: i.Scope.WorldInstanceID}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", ws.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var init world.Initialization
	if e = json.Unmarshal(raw, &init); e != nil {
		t.Fatal(e)
	}
	actor := init.Actors[0].ID
	q, e := r.Turns.QuoteTurn(ctx, ws, generated.QuoteRequest{Scope: i.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}, "ordinary-skill-quote")
	if e != nil {
		t.Fatal(e)
	}
	a, e := r.Turns.SubmitTurn(ctx, ws, generated.TurnRequest{QuoteID: q.QuoteID, ExpectedVersions: generated.ExpectedVersions{WorldRevision: i.Versions.WorldRevision, ControlVersion: i.Versions.ControlVersion}, PrimaryActorID: actor, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"Hello"}`)}, "ordinary-skill-turn")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var calls, steps, count int
	if e = r.Store.DB.QueryRow("SELECT calls_used,e.steps_used,(SELECT COUNT(*) FROM harness_skill_invocations WHERE task_id=t.task_id) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", a.TurnID).Scan(&calls, &steps, &count); e != nil || calls != 1 || steps != 11 || count != 1 {
		t.Fatal("ordinary exact saved binding", calls, steps, count, e)
	}
	view, e := r.Turns.GetTurn(ctx, ws, string(a.TurnID))
	if e != nil || len(view.ApprovedPackets) != 1 {
		t.Fatal("original approved result", e)
	}
}

func skillPrefix(t *testing.T) (context.Context, *app.Runtime, domain.TaskLease, domain.TaskLease, store.DispatchRequest, domain.GenerationMaterial) {
	return skillPrefixDuration(t, "")
}
func skillPrefixDuration(t *testing.T, duration string) (context.Context, *app.Runtime, domain.TaskLease, domain.TaskLease, store.DispatchRequest, domain.GenerationMaterial) {
	ctx, r, root, child, d, m, close := skillPrefixOwned(t, duration)
	t.Cleanup(close)
	return ctx, r, root, child, d, m
}
func skillPrefixOwned(t *testing.T, duration string) (context.Context, *app.Runtime, domain.TaskLease, domain.TaskLease, store.DispatchRequest, domain.GenerationMaterial, func()) {
	t.Helper()
	r, draft, req := skillRehearsalFixture(t, "GenerationMaterial")
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	if duration != "" {
		d, e := r.Content.GetDraft(ctx, scope, draft, "")
		if e != nil {
			t.Fatal(e)
		}
		for n, raw := range d.Input.Objects {
			var obj map[string]any
			json.Unmarshal(raw, &obj)
			if obj["object_id"] == "online-text" {
				obj["max_duration_ms"] = duration
				d.Input.Objects[n], _ = json.Marshal(obj)
			}
		}
		saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 2, "actual-skill-duration")
		if e != nil {
			t.Fatal(e)
		}
		req.ExpectedDraftRevision = saved.DraftRevision
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "skill-prefix", req)
	if e != nil {
		t.Fatal(e)
	}
	rl, e := r.Store.ClaimRegistered(ctx, "skill-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, "skill-worker"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.StartRehearsalChild(ctx, rl.TaskLease, "skill-worker"); e != nil {
		t.Fatal(e)
	}
	cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, "skill-worker")
	if e != nil {
		t.Fatal(e)
	}
	m, e := r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, "skill-worker")
	if e != nil {
		t.Fatal(e)
	}
	d, e := r.Store.PrepareRehearsalTurn(ctx, rl.TaskLease, cl.TaskLease, "skill-worker")
	if e != nil {
		t.Fatal(e)
	}
	ctx, close, e := r.Store.EnterRehearsalChildExecution(ctx, rl.TaskLease, cl.TaskLease, "skill-worker")
	if e != nil {
		t.Fatal(e)
	}
	return ctx, r, rl.TaskLease, cl.TaskLease, d, m, close
}

func TestOnlineSkillDrainOwnsActualInvocation(t *testing.T) {
	ctx, r, _, child, d, _, close := skillPrefixOwned(t, "")
	defer close()
	closing, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	drained := make(chan error, 1)
	r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
		go func() {
			e := r.Local.Coordinator.BeginClose(closing, "skill-current-c")
			if e == nil {
				e = r.Local.Coordinator.Drain(closing, "skill-current-c")
			}
			drained <- e
		}()
		select {
		case <-drained:
			return errors.New("drain crossed live Skill owning Tx")
		case <-time.After(80 * time.Millisecond):
		}
		return nil
	}
	_, e := r.Store.AuthorizeDispatch(ctx, d)
	r.Store.BeforeCommit = nil
	close()
	select {
	case finish := <-drained:
		if finish != nil {
			t.Fatal("owning Skill drain", finish)
		}
	case <-closing.Done():
		t.Fatal("Skill admission leaked", closing.Err())
	}
	var inv, calls int
	if q := r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_skill_invocations WHERE task_id=?) FROM tasks WHERE task_id=?", child.TaskID, child.TaskID).Scan(&calls, &inv); q != nil {
		t.Fatal(q)
	}
	if e == nil {
		if inv != 1 || calls != 1 {
			t.Fatal("known transaction", inv, calls)
		}
	} else if inv != 0 || calls != 0 {
		t.Fatal("closing partial Skill transaction", e, inv, calls)
	}
	if _, e = r.Store.AuthorizeDispatch(context.Background(), d); e == nil {
		t.Fatal("closed installation allowed original Skill replay")
	}
}

func TestOnlineSkillActualElapsedDeadline(t *testing.T) {
	ctx, r, _, child, d, _ := skillPrefixDuration(t, "1000")
	if _, e := r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	var start, end time.Time
	if e := r.Store.DB.QueryRow("SELECT started_at,deadline FROM harness_skill_invocations WHERE task_id=?", child.TaskID).Scan(&start, &end); e != nil || end.Sub(start) != time.Second {
		t.Fatal(start, end, e)
	}
	time.Sleep(1100 * time.Millisecond)
	d.CommandKey = child.TaskID + "-elapsed-send"
	d.CommitKey = d.CommandKey
	if _, e := r.Store.StartSend(ctx, d, d.AttemptID); !errors.Is(e, store.ErrDenied) {
		t.Fatal("actual elapsed Skill bound", e)
	}
	var pending, valid int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*),(SELECT COUNT(*) FROM harness_envelopes WHERE task_id=? AND execute_by>UTC_TIMESTAMP(6)) FROM attempts WHERE task_id=? AND send_started_at IS NULL", child.TaskID, child.TaskID).Scan(&pending, &valid); e != nil || pending != 1 || valid != 1 {
		t.Fatal("wrong timeout gate", pending, valid, e)
	}
}

func TestOnlineSkillCanonicalAndFinisher(t *testing.T) {
	for _, mode := range []string{"CONTENT", "FINISH", "FINISH_RECEIPT"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, _, child, d, m := skillPrefix(t)
			if mode == "CONTENT" {
				if !bytes.Contains(m.Canonical, []byte("Use the original actor voice with the current shared budget.")) {
					t.Fatal("real saved Skill content omitted from sole original canonical")
				}
				return
			}
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				if mode == "FINISH_RECEIPT" {
					_, e := tx.ExecContext(ctx, "UPDATE commit_receipts SET operation='NOT_ORIGINAL_DISPATCH' WHERE phase='AUTHORIZE' AND task_id=?", child.TaskID)
					return e
				}
				_, e := tx.ExecContext(ctx, "UPDATE harness_skill_invocations SET output_hash=UNHEX(REPEAT('ab',32)) WHERE task_id=?", child.TaskID)
				return e
			}
			if _, e := r.Store.AuthorizeDispatch(ctx, d); e == nil {
				t.Fatal("corrupted typed Skill output committed at finish")
			}
			r.Store.BeforeCommit = nil
			var n int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=?", child.TaskID).Scan(&n); e != nil || n != 0 {
				t.Fatal("failed Skill left partial purchase", n, e)
			}
		})
	}
}

func TestOnlineSkillRejectsIncompatibleSavedSchemaBeforeIntake(t *testing.T) {
	r, draft, req := skillRehearsalFixture(t, "TurnRequest")
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	if _, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "bad-skill-schema", req); e == nil {
		t.Fatal("unregistered typed Skill signature accepted for execution")
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_rehearsals").Scan(&n); e != nil || n != 0 {
		t.Fatal("unsupported Skill created task/holds", n, e)
	}
}

func TestOnlineSkillPersistedEvidenceAtSend(t *testing.T) {
	for _, mode := range []string{"VALID", "OUTPUT", "DEPTH", "RECEIPT", "DEADLINE", "MISSING", "OPERATION", "ADAPTER_MISSING", "ADAPTER_HASH"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, _, child, d, _ := skillPrefix(t)
			if _, e := r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			query := "UPDATE harness_skill_invocations SET output_hash=UNHEX(REPEAT('ab',32)) WHERE task_id=?"
			switch mode {
			case "OPERATION":
				query = "UPDATE commit_receipts SET operation='NOT_ORIGINAL_DISPATCH' WHERE phase='AUTHORIZE' AND task_id=?"
			case "ADAPTER_MISSING":
				query = "DELETE FROM commit_receipts WHERE operation='SKILL_ADAPTER_TEXT' AND task_id=?"
			case "ADAPTER_HASH":
				query = "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('ab',32)) WHERE operation='SKILL_ADAPTER_TEXT' AND task_id=?"
			case "MISSING":
				query = "DELETE FROM harness_skill_invocations WHERE task_id=?"
			case "DEPTH":
				query = "UPDATE harness_skill_invocations SET depth=2 WHERE task_id=?"
			case "RECEIPT":
				query = "UPDATE harness_skill_invocations SET receipt_key='missing-original-receipt' WHERE task_id=?"
			case "DEADLINE":
				query = "UPDATE harness_skill_invocations SET started_at=TIMESTAMPADD(SECOND,-61,UTC_TIMESTAMP(6)),deadline=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE task_id=?"
			}
			if mode != "VALID" {
				if _, e := r.Store.DB.Exec(query, child.TaskID); e != nil {
					t.Fatal(e)
				}
			}
			d.CommandKey = child.TaskID + "-skill-send"
			d.CommitKey = d.CommandKey
			if mode == "VALID" {
				p, e := r.Store.StartSend(ctx, d, d.AttemptID)
				if e != nil || !p.CanSend {
					t.Fatal("actual valid Skill send", e)
				}
				return
			}
			if _, e := r.Store.StartSend(ctx, d, d.AttemptID); !errors.Is(e, store.ErrDenied) && !(mode == "RECEIPT" && errors.Is(e, sql.ErrNoRows)) {
				t.Fatal("invalid original Skill evidence send rejection", mode, e)
			}
			var sent int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=? AND state<>'AUTHORIZED'", child.TaskID).Scan(&sent); e != nil || sent != 0 {
				t.Fatal(sent, e)
			}
		})
	}
}

func TestOnlineSkillAtomicBudgetAndReceipt(t *testing.T) {
	for _, mode := range []string{"ACK_COMMIT", "ACK_ROLLBACK", "CHILD_STEPS", "ROOT_STEPS", "FINISH_DEADLINE"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, root, child, d, _ := skillPrefix(t)
			var beforeRoot, beforeChild int
			if mode == "CHILD_STEPS" || mode == "ROOT_STEPS" {
				task := child.TaskID
				if mode == "ROOT_STEPS" {
					task = root.TaskID
				}
				if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET steps_used=max_steps-1 WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
			}
			if e := r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&beforeRoot); e != nil {
				t.Fatal(e)
			}
			if e := r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", child.TaskID).Scan(&beforeChild); e != nil {
				t.Fatal(e)
			}
			if mode == "FINISH_DEADLINE" {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					_, e := tx.ExecContext(c, "UPDATE harness_skill_invocations SET started_at=TIMESTAMPADD(SECOND,-61,UTC_TIMESTAMP(6)),deadline=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE task_id=?", child.TaskID)
					return e
				}
			}
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					var e error
					if mode == "ACK_COMMIT" {
						e = tx.Commit()
					} else {
						e = tx.Rollback()
					}
					if e != nil {
						return e
					}
					return errors.New("original skill authorization ACK lost")
				}
			}
			_, e := r.Store.AuthorizeDispatch(ctx, d)
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			} else if !errors.Is(e, store.ErrDenied) && !errors.Is(e, store.ErrConflict) {
				t.Fatal("wrong budget/finish rejection", e)
			}
			var calls, inv, holds, rs, cs int
			if e = r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_skill_invocations WHERE task_id=?),(SELECT COUNT(*) FROM reservations),(SELECT steps_used FROM harness_envelopes WHERE task_id=?),(SELECT steps_used FROM harness_envelopes WHERE task_id=?) FROM tasks WHERE task_id=?", child.TaskID, root.TaskID, child.TaskID, child.TaskID).Scan(&calls, &inv, &holds, &rs, &cs); e != nil {
				t.Fatal(e)
			}
			if mode == "ACK_COMMIT" {
				if calls != 1 || inv != 1 || holds != 5 || rs != beforeRoot+3 || cs != beforeChild+3 {
					t.Fatal(calls, inv, holds, rs, cs)
				}
			} else if calls != 0 || inv != 0 || holds != 3 || rs != beforeRoot || cs != beforeChild {
				t.Fatal("partial skill purchase", calls, inv, holds, rs, cs)
			}
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
					t.Fatal("original retry", e)
				}
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
					t.Fatal("original replay", e)
				}
				if e = r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_skill_invocations WHERE task_id=?),(SELECT steps_used FROM harness_envelopes WHERE task_id=?) FROM tasks WHERE task_id=?", child.TaskID, child.TaskID, child.TaskID).Scan(&calls, &inv, &cs); e != nil || calls != 1 || inv != 1 || cs != beforeChild+3 {
					t.Fatal("replay reset budget", calls, inv, cs, e)
				}
			}
		})
	}
}

func TestOnlineSkillTakeoverPreservesOriginalInvocation(t *testing.T) {
	ctx, r, root, child, d, _ := skillPrefix(t)
	if _, e := r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	var started, deadline time.Time
	var originalFence uint64
	if e := r.Store.DB.QueryRow("SELECT started_at,deadline,fence FROM harness_skill_invocations WHERE task_id=?", child.TaskID).Scan(&started, &deadline, &originalFence); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE task_id IN (?,?)", root.TaskID, child.TaskID); e != nil {
		t.Fatal(e)
	}
	background := context.Background()
	nextRoot, e := r.Store.ClaimRegistered(background, "skill-takeover", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: root.TaskID}})
	if e != nil {
		t.Fatal(e)
	}
	issuer, e := r.Store.EnterDraftTaskIssuer(background, nextRoot.TaskLease, "skill-takeover")
	if e != nil {
		t.Fatal("current persisted root issuer", e)
	}
	nextChild, e := r.Store.ClaimRehearsalChild(issuer, nextRoot.TaskLease, "skill-takeover")
	if e != nil {
		t.Fatal(e)
	}
	if nextRoot.Fence <= root.Fence || nextChild.Fence <= child.Fence {
		t.Fatal("actual fence not advanced")
	}
	d.CommandKey = child.TaskID + "-old-send"
	d.CommitKey = d.CommandKey
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); !errors.Is(e, store.ErrDenied) {
		t.Fatal("old Skill lease", e)
	}
	current, close, e := r.Store.EnterRehearsalChildExecution(issuer, nextRoot.TaskLease, nextChild.TaskLease, "skill-takeover")
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	next, e := r.Store.PrepareRehearsalTurn(current, nextRoot.TaskLease, nextChild.TaskLease, "skill-takeover")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(current, next); e != nil {
		t.Fatal("same original authorization", e)
	}
	third := next
	third.Slot = 2
	if _, e = r.Store.AuthorizeDispatch(current, third); !errors.Is(e, store.ErrDenied) {
		t.Fatal("Skill created third slot", e)
	}
	next.CommandKey = child.TaskID + "-new-send"
	next.CommitKey = next.CommandKey
	permit, e := r.Store.StartSend(current, next, next.AttemptID)
	if e != nil || !permit.CanSend {
		t.Fatal("actual current Skill send", e)
	}
	var afterStart, afterDeadline time.Time
	var afterFence uint64
	var count, calls int
	if e = r.Store.DB.QueryRow("SELECT started_at,deadline,fence,(SELECT COUNT(*) FROM harness_skill_invocations WHERE task_id=?),(SELECT calls_used FROM tasks WHERE task_id=?) FROM harness_skill_invocations WHERE task_id=?", child.TaskID, child.TaskID, child.TaskID).Scan(&afterStart, &afterDeadline, &afterFence, &count, &calls); e != nil {
		t.Fatal(e)
	}
	if !started.Equal(afterStart) || !deadline.Equal(afterDeadline) || originalFence != afterFence || count != 1 || calls != 1 {
		t.Fatal("takeover reset original Skill", afterStart, afterDeadline, afterFence, count, calls)
	}
}

func TestOnlineTextSkillSharesOriginalAttempts(t *testing.T) {
	r, draft, req := skillRehearsalFixture(t, "GenerationMaterial")
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "online-skill", req)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	var steps, calls, holds int
	if e = r.Store.DB.QueryRow("SELECT steps_used,calls_allocated,(SELECT COUNT(*) FROM reservations) FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&steps, &calls, &holds); e != nil {
		t.Fatal(e)
	}
	if steps != 20 || calls != 2 || holds != 7 {
		t.Fatal("real declared TEXT was not budgeted on original two attempts", steps, calls, holds)
	}
	var invocations int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_skill_invocations WHERE capability='TEXT' AND state='COMPLETED'").Scan(&invocations); e != nil || invocations != 2 {
		t.Fatal("missing durable typed delegation", invocations, e)
	}
	var starts, deadlines int
	if e = r.Store.DB.QueryRow("SELECT COUNT(DISTINCT started_at),COUNT(DISTINCT deadline) FROM harness_skill_invocations").Scan(&starts, &deadlines); e != nil || starts != 1 || deadlines != 1 {
		t.Fatal("extra reset original Skill time", starts, deadlines, e)
	}
	if _, e = r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e != nil {
		t.Fatal(e)
	}
}

func TestOnlineProgramSkillUsesActualActionDecision(t *testing.T) {
	runOnlineProgramSkill(t, false)
}
func TestOnlineProgramSkillRejectsActualDeniedAction(t *testing.T) { runOnlineProgramSkill(t, true) }
func runOnlineProgramSkill(t *testing.T, deny bool) {
	r, draft, req := skillRehearsalFixture(t, "GenerationMaterial")
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	d, e := r.Content.GetDraft(ctx, scope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	for n, raw := range d.Input.Objects {
		var obj map[string]any
		json.Unmarshal(raw, &obj)
		if deny && obj["kind"] == "RULE" {
			obj["effect"] = "DENY"
			d.Input.Objects[n], _ = json.Marshal(obj)
		}
		if obj["object_id"] == "online-text" {
			obj["implementation_kind"] = "PROGRAM"
			obj["registered_capability_ids"] = []string{"SIGN_CUSTODY"}
			obj["input_schema_id"] = "CandidateActions"
			obj["output_schema_id"] = "ActionDecision"
			d.Input.Objects[n], _ = json.Marshal(obj)
		}
	}
	d.Input.SkillBindings[0].Trigger = "PRECHECK"
	saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 2, "program-skill")
	if e != nil {
		t.Fatal(e)
	}
	req.ExpectedDraftRevision = saved.DraftRevision
	req.Inputs[0].Text = "Pass the sign and remember my request."
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "program-skill", req)
	if e != nil {
		t.Fatal(e)
	}
	e = r.Worker.Once(ctx)
	if deny {
		if !errors.Is(e, store.ErrDenied) {
			t.Fatal("actual ActionDecision rejection", e)
		}
		var calls, events, approvals, skills int
		if e = r.Store.DB.QueryRow("SELECT calls_allocated,(SELECT COUNT(*) FROM events),(SELECT COUNT(*) FROM harness_candidate_approvals),(SELECT COUNT(*) FROM harness_skill_invocations) FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&calls, &events, &approvals, &skills); e != nil || calls != 1 || events != 0 || approvals != 0 || skills != 0 {
			t.Fatal("program rejection produced partial approval/purchase", calls, events, approvals, skills, e)
		}
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	var steps, calls, events int
	if e = r.Store.DB.QueryRow("SELECT steps_used,calls_allocated,(SELECT COUNT(*) FROM events WHERE task_id IN (SELECT child_task_id FROM harness_rehearsal_children WHERE root_task_id=?)) FROM harness_envelopes WHERE task_id=?", root.TaskID, root.TaskID).Scan(&steps, &calls, &events); e != nil {
		t.Fatal(e)
	}
	if steps != 13 || calls != 1 || events != 1 {
		t.Fatal("declared real program was not budgeted/used", steps, calls, events)
	}
	var n int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_skill_invocations WHERE capability='SIGN_CUSTODY' AND state='COMPLETED'").Scan(&n); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}
