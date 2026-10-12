package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
)

func TestRehearsalTwentyInputsFortyActualSends(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	inputs := make([]map[string]string, 20)
	for n := range inputs {
		inputs[n] = map[string]string{"actor_slot": "mia", "text": fmt.Sprintf("Frozen input %d", n)}
	}
	raw, _ := json.Marshal(map[string]any{"product_id": "coffee", "expected_draft_revision": "1", "mode": "MOCK", "fixture": "COFFEE", "inputs": inputs, "max_cost_units": "0", "assertions": []string{"ISOLATED", "RIGHTS", "DUTIES", "SIGN_CUSTODY"}})
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", raw, &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "twenty-inputs", req)
	if e != nil {
		t.Fatal(e)
	}
	var accepted, execute, settle time.Time
	var maxCalls, maxSteps int
	if e = r.Store.DB.QueryRow("SELECT accepted_at,execute_by,settle_by,max_calls,max_steps FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&accepted, &execute, &settle, &maxCalls, &maxSteps); e != nil {
		t.Fatal(e)
	}
	if maxCalls != 40 || maxSteps != 402 || execute.Sub(accepted) != 1230*time.Second || settle.Sub(execute) != 60*time.Second {
		t.Fatal("original bounded root", maxCalls, maxSteps, accepted, execute, settle)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	for pass := 0; pass < 4; pass++ {
		e = r.Worker.Once(ctx)
		if e == nil {
			break
		}
		var expired, eligible bool
		var allocated, next int
		var fence uint64
		var originalExecute, originalSettle time.Time
		if q := r.Store.DB.QueryRow("SELECT t.lease_expires_at<=UTC_TIMESTAMP(6),e.execute_by>UTC_TIMESTAMP(6),t.fence,e.calls_allocated,a.next_input,e.execute_by,e.settle_by FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE t.task_id=?", root.TaskID).Scan(&expired, &eligible, &fence, &allocated, &next, &originalExecute, &originalSettle); q != nil {
			t.Fatal(q)
		}
		if !expired || !eligible || originalExecute != execute || originalSettle != settle {
			t.Fatal("actual worker failed without a safely resumable expired lease", e, expired, eligible, allocated, next)
		}
		t.Logf("bounded worker pass %d ended at expired fence %d; actual calls=%d next=%d; original deadlines unchanged: %v", pass, fence, allocated, next, e)
	}
	if e != nil {
		t.Fatal("bounded registered takeover failed to complete", e)
	}
	if _, e = r.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID)); e != nil {
		t.Fatal(e)
	}
	var calls, steps, own, next, children, commits, attempts, holds, services, consent int
	if e = r.Store.DB.QueryRow("SELECT e.calls_allocated,e.steps_used,a.own_steps,a.next_input,(SELECT COUNT(*) FROM harness_rehearsal_children WHERE root_task_id=?),(SELECT COUNT(*) FROM harness_turn_commits),(SELECT COUNT(*) FROM attempts),(SELECT COUNT(*) FROM reservations),(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use') FROM harness_envelopes e JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE e.task_id=?", root.TaskID, root.TaskID).Scan(&calls, &steps, &own, &next, &children, &commits, &attempts, &holds, &services, &consent); e != nil {
		t.Fatal(e)
	}
	if calls != 40 || steps != 282 || own != 2 || next != 20 || children != 20 || commits != 20 || attempts != 40 || holds != 83 || services != 1 || consent != 0 {
		t.Fatal("actual aggregate", calls, steps, own, next, children, commits, attempts, holds, services, consent)
	}
	rows, e := r.Store.DB.Query("SELECT t.task_id,t.calls_used,e.steps_used,e.max_calls,e.max_steps,c.started_at,c.execute_by,e.execute_by,e.settle_by FROM harness_rehearsal_children c JOIN tasks t ON t.task_id=c.child_task_id JOIN harness_envelopes e ON e.task_id=t.task_id WHERE c.root_task_id=? ORDER BY c.input_index", root.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	for rows.Next() {
		var id string
		var cc, ss, mc, ms int
		var start, end, childEnd, childSettle time.Time
		if e = rows.Scan(&id, &cc, &ss, &mc, &ms, &start, &end, &childEnd, &childSettle); e != nil {
			t.Fatal(e)
		}
		if cc != 2 || ss != 14 || mc != 2 || ms != 20 || end.Sub(start) > time.Minute || end != childEnd || childSettle != settle || end.After(execute) {
			t.Fatal("child limit or original deadline", id, cc, ss, mc, ms, start, end, childEnd, childSettle)
		}
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	rows.Close()
	starts, ends := map[string]domain.MockRequestIdentity{}, map[string]bool{}
	for _, line := range bytes.Split(journalBytes(t, r), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry struct{ Result domain.MockResultMetadata }
		if e = json.Unmarshal(line, &entry); e != nil {
			t.Fatal(e)
		}
		id := entry.Result.Identity
		if entry.Result.State == "UNKNOWN" {
			if _, exists := starts[id.AttemptID]; exists {
				t.Fatal("duplicate supplier send", id.AttemptID)
			}
			starts[id.AttemptID] = id
		}
		if entry.Result.State == "SUCCEEDED" {
			if starts[id.AttemptID] != id || ends[id.AttemptID] {
				t.Fatal("result identity mismatch")
			}
			ends[id.AttemptID] = true
		}
	}
	if len(starts) != 40 || len(ends) != 40 {
		t.Fatal("actual independent J sends", len(starts), len(ends))
	}
	before := journalBytes(t, r)
	replay, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "twenty-inputs", req)
	if e != nil || replay.TaskID != root.TaskID {
		t.Fatal("original acceptance replay", replay, e)
	}
	body, e := r.Files.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
	if e != nil || len(body) == 0 {
		t.Fatal("real maximum report", e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("read/replay generated again")
	}
	var afterExecute, afterSettle time.Time
	if e = r.Store.DB.QueryRow("SELECT execute_by,settle_by FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&afterExecute, &afterSettle); e != nil || afterExecute != execute || afterSettle != settle {
		t.Fatal("root deadline changed", e)
	}
	t.Log("actual independent sends=40 children=20 child steps=14 root own=2 aggregate steps=282/max402 supplier holds=80 parent holds=3")
}

func TestOriginalTurnRebasesAfterUnrelatedActualTask4Action(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	original, _, e := readOriginalResult(ctx, r.Files, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	var snapshot []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=?", task).Scan(&snapshot); e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	br, action := actionTask(t, r, scope.ProductScope, instance, "unrelated-real-action")
	if _, e = commitAction(r, br, action); e != nil {
		t.Fatal("actual Task4 action", e)
	}
	var actualEvents int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id=?", action.TaskID).Scan(&actualEvents); e != nil || actualEvents != 1 {
		t.Fatal("unrelated action not committed", actualEvents, e)
	}
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal("pure program revalidation of no-action candidate", e)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal("original rebase commit", e)
	}
	var calls, steps, extras, events int
	if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,(SELECT COUNT(*) FROM harness_recovery_approvals WHERE task_id=?),(SELECT COUNT(*) FROM events WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task).Scan(&calls, &steps, &extras, &events); e != nil || calls != 1 || steps != 9 || extras != 0 || events != 0 {
		t.Fatal("unrelated action caused purchase or invented action", calls, steps, extras, events, e)
	}
	var after []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=?", task).Scan(&after); e != nil || !bytes.Equal(snapshot, after) || !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("original input or J rewritten", e)
	}
	view, e := r.Turns.GetTurn(ctx, scope, task)
	if e != nil || len(view.ApprovedPackets) != 1 || len(original) == 0 {
		t.Fatal("actual terminal original", e)
	}
}
