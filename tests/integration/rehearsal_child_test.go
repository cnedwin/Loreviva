package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
)

type rehearsalChildStarter interface {
	StartRehearsalChild(context.Context, domain.TaskLease, string) (generated.TurnAccepted, error)
}
type rehearsalContextBinder interface {
	BindRehearsalChildContext(context.Context, domain.TaskLease, domain.TaskLease, string) (domain.GenerationMaterial, error)
}
type rehearsalTurnPreparer interface {
	PrepareRehearsalTurn(context.Context, domain.TaskLease, domain.TaskLease, string) (store.DispatchRequest, error)
}
type rehearsalExecutionEntry interface {
	EnterRehearsalChildExecution(context.Context, domain.TaskLease, domain.TaskLease, string) (context.Context, func(), error)
}
type rehearsalChildClaimer interface {
	ClaimRehearsalChild(context.Context, domain.TaskLease, string) (store.RegisteredLease, error)
}
type rehearsalChildResultReader interface {
	ReadRehearsalChildResult(context.Context, domain.TaskLease, string, int) ([]byte, domain.CandidateCommit, error)
}

func TestRehearsalFixedClaimIsolation(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "fixed-claim", req)
	if e != nil {
		t.Fatal(e)
	}
	rl, e := r.Store.ClaimRegistered(ctx, "rehearsal-claim", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, "rehearsal-claim"); e != nil {
		t.Fatal(e)
	}
	child, e := r.Store.StartRehearsalChild(ctx, rl.TaskLease, "rehearsal-claim")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ClaimRegistered(ctx, "ordinary-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "REHEARSAL_TURN", TaskID: child.TurnID}}); !errors.Is(e, store.ErrNoTask) {
		t.Fatal("ordinary registered queue claimed isolation", e)
	}
	if _, e = r.Store.ClaimTask(ctx, "ordinary-worker", time.Now(), time.Minute); !errors.Is(e, store.ErrNoTask) {
		t.Fatal("ordinary queue claimed isolation", e)
	}
	port, ok := any(r.Store).(rehearsalChildClaimer)
	if !ok {
		t.Fatal("fixed current root-child claim missing")
	}
	cl, e := port.ClaimRehearsalChild(ctx, rl.TaskLease, "rehearsal-claim")
	if e != nil || cl.TaskID != child.TurnID || cl.Operation != "REHEARSAL_TURN" {
		t.Fatal("fixed child claim", e)
	}
	again, e := port.ClaimRehearsalChild(ctx, rl.TaskLease, "rehearsal-claim")
	if e != nil || again.Fence != cl.Fence || again.TaskID != cl.TaskID || again.ExpiresAt != cl.ExpiresAt || again.WorldInstanceID == nil || *again.WorldInstanceID != *cl.WorldInstanceID {
		t.Fatal("claim replay changed lease", again, cl, e)
	}
	old := rl.TaskLease
	old.Fence++
	if _, e = port.ClaimRehearsalChild(ctx, old, "rehearsal-claim"); e == nil {
		t.Fatal("wrong root fence claimed child")
	}
	if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, "rehearsal-claim"); e != nil {
		t.Fatal("claimed child cannot execute", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_turns SET command_hash=UNHEX(REPEAT('11',32)) WHERE task_id=?", child.TurnID); e != nil {
		t.Fatal(e)
	}
	if _, e = port.ClaimRehearsalChild(ctx, rl.TaskLease, "rehearsal-claim"); e == nil {
		t.Fatal("claim receipt bypassed original child binding on replay")
	}
}

func TestRehearsalChildContext(t *testing.T) { runRehearsalChildContext(t, "NORMAL") }

func TestRehearsalClaimOutcomes(t *testing.T) {
	for _, mode := range []string{"ROLLBACK", "COMMIT_ACK", "FINISH_REVOKE", "FINISH_BINDING", "TAKEOVER", "REPLAY_CANCEL"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, draft, _ := candidateHTTPFixture(t)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "claim-outcome", req)
			if e != nil {
				t.Fatal(e)
			}
			worker := "claim-outcome"
			rl, e := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, worker); e != nil {
				t.Fatal(e)
			}
			child, e := r.Store.StartRehearsalChild(ctx, rl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			var originalEnd time.Time
			if e = r.Store.DB.QueryRow("SELECT execute_by FROM harness_rehearsal_children WHERE child_task_id=?", child.TurnID).Scan(&originalEnd); e != nil {
				t.Fatal(e)
			}
			injected := errors.New("fixed claim outcome")
			if mode == "ROLLBACK" || mode == "FINISH_REVOKE" || mode == "FINISH_BINDING" {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					var exists bool
					if e := tx.QueryRowContext(c, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='REHEARSAL_CHILD_CLAIM')").Scan(&exists); e != nil {
						return e
					}
					if !exists {
						return nil
					}
					if mode == "ROLLBACK" {
						return injected
					}
					if mode == "FINISH_BINDING" {
						_, e := tx.ExecContext(c, "UPDATE harness_turns SET command_hash=UNHEX(REPEAT('22',32)) WHERE task_id=?", child.TurnID)
						return e
					}
					_, e := tx.ExecContext(c, "DELETE FROM operator_actions WHERE action='content.rehearse'")
					return e
				}
			}
			if mode == "COMMIT_ACK" {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					var exists bool
					if e := tx.QueryRowContext(c, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE operation='REHEARSAL_CHILD_CLAIM')").Scan(&exists); e != nil {
						return e
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					if exists {
						return injected
					}
					return nil
				}
			}
			cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if mode == "ROLLBACK" || mode == "FINISH_REVOKE" || mode == "FINISH_BINDING" {
				if e == nil {
					t.Fatal("claim ignored owning rollback", mode)
				}
				var state string
				var fence, count int
				if e = r.Store.DB.QueryRow("SELECT state,fence FROM tasks WHERE task_id=?", child.TurnID).Scan(&state, &fence); e != nil {
					t.Fatal(e)
				}
				if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_CHILD_CLAIM'").Scan(&count); e != nil {
					t.Fatal(e)
				}
				if state != "ACCEPTED" || fence != 0 || count != 0 {
					t.Fatal("partial claim survived rollback", state, fence, count)
				}
				cl, e = r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
			} else if mode == "COMMIT_ACK" {
				if !errors.Is(e, store.ErrCommitUnknown) || cl.Fence != 0 {
					t.Fatal("unknown claim minted lease", cl, e)
				}
				cl, e = r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
			}
			if e != nil {
				t.Fatal(e)
			}
			if mode == "TAKEOVER" {
				if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id IN (?,?)", root.TaskID, child.TurnID); e != nil {
					t.Fatal(e)
				}
				newRoot, e := r.Store.ClaimRegistered(ctx, "takeover", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
				if e != nil {
					t.Fatal(e)
				}
				next, e := r.Store.ClaimRehearsalChild(ctx, newRoot.TaskLease, "takeover")
				if e != nil || next.Fence != cl.Fence+1 {
					t.Fatal("takeover", next, e)
				}
				if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, worker); e == nil {
					t.Fatal("old fences survived takeover")
				}
				if _, e = r.Store.BindRehearsalChildContext(ctx, newRoot.TaskLease, next.TaskLease, "takeover"); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "REPLAY_CANCEL" {
				if _, e = r.Store.DB.Exec("UPDATE tasks SET cancelled=1 WHERE task_id=?", root.TaskID); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker); e == nil {
					t.Fatal("cancelled root replay claimed child")
				}
			}
			var end time.Time
			var calls, holds int
			if e = r.Store.DB.QueryRow("SELECT execute_by FROM harness_rehearsal_children WHERE child_task_id=?", child.TurnID).Scan(&end); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRow("SELECT calls_allocated FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&calls); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&holds); e != nil {
				t.Fatal(e)
			}
			if end != originalEnd || calls != 0 || holds != 3 {
				t.Fatal("claim changed execution/budget", originalEnd, end, calls, holds)
			}
		})
	}
}
func TestRehearsalChildSharedRepair(t *testing.T)         { runRehearsalChildContext(t, "REPAIR") }
func TestRehearsalParentReadsCommittedChild(t *testing.T) { runRehearsalChildContext(t, "READ") }
func TestRehearsalParentReadsRepairedChild(t *testing.T)  { runRehearsalChildContext(t, "READ_REPAIR") }
func TestRehearsalParentReadsActionMemory(t *testing.T)   { runRehearsalChildContext(t, "READ_ACTION") }
func TestRehearsalChildCostPending(t *testing.T)          { runRehearsalChildContext(t, "COST_PENDING") }
func TestRehearsalReportEvidence(t *testing.T)            { runRehearsalChildContext(t, "REPORT_EVIDENCE") }
func TestRehearsalReportEvidenceTwoInputs(t *testing.T)   { runRehearsalChildContext(t, "REPORT_REPAIR") }
func TestRehearsalReportAssertions(t *testing.T)          { runRehearsalChildContext(t, "REPORT_ASSERTIONS") }
func TestRehearsalReportHistory(t *testing.T)             { runRehearsalChildContext(t, "REPORT_HISTORY") }
func TestRehearsalReportHistoryRepair(t *testing.T) {
	runRehearsalChildContext(t, "REPORT_HISTORY_REPAIR")
}
func TestRehearsalReportAssertionOutcomes(t *testing.T) {
	for _, m := range []string{"FAIL", "NOT_RUN", "DUPLICATE"} {
		t.Run(m, func(t *testing.T) { runRehearsalChildContext(t, "REPORT_ASSERTIONS_"+m) })
	}
}
func TestRehearsalChildActionMemory(t *testing.T) {
	for _, m := range []string{"ACTION", "ACTION_ROLLBACK", "ACTION_FINISH_REVOKE", "ACTION_FINISH_SOURCE", "ACTION_FINISH_GRANT", "ACTION_FINISH_ACQUISITION", "ACTION_FINISH_PRIVACY"} {
		t.Run(m, func(t *testing.T) { runRehearsalChildContext(t, m) })
	}
}
func TestRehearsalChildCommitBarriers(t *testing.T) {
	for _, mode := range []string{"ROOT_FENCE", "CHILD_FENCE", "ROOT_CANCEL", "SOURCE_REVOKED", "INPUT_TAMPER", "REGISTERED_TAMPER", "DEADLINE_TAMPER", "FINISH_REVOKE", "ROLLBACK", "COMMIT_ACK"} {
		t.Run(mode, func(t *testing.T) { runRehearsalChildContext(t, mode) })
	}
}
func TestRehearsalReportCommitBoundaries(t *testing.T) {
	for _, m := range []string{"ROLLBACK", "ACK_COMMIT", "ACK_ROLLBACK", "FINISH_SOURCE", "FINISH_PRIVACY", "FINISH_BINDING", "FINISH_VIEW", "FINISH_NODE", "FINISH_LINE", "COST_PENDING"} {
		t.Run(m, func(t *testing.T) { runRehearsalChildContext(t, "REPORT_"+m) })
	}
}
func TestRehearsalReportStageEpochFinish(t *testing.T) {
	for _, mode := range []string{"PRIVACY", "VIEW"} {
		t.Run(mode, func(t *testing.T) { runRehearsalChildContext(t, "REPORT_FINISH_STAGE_"+mode) })
	}
}
func runRehearsalChildContext(t *testing.T, mode string) {
	reportMode := strings.HasPrefix(mode, "REPORT_")
	action := mode == "READ_ACTION" || mode == "ACTION" || mode == "ACTION_ROLLBACK" || mode == "ACTION_FINISH_REVOKE" || mode == "ACTION_FINISH_SOURCE" || mode == "ACTION_FINISH_GRANT" || mode == "ACTION_FINISH_ACQUISITION" || mode == "ACTION_FINISH_PRIVACY"
	repair := mode == "REPAIR" || mode == "READ_REPAIR" || (mode == "REPORT_REPAIR" || mode == "REPORT_HISTORY_REPAIR")
	rollback := mode == "ROLLBACK" || mode == "ACTION_ROLLBACK"
	revoke := mode == "FINISH_REVOKE" || mode == "ACTION_FINISH_REVOKE" || mode == "ACTION_FINISH_SOURCE" || mode == "ACTION_FINISH_GRANT" || mode == "ACTION_FINISH_ACQUISITION" || mode == "ACTION_FINISH_PRIVACY"
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	if action {
		req.Inputs[0].Text = "Pass the sign and remember my request."
	}
	if strings.HasPrefix(mode, "REPORT_ASSERTIONS") {
		req.Assertions = []string{"ISOLATED", "SIGN_CUSTODY", "RIGHTS", "DUTIES"}
		if mode == "REPORT_ASSERTIONS_DUPLICATE" {
			req.Assertions = []string{"SIGN_CUSTODY", "RIGHTS", "SIGN_CUSTODY", "DUTIES"}
		}
		if mode == "REPORT_ASSERTIONS_FAIL" || mode == "REPORT_ASSERTIONS_NOT_RUN" {
			d, e := r.Content.GetDraft(ctx, scope, draft, "")
			if e != nil {
				t.Fatal(e)
			}
			for i, raw := range d.Input.Objects {
				var o map[string]any
				if e = json.Unmarshal(raw, &o); e != nil {
					t.Fatal(e)
				}
				if o["kind"] == "RULE" {
					if mode == "REPORT_ASSERTIONS_FAIL" {
						o["effect"] = "DENY"
					} else {
						o["strength"] = "SOFT"
					}
					d.Input.Objects[i], _ = json.Marshal(o)
				}
			}
			saved, e := r.Content.SaveDraft(ctx, scope, draft, d.Input, 1, "assertion-original-basis")
			if e != nil {
				t.Fatal(e)
			}
			req.ExpectedDraftRevision = saved.DraftRevision
		}
	}
	if mode == "READ_ACTION" || (mode == "REPORT_REPAIR" || mode == "REPORT_HISTORY_REPAIR") {
		next := req.Inputs[0]
		next.Text = "Second frozen input"
		req.Inputs = append(req.Inputs, next)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "context", req)
	if e != nil {
		t.Fatal(e)
	}
	rl, e := r.Store.ClaimRegistered(ctx, "rehearsal-context", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, "rehearsal-context"); e != nil {
		t.Fatal(e)
	}
	child, e := r.Store.StartRehearsalChild(ctx, rl.TaskLease, "rehearsal-context")
	if e != nil {
		t.Fatal(e)
	}
	cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, "rehearsal-context")
	if e != nil {
		t.Fatal(e)
	}
	binder, ok := any(r.Store).(rehearsalContextBinder)
	if !ok {
		t.Fatal("actual fixed child context bridge missing")
	}
	m, e := binder.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, "rehearsal-context")
	if e != nil {
		t.Fatal(e)
	}
	if m.Module != "REHEARSAL" || len(m.Canonical) == 0 || m.InputBound != 16000 || m.OutputBound != 4000 {
		t.Fatal("wrong actual rehearsal canonical", m.Module, m.InputBound, m.OutputBound)
	}
	var prompt struct {
		Core         struct{ ActorID string }
		Relationship []memory.MemoryRecord
		Route        struct{ Module string }
		World        domain.WorldPerception
	}
	if e = json.Unmarshal(m.Canonical, &prompt); e != nil {
		t.Fatal(e)
	}
	var actor string
	if e = r.Store.DB.QueryRow("SELECT actor_id FROM actors WHERE world_instance_id=? AND slot_id='mia'", *cl.WorldInstanceID).Scan(&actor); e != nil {
		t.Fatal(e)
	}
	if prompt.Core.ActorID != actor || prompt.Route.Module != "REHEARSAL" || prompt.World.ActorID != actor || len(prompt.Relationship) != 1 || prompt.Relationship[0].Domain != memory.Sandbox || prompt.Relationship[0].Body != req.Inputs[0].Text {
		t.Fatal("synthetic current context mismatch", prompt)
	}
	var rootSteps, childSteps, consent int
	if e = r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&rootSteps); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", child.TurnID).Scan(&childSteps); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'").Scan(&consent); e != nil {
		t.Fatal(e)
	}
	if rootSteps != 2 || childSteps != 1 || consent != 0 {
		t.Fatal(rootSteps, childSteps, consent)
	}
	repeated, e := binder.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, "rehearsal-context")
	if e != nil || string(repeated.Canonical) != string(m.Canonical) {
		t.Fatal("canonical replay", e)
	}
	prepare, ok := any(r.Store).(rehearsalTurnPreparer)
	if !ok {
		t.Fatal("actual rehearsal dispatch bridge missing")
	}
	d, e := prepare.PrepareRehearsalTurn(ctx, rl.TaskLease, cl.TaskLease, "rehearsal-context")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal("authorize real child", e)
	}
	d.CommandKey = content.Identity(cl.TaskID, "send")
	d.CommitKey = d.CommandKey
	send, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || !send.CanSend || send.OriginalSend() == nil {
		t.Fatal("real child send", e)
	}
	if repair {
		r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	}
	if mode == "COST_PENDING" || mode == "REPORT_COST_PENDING" {
		r.Worker.Turns.Fault = harness.MockFault(15)
	}
	if e = r.Worker.Turns.Generate(ctx, send.OriginalSend()); e != nil {
		t.Fatal("real child Generate", e)
	}
	var rootCalls, childCalls, holds int
	if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&rootCalls, &rootSteps); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", cl.TaskID).Scan(&childCalls); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&holds); e != nil {
		t.Fatal(e)
	}
	if rootCalls != 1 || childCalls != 1 || rootSteps != 4 || holds != 5 {
		t.Fatal("shared purchase identities/counters", rootCalls, childCalls, rootSteps, holds)
	}
	entry, ok := any(r.Store).(rehearsalExecutionEntry)
	if !ok {
		t.Fatal("fixed original-result rehearsal execution admission missing")
	}
	childCtx, close, e := entry.EnterRehearsalChildExecution(ctx, rl.TaskLease, cl.TaskLease, "rehearsal-context")
	if e != nil {
		t.Fatal(e)
	}
	defer close()
	if _, e = r.Store.ObserveOriginalReply(childCtx, send.OriginalSend()); e != nil {
		t.Fatal("original child reply", e)
	}
	if mode == "COST_PENDING" || mode == "REPORT_COST_PENDING" {
		id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: *cl.WorldInstanceID}, cl.TaskID)
		if e != nil {
			t.Fatal(e)
		}
		meta, e := r.Worker.Turns.Journal.OriginalMetadata(ctx, id)
		if e != nil || meta.State != "SUCCEEDED" || !meta.BodyAvailable || !meta.UsageKnown || meta.NoFurtherCost {
			t.Fatal("expected real complete narrative with unresolved supplier finality", meta.State, meta.NoFurtherCost, e)
		}
	}
	if e = r.Store.ValidateOriginalTurn(childCtx, cl.TaskLease, "rehearsal-context"); e != nil {
		t.Fatal("actual child original body validation", e)
	}
	var state string
	if e = r.Store.DB.QueryRow("SELECT fsm_state FROM harness_turns WHERE task_id=?", child.TurnID).Scan(&state); e != nil || state != "VALIDATED" {
		t.Fatal(state, e)
	}
	if repair {
		d, e = r.Store.PrepareTurnRecovery(childCtx, cl.TaskLease, "rehearsal-context")
		if e != nil {
			t.Fatal("actual child repair approval", e)
		}
		if _, e = r.Store.AuthorizeDispatch(childCtx, d); e != nil {
			t.Fatal("actual child extra authorize", e)
		}
		d.CommandKey = content.Identity(cl.TaskID, "repair-send")
		d.CommitKey = d.CommandKey
		extra, e := r.Store.StartSend(childCtx, d, d.AttemptID)
		if e != nil || !extra.CanSend {
			t.Fatal("actual child extra send", e)
		}
		r.Worker.Turns.Fault = 0
		if e = r.Worker.Turns.Generate(childCtx, extra.OriginalSend()); e != nil {
			t.Fatal("actual child extra Generate", e)
		}
		if _, e = r.Store.ObserveOriginalReply(childCtx, extra.OriginalSend()); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.ValidateAttemptTurn(childCtx, cl.TaskLease, "rehearsal-context", d.AttemptID); e != nil {
			t.Fatal("actual child extra validation", e)
		}
	}
	switch mode {
	case "ROOT_FENCE":
		_, e = r.Store.DB.Exec("UPDATE tasks SET fence=fence+1 WHERE task_id=?", root.TaskID)
	case "CHILD_FENCE":
		_, e = r.Store.DB.Exec("UPDATE tasks SET fence=fence+1 WHERE task_id=?", child.TurnID)
	case "ROOT_CANCEL":
		_, e = r.Store.DB.Exec("UPDATE tasks SET cancelled=1,state='CANCELLED' WHERE task_id=?", root.TaskID)
	case "SOURCE_REVOKED":
		_, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
	case "INPUT_TAMPER":
		_, e = r.Store.DB.Exec("UPDATE harness_rehearsal_input_sources SET document=JSON_SET(document,'$.inputs[0].text','changed') WHERE task_id=?", root.TaskID)
	case "REGISTERED_TAMPER":
		_, e = r.Store.DB.Exec("UPDATE registered_jobs SET operation='TURN' WHERE task_id=?", child.TurnID)
	case "DEADLINE_TAMPER":
		_, e = r.Store.DB.Exec("UPDATE harness_envelopes SET execute_by=DATE_SUB(execute_by,INTERVAL 1 SECOND) WHERE task_id=?", child.TurnID)
	}
	if e != nil {
		t.Fatal("fixture", e)
	}
	if mode != "NORMAL" && !reportMode && mode != "COST_PENDING" && mode != "READ" && !repair && !action && !revoke && !rollback && mode != "COMMIT_ACK" {
		if e = r.Store.ApproveOriginalTurn(childCtx, cl.TaskLease, "rehearsal-context"); e == nil {
			t.Fatal("cached entry became authority", mode)
		}
		var approvals, commits int
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_candidate_approvals").Scan(&approvals); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turn_commits").Scan(&commits); e != nil {
			t.Fatal(e)
		}
		if approvals != 0 || commits != 0 {
			t.Fatal("invalid proof wrote candidate", approvals, commits)
		}
		return
	}
	if e = r.Store.ApproveAttemptTurn(childCtx, cl.TaskLease, "rehearsal-context", d.AttemptID); e != nil {
		t.Fatal("actual child program approval", e)
	}
	var beforeMemory, beforeItem []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&beforeMemory); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&beforeItem); e != nil {
		t.Fatal(e)
	}
	if rollback || revoke {
		r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
			var n int
			if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?", child.TurnID).Scan(&n); e != nil {
				return e
			}
			if n == 0 {
				return nil
			}
			if rollback {
				return errors.New("child joint commit rollback")
			}
			if mode == "ACTION_FINISH_PRIVACY" {
				_, e := tx.ExecContext(c, "UPDATE world_controls SET privacy_epoch=privacy_epoch+1 WHERE world_instance_id=?", *cl.WorldInstanceID)
				return e
			}
			if mode == "ACTION_FINISH_SOURCE" || mode == "ACTION_FINISH_GRANT" || mode == "ACTION_FINISH_ACQUISITION" {
				var raw []byte
				var source string
				if e := tx.QueryRowContext(c, "SELECT input_source_id FROM harness_turns WHERE task_id=?", child.TurnID).Scan(&source); e != nil {
					return e
				}
				if e := tx.QueryRowContext(c, "SELECT document FROM memory_aggregates WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&raw); e != nil {
					return e
				}
				var a memory.Aggregate
				if e := json.Unmarshal(raw, &a); e != nil {
					return e
				}
				if mode == "ACTION_FINISH_SOURCE" {
					delete(a.Sources, source)
				}
				if mode == "ACTION_FINISH_GRANT" {
					kept := a.Grants[:0]
					for _, g := range a.Grants {
						if g.SourceID != source {
							kept = append(kept, g)
						}
					}
					a.Grants = kept
				}
				if mode == "ACTION_FINISH_ACQUISITION" {
					kept := a.Acquisitions[:0]
					for _, g := range a.Acquisitions {
						if g.SourceID != source {
							kept = append(kept, g)
						}
					}
					a.Acquisitions = kept
				}
				raw, e := json.Marshal(a)
				if e != nil {
					return e
				}
				_, e = tx.ExecContext(c, "UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", raw, *cl.WorldInstanceID)
				return e
			}
			_, e := tx.ExecContext(c, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
			return e
		}
	}
	if mode == "COMMIT_ACK" {
		r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
			var n int
			if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?", child.TurnID).Scan(&n); e != nil {
				return e
			}
			if e := tx.Commit(); e != nil {
				return e
			}
			if n > 0 {
				return errors.New("child world ACK lost")
			}
			return nil
		}
	}
	committed, e := r.Store.CommitAttemptTurn(childCtx, cl.TaskLease, "rehearsal-context", d.AttemptID)
	if rollback || revoke {
		if e == nil {
			t.Fatal("joint commit escaped rollback", mode)
		}
		r.Store.BeforeCommit = nil
		for _, table := range []string{"harness_turn_commits", "approved_results", "service_events", "postings", "outbox"} {
			var n int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil || n != 0 {
				t.Fatal("partial child commit", table, n, e)
			}
		}
		var afterMemory, afterItem []byte
		if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&afterMemory); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT document FROM items WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&afterItem); e != nil {
			t.Fatal(e)
		}
		if string(beforeMemory) != string(afterMemory) || string(beforeItem) != string(afterItem) {
			t.Fatal("action/memory did not rollback atomically")
		}
		var next int
		if e = r.Store.DB.QueryRow("SELECT next_input FROM harness_admin_rehearsals WHERE task_id=?", root.TaskID).Scan(&next); e != nil || next != 0 {
			t.Fatal("rolled-back child advanced root input", next, e)
		}
		return
	}
	if mode == "COMMIT_ACK" {
		if !errors.Is(e, store.ErrCommitUnknown) {
			t.Fatal("expected original ACK uncertainty", e)
		}
		r.Store.Commit = nil
		committed, e = r.Store.QueryOriginalTurnCommit(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: *cl.WorldInstanceID}, cl.TaskID)
	}
	if e != nil || !committed.Committed {
		t.Fatal("actual child business COMMIT", e)
	}
	var nextInput int
	if e = r.Store.DB.QueryRow("SELECT next_input FROM harness_admin_rehearsals WHERE task_id=?", root.TaskID).Scan(&nextInput); e != nil || nextInput != 1 {
		t.Fatal("joint original input progress", nextInput, e)
	}
	var services, held, childStepsFinal int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM service_events").Scan(&services); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE status='HELD'").Scan(&held); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", cl.TaskID).Scan(&childStepsFinal); e != nil {
		t.Fatal(e)
	}
	expectedCalls, expectedSteps, expectedHolds := 1, 9, 5
	if repair {
		expectedCalls, expectedSteps, expectedHolds = 2, 14, 7
	}
	if services != 0 || held != expectedHolds || childStepsFinal != expectedSteps {
		t.Fatal("child cannot settle parent service/holds", services, held, childStepsFinal)
	}
	var parentHeld, supplierHeld, lines, postings int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=? AND purpose='PARENT' AND status='HELD' AND kind IN ('customer','project','platform_risk')", root.TaskID).Scan(&parentHeld); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=? AND purpose=? AND status='HELD' AND kind IN ('supplier_global','supplier_tenant')", child.TurnID, d.AttemptID).Scan(&supplierHeld); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_service_lines").Scan(&lines); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM postings").Scan(&postings); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT steps_used,calls_allocated FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&rootSteps, &rootCalls); e != nil {
		t.Fatal(e)
	}
	if parentHeld != 3 || supplierHeld != 2 || lines != 0 || postings != 0 || rootSteps != expectedSteps+1 || rootCalls != expectedCalls {
		t.Fatal("shared original root accounting", parentHeld, supplierHeld, lines, postings, rootSteps, rootCalls)
	}
	if reportMode {
		reader, ok := any(r.Files).(store.RehearsalReportReader)
		if !ok {
			t.Fatal("fixed all-child report evidence owner missing")
		}
		if mode == "REPORT_REPAIR" || mode == "REPORT_HISTORY_REPAIR" {
			if partial, e := reader.ReadRehearsalReportEvidence(ctx, rl.TaskLease, "rehearsal-context"); e == nil {
				partial.Close()
				t.Fatal("partial child set became complete report")
			}
			second, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, "rehearsal-context")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, second.TaskLease, "rehearsal-context"); e != nil {
				t.Fatal(e)
			}
			d, e := r.Store.PrepareRehearsalTurn(ctx, rl.TaskLease, second.TaskLease, "rehearsal-context")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d.CommandKey = content.Identity(second.TaskID, "send")
			d.CommitKey = d.CommandKey
			permit, e := r.Store.StartSend(ctx, d, d.AttemptID)
			if e != nil || !permit.CanSend {
				t.Fatal(e)
			}
			if e = r.Worker.Turns.Generate(ctx, permit.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			secondCtx, done, e := r.Store.EnterRehearsalChildExecution(ctx, rl.TaskLease, second.TaskLease, "rehearsal-context")
			if e != nil {
				t.Fatal(e)
			}
			defer done()
			if _, e = r.Store.ObserveOriginalReply(secondCtx, permit.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.ValidateOriginalTurn(secondCtx, second.TaskLease, "rehearsal-context"); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.ApproveOriginalTurn(secondCtx, second.TaskLease, "rehearsal-context"); e != nil {
				t.Fatal(e)
			}
			if result, e := r.Store.CommitOriginalTurn(secondCtx, second.TaskLease, "rehearsal-context"); e != nil || !result.Committed {
				t.Fatal(e)
			}
		}
		evidence, e := reader.ReadRehearsalReportEvidence(ctx, rl.TaskLease, "rehearsal-context")
		if e != nil {
			t.Fatal(e)
		}
		defer evidence.Close()
		children, e := reader.RehearsalReportChildren(evidence, rl.TaskLease, "rehearsal-context")
		if e != nil || len(children) != len(req.Inputs) || children[0].Index != 0 || children[0].Commit.ResultID == "" || len(children[0].Body) == 0 {
			t.Fatal("incomplete original report evidence", e)
		}
		wrong := rl.TaskLease
		wrong.Fence++
		if _, e = reader.RehearsalReportChildren(evidence, wrong, "rehearsal-context"); e == nil {
			t.Fatal("wrong root evidence")
		}
		original := append([]byte(nil), children[0].Body...)
		children[0].Body[0] = '!'
		again, e := reader.RehearsalReportChildren(evidence, rl.TaskLease, "rehearsal-context")
		if e != nil || !bytes.Equal(original, again[0].Body) {
			t.Fatal("mutable candidate changed held bytes", e)
		}
		if (mode == "REPORT_REPAIR" || mode == "REPORT_HISTORY_REPAIR") && (again[0].Commit.OriginalDependency == nil || again[1].Index != 1 || again[1].Commit.OriginalDependency != nil) {
			t.Fatal("0/1 attribution lost")
		}
		// Probe every actual held body lock directly. A short full-reader call can
		// expire in its SQL/source preflight before it ever reaches the body lock.
		var bodyLocks []*localstate.Root
		for _, child := range again {
			metas := []domain.MockResultMetadata{child.Commit.Metadata}
			if child.Commit.OriginalDependency != nil {
				metas = append([]domain.MockResultMetadata{*child.Commit.OriginalDependency}, metas...)
			}
			for _, meta := range metas {
				lock, err := localstate.Open(filepath.Join(r.Files.RootPath, rl.TenantID, rl.ProductID, ".memory-locks", meta.BodyStorage), false)
				if err != nil {
					t.Fatal("original body lock missing", err)
				}
				waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				unlock, waitErr := lock.Lock(waiting, "operations.lock")
				cancel()
				if unlock != nil {
					unlock()
					t.Fatal("report evidence released original body before Close", child.Index, meta.Identity.AttemptID)
				}
				if !errors.Is(waitErr, context.DeadlineExceeded) {
					t.Fatal("wrong held original body lock rejection", waitErr)
				}
				bodyLocks = append(bodyLocks, lock)
			}
		}
		evidence.Close()
		for _, lock := range bodyLocks {
			waiting, cancel := context.WithTimeout(ctx, 5*time.Second)
			unlock, lockErr := lock.Lock(waiting, "operations.lock")
			cancel()
			if lockErr != nil || unlock == nil {
				t.Fatal("Close retained original body lock", lockErr)
			}
			unlock()
		}
		for _, child := range again {
			body, commit, readErr := r.Files.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", child.Index)
			if readErr != nil || child.Commit.World == nil || child.Commit.Negative != nil ||
				!reflect.DeepEqual(commit, *child.Commit.World) || !bytes.Equal(body, child.Body) {
				t.Fatal("Close changed readable original child result", child.Index, readErr)
			}
		}
		if _, e = reader.RehearsalReportChildren(evidence, rl.TaskLease, "rehearsal-context"); e == nil {
			t.Fatal("closed report evidence accepted")
		}
		var lastReportLock store.LockKey
		r.Store.Trace = func(k store.LockKey) { lastReportLock = k }
		results, e := r.Store.InspectRehearsalReport(ctx, rl.TaskLease, "rehearsal-context")
		r.Store.Trace = nil
		if e != nil || len(results) != len(req.Inputs) {
			t.Fatal("joint report proof", e, "last lock", lastReportLock)
		}
		// This fixture committed every child. The owning inspection now also
		// represents negative outcomes, which must not masquerade as these commits.
		for index, result := range results {
			if result.World == nil || result.Negative != nil || result.ResultID == "" ||
				again[index].Index != index || !reflect.DeepEqual(result, again[index].Commit) {
				t.Fatal("joint inspection changed the original committed child identity", index)
			}
		}
		committer, ok := any(r.Store).(interface {
			CommitRehearsalReport(context.Context, domain.TaskLease, string) (store.CommitReceipt, error)
		})
		if !ok {
			t.Fatal("actual typed root report joint COMMIT missing")
		}
		var injected bool
		switch mode {
		case "REPORT_ROLLBACK", "REPORT_FINISH_SOURCE", "REPORT_FINISH_PRIVACY", "REPORT_FINISH_BINDING", "REPORT_FINISH_VIEW", "REPORT_FINISH_NODE", "REPORT_FINISH_LINE", "REPORT_FINISH_STAGE_PRIVACY", "REPORT_FINISH_STAGE_VIEW":
			r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
				var n int
				if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_rehearsal_reports WHERE task_id=? AND state='COMMITTED'", root.TaskID).Scan(&n); e != nil {
					return e
				}
				if n == 0 {
					return nil
				}
				injected = true
				switch mode {
				case "REPORT_ROLLBACK":
					return errors.New("original report rollback")
				case "REPORT_FINISH_SOURCE":
					_, e := tx.ExecContext(c, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
					return e
				case "REPORT_FINISH_VIEW":
					_, e := tx.ExecContext(c, "UPDATE subject_governance SET view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID)
					return e
				case "REPORT_FINISH_NODE":
					var raw []byte
					var source string
					if e := tx.QueryRowContext(c, "SELECT input_source_id FROM harness_turns WHERE task_id=?", cl.TaskID).Scan(&source); e != nil {
						return e
					}
					if e := tx.QueryRowContext(c, "SELECT document FROM memory_aggregates WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&raw); e != nil {
						return e
					}
					var aggregate memory.Aggregate
					if e := json.Unmarshal(raw, &aggregate); e != nil {
						return e
					}
					delete(aggregate.Sources, source)
					raw, e := json.Marshal(aggregate)
					if e != nil {
						return e
					}
					_, e = tx.ExecContext(c, "UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", raw, *cl.WorldInstanceID)
					return e
				case "REPORT_FINISH_LINE":
					_, e := tx.ExecContext(c, "UPDATE harness_service_lines SET input_units=input_units+1 WHERE service_id=?", content.Identity(string(root.TaskID), "service"))
					return e
				case "REPORT_FINISH_PRIVACY":
					_, e := tx.ExecContext(c, "UPDATE subject_governance SET privacy_epoch=privacy_epoch+1 WHERE tenant_id=? AND product_id=? AND subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID)
					return e
				case "REPORT_FINISH_STAGE_PRIVACY":
					_, e := tx.ExecContext(c, "UPDATE harness_rehearsal_reports SET subject_privacy=subject_privacy+1 WHERE task_id=?", root.TaskID)
					return e
				case "REPORT_FINISH_STAGE_VIEW":
					_, e := tx.ExecContext(c, "UPDATE harness_rehearsal_reports SET subject_view=subject_view+1 WHERE task_id=?", root.TaskID)
					return e
				default:
					_, e := tx.ExecContext(c, "UPDATE harness_rehearsal_children SET input_hash=UNHEX(REPEAT('ab',32)) WHERE root_task_id=?", root.TaskID)
					return e
				}
			}
		case "REPORT_ACK_COMMIT", "REPORT_ACK_ROLLBACK":
			r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
				var n int
				if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_rehearsal_reports WHERE task_id=? AND state='COMMITTED'", root.TaskID).Scan(&n); e != nil {
					return e
				}
				if n == 0 {
					return tx.Commit()
				}
				injected = true
				if mode == "REPORT_ACK_COMMIT" {
					if e := tx.Commit(); e != nil {
						return e
					}
				} else {
					if e := tx.Rollback(); e != nil {
						return e
					}
				}
				return errors.New("report original ACK unavailable")
			}
		}
		receipt, e := committer.CommitRehearsalReport(ctx, rl.TaskLease, "rehearsal-context")
		r.Store.BeforeCommit = nil
		r.Store.Commit = nil
		if mode == "REPORT_ACK_COMMIT" || mode == "REPORT_ACK_ROLLBACK" {
			if !injected || !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("missing original report ACK ambiguity", e)
			}
			receipt, e = r.Store.QueryRehearsalReportCommit(ctx, scope, string(root.TaskID))
			if mode == "REPORT_ACK_ROLLBACK" && !errors.Is(e, sql.ErrNoRows) {
				t.Fatal("rollback ACK became report", e)
			}
		}
		retry := mode == "REPORT_ROLLBACK" || strings.HasPrefix(mode, "REPORT_FINISH_") || mode == "REPORT_ACK_ROLLBACK"
		if retry {
			if !injected || e == nil {
				t.Fatal("report escaped terminal rollback", mode, e)
			}
			var n, actualHeld, next, own int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events)+(SELECT COUNT(*) FROM postings)+(SELECT COUNT(*) FROM assets WHERE asset_id=?)+(SELECT COUNT(*) FROM commit_receipts WHERE operation='REHEARSAL_REPORT_COMMIT')", content.Identity(string(root.TaskID), "report-asset")).Scan(&n); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE status='HELD'").Scan(&actualHeld); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRow("SELECT next_input,own_steps FROM harness_admin_rehearsals WHERE task_id=? AND fsm_state='ISOLATED'", root.TaskID).Scan(&next, &own); e != nil {
				t.Fatal(e)
			}
			if n != 0 || actualHeld != 5 || next != 1 || own != 1 {
				t.Fatal("partial parent commit", n, actualHeld, next, own)
			}
			receipt, e = committer.CommitRehearsalReport(ctx, rl.TaskLease, "rehearsal-context")
		}
		if e != nil || !receipt.Committed {
			t.Fatal("root business COMMIT", e)
		}
		var reports, servicesAfter, own int
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_rehearsal_reports WHERE task_id=? AND state='COMMITTED'", root.TaskID).Scan(&reports); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM service_events WHERE task_id=?", root.TaskID).Scan(&servicesAfter); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT own_steps FROM harness_admin_rehearsals WHERE task_id=? AND fsm_state='COMMITTED'", root.TaskID).Scan(&own); e != nil {
			t.Fatal(e)
		}
		var actualSteps, actualCalls, childrenSteps int
		if e = r.Store.DB.QueryRow("SELECT steps_used,calls_allocated FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&actualSteps, &actualCalls); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT SUM(e.steps_used) FROM harness_envelopes e JOIN harness_rehearsal_children c ON c.tenant_id=e.tenant_id AND c.product_id=e.product_id AND c.child_task_id=e.task_id WHERE c.root_task_id=?", root.TaskID).Scan(&childrenSteps); e != nil {
			t.Fatal(e)
		}
		wantCalls := 1
		if mode == "REPORT_REPAIR" || mode == "REPORT_HISTORY_REPAIR" {
			wantCalls = 3
		}
		if actualSteps != childrenSteps+2 || actualCalls != wantCalls {
			t.Fatal("parent counters reset", actualSteps, childrenSteps, actualCalls)
		}
		if reports != 1 || servicesAfter != 1 || own != 2 {
			t.Fatal("missing joint parent effects", reports, servicesAfter, own)
		}
		var linesAfter, retained int
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_service_lines l JOIN attempts a ON a.tenant_id=l.tenant_id AND a.product_id=l.product_id AND a.task_id=l.task_id AND a.attempt_id=l.attempt_id JOIN harness_original_queries q ON q.tenant_id=a.tenant_id AND q.product_id=a.product_id AND q.task_id=a.task_id AND q.attempt_id=a.attempt_id WHERE l.service_id=? AND l.input_units=q.metadata->'$.InputUnits' AND l.output_units=q.metadata->'$.OutputUnits' AND l.service_atoms=0 AND l.supplier_atoms=0", content.Identity(string(root.TaskID), "service")).Scan(&linesAfter); e != nil {
			t.Fatal(e)
		}
		wantLines := 1
		if mode == "REPORT_REPAIR" || mode == "REPORT_HISTORY_REPAIR" {
			wantLines = 3
		}
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE status='HELD'").Scan(&retained); e != nil {
			t.Fatal(e)
		}
		wantRetained := 0
		if mode == "REPORT_COST_PENDING" {
			wantRetained = 3
		}
		if linesAfter != wantLines || retained != wantRetained {
			t.Fatal("actual attempts/finality lost", linesAfter, retained)
		}
		var key string
		if e = r.Store.DB.QueryRow("SELECT storage_key FROM harness_rehearsal_reports WHERE task_id=?", root.TaskID).Scan(&key); e != nil {
			t.Fatal(e)
		}
		reportBytes, e := os.ReadFile(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, key+".part"))
		if e != nil {
			t.Fatal(e)
		}
		var report struct {
			Inputs []struct {
				State    string `json:"state"`
				Supplier string `json:"supplier_reconciliation_state"`
			}
			Assertions []struct {
				State string `json:"state"`
			}
		}
		if json.Unmarshal(reportBytes, &report) != nil || len(report.Inputs) != len(req.Inputs) || report.Inputs[0].State != "COMMITTED" || report.Assertions[0].State != "PASS" {
			t.Fatal("not actual report", string(reportBytes))
		}
		if mode == "REPORT_COST_PENDING" && report.Inputs[0].Supplier != "PENDING_VERIFICATION" {
			t.Fatal("finality invented")
		}
		if strings.HasPrefix(mode, "REPORT_ASSERTIONS") {
			if len(report.Assertions) != len(req.Assertions) {
				t.Fatal("assertion order/count lost")
			}
			for i, a := range report.Assertions {
				want := "PASS"
				if req.Assertions[i] == "SIGN_CUSTODY" && mode == "REPORT_ASSERTIONS_FAIL" {
					want = "FAIL"
				}
				if req.Assertions[i] == "SIGN_CUSTODY" && mode == "REPORT_ASSERTIONS_NOT_RUN" {
					want = "NOT_RUN"
				}
				if a.State != want {
					t.Fatalf("actual %s program not executed: %s", req.Assertions[i], a.State)
				}
			}
			var events int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM runtime_events r JOIN events e USING(tenant_id,product_id,record_id) WHERE e.world_instance_id=?", *cl.WorldInstanceID).Scan(&events); e != nil || events != 0 {
				t.Fatal("assertion program wrote actions", events, e)
			}
		}
		replay, e := committer.CommitRehearsalReport(ctx, rl.TaskLease, "same original receipt")
		if e != nil || replay != receipt {
			t.Fatal("original report replay", e)
		}
		var replaySteps, replayCalls, replayServices int
		if e = r.Store.DB.QueryRow("SELECT steps_used,calls_allocated FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&replaySteps, &replayCalls); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM service_events WHERE task_id=?", root.TaskID).Scan(&replayServices); e != nil {
			t.Fatal(e)
		}
		if replaySteps != actualSteps || replayCalls != actualCalls || replayServices != 1 {
			t.Fatal("report replay repeated work")
		}
		if mode == "REPORT_HISTORY" || mode == "REPORT_HISTORY_REPAIR" {
			asset := content.Identity(string(root.TaskID), "report-asset")
			if record, e := r.Files.Get(ctx, scope, asset); e != nil || string(record.AssetID) != asset {
				t.Fatal("fixed report asset metadata", e)
			}
			opened, e := r.Files.OpenAuthorized(ctx, scope, asset)
			if e != nil {
				t.Fatal("fixed report asset bytes", e)
			}
			actual, readErr := io.ReadAll(opened)
			opened.Close()
			if readErr != nil || !bytes.Equal(actual, reportBytes) {
				t.Fatal("fixed report asset differs", readErr)
			}
		}
		if mode == "REPORT_HISTORY" || mode == "REPORT_HISTORY_REPAIR" {
			reader, ok := any(r.Files).(interface {
				ReadCommittedRehearsalReport(context.Context, domain.ProductScope, string) ([]byte, error)
			})
			if !ok {
				t.Fatal("fixed S57 terminal report reader missing")
			}
			body, e := reader.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
			if e != nil || !bytes.Equal(body, reportBytes) {
				t.Fatal("original report history", e)
			}
			draftNow, e := r.Content.GetDraft(ctx, scope, draft, "")
			if e != nil {
				t.Fatal(e)
			}
			for i, raw := range draftNow.Input.Objects {
				var o map[string]any
				json.Unmarshal(raw, &o)
				if o["kind"] == "WORLD" {
					o["description"] = "New selected world text after the original rehearsal."
					o["generation"] = map[string]any{"mode": "NONE", "required_capabilities": []any{}, "allowed_external_uses": []string{"NONE"}}
					draftNow.Input.Objects[i], _ = json.Marshal(o)
				}
			}
			if _, e = r.Content.SaveDraft(ctx, scope, draft, draftNow.Input, 1, "after-real-report"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=accepted_at-INTERVAL 1 DAY,execute_by=execute_by-INTERVAL 1 DAY,settle_by=settle_by-INTERVAL 1 DAY"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE harness_rehearsal_children SET started_at=started_at-INTERVAL 1 DAY,execute_by=execute_by-INTERVAL 1 DAY"); e != nil {
				t.Fatal(e)
			}
			var receiptsBefore, stepsBefore, servicesBefore int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT SUM(steps_used) FROM harness_envelopes),(SELECT COUNT(*) FROM service_events)").Scan(&receiptsBefore, &stepsBefore, &servicesBefore); e != nil {
				t.Fatal(e)
			}
			body, e = reader.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID))
			if e != nil || !bytes.Equal(body, reportBytes) {
				t.Fatal("history incorrectly reused latest draft/purchase/lease", e)
			}
			asset := content.Identity(string(root.TaskID), "report-asset")
			opened, e := r.Files.OpenAuthorized(ctx, scope, asset)
			if e != nil {
				t.Fatal("asset history reused execution gate", e)
			}
			actual, readErr := io.ReadAll(opened)
			opened.Close()
			if readErr != nil || !bytes.Equal(actual, reportBytes) {
				t.Fatal("asset history bytes", readErr)
			}
			request := httptest.NewRequest("GET", "/world/v1/assets/"+asset+"/content?product_id=coffee", nil)
			request.Header.Set("Authorization", "Bearer "+runtimeToken)
			response := httptest.NewRecorder()
			r.Handler().ServeHTTP(response, request)
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), reportBytes) {
				t.Fatal("HTTP committed report history", response.Code, response.Body.String())
			}
			for _, target := range []struct{ name, path, token string }{
				{"WRONG_BEARER", "/world/v1/assets/" + asset + "/content?product_id=coffee", "not-the-owner"},
				{"WRONG_PRODUCT", "/world/v1/assets/" + asset + "/content?product_id=other", runtimeToken},
			} {
				t.Run(target.name, func(t *testing.T) {
					q := httptest.NewRequest("GET", target.path, nil)
					q.Header.Set("Authorization", "Bearer "+target.token)
					w := httptest.NewRecorder()
					r.Handler().ServeHTTP(w, q)
					if w.Code == 200 {
						t.Fatal("HTTP history authenticated wrong principal/scope")
					}
				})
			}
			t.Run("STREAM_REVOKE", func(t *testing.T) {
				stream, e := r.Files.OpenAuthorized(ctx, scope, asset)
				if e != nil {
					t.Fatal(e)
				}
				defer stream.Close()
				if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='asset.read'"); e != nil {
					t.Fatal(e)
				}
				defer r.Store.DB.Exec("INSERT INTO operator_actions(credential_id,action) VALUES('owner','asset.read')")
				if n, e := stream.Read(make([]byte, 8)); e == nil || n != 0 {
					t.Fatal("stream reused prior permission", n, e)
				}
			})
			for _, change := range []struct {
				name, apply, restore string
				arg                  any
			}{
				{"CONTENT_READ", "DELETE FROM operator_actions WHERE credential_id='owner' AND action=?", "INSERT INTO operator_actions(credential_id,action) VALUES('owner',?)", "content.read"},
				{"ASSET_READ", "DELETE FROM operator_actions WHERE credential_id='owner' AND action=?", "INSERT INTO operator_actions(credential_id,action) VALUES('owner',?)", "asset.read"},
				{"SUBJECT_PRIVACY", "UPDATE subject_governance SET privacy_epoch=privacy_epoch+1 WHERE subject_id=?", "UPDATE subject_governance SET privacy_epoch=privacy_epoch-1 WHERE subject_id=?", "creator"},
				{"SUBJECT_VIEW", "UPDATE subject_governance SET view_epoch=view_epoch+1 WHERE subject_id=?", "UPDATE subject_governance SET view_epoch=view_epoch-1 WHERE subject_id=?", "creator"},
				{"DISABLED", "UPDATE subject_governance SET enabled=0 WHERE subject_id=?", "UPDATE subject_governance SET enabled=1 WHERE subject_id=?", "creator"},
				{"WORLD_PRIVACY", "UPDATE world_controls SET privacy_epoch=privacy_epoch+1 WHERE world_instance_id=?", "UPDATE world_controls SET privacy_epoch=privacy_epoch-1 WHERE world_instance_id=?", *cl.WorldInstanceID},
				{"WORLD_CONTROL", "UPDATE world_controls SET control_version=control_version+1 WHERE world_instance_id=?", "UPDATE world_controls SET control_version=control_version-1 WHERE world_instance_id=?", *cl.WorldInstanceID},
				{"ROOT_COMMIT", "UPDATE commit_receipts SET operation='REHEARSAL_REPORT_STAGE' WHERE commit_key=?", "UPDATE commit_receipts SET operation='REHEARSAL_REPORT_COMMIT' WHERE commit_key=?", content.Identity(string(root.TaskID), "report-commit")},
				{"CHILD_COMMIT", "UPDATE commit_receipts SET operation='HARNESS_RESULT_VALIDATE' WHERE commit_key=?", "UPDATE commit_receipts SET operation='HARNESS_TURN_COMMIT' WHERE commit_key=?", content.Identity(cl.TaskID, "world-commit")},
				{"REPORT_DELETE", "UPDATE harness_rehearsal_reports SET delete_intent=1 WHERE task_id=?", "UPDATE harness_rehearsal_reports SET delete_intent=0 WHERE task_id=?", root.TaskID},
				{"DRAFT_DELETE", "UPDATE content_drafts SET deleted=1 WHERE draft_id=?", "UPDATE content_drafts SET deleted=0 WHERE draft_id=?", draft},
			} {
				t.Run(change.name, func(t *testing.T) {
					if _, e := r.Store.DB.Exec(change.apply, change.arg); e != nil {
						t.Fatal(e)
					}
					defer func() {
						if _, e := r.Store.DB.Exec(change.restore, change.arg); e != nil {
							t.Fatal(e)
						}
					}()
					if _, e := reader.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
						t.Fatal("terminal ignored current proof")
					}
					if _, e := r.Files.Get(ctx, scope, asset); e == nil {
						t.Fatal("asset metadata bypassed fixed proof")
					}
					if stream, e := r.Files.OpenAuthorized(ctx, scope, asset); e == nil {
						stream.Close()
						t.Fatal("asset body bypassed fixed proof")
					}
					q := httptest.NewRequest("GET", "/world/v1/assets/"+asset+"/content?product_id=coffee", nil)
					q.Header.Set("Authorization", "Bearer "+runtimeToken)
					w := httptest.NewRecorder()
					r.Handler().ServeHTTP(w, q)
					if w.Code == 200 {
						t.Fatal("HTTP history bypassed current proof")
					}
				})
			}
			for _, column := range []string{"commit_hash", "evidence_hash"} {
				t.Run("REPORT_"+column, func(t *testing.T) {
					var original []byte
					if e := r.Store.DB.QueryRow("SELECT "+column+" FROM harness_rehearsal_reports WHERE task_id=?", root.TaskID).Scan(&original); e != nil {
						t.Fatal(e)
					}
					if _, e := r.Store.DB.Exec("UPDATE harness_rehearsal_reports SET "+column+"=UNHEX(REPEAT('ab',32)) WHERE task_id=?", root.TaskID); e != nil {
						t.Fatal(e)
					}
					defer r.Store.DB.Exec("UPDATE harness_rehearsal_reports SET "+column+"=? WHERE task_id=?", original, root.TaskID)
					if _, e := reader.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
						t.Fatal("report original evidence binding ignored")
					}
				})
			}
			var stored []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", cl.TaskID).Scan(&stored); e != nil {
				t.Fatal(e)
			}
			var committed domain.CandidateCommit
			if e = json.Unmarshal(stored, &committed); e != nil {
				t.Fatal(e)
			}
			stores := []string{committed.Metadata.BodyStorage, key}
			if committed.OriginalDependency != nil {
				stores = append(stores, committed.OriginalDependency.BodyStorage)
			}
			for i, storage := range stores {
				t.Run(fmt.Sprintf("BODY_%d", i), func(t *testing.T) {
					p := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".part")
					original, e := os.ReadFile(p)
					if e != nil {
						t.Fatal(e)
					}
					if e = os.WriteFile(p, []byte("corrupt original governed bytes"), 0600); e != nil {
						t.Fatal(e)
					}
					defer os.WriteFile(p, original, 0600)
					if _, e = reader.ReadCommittedRehearsalReport(ctx, scope, string(root.TaskID)); e == nil {
						t.Fatal("corrupt dependency read")
					}
					if stream, e := r.Files.OpenAuthorized(ctx, scope, asset); e == nil {
						stream.Close()
						t.Fatal("asset body used fallback")
					}
				})
			}
			var ar, as, av int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT SUM(steps_used) FROM harness_envelopes),(SELECT COUNT(*) FROM service_events)").Scan(&ar, &as, &av); e != nil || ar != receiptsBefore || as != stepsBefore || av != servicesBefore {
				t.Fatal("history mutated execution", e, ar, as, av)
			}
		}
		return
	}
	if mode == "COST_PENDING" || mode == "REPORT_COST_PENDING" {
		id, e := r.Store.OriginalMockIdentity(ctx, domain.Scope{ProductScope: scope, WorldInstanceID: *cl.WorldInstanceID}, cl.TaskID)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
			t.Fatal("necessary cost used before original foreground/deadline gate")
		}
		if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(accepted_at,INTERVAL 61 SECOND),execute_by=DATE_SUB(execute_by,INTERVAL 61 SECOND),settle_by=DATE_SUB(settle_by,INTERVAL 61 SECOND) WHERE task_id=?", cl.TaskID); e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action IN ('content.rehearse','content.read','asset.read')"); e != nil {
			t.Fatal(e)
		}
		due, e := r.Store.DueNecessaryCostQuery(ctx)
		if e != nil || due != id {
			t.Fatal("real narrative success lost necessary cost scan", e)
		}
		q, e := r.Store.BeginNecessaryCostQuery(ctx, due)
		if e != nil {
			t.Fatal(e)
		}
		cost, e := r.Store.RecordNecessaryCostQuery(ctx, q)
		if e != nil || cost.Identity != id || !cost.UsageKnown || cost.NoFurtherCost {
			t.Fatal("nonfinal metadata cost receipt", e)
		}
		if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
			t.Fatal("cost backoff bypassed")
		}
		var stillHeld, stillCommits, stillServices, stillCalls, stillSteps int
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE status='HELD'").Scan(&stillHeld); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turn_commits").Scan(&stillCommits); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM service_events").Scan(&stillServices); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT calls_allocated,steps_used FROM harness_envelopes WHERE task_id=?", root.TaskID).Scan(&stillCalls, &stillSteps); e != nil {
			t.Fatal(e)
		}
		if stillHeld != 5 || stillCommits != 1 || stillServices != 0 || stillCalls != rootCalls || stillSteps != rootSteps {
			t.Fatal("cost query changed narrative or original liability", stillHeld, stillCommits, stillServices, stillCalls, stillSteps)
		}
		return
	}
	var consumer string
	if e = r.Store.DB.QueryRow("SELECT consumer_id FROM outbox WHERE task_id=?", child.TurnID).Scan(&consumer); e != nil || consumer != content.Identity(string(root.TaskID), "rehearsal-consumer") {
		t.Fatal("isolated notification escaped root", consumer, e)
	}
	if action {
		var raw []byte
		var c domain.CandidateCommit
		if e = r.Store.DB.QueryRow("SELECT document FROM harness_turn_commits WHERE task_id=?", child.TurnID).Scan(&raw); e != nil || json.Unmarshal(raw, &c) != nil {
			t.Fatal(e)
		}
		if len(c.EventIDs) != 1 || len(c.MemoryIDs) != 1 {
			t.Fatal("actual sandbox action and derived memory missing", c.EventIDs, c.MemoryIDs)
		}
		if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&raw); e != nil {
			t.Fatal(e)
		}
		var a memory.Aggregate
		if e = json.Unmarshal(raw, &a); e != nil {
			t.Fatal(e)
		}
		if a.Nodes[c.MemoryIDs[0]].Domain != memory.Sandbox {
			t.Fatal("synthetic memory escaped domain")
		}
	}
	if mode == "READ" || mode == "READ_REPAIR" || mode == "READ_ACTION" {
		if mode == "READ_ACTION" {
			var next int
			if e = r.Store.DB.QueryRow("SELECT next_input FROM harness_admin_rehearsals WHERE task_id=?", root.TaskID).Scan(&next); e != nil || next != 1 {
				t.Fatal("known child joint COMMIT did not advance exact original input", next, e)
			}
		}
		reader, ok := any(r.Files).(rehearsalChildResultReader)
		if !ok {
			t.Fatal("fixed parent-report child body reader missing")
		}
		if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE task_id=?", cl.TaskID); e != nil {
			t.Fatal(e)
		}
		var beforeReceipts, beforeSteps, beforeAttempts int
		if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT SUM(steps_used) FROM harness_envelopes),(SELECT COUNT(*) FROM attempts)").Scan(&beforeReceipts, &beforeSteps, &beforeAttempts); e != nil {
			t.Fatal(e)
		}
		body, c, e := reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0)
		if e != nil || len(body) == 0 || c.Metadata.Identity.TaskID != cl.TaskID {
			t.Fatal("parent read actual committed child", e)
		}
		actual, e := os.ReadFile(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, c.Metadata.BodyStorage+".part"))
		if e != nil || !bytes.Equal(actual, body) {
			t.Fatal("actual original bytes", e)
		}
		if repair && c.OriginalDependency == nil {
			t.Fatal("missing actual original0 dependency")
		}
		badRoot := rl.TaskLease
		badRoot.Fence++
		if _, _, e = reader.ReadRehearsalChildResult(ctx, badRoot, "rehearsal-context", 0); e == nil {
			t.Fatal("old root fence read")
		}
		if _, _, e = reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 1); e == nil {
			t.Fatal("unstarted index read")
		}
		for _, change := range []struct {
			name, apply, restore string
			arg                  any
		}{
			{"ROOT_CANCEL", "UPDATE tasks SET cancelled=1 WHERE task_id=?", "UPDATE tasks SET cancelled=0 WHERE task_id=?", root.TaskID},
			{"PRIVACY", "UPDATE world_controls SET privacy_epoch=privacy_epoch+1 WHERE world_instance_id=?", "UPDATE world_controls SET privacy_epoch=privacy_epoch-1 WHERE world_instance_id=?", *cl.WorldInstanceID},
			{"VIEW", "UPDATE world_controls SET view_epoch=view_epoch+1 WHERE world_instance_id=?", "UPDATE world_controls SET view_epoch=view_epoch-1 WHERE world_instance_id=?", *cl.WorldInstanceID},
			{"CONTROL", "UPDATE world_controls SET control_version=control_version+1 WHERE world_instance_id=?", "UPDATE world_controls SET control_version=control_version-1 WHERE world_instance_id=?", *cl.WorldInstanceID},
			{"COMMIT_RECEIPT", "UPDATE commit_receipts SET operation='HARNESS_RESULT_VALIDATE' WHERE commit_key=?", "UPDATE commit_receipts SET operation='HARNESS_TURN_COMMIT' WHERE commit_key=?", c.ReceiptKey},
			{"CHILD_ACCEPTED_RECEIPT", "UPDATE commit_receipts SET operation='REHEARSAL_INIT' WHERE commit_key=?", "UPDATE commit_receipts SET operation='REHEARSAL_CHILD_START' WHERE commit_key=?", content.Identity(cl.TaskID, "accepted")},
			{"APPROVAL_RECEIPT", "UPDATE commit_receipts SET operation='HARNESS_RESULT_VALIDATE' WHERE commit_key=?", "UPDATE commit_receipts SET operation='HARNESS_CANDIDATE_APPROVE' WHERE commit_key=?", content.Identity(d.AttemptID, "approval")},
			{"LINEAGE_DELETE", "UPDATE memory_asset_lineage SET delete_intent=1 WHERE storage_key=?", "UPDATE memory_asset_lineage SET delete_intent=0 WHERE storage_key=?", c.Metadata.BodyStorage},
		} {
			t.Run(change.name, func(t *testing.T) {
				if _, e := r.Store.DB.Exec(change.apply, change.arg); e != nil {
					t.Fatal(e)
				}
				defer func() {
					if _, e := r.Store.DB.Exec(change.restore, change.arg); e != nil {
						t.Fatal(e)
					}
				}()
				if _, _, e := reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0); e == nil {
					t.Fatal("read ignored current proof", change.name)
				}
			})
		}
		var memoryRaw []byte
		if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", *cl.WorldInstanceID).Scan(&memoryRaw); e != nil {
			t.Fatal(e)
		}
		t.Run("CURRENT_ACQUISITION", func(t *testing.T) {
			var a memory.Aggregate
			if e := json.Unmarshal(memoryRaw, &a); e != nil {
				t.Fatal(e)
			}
			a.Acquisitions = nil
			raw, e := json.Marshal(a)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", raw, *cl.WorldInstanceID); e != nil {
				t.Fatal(e)
			}
			defer func() {
				if _, e := r.Store.DB.Exec("UPDATE memory_aggregates SET document=? WHERE world_instance_id=?", memoryRaw, *cl.WorldInstanceID); e != nil {
					t.Fatal(e)
				}
			}()
			if _, _, e := reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0); e == nil {
				t.Fatal("lost acquisition read")
			}
		})
		metas := []domain.MockResultMetadata{c.Metadata}
		if c.OriginalDependency != nil {
			metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
		}
		for i, meta := range metas {
			t.Run("BODY_"+string(rune('0'+i)), func(t *testing.T) {
				path := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, meta.BodyStorage+".part")
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				defer func() {
					if e := os.WriteFile(path, raw, 0600); e != nil {
						t.Fatal(e)
					}
				}()
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if _, _, e = reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0); e == nil {
					t.Fatal("missing body read")
				}
				if e = os.WriteFile(path, []byte("corrupt governed bytes"), 0600); e != nil {
					t.Fatal(e)
				}
				if _, _, e = reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0); e == nil {
					t.Fatal("corrupt body read")
				}
			})
		}
		again, _, e := reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0)
		if e != nil || !bytes.Equal(body, again) {
			t.Fatal("current restored evidence", e)
		}
		var receipts, steps, attempts int
		if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),(SELECT SUM(steps_used) FROM harness_envelopes),(SELECT COUNT(*) FROM attempts)").Scan(&receipts, &steps, &attempts); e != nil {
			t.Fatal(e)
		}
		if receipts != beforeReceipts || steps != beforeSteps || attempts != beforeAttempts {
			t.Fatal("reader wrote receipt/FSM/purchase", beforeReceipts, receipts, beforeSteps, steps, beforeAttempts, attempts)
		}
		if mode == "READ_ACTION" {
			next, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, "rehearsal-context")
			if e != nil || next.TaskID == cl.TaskID {
				t.Fatal("actual next input claim", e)
			}
			if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, next.TaskLease, "rehearsal-context"); e != nil {
				t.Fatal("actual second TOLD/context", e)
			}
			after, _, e := reader.ReadRehearsalChildResult(ctx, rl.TaskLease, "rehearsal-context", 0)
			if e != nil || !bytes.Equal(after, body) {
				t.Fatal("later real sandbox graph growth revoked earlier child", e)
			}
		}
	}
}

func TestRehearsalChildOriginalInput(t *testing.T) {
	ctx := context.Background()
	r, draft, _ := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""},{"actor_slot":"mia","text":"next input must wait"}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "child-original", req)
	if e != nil {
		t.Fatal(e)
	}
	lease, e := r.Store.ClaimRegistered(ctx, "rehearsal-child-owner", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.InitializeDraftRehearsal(ctx, lease.TaskLease, "rehearsal-child-owner"); e != nil {
		t.Fatal(e)
	}
	starter, ok := any(r.Store).(rehearsalChildStarter)
	if !ok {
		t.Fatal("actual owning rehearsal child intake is missing")
	}
	child, e := starter.StartRehearsalChild(ctx, lease.TaskLease, "rehearsal-child-owner")
	if e != nil {
		t.Fatal(e)
	}
	world := content.Identity(string(root.TaskID), "rehearsal-world")
	var actor, slot string
	if e = r.Store.DB.QueryRow("SELECT actor_id,slot_id FROM actors WHERE world_instance_id=? AND slot_id='mia'", world).Scan(&actor, &slot); e != nil {
		t.Fatal(e)
	}
	var selected, source string
	var start, end, rootEnd time.Time
	if e = r.Store.DB.QueryRow("SELECT h.actor_id,h.input_source_id,c.started_at,c.execute_by,e.execute_by FROM harness_rehearsal_children c JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.child_task_id JOIN harness_envelopes e ON e.tenant_id=c.tenant_id AND e.product_id=c.product_id AND e.task_id=c.root_task_id WHERE c.child_task_id=?", child.TurnID).Scan(&selected, &source, &start, &end, &rootEnd); e != nil || selected != actor || end.Sub(start) != time.Minute || end.After(rootEnd) {
		t.Fatal("selected actor and original input deadline", selected, actor, start, end, rootEnd, e)
	}
	var raw []byte
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", world).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var a memory.Aggregate
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	src, ok := a.Sources[source]
	if !ok || src.Domain != memory.Sandbox || src.Body != "" {
		t.Fatal("actual empty synthetic source", src, ok)
	}
	told := 0
	for _, ac := range a.Acquisitions {
		if ac.SourceID == source {
			if ac.ActorID != actor || ac.Kind != memory.Told || ac.EvidenceID != string(child.TurnID) {
				t.Fatal("wrong actual acquisition", ac)
			}
			told++
		}
	}
	if told != 1 {
		t.Fatal("missing unique TOLD", told)
	}
	repeat, e := starter.StartRehearsalChild(ctx, lease.TaskLease, "rehearsal-child-owner")
	if e != nil || repeat.TurnID != child.TurnID {
		t.Fatal("original child replay", repeat, e)
	}
	var children, holds, consent, steps int
	for _, q := range []struct {
		sql  string
		dest *int
	}{{"SELECT COUNT(*) FROM harness_rehearsal_children", &children}, {"SELECT COUNT(*) FROM reservations", &holds}, {"SELECT COUNT(*) FROM memory_subject_permissions WHERE action='memory.generation.use'", &consent}, {"SELECT steps_used FROM harness_envelopes WHERE task_id='" + string(root.TaskID) + "'", &steps}} {
		if e = r.Store.DB.QueryRow(q.sql).Scan(q.dest); e != nil {
			t.Fatal(e)
		}
	}
	if children != 1 || holds != 3 || consent != 0 || steps != 1 {
		t.Fatal("child intake must not duplicate root budget or advance later input", children, holds, consent, steps)
	}
	var againStart, againEnd time.Time
	if e = r.Store.DB.QueryRow("SELECT started_at,execute_by FROM harness_rehearsal_children WHERE child_task_id=?", child.TurnID).Scan(&againStart, &againEnd); e != nil || againStart != start || againEnd != end {
		t.Fatal("replay extended deadline", e)
	}
}
