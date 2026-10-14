package integration

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type rehearsalCutoffOwner interface {
	CutoffDraftRehearsal(context.Context, domain.ProductScope, string) (store.HarnessCutoffResult, error)
}

func TestRehearsalCutoffOldWritersAndFinisher(t *testing.T) {
	for _, mode := range []string{"UNSENT", "MARKER", "CHILD_COMMITTED", "CHILD_COMMIT_UNKNOWN", "CANCEL", "MISSING_J", "FINISH_SERVICE", "FINISH_BINDING", "FINISH_HOLD", "ACK_COMMIT", "ACK_ROLLBACK"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "cutoff-writers", req)
			if e != nil {
				t.Fatal(e)
			}
			worker := "cutoff-worker"
			rl, e := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(root.TaskID)}})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.InitializeDraftRehearsal(ctx, rl.TaskLease, worker); e != nil {
				t.Fatal(e)
			}
			cl, e := r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, worker); e != nil {
				t.Fatal(e)
			}
			childCtx, close, e := r.Store.EnterRehearsalChildExecution(ctx, rl.TaskLease, cl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			defer close()
			d, e := r.Store.PrepareRehearsalTurn(childCtx, rl.TaskLease, cl.TaskLease, worker)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AuthorizeDispatch(childCtx, d); e != nil {
				t.Fatal(e)
			}
			var permit *store.OriginalSendPermit
			if mode == "MARKER" {
				d.CommandKey = content.Identity(cl.TaskID, "send")
				d.CommitKey = d.CommandKey
				send, e := r.Store.StartSend(childCtx, d, d.AttemptID)
				if e != nil {
					t.Fatal(e)
				}
				permit = send.OriginalSend()
			}
			if mode == "CHILD_COMMITTED" || mode == "CHILD_COMMIT_UNKNOWN" {
				if mode == "CHILD_COMMIT_UNKNOWN" {
					r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
						var n int
						if e := tx.QueryRowContext(c, "SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?", cl.TaskID).Scan(&n); e != nil {
							return e
						}
						if n == 0 {
							return tx.Commit()
						}
						if e := tx.Rollback(); e != nil {
							return e
						}
						return errors.New("child business ACK unknown")
					}
				}
				e = r.Worker.Turns.Execute(childCtx, cl.TaskLease, worker)
				r.Store.Commit = nil
				if mode == "CHILD_COMMIT_UNKNOWN" {
					if !errors.Is(e, store.ErrCommitUnknown) {
						t.Fatal("actual child COMMIT ambiguity", e)
					}
				} else if e != nil {
					t.Fatal("actual original child COMMIT", e)
				}
			}
			if mode == "CANCEL" {
				runtimeHTTP(t, r, "POST", "/tasks/"+string(root.TaskID)+"/cancel", "cancel-root", generated.AdminCancelRequest{ProductID: "coffee", Reason: "stop isolation"}, 202, nil)
				if _, e = r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker); e == nil {
					t.Fatal("cancel did not block original child")
				}
			}
			expireCutoff(t, r.Store.DB, string(root.TaskID))
			if mode == "MISSING_J" {
				path := filepath.Join(filepath.Dir(r.Local.Coordinator.Directory()), "mock", "anchor.json")
				if e = os.Rename(path, path+".held"); e != nil {
					t.Fatal(e)
				}
				_, got := r.Store.CutoffDraftRehearsal(ctx, scope, string(root.TaskID))
				if e = os.Rename(path+".held", path); e != nil {
					t.Fatal(e)
				}
				if got == nil {
					t.Fatal("cutoff trusted missing independent J anchor")
				}
			}
			injected := false
			if mode == "FINISH_SERVICE" || mode == "FINISH_BINDING" || mode == "FINISH_HOLD" {
				r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					injected = true
					var e error
					switch mode {
					case "FINISH_SERVICE":
						_, e = tx.ExecContext(c, "UPDATE harness_settlement_services SET enabled=0")
					case "FINISH_BINDING":
						_, e = tx.ExecContext(c, "UPDATE harness_rehearsal_children SET input_hash=UNHEX(REPEAT('ab',32)) WHERE root_task_id=?", root.TaskID)
					case "FINISH_HOLD":
						_, e = tx.ExecContext(c, "UPDATE reservations SET status='HELD' WHERE task_id=? AND kind='customer'", root.TaskID)
					}
					return e
				}
			}
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					injected = true
					var e error
					if mode == "ACK_COMMIT" {
						e = tx.Commit()
					} else {
						e = tx.Rollback()
					}
					if e != nil {
						return e
					}
					return errors.New("cutoff original ACK unavailable")
				}
			}
			_, e = r.Store.CutoffDraftRehearsal(ctx, scope, string(root.TaskID))
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if mode == "ACK_COMMIT" || mode == "ACK_ROLLBACK" {
				if !injected || !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal("missing actual cutoff ambiguity", e)
				}
			} else if injected {
				if e == nil {
					t.Fatal("cutoff finisher bypass", mode)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			if injected && mode != "ACK_COMMIT" {
				var n int
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_cutoffs)+(SELECT COUNT(*) FROM postings)+(SELECT COUNT(*) FROM reservations WHERE status<>'HELD')").Scan(&n); e != nil || n != 0 {
					t.Fatal("partial cutoff escaped rollback", n, e)
				}
			}
			out, e := r.Store.CutoffDraftRehearsal(ctx, scope, string(root.TaskID))
			if e != nil || out.State != "CUTOFF" {
				t.Fatal("original cutoff recovery", out, e)
			}
			if _, e = r.Store.AuthorizeDispatch(childCtx, d); e == nil {
				t.Fatal("retired authorization replay")
			}
			if send, e := r.Store.StartSend(childCtx, d, d.AttemptID); e == nil && send.CanSend {
				t.Fatal("retired send")
			}
			if permit != nil {
				if e = r.Worker.Turns.Generate(childCtx, permit); e == nil {
					t.Fatal("old unused opaque permit generated after cutoff")
				}
			}
			if _, e = r.Store.ClaimRehearsalChild(ctx, rl.TaskLease, worker); e == nil {
				t.Fatal("retired root claimed child")
			}
			if _, e = r.Store.BindRehearsalChildContext(ctx, rl.TaskLease, cl.TaskLease, worker); e == nil {
				t.Fatal("retired root rebound input")
			}
			if _, e = r.Store.CommitRehearsalReport(ctx, rl.TaskLease, worker); e == nil {
				t.Fatal("retired root reported")
			}
			var released, held, risk int
			if e = r.Store.DB.QueryRow("SELECT SUM(status='RELEASED'),SUM(status='HELD'),SUM(status='RISK') FROM reservations").Scan(&released, &held, &risk); e != nil {
				t.Fatal(e)
			}
			if mode == "MARKER" || mode == "CHILD_COMMITTED" || mode == "CHILD_COMMIT_UNKNOWN" {
				if released != 2 || held != 2 || risk != 1 {
					t.Fatal(released, held, risk)
				}
			} else if released != 5 || held != 0 || risk != 0 {
				t.Fatal(released, held, risk)
			}
			if mode == "CHILD_COMMITTED" || mode == "CHILD_COMMIT_UNKNOWN" {
				var commits, unresolved int
				if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_turn_commits),(SELECT COUNT(*) FROM harness_local_rounds WHERE phase='WORLD_COMMIT' AND state IN ('PENDING','UNKNOWN'))").Scan(&commits, &unresolved); e != nil {
					t.Fatal(e)
				}
				if mode == "CHILD_COMMITTED" && commits != 1 {
					t.Fatal("cutoff erased child fact", commits)
				}
				if mode == "CHILD_COMMIT_UNKNOWN" && (commits != 0 || unresolved != 1) {
					t.Fatal("cutoff invented original rollback", commits, unresolved)
				}
			}
		})
	}
}

func TestRehearsalOriginalRootCutoff(t *testing.T) {
	for _, mode := range []string{"UNSTARTED", "SENT_UNKNOWN", "REPORT_COMMITTED", "SCANNER"} {
		t.Run(mode, func(t *testing.T) {
			r, draft, _ := candidateHTTPFixture(t)
			ctx := runtimeContext(t, r)
			scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
			var req generated.RehearsalRequest
			if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
				t.Fatal(e)
			}
			root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "root-cutoff", req)
			if e != nil {
				t.Fatal(e)
			}
			if mode != "UNSTARTED" && mode != "SCANNER" {
				if mode == "SENT_UNKNOWN" {
					r.Worker.Turns.Fault = harness.MockDropOriginalReply
				}
				e = r.Worker.Once(ctx)
				if mode == "SENT_UNKNOWN" {
					if !errors.Is(e, harness.ErrMockReplyLost) {
						t.Fatal(e)
					}
				} else if e != nil {
					t.Fatal(e)
				}
			}
			port, ok := any(r.Store).(rehearsalCutoffOwner)
			if !ok {
				t.Fatal("missing fixed root-child cutoff owner")
			}
			if mode != "REPORT_COMMITTED" {
				if _, e = port.CutoffDraftRehearsal(ctx, scope, string(root.TaskID)); e == nil {
					t.Fatal("cutoff before original deadline")
				}
			}
			expireCutoff(t, r.Store.DB, string(root.TaskID))
			if mode == "SCANNER" {
				done, failed, e := r.Store.RunHarnessCutoffs(ctx)
				if e != nil || done != 1 || failed != 0 {
					t.Fatal("actual registered original root cutoff work", done, failed, e)
				}
			}
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action IN ('content.rehearse','content.read','asset.read')"); e != nil {
				t.Fatal(e)
			}
			out, e := port.CutoffDraftRehearsal(ctx, scope, string(root.TaskID))
			if e != nil {
				t.Fatal("actual original root cutoff", e)
			}
			again, e := port.CutoffDraftRehearsal(ctx, scope, string(root.TaskID))
			if e != nil || again != out {
				t.Fatal("original cutoff replay", again, out, e)
			}
			view, e := r.Store.ReadDraftRehearsalTask(ctx, scope, string(root.TaskID))
			if e != nil {
				t.Fatal(e)
			}
			wantState := "FAILED"
			if mode == "REPORT_COMMITTED" {
				wantState = "COMMITTED"
			}
			if view.TaskState != wantState {
				t.Fatal("cutoff proof narrative projection", view.TaskState, wantState)
			}
			var released, held, risk, services, commits int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM reservations WHERE status='RELEASED'),(SELECT COUNT(*) FROM reservations WHERE status='HELD'),(SELECT COUNT(*) FROM reservations WHERE status='RISK'),(SELECT COUNT(*) FROM service_events),(SELECT COUNT(*) FROM harness_turn_commits)").Scan(&released, &held, &risk, &services, &commits); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "UNSTARTED", "SCANNER":
				if out.State != "CUTOFF" || released != 3 || held != 0 || risk != 0 || services != 0 || commits != 0 {
					t.Fatal(out, released, held, risk, services, commits)
				}
			case "SENT_UNKNOWN":
				if out.State != "CUTOFF" || released != 2 || held != 2 || risk != 1 || services != 0 || commits != 0 {
					t.Fatal(out, released, held, risk, services, commits)
				}
			case "REPORT_COMMITTED":
				if out.State != "COMMITTED" || services != 1 || commits != 1 || risk != 0 {
					t.Fatal(out, released, held, risk, services, commits)
				}
			}
		})
	}
}

func TestRehearsalCutoffTaskLockFailedRetry(t *testing.T) {
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContext(t, r)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	var req generated.RehearsalRequest
	if e := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req); e != nil {
		t.Fatal(e)
	}
	root, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "cutoff-lock", req)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, string(root.TaskID))
	held, e := r.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer held.Rollback()
	var id string
	if e = held.QueryRowContext(ctx, "SELECT task_id FROM tasks WHERE task_id=? FOR UPDATE", root.TaskID).Scan(&id); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	done, failed, e := r.Store.RunHarnessCutoffs(ctx)
	elapsed := time.Since(start)
	if e != nil || done != 0 || failed != 1 || elapsed > 5500*time.Millisecond {
		t.Fatal("bounded original root lock", done, failed, elapsed, e)
	}
	if e = held.Rollback(); e != nil {
		t.Fatal(e)
	}
	var failures int
	var finished bool
	if e = r.Store.DB.QueryRow("SELECT failures,done FROM harness_cutoff_work WHERE task_id=?", root.TaskID).Scan(&failures, &finished); e != nil || failures != 1 || finished {
		t.Fatal("failed root permanently skipped", failures, finished, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_cutoff_work SET next_run_at=UTC_TIMESTAMP(6) WHERE task_id=?", root.TaskID); e != nil {
		t.Fatal(e)
	}
	done, failed, e = r.Store.RunHarnessCutoffs(ctx)
	if e != nil || done != 1 || failed != 0 {
		t.Fatal("same original root retry", done, failed, e)
	}
	t.Log("actual task lock wait plus failed-item persistence", elapsed)
}
