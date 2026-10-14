package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"

	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
)

func TestRehearsalChildStartBoundedRootLease(t *testing.T) {
	t.Run("first_live_child_window", func(t *testing.T) {
		base, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		r, draft, _ := candidateHTTPFixture(t)
		ctx := runtimeContextWithParent(t, r, base)
		scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
		require := func(ok bool, stage string) {
			t.Helper()
			if !ok {
				t.Fatalf("C_RED_PRECONDITION_FAILED stage=%s", stage)
			}
		}
		var request generated.RehearsalRequest
		err := generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &request)
		require(err == nil && len(request.Inputs) == 1, "one_original_input")
		accepted, err := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "c-first-live-child", request)
		require(err == nil, "actual_intake")
		const worker = "c-first-live-child-worker"
		root, err := r.Store.ClaimRegistered(ctx, worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: string(accepted.TaskID)}})
		require(err == nil, "actual_sixty_second_claim")
		ctx, err = r.Store.EnterDraftTaskIssuer(ctx, root.TaskLease, worker)
		require(err == nil, "actual_durable_issuer")
		_, err = r.Store.InitializeDraftRehearsal(ctx, root.TaskLease, worker)
		require(err == nil, "actual_initialize")
		var acceptedAt, rootEnd, rootSettle time.Time
		var calls, steps int
		err = r.Store.DB.QueryRowContext(ctx, "SELECT accepted_at,execute_by,settle_by,max_calls,max_steps FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, root.TaskID).Scan(&acceptedAt, &rootEnd, &rootSettle, &calls, &steps)
		require(err == nil && rootEnd.Sub(acceptedAt) == 90*time.Second && rootSettle.Sub(rootEnd) == time.Minute && calls == 2 && steps == 22, "original_one_input_budget")

		// Wait naturally before the first child exists; never edit a lease or deadline.
		var beforeExpiry, beforeClock time.Time
		window := false
		for poll := 0; poll < 100; poll++ {
			var now, expiry time.Time
			var exact bool
			err = r.Store.DB.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6),lease_expires_at,fence=? AND lease_owner=? AND owner_subject_id=? AND task_kind='ADMIN' AND scope_kind='PRODUCT' AND world_instance_id IS NULL AND state='RUNNING' AND cancelled=0 FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", root.Fence, worker, scope.SubjectID, scope.TenantID, scope.ProductID, root.TaskID).Scan(&now, &expiry, &exact)
			require(err == nil && exact && ctx.Err() == nil, "current_root_before_start")
			if beforeExpiry.IsZero() {
				beforeExpiry = expiry
			}
			require(expiry.Equal(beforeExpiry), "no_fixture_lease_change")
			remaining := expiry.Sub(now)
			require(remaining >= 55*time.Second, "first_start_window_not_missed")
			if remaining <= 57*time.Second {
				beforeClock, window = now, true
				break
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				t.Fatal("C_RED_PRECONDITION_FAILED stage=natural_wait_context")
			case <-timer.C:
			}
		}
		require(window, "natural_wait_poll_limit")
		child, err := r.Store.StartRehearsalChild(ctx, root.TaskLease, worker)
		require(err == nil, "first_owning_start")

		var startedAt, childEnd, childAccepted, childEnvelopeEnd, childSettle time.Time
		err = r.Store.DB.QueryRowContext(ctx, "SELECT c.started_at,c.execute_by,e.accepted_at,e.execute_by,e.settle_by FROM harness_rehearsal_children c JOIN harness_envelopes e ON e.tenant_id=c.tenant_id AND e.product_id=c.product_id AND e.task_id=c.child_task_id WHERE c.tenant_id=? AND c.product_id=? AND c.root_task_id=? AND c.child_task_id=? AND c.input_index=0", scope.TenantID, scope.ProductID, root.TaskID, child.TurnID).Scan(&startedAt, &childEnd, &childAccepted, &childEnvelopeEnd, &childSettle)
		require(err == nil && startedAt.Equal(childAccepted) && childEnd.Equal(childEnvelopeEnd) && childSettle.Equal(rootSettle), "original_child_rows_agree")
		expectedEnd := startedAt.Add(time.Minute)
		if rootEnd.Before(expectedEnd) {
			expectedEnd = rootEnd
		}
		require(childEnd.Equal(expectedEnd) && childEnd.After(startedAt), "unchanged_original_child_deadline")
		var afterExpiry, afterRootEnd, afterRootSettle time.Time
		var exact bool
		err = r.Store.DB.QueryRowContext(ctx, "SELECT t.lease_expires_at,e.execute_by,e.settle_by,t.fence=? AND t.lease_owner=? AND t.owner_subject_id=? AND t.cancelled=0 AND t.state='RUNNING' FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?", root.Fence, worker, scope.SubjectID, scope.TenantID, scope.ProductID, root.TaskID).Scan(&afterExpiry, &afterRootEnd, &afterRootSettle, &exact)
		require(err == nil && exact && afterRootEnd.Equal(rootEnd) && afterRootSettle.Equal(rootSettle) && ctx.Err() == nil, "same_root_identity_and_original_deadlines")
		t.Logf("C_RED measured original_budget=90s calls=2 steps=22 pre_start_db_utc=%s pre_start_remaining_ms=%d original_root_expiry=%s after_root_expiry=%s child_started_at=%s child_original_end=%s root_original_end=%s root_original_settle=%s same_fence=%d", beforeClock.UTC().Format(time.RFC3339Nano), beforeExpiry.Sub(beforeClock).Milliseconds(), beforeExpiry.UTC().Format(time.RFC3339Nano), afterExpiry.UTC().Format(time.RFC3339Nano), startedAt.UTC().Format(time.RFC3339Nano), childEnd.UTC().Format(time.RFC3339Nano), rootEnd.UTC().Format(time.RFC3339Nano), rootSettle.UTC().Format(time.RFC3339Nano), root.Fence)
		// A child's original execution window must not outlive its owning root lease.
		if afterExpiry.Before(childEnd) {
			t.Fatalf("C_RED_EXPECTED_COVERAGE_FAILURE: root lease ends before first child's original execute deadline; uncovered_ms=%d", childEnd.Sub(afterExpiry).Milliseconds())
		}
		claimed, err := r.Store.ClaimRehearsalChild(ctx, root.TaskLease, worker)
		require(err == nil, "actual_child_claim")
		var claimedEnd time.Time
		err = r.Store.DB.QueryRowContext(ctx, "SELECT lease_expires_at FROM tasks WHERE task_id=?", claimed.TaskID).Scan(&claimedEnd)
		require(err == nil && !claimedEnd.After(childEnd) && !claimedEnd.After(afterExpiry), "claim_remains_capped")
	})
	for _, mode := range []string{"old_greater", "old_equal", "cas_zero", "sql_error", "finish_rollback", "finish_binding", "finish_action", "wrong_worker", "wrong_fence", "wrong_scope", "expired", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			n := 1
			if mode == "old_equal" {
				n = 2
			}
			f := cLeaseFixture(t, n)
			if mode == "old_greater" {
				f.waitRemaining(t, "execute_by", 50*time.Second, 55*time.Second, 180)
			}
			if mode == "old_equal" {
				f.waitRemaining(t, "execute_by", 70*time.Second, 75*time.Second, 180)
			}
			f.claim(t)
			var probe *cLeaseSQLProbe
			if mode == "old_greater" || mode == "old_equal" || mode == "cas_zero" || mode == "sql_error" {
				probe = f.installLeaseSQLProbe(t)
			}
			if mode == "old_equal" {
				f.waitRemaining(t, "execute_by", 50*time.Second, 55*time.Second, 100)
				probe.arm(t, f, "OBSERVE")
				f.start(t)
				probe.finish(t, 1, nil)
				f.execute(t)
			}
			before := f.snapshot(t)
			if mode == "old_greater" || mode == "old_equal" {
				var end time.Time
				cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT execute_by FROM harness_envelopes WHERE task_id=?", f.root.TaskID).Scan(&end))
				if (mode == "old_greater" && !before.Expiry.After(end)) || (mode == "old_equal" && !before.Expiry.Equal(end)) {
					t.Fatal("natural no-shortening precondition", mode, before.Expiry, end)
				}
				probe.arm(t, f, "OBSERVE")
				f.start(t)
				probe.finish(t, 0, nil)
				after := f.snapshot(t)
				if !after.Expiry.Equal(before.Expiry) || after.Children != before.Children+1 {
					t.Fatal("no-update branch changed lease or lost child", before, after)
				}
				return
			}
			var faultBefore []string
			if mode == "cas_zero" || mode == "sql_error" {
				faultBefore = f.exactRows(t)
				faultMode := "CAS_ZERO"
				if mode == "sql_error" {
					faultMode = "SQL_SYNTAX"
				}
				probe.arm(t, f, faultMode)
			}
			injected := errors.New("c injected finish rollback")
			hit := false
			if mode == "finish_rollback" || mode == "finish_binding" || mode == "finish_action" {
				f.r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					if !f.receiptTx(c, tx) {
						return nil
					}
					hit = true
					var expiry time.Time
					if e := tx.QueryRowContext(c, "SELECT lease_expires_at FROM tasks WHERE task_id=?", f.root.TaskID).Scan(&expiry); e != nil {
						return e
					}
					if !expiry.After(before.Expiry) {
						return errors.New("c CAS was not performed before finish")
					}
					switch mode {
					case "finish_binding":
						_, e := tx.ExecContext(c, "UPDATE harness_admin_rehearsals SET next_input=next_input+1 WHERE task_id=?", f.root.TaskID)
						return e
					case "finish_action":
						_, e := tx.ExecContext(c, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
						return e
					default:
						return injected
					}
				}
			}
			lease, worker := f.root, f.worker
			switch mode {
			case "wrong_worker":
				worker += "-wrong"
			case "wrong_fence":
				lease.Fence++
			case "wrong_scope":
				lease.ProductID = "wrong-product"
			case "expired":
				f.waitRemaining(t, "lease_expires_at", -2*time.Second, -time.Millisecond, 180)
			case "cancelled":
				runtimeHTTP(t, f.r, "POST", "/tasks/"+f.root.TaskID+"/cancel", "c-cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "bounded lease cancellation"}, 202, nil)
				before = f.snapshot(t)
			}
			_, e := f.r.Store.StartRehearsalChild(f.ctx, lease, worker)
			if probe != nil {
				probe.finish(t, 1, e)
				if !reflect.DeepEqual(faultBefore, f.exactRows(t)) {
					t.Fatal("real SQL fault partially persisted exact rows")
				}
			}
			f.r.Store.BeforeCommit = nil
			if e == nil {
				t.Fatal("invalid child start accepted", mode)
			}
			if mode == "cas_zero" && !errors.Is(e, store.ErrReplan) {
				t.Fatal("actual zero-row CAS did not fail with replan", e)
			}
			if mode == "finish_rollback" && (!hit || !errors.Is(e, injected)) {
				t.Fatal("missing exact finish rollback", hit, e)
			}
			if (mode == "finish_binding" || mode == "finish_action") && !hit {
				t.Fatal("finish negative was not reached", e)
			}
			if after := f.snapshot(t); !reflect.DeepEqual(before, after) {
				t.Fatal("rejected start partially persisted", mode, before, after, e)
			}
			t.Logf("mode=%s error=%v exact_state_unchanged=true finish_hit=%t", mode, e, hit)
		})
	}
}

// These helpers enter the real intake and authenticated owning paths. Natural
// waits observe DB UTC; no test helper changes a lease or execution deadline.
type cLeaseCase struct {
	r          *app.Runtime
	ctx        context.Context
	root       domain.TaskLease
	worker     string
	child, key string
}
type cLeaseState struct {
	Expiry                                        time.Time
	Children, Quotes, Receipts, Calls, Next       int
	Tasks, Registered, Envelopes, Turns, Attempts int
	Journal                                       [32]byte
}

func cMust(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func cLeaseFixture(t *testing.T, inputs int) *cLeaseCase {
	t.Helper()
	base, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	if inputs == 2 {
		cancel()
		base, cancel = context.WithTimeout(context.Background(), 240*time.Second)
	}
	t.Cleanup(cancel)
	r, draft, _ := candidateHTTPFixture(t)
	ctx := runtimeContextWithParent(t, r, base)
	var req generated.RehearsalRequest
	cMust(t, generated.Decode("RehearsalRequest", []byte(`{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[{"actor_slot":"mia","text":""}],"max_cost_units":"0","assertions":["ISOLATED"]}`), &req))
	if inputs == 2 {
		req.Inputs = append(req.Inputs, req.Inputs[0])
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	a, e := r.Store.AcceptDraftRehearsal(ctx, scope, draft, "c-lease-intake", req)
	cMust(t, e)
	id := string(a.TaskID)
	child := content.Identity(id, "rehearsal-child", "0")
	return &cLeaseCase{r: r, ctx: ctx, root: domain.TaskLease{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: id}, worker: "c-lease-worker", child: child, key: content.Identity(child, "accepted")}
}
func (f *cLeaseCase) claim(t *testing.T) {
	t.Helper()
	r, e := f.r.Store.ClaimRegistered(f.ctx, f.worker, time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "REHEARSAL", TaskID: f.root.TaskID}})
	cMust(t, e)
	f.root = r.TaskLease
	f.ctx, e = f.r.Store.EnterDraftTaskIssuer(f.ctx, f.root, f.worker)
	cMust(t, e)
	_, e = f.r.Store.InitializeDraftRehearsal(f.ctx, f.root, f.worker)
	cMust(t, e)
}
func (f *cLeaseCase) waitRemaining(t *testing.T, field string, low, high time.Duration, maxSeconds int) {
	t.Helper()
	query := "SELECT UTC_TIMESTAMP(6),execute_by FROM harness_envelopes WHERE task_id=?"
	if field == "lease_expires_at" {
		query = "SELECT UTC_TIMESTAMP(6),lease_expires_at FROM tasks WHERE task_id=?"
	}
	for i := 0; i < maxSeconds*10; i++ {
		var now, end time.Time
		cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, query, f.root.TaskID).Scan(&now, &end))
		d := end.Sub(now)
		if d < low {
			t.Fatalf("natural window missed field=%s remaining=%v", field, d)
		}
		if d <= high {
			t.Logf("natural window field=%s remaining_ms=%d", field, d.Milliseconds())
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-f.ctx.Done():
			timer.Stop()
			t.Fatal(f.ctx.Err())
		case <-timer.C:
		}
	}
	t.Fatal("natural window poll limit")
}
func (f *cLeaseCase) snapshot(t *testing.T) cLeaseState {
	t.Helper()
	var s cLeaseState
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT lease_expires_at FROM tasks WHERE task_id=?", f.root.TaskID).Scan(&s.Expiry))
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM harness_rehearsal_children WHERE root_task_id=?", f.root.TaskID).Scan(&s.Children))
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM harness_quotes WHERE quote_id=?", content.Identity(f.child, "quote")).Scan(&s.Quotes))
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND operation='REHEARSAL_CHILD_START'", f.root.TaskID).Scan(&s.Receipts))
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT calls_allocated FROM harness_envelopes WHERE task_id=?", f.root.TaskID).Scan(&s.Calls))
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT next_input FROM harness_admin_rehearsals WHERE task_id=?", f.root.TaskID).Scan(&s.Next))
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT (SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM registered_jobs),(SELECT COUNT(*) FROM harness_envelopes),(SELECT COUNT(*) FROM harness_turns),(SELECT COUNT(*) FROM attempts)").Scan(&s.Tasks, &s.Registered, &s.Envelopes, &s.Turns, &s.Attempts))
	raw, e := os.ReadFile(filepath.Join(filepath.Dir(f.r.Local.Coordinator.Directory()), "mock", "journal.jsonl"))
	cMust(t, e)
	s.Journal = sha256.Sum256(raw)
	return s
}
func (f *cLeaseCase) start(t *testing.T) generated.TurnAccepted {
	t.Helper()
	a, e := f.r.Store.StartRehearsalChild(f.ctx, f.root, f.worker)
	cMust(t, e)
	return a
}
func (f *cLeaseCase) receiptTx(ctx context.Context, tx *sql.Tx) bool {
	var ok bool
	e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE commit_key=? AND task_id=? AND operation='REHEARSAL_CHILD_START' AND phase='COMMIT')", f.key, f.root.TaskID).Scan(&ok)
	return e == nil && ok
}
func (f *cLeaseCase) claimed(t *testing.T) domain.TaskLease {
	t.Helper()
	cl, e := f.r.Store.ClaimRehearsalChild(f.ctx, f.root, f.worker)
	cMust(t, e)
	return cl.TaskLease
}
func (f *cLeaseCase) execute(t *testing.T) domain.TaskLease {
	t.Helper()
	cl := f.claimed(t)
	_, e := f.r.Store.BindRehearsalChildContext(f.ctx, f.root, cl, f.worker)
	cMust(t, e)
	c, release, e := f.r.Store.EnterRehearsalChildExecution(f.ctx, f.root, cl, f.worker)
	cMust(t, e)
	e = f.r.Worker.Turns.Execute(c, cl, f.worker)
	release()
	cMust(t, e)
	return cl
}

func TestRehearsalChildStartOriginalReceiptRecovery(t *testing.T) {
	for _, mode := range []string{"COMMIT_ACK", "ROLLBACK_ACK", "CALLER_UNRESOLVED", "REVOKE", "BINDING", "CANCEL", "WRONG_SUBJECT", "WRONG_SCOPE", "NO_PRINCIPAL", "NEXT_CHILD"} {
		t.Run(mode, func(t *testing.T) {
			n := 1
			if mode == "NEXT_CHILD" {
				n = 2
			}
			f := cLeaseFixture(t, n)
			f.claim(t)
			before := f.snapshot(t)
			hits := 0
			if mode == "COMMIT_ACK" || mode == "ROLLBACK_ACK" || mode == "CALLER_UNRESOLVED" {
				f.r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
					if !f.receiptTx(c, tx) {
						return tx.Commit()
					}
					hits++
					var e error
					if mode == "ROLLBACK_ACK" {
						e = tx.Rollback()
					} else {
						e = tx.Commit()
					}
					if e != nil {
						return e
					}
					return errors.New("c original ACK unavailable")
				}
				_, e := f.r.Store.StartRehearsalChild(f.ctx, f.root, f.worker)
				f.r.Store.Commit = nil
				if !errors.Is(e, store.ErrCommitUnknown) || hits != 1 {
					t.Fatal("original unknown must stop once", hits, e)
				}
				if mode == "CALLER_UNRESOLVED" {
					t.Log("original caller stops UNKNOWN without query, claim, or resend")
					return
				}
				if mode == "ROLLBACK_ACK" {
					if after := f.snapshot(t); !reflect.DeepEqual(before, after) {
						t.Fatal("actual rollback partially persisted", before, after)
					}
					return
				}
			} else {
				f.start(t)
			}
			// Observe only the frozen child/key; current next_input is never a recovery selector.
			var bound, payload []byte
			var hash []byte
			var index int
			cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT binding FROM harness_admin_rehearsals WHERE task_id=?", f.root.TaskID).Scan(&bound))
			var binding domain.RehearsalBinding
			cMust(t, json.Unmarshal(bound, &binding))
			payload, e := json.Marshal(struct {
				Root    string
				Index   int
				Binding domain.RehearsalBinding
			}{f.root.TaskID, 0, binding})
			cMust(t, e)
			canonical, e := wire.Canonical(payload)
			cMust(t, e)
			expectedHash := sha256.Sum256(canonical)
			var exact bool
			cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT c.input_index,r.payload_hash,r.tenant_id=? AND r.product_id=? AND r.owner_subject_id=? AND r.scope_kind='PRODUCT' AND r.world_instance_id IS NULL AND r.task_id=? AND r.operation='REHEARSAL_CHILD_START' AND r.phase='COMMIT' AND r.command_key=? AND c.accepted_receipt=r.commit_key AND c.started_at=e.accepted_at AND c.execute_by=e.execute_by FROM commit_receipts r JOIN harness_rehearsal_children c ON c.accepted_receipt=r.commit_key JOIN harness_envelopes e ON e.task_id=c.child_task_id WHERE r.commit_key=? AND c.child_task_id=? AND c.root_task_id=?", f.root.TenantID, f.root.ProductID, f.root.SubjectID, f.root.TaskID, f.key, f.key, f.child, f.root.TaskID).Scan(&index, &hash, &exact))
			if !exact || index != 0 || !bytes.Equal(hash, expectedHash[:]) {
				t.Fatal("original tuple receipt mismatch")
			}
			after := f.snapshot(t)
			if mode == "COMMIT_ACK" {
				if after.Children != 1 || after.Receipts != 1 || !after.Expiry.After(before.Expiry) {
					t.Fatal("missing atomic original commit", before, after)
				}
				if a := f.start(t); a.TurnID != f.child {
					t.Fatal("replay changed child")
				}
				if replay := f.snapshot(t); !reflect.DeepEqual(after, replay) {
					t.Fatal("receipt replay wrote", after, replay)
				}
				cl := f.boundChild(t)
				inspection := f.boundSnapshot(t, cl)
				_, release, e := f.r.Store.EnterRehearsalChildExecution(f.ctx, f.root, cl, f.worker)
				cMust(t, e)
				release()
				if again := f.boundSnapshot(t, cl); !reflect.DeepEqual(inspection, again) {
					t.Fatal("inspection persisted writes")
				}
				return
			}
			if mode == "NEXT_CHILD" {
				old := f.execute(t)
				completed := f.snapshot(t)
				if completed.Next != 1 || completed.Children != 1 {
					t.Fatal("real first child did not advance", completed)
				}
				_, release, e := f.r.Store.EnterRehearsalChildExecution(f.ctx, f.root, old, f.worker)
				if e == nil {
					release()
					t.Fatal("old child executed after next_input advanced")
				}
				if observed := f.snapshot(t); !reflect.DeepEqual(completed, observed) {
					t.Fatal("old tuple inspection wrote")
				}
				next := f.start(t)
				if next.TurnID == f.child || next.TurnID != content.Identity(f.root.TaskID, "rehearsal-child", "1") {
					t.Fatal("normal scheduling selected wrong next child")
				}
				if fresh := f.snapshot(t); fresh.Children != 2 || fresh.Receipts != 2 || fresh.Calls != completed.Calls {
					t.Fatal("new stage/observation boundary", fresh)
				}
				return
			}
			cl := f.boundChild(t)
			replayed := f.claimed(t)
			if replayed.TaskID != cl.TaskID || replayed.Fence != cl.Fence {
				t.Fatal("positive claim replay changed original child")
			}
			liveUntil := f.requireLive(t, cl)
			t.Log("bound positive execution and original claim replay succeeded before one negative factor")
			ctx, root := f.ctx, f.root
			switch mode {
			case "REVOKE":
				_, e = f.r.Store.DB.ExecContext(f.ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.rehearse'")
				cMust(t, e)
			case "BINDING":
				_, e = f.r.Store.DB.ExecContext(f.ctx, "UPDATE harness_rehearsal_children SET input_hash=UNHEX(REPEAT('ab',32)) WHERE root_task_id=?", root.TaskID)
				cMust(t, e)
			case "CANCEL":
				runtimeHTTP(t, f.r, "POST", "/tasks/"+root.TaskID+"/cancel", "c-replay-cancel", generated.AdminCancelRequest{ProductID: "coffee", Reason: "cancel original rehearsal"}, 202, nil)
			case "WRONG_SUBJECT":
				root.SubjectID = "wrong-subject"
			case "WRONG_SCOPE":
				root.ProductID = "wrong-product"
			case "NO_PRINCIPAL":
				deadline, ok := f.ctx.Deadline()
				if !ok {
					t.Fatal("missing original context deadline")
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(context.Background(), deadline)
				defer cancel()
			}
			negative := f.boundSnapshot(t, cl)
			_, e = f.r.Store.ClaimRehearsalChild(ctx, root, f.worker)
			if e == nil {
				t.Fatal("receipt conferred current claim authority", mode)
			}
			_, release, e := f.r.Store.EnterRehearsalChildExecution(ctx, root, cl, f.worker)
			if e == nil {
				release()
				t.Fatal("receipt conferred current execution authority", mode)
			}
			cMust(t, ctx.Err())
			f.requireOriginalWindow(t, liveUntil)
			if last := f.boundSnapshot(t, cl); !reflect.DeepEqual(negative, last) {
				t.Fatal("denied replay wrote", mode)
			}
			t.Logf("mode=%s one_factor_rejection=true bound_snapshot_unchanged=true original_deadlines_live=true", mode)
		})
	}
}

func TestRehearsalChildStartCloseDrain(t *testing.T) {
	for _, mode := range []string{"CLOSE_FIRST", "START_INFLIGHT", "ROLLBACK_INFLIGHT", "ACK_INFLIGHT"} {
		t.Run(mode, func(t *testing.T) {
			f := cLeaseFixture(t, 1)
			f.claim(t)
			before := f.snapshot(t)
			const key = "c-start-close"
			blockedDrain := func() {
				t.Helper()
				c, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer stop()
				if e := f.r.Local.Coordinator.Drain(c, key); e == nil {
					t.Fatal("drain ignored actual held operation")
				}
			}
			if mode == "CLOSE_FIRST" {
				outer, release, e := f.r.Local.Participant.Enter(f.ctx, maintenance.HarnessWriter)
				cMust(t, e)
				cMust(t, f.r.Local.Coordinator.BeginClose(f.ctx, key))
				_, e = f.r.Store.StartRehearsalChild(outer, f.root, f.worker)
				if e == nil {
					release()
					t.Fatal("outer permit bypassed closing")
				}
				blockedDrain()
				release()
				if after := f.snapshot(t); !reflect.DeepEqual(before, after) {
					t.Fatal("close-first changed state")
				}
			} else {
				entered, release := make(chan struct{}), make(chan struct{})
				hit := false
				injected := errors.New("c inflight rollback")
				f.r.Store.BeforeCommit = func(c context.Context, tx *sql.Tx) error {
					if !f.receiptTx(c, tx) {
						return nil
					}
					if hit {
						return errors.New("c repeated inflight callback")
					}
					hit = true
					close(entered)
					timer := time.NewTimer(2 * time.Second)
					defer timer.Stop()
					select {
					case <-release:
					case <-c.Done():
						return c.Err()
					case <-timer.C:
						return errors.New("c close synchronization timeout")
					}
					if mode == "ROLLBACK_INFLIGHT" {
						return injected
					}
					return nil
				}
				if mode == "ACK_INFLIGHT" {
					f.r.Store.Commit = func(c context.Context, tx *sql.Tx) error {
						matched := f.receiptTx(c, tx)
						if e := tx.Commit(); e != nil {
							return e
						}
						if matched {
							return errors.New("c inflight ACK unavailable")
						}
						return nil
					}
				}
				done := make(chan error, 1)
				go func() { _, e := f.r.Store.StartRehearsalChild(f.ctx, f.root, f.worker); done <- e }()
				select {
				case <-entered:
				case e := <-done:
					t.Fatal("start ended before actual admission", e)
				case <-time.After(2 * time.Second):
					t.Fatal("start admission timeout")
				}
				cMust(t, f.r.Local.Coordinator.BeginClose(f.ctx, key))
				blockedDrain()
				close(release)
				var e error
				select {
				case e = <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("start failed to release")
				}
				f.r.Store.BeforeCommit = nil
				f.r.Store.Commit = nil
				after := f.snapshot(t)
				if mode == "ROLLBACK_INFLIGHT" {
					if !errors.Is(e, injected) || !reflect.DeepEqual(before, after) {
						t.Fatal("inflight rollback", e, before, after)
					}
				} else {
					if mode == "START_INFLIGHT" && e != nil {
						t.Fatal("admitted short transaction failed", e)
					}
					if mode == "ACK_INFLIGHT" && !errors.Is(e, store.ErrCommitUnknown) {
						t.Fatal("inflight ACK semantics", e)
					}
					if after.Children != 1 || after.Receipts != 1 || !after.Expiry.After(before.Expiry) {
						t.Fatal("inflight atomic commit missing", after)
					}
				}
			}
			if _, e := f.r.Store.StartRehearsalChild(f.ctx, f.root, f.worker); e == nil {
				t.Fatal("new Start while closing")
			}
			if _, e := f.r.Store.ClaimRehearsalChild(f.ctx, f.root, f.worker); e == nil {
				t.Fatal("new Claim while closing")
			}
			c, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			cMust(t, f.r.Local.Coordinator.Drain(c, key))
			t.Logf("mode=%s actual held handle blocked drain; release completed drain", mode)
		})
	}
}

const cLeaseCASQuery = "UPDATE tasks t SET lease_expires_at=? WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.task_kind='ADMIN' AND t.scope_kind='PRODUCT' AND t.world_instance_id IS NULL AND t.state='RUNNING' AND t.cancelled=0 AND t.fence=? AND t.lease_owner=? AND t.lease_expires_at=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND ?>UTC_TIMESTAMP(6) AND EXISTS(SELECT 1 FROM registered_jobs j JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id AND j.operation='REHEARSAL' AND h.state='ACTIVE' AND h.execute_by=? AND h.settle_by=? AND h.execute_by>UTC_TIMESTAMP(6) AND h.settle_by>UTC_TIMESTAMP(6))"

// Binding is a real transition. Read-only inspection starts only after it and a
// successful current Enter/release, never from a commonly unbound negative.
func (f *cLeaseCase) boundChild(t *testing.T) domain.TaskLease {
	t.Helper()
	cl := f.claimed(t)
	_, e := f.r.Store.BindRehearsalChildContext(f.ctx, f.root, cl, f.worker)
	cMust(t, e)
	_, release, e := f.r.Store.EnterRehearsalChildExecution(f.ctx, f.root, cl, f.worker)
	cMust(t, e)
	release()
	f.requireLive(t, cl)
	t.Log("real Claim/Bind/positive Enter/release completed")
	return cl
}

func (f *cLeaseCase) requireLive(t *testing.T, cl domain.TaskLease) time.Time {
	t.Helper()
	cMust(t, f.ctx.Err())
	var until time.Time
	for _, lease := range []domain.TaskLease{f.root, cl} {
		var live bool
		cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT t.fence=? AND t.lease_owner=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND h.execute_by>UTC_TIMESTAMP(6) AND h.settle_by>UTC_TIMESTAMP(6) FROM tasks t JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?", lease.Fence, f.worker, lease.TenantID, lease.ProductID, lease.TaskID).Scan(&live))
		if !live {
			t.Fatal("qualification observation crossed original lease/deadline")
		}
		var expiry, end, settle time.Time
		cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT t.lease_expires_at,h.execute_by,h.settle_by FROM tasks t JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&expiry, &end, &settle))
		for _, limit := range []time.Time{expiry, end, settle} {
			if until.IsZero() || limit.Before(until) {
				until = limit
			}
		}
	}
	return until
}
func (f *cLeaseCase) requireOriginalWindow(t *testing.T, until time.Time) {
	t.Helper()
	cMust(t, f.ctx.Err())
	var now time.Time
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now))
	if !now.Before(until) {
		t.Fatal("negative crossed original captured deadline")
	}
}

type cLeaseBoundSnapshot struct {
	Base cLeaseState
	Rows []string
}

func (f *cLeaseCase) boundSnapshot(t *testing.T, cl domain.TaskLease) cLeaseBoundSnapshot {
	t.Helper()
	if cl.TaskID != f.child {
		t.Fatal("inspection selected a different child")
	}
	return cLeaseBoundSnapshot{f.snapshot(t), f.exactRows(t)}
}

// Full row bytes include binding documents, current state, receipt payload/phase,
// deadlines and counters. These queries run outside owning transactions.
func (f *cLeaseCase) exactRows(t *testing.T) []string {
	t.Helper()
	var out []string
	queries := []string{
		"SELECT * FROM tasks WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id",
		"SELECT * FROM registered_jobs WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id",
		"SELECT * FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id",
		"SELECT * FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id",
		"SELECT * FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id",
		"SELECT * FROM harness_turn_steps WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id,step_number",
		"SELECT * FROM commit_receipts WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY commit_key,phase",
		"SELECT * FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id IN (?,?) ORDER BY task_id",
		"SELECT * FROM harness_rehearsal_children WHERE tenant_id=? AND product_id=? AND root_task_id IN (?,?) ORDER BY root_task_id,input_index",
	}
	for _, q := range queries {
		rows, e := f.r.Store.DB.QueryContext(f.ctx, q, f.root.TenantID, f.root.ProductID, f.root.TaskID, f.child)
		cMust(t, e)
		func() {
			defer func() { cMust(t, rows.Close()) }()
			cols, e := rows.Columns()
			cMust(t, e)
			out = append(out, q)
			for rows.Next() {
				values := make([]any, len(cols))
				dest := make([]any, len(cols))
				for i := range values {
					dest[i] = &values[i]
				}
				cMust(t, rows.Scan(dest...))
				b, e := json.Marshal(values)
				cMust(t, e)
				out = append(out, string(b))
			}
			cMust(t, rows.Err())
		}()
	}
	return out
}

// This instance-local adapter never registers a driver or changes DSN options.
type cLeaseDriverConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.NamedValueChecker
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}
type cLeaseConnector struct {
	inner driver.Connector
	probe *cLeaseSQLProbe
}
type cLeaseConn struct {
	cLeaseDriverConn
	probe *cLeaseSQLProbe
}
type cLeaseSQLRecord struct {
	Mode, Path, OriginalQueryHash, EffectiveQueryHash string
	Original, Effective                               []driver.NamedValue
	Affected                                          int64
	Err                                               error
}
type cLeaseSQLProbe struct {
	mu                                                                            sync.Mutex
	mode                                                                          string
	root                                                                          domain.TaskLease
	worker                                                                        string
	oldExpiry, rootEnd, settle                                                    time.Time
	selects, intents                                                              int
	records                                                                       []cLeaseSQLRecord
	gaps                                                                          []string
	opened, closeAttempted, closed, closeErrors, rejected, stmtOpened, stmtClosed int
}

func (p *cLeaseSQLProbe) gap(s string) { p.mu.Lock(); defer p.mu.Unlock(); p.gaps = append(p.gaps, s) }
func (p *cLeaseSQLProbe) disarm()      { p.mu.Lock(); defer p.mu.Unlock(); p.mode = "" }
func (p *cLeaseSQLProbe) closedConn(e error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeAttempted++
	if e == nil {
		p.closed++
	} else {
		p.closeErrors++
		p.gaps = append(p.gaps, "real Conn.Close error")
	}
}
func (c *cLeaseConnector) Driver() driver.Driver { return c.inner.Driver() }
func (c *cLeaseConnector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, e := c.inner.Connect(ctx)
	if e != nil {
		return raw, e
	}
	c.probe.mu.Lock()
	c.probe.opened++
	c.probe.mu.Unlock()
	inner, ok := raw.(cLeaseDriverConn)
	if !ok {
		c.probe.mu.Lock()
		c.probe.rejected++
		c.probe.mu.Unlock()
		e = raw.Close()
		c.probe.closedConn(e)
		return nil, errors.New("lease probe: current driver interfaces unavailable")
	}
	return &cLeaseConn{inner, c.probe}, nil
}
func (c *cLeaseConn) Close() error { e := c.cLeaseDriverConn.Close(); c.probe.closedConn(e); return e }
func (c *cLeaseConn) Prepare(q string) (driver.Stmt, error) {
	c.probe.checkExternalPrepare(q)
	return c.cLeaseDriverConn.Prepare(q)
}
func (c *cLeaseConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	c.probe.checkExternalPrepare(q)
	return c.cLeaseDriverConn.PrepareContext(ctx, q)
}
func (p *cLeaseSQLProbe) checkExternalPrepare(q string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode != "" && q == cLeaseCASQuery {
		p.gaps = append(p.gaps, "unexpected externally prepared CAS")
	}
}

const cLeaseExpiryQuery = "SELECT UTC_TIMESTAMP(6),h.execute_by,h.settle_by,h.narrative_generation,t.lease_expires_at FROM harness_envelopes h JOIN tasks t USING(tenant_id,product_id,task_id) WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=?"

func (c *cLeaseConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	p := c.probe
	p.mu.Lock()
	if p.mode != "" && q == cLeaseExpiryQuery && len(a) == 3 && a[0].Value == p.root.TenantID && a[1].Value == p.root.ProductID && a[2].Value == p.root.TaskID {
		p.selects++
	}
	p.mu.Unlock()
	return c.cLeaseDriverConn.QueryContext(ctx, q, a)
}
func (p *cLeaseSQLProbe) shape(a []driver.NamedValue) bool {
	if len(a) != 11 {
		return false
	}
	for i, v := range a {
		if v.Name != "" || v.Ordinal != i+1 {
			return false
		}
	}
	end, ok := a[0].Value.(time.Time)
	if !ok {
		return false
	}
	old, ok := a[7].Value.(time.Time)
	if !ok {
		return false
	}
	target, ok := a[8].Value.(time.Time)
	if !ok {
		return false
	}
	rootEnd, ok := a[9].Value.(time.Time)
	if !ok {
		return false
	}
	settle, ok := a[10].Value.(time.Time)
	if !ok {
		return false
	}
	return a[1].Value == p.root.TenantID && a[2].Value == p.root.ProductID && a[3].Value == p.root.TaskID && a[4].Value == p.root.OwnerSubjectID && a[5].Value == p.root.Fence && a[6].Value == p.worker && old.Equal(p.oldExpiry) && end.After(old) && target.Equal(end) && rootEnd.Equal(p.rootEnd) && settle.Equal(p.settle)
}
func (c *cLeaseConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	p := c.probe
	p.mu.Lock()
	mode := p.mode
	if mode == "" {
		p.mu.Unlock()
		return c.cLeaseDriverConn.ExecContext(ctx, q, a)
	}
	if q != cLeaseCASQuery {
		if strings.HasPrefix(q, "UPDATE tasks") {
			for _, v := range a {
				if id, ok := v.Value.(string); ok && id == p.root.TaskID {
					p.gaps = append(p.gaps, "unexpected root UPDATE")
				}
			}
		}
		p.mu.Unlock()
		return c.cLeaseDriverConn.ExecContext(ctx, q, a)
	}
	p.intents++
	if p.intents != 1 || !p.shape(a) {
		p.gaps = append(p.gaps, "duplicate CAS or unexpected original argument shape")
		p.mu.Unlock()
		return c.cLeaseDriverConn.ExecContext(ctx, q, a)
	}
	original := append([]driver.NamedValue(nil), a...)
	effective := append([]driver.NamedValue(nil), a...)
	effectiveQuery := q
	// Fixture-only fault: change the SET value, never any WHERE authority argument.
	// This proves propagation of an actual server result, not a production race.
	if mode == "CAS_ZERO" {
		effective[0].Value = original[7].Value
	}
	if mode == "SQL_SYNTAX" {
		effectiveQuery = "UPDTE" + strings.TrimPrefix(q, "UPDATE")
	}
	p.mu.Unlock()
	result, e := c.cLeaseDriverConn.ExecContext(ctx, effectiveQuery, effective)
	path := "direct"
	if e == driver.ErrSkip {
		path = "same_conn_prepared"
		result, e = c.execPrepared(ctx, effectiveQuery, effective)
	}
	record := cLeaseSQLRecord{Mode: mode, Path: path, OriginalQueryHash: fmt.Sprintf("%x", sha256.Sum256([]byte(q))), EffectiveQueryHash: fmt.Sprintf("%x", sha256.Sum256([]byte(effectiveQuery))), Original: original, Effective: effective, Affected: -1, Err: e}
	if e == nil {
		if result == nil {
			p.gap("nil actual driver Result")
		} else {
			var re error
			record.Affected, re = result.RowsAffected()
			if re != nil {
				p.gap("actual RowsAffected error")
			}
		}
	}
	p.mu.Lock()
	p.records = append(p.records, record)
	p.mu.Unlock()
	return result, e
}
func (c *cLeaseConn) execPrepared(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	// database/sql's normal ErrSkip fallback would reuse original caller args.
	// Keep the precise fixture arguments on this same real connection and Tx.
	s, e := c.cLeaseDriverConn.PrepareContext(ctx, q)
	if e != nil {
		return nil, e
	}
	c.probe.mu.Lock()
	c.probe.stmtOpened++
	c.probe.mu.Unlock()
	defer func() {
		e := s.Close()
		c.probe.mu.Lock()
		defer c.probe.mu.Unlock()
		if e == nil {
			c.probe.stmtClosed++
		} else {
			c.probe.gaps = append(c.probe.gaps, "real Stmt.Close error")
		}
	}()
	exec, ok := s.(driver.StmtExecContext)
	check, ok2 := s.(driver.NamedValueChecker)
	if !ok || !ok2 || s.NumInput() != 11 {
		c.probe.gap("unexpected prepared statement interfaces/shape")
		return nil, errors.New("lease probe prepared shape unavailable")
	}
	converted := append([]driver.NamedValue(nil), a...)
	for i := range converted {
		if e := check.CheckNamedValue(&converted[i]); e != nil {
			c.probe.gap("unexpected prepared argument conversion")
			return nil, e
		}
	}
	if !reflect.DeepEqual(converted, a) {
		c.probe.gap("prepared arguments changed")
		return nil, errors.New("lease probe prepared argument mismatch")
	}
	return exec.ExecContext(ctx, converted)
}
func (f *cLeaseCase) installLeaseSQLProbe(t *testing.T) *cLeaseSQLProbe {
	t.Helper()
	originalDB := f.r.Store.DB
	var name string
	cMust(t, originalDB.QueryRowContext(f.ctx, "SELECT DATABASE()").Scan(&name))
	if !regexp.MustCompile(`^loreviva_test_[a-f0-9]{24}$`).MatchString(name) {
		t.Fatal("unsafe current fixture database")
	}
	cfg, e := mysqlDriver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if e != nil {
		t.Fatal("invalid private test connection")
	}
	if cfg.Net != "tcp" || cfg.Addr != "127.0.0.1:33267" || strings.EqualFold(cfg.User, "root") {
		t.Fatal("test database must be approved loopback endpoint")
	}
	cfg.DBName = name
	cfg.ParseTime = true
	inner, e := mysqlDriver.NewConnector(cfg)
	if e != nil {
		t.Fatal("invalid scoped probe connector")
	}
	p := &cLeaseSQLProbe{}
	swapped := false
	observedDB := sql.OpenDB(&cLeaseConnector{inner, p})
	// Register cleanup before validation can fail: OpenDB already owns resources.
	t.Cleanup(func() {
		p.disarm()
		if swapped {
			f.r.Store.DB = originalDB
		}
		closeErr := observedDB.Close()
		p.mu.Lock()
		opened, attempted, closed, failed, rejected, so, sc := p.opened, p.closeAttempted, p.closed, p.closeErrors, p.rejected, p.stmtOpened, p.stmtClosed
		gaps := append([]string(nil), p.gaps...)
		p.mu.Unlock()
		t.Logf("probe_cleanup swapped=%t opened=%d close_attempted=%d closed_success=%d close_errors=%d early_rejected=%d stmt_opened=%d stmt_closed=%d in_use=%d", swapped, opened, attempted, closed, failed, rejected, so, sc, observedDB.Stats().InUse)
		if closeErr != nil {
			t.Error("observed pool Close failed", closeErr)
		}
		if opened != attempted || opened != closed || failed != 0 || so != sc || observedDB.Stats().InUse != 0 || len(gaps) != 0 {
			t.Error("probe resource/protocol failure", gaps)
		}
	})
	var current string
	cMust(t, observedDB.QueryRowContext(f.ctx, "SELECT DATABASE()").Scan(&current))
	if current != name {
		t.Fatal("probe connection did not select original fixture")
	}
	if originalDB.Stats().InUse != 0 {
		t.Fatal("probe swap encountered active original connection")
	}
	f.r.Store.DB = observedDB
	swapped = true
	return p
}
func (p *cLeaseSQLProbe) arm(t *testing.T, f *cLeaseCase, mode string) {
	t.Helper()
	var old, end, settle time.Time
	cMust(t, f.r.Store.DB.QueryRowContext(f.ctx, "SELECT t.lease_expires_at,h.execute_by,h.settle_by FROM tasks t JOIN harness_envelopes h USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?", f.root.TenantID, f.root.ProductID, f.root.TaskID).Scan(&old, &end, &settle))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode != "" || (mode != "OBSERVE" && mode != "CAS_ZERO" && mode != "SQL_SYNTAX") {
		t.Fatal("invalid probe arm")
	}
	p.root = f.root
	p.worker = f.worker
	p.oldExpiry = old
	p.rootEnd = end
	p.settle = settle
	p.mode = mode
	p.selects = 0
	p.intents = 0
	p.records = nil
}
func (p *cLeaseSQLProbe) finish(t *testing.T, want int, owningErr error) {
	t.Helper()
	p.mu.Lock()
	mode, selects, intents := p.mode, p.selects, p.intents
	records := append([]cLeaseSQLRecord(nil), p.records...)
	gaps := append([]string(nil), p.gaps...)
	p.mode = ""
	p.mu.Unlock()
	if mode == "" || selects != 1 || intents != want || len(records) != want || len(gaps) != 0 {
		t.Fatalf("probe path mismatch mode=%s selects=%d intents=%d results=%d gaps=%v", mode, selects, intents, len(records), gaps)
	}
	for _, r := range records {
		original, e := json.Marshal(r.Original)
		cMust(t, e)
		effective, e := json.Marshal(r.Effective)
		cMust(t, e)
		errno := uint16(0)
		var server *mysqlDriver.MySQLError
		if errors.As(r.Err, &server) {
			errno = server.Number
		}
		t.Logf("probe mode=%s path=%s original_query_sha=%s effective_query_sha=%s original_args=%s effective_args=%s actual_rows=%d server_errno=%d", mode, r.Path, r.OriginalQueryHash, r.EffectiveQueryHash, original, effective, r.Affected, errno)
		if mode == "OBSERVE" && (r.Err != nil || r.Affected != 1 || !reflect.DeepEqual(r.Original, r.Effective) || r.OriginalQueryHash != r.EffectiveQueryHash || owningErr != nil) {
			t.Fatal("unmodified CAS observation failed")
		}
		if mode == "CAS_ZERO" && (r.Err != nil || r.Affected != 0 || !errors.Is(owningErr, store.ErrReplan)) {
			t.Fatal("real zero-row CAS propagation failed", owningErr)
		}
		if mode == "SQL_SYNTAX" && (errno != 1064 || owningErr != r.Err) {
			t.Fatal("real server syntax-error identity missing", owningErr)
		}
	}
	t.Logf("probe arm complete mode=%s real_select=%d logical_CAS=%d", mode, selects, intents)
}
