package integration

import (
	"context"
	"database/sql"
	"errors"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sync/atomic"
	"testing"
)

// This fixture schedules a real independent permission withdrawal after the
// owning SQL rollback and before its next guarded Store operation. The actual
// Local guard is always delegated to; no production admission is bypassed.
type withdrawAtOutcomeGuard struct {
	inner store.OperationGuard
	db    *sql.DB
	armed atomic.Bool
	done  atomic.Bool
}

func (g *withdrawAtOutcomeGuard) EnterStore(ctx context.Context) (context.Context, func(), error) {
	if g.armed.CompareAndSwap(true, false) {
		if _, e := g.db.ExecContext(ctx, "UPDATE memory_subject_permissions SET enabled=0 WHERE tenant_id='demo-local' AND product_id='coffee' AND action='memory.generation.use'"); e != nil {
			return ctx, nil, e
		}
		g.done.Store(true)
	}
	return g.inner.EnterStore(ctx)
}

func TestWithdrawalBetweenRollbackAndOutcomeRecordStaysPending(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	g := &withdrawAtOutcomeGuard{inner: r.Store.Guard, db: r.Store.DB}
	r.Store.Guard = g
	defer func() { r.Store.Guard = g.inner; r.Store.BeforeCommit = nil }()
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var services int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
			return e
		}
		if services == 0 {
			return nil
		}
		g.armed.Store(true)
		return errors.New("synthetic known rollback before independent withdrawal")
	}
	_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
	r.Store.BeforeCommit = nil
	if e == nil || !g.done.Load() {
		t.Fatal("withdrawal boundary not exercised", e, g.done.Load())
	}
	var state, reason string
	var steps, calls, commits, attempts int
	if e = r.Store.DB.QueryRow("SELECT l.state,l.reason,e.steps_used,t.calls_used,(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),(SELECT COUNT(*) FROM attempts WHERE task_id=?) FROM harness_local_rounds l JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) WHERE l.task_id=? AND l.phase='WORLD_COMMIT'", task, task, task).Scan(&state, &reason, &steps, &calls, &commits, &attempts); e != nil {
		t.Fatal(e)
	}
	if state != "PENDING" || reason != "" || steps != 8 || calls != 1 || commits != 0 || attempts != 1 {
		t.Fatal("withdrawal minted outcome/purchase", state, reason, steps, calls, commits, attempts)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); e == nil {
		t.Fatal("withdrawn original execution resumed")
	}
}

func TestCandidateTransactionKnownRollbackEvidence(t *testing.T) {
	for _, phase := range []string{"APPROVAL", "WORLD_COMMIT"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if phase == "WORLD_COMMIT" {
				if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
					t.Fatal(e)
				}
			}
			instance, e := r.World.GetInstance(ctx, scope)
			if e != nil {
				t.Fatal(e)
			}
			br, q := actionTask(t, r, scope.ProductScope, instance, "known-conflict-action")
			if _, e = commitAction(r, br, q); e != nil {
				t.Fatal(e)
			}
			if phase == "APPROVAL" {
				e = r.Store.ApproveOriginalTurn(ctx, lease, worker)
			} else {
				_, e = r.Store.CommitOriginalTurn(ctx, lease, worker)
			}
			if !errors.Is(e, store.ErrCandidateResourceConflict) {
				t.Fatal("actual conflict absent", e)
			}
			var state, reason string
			var round uint64
			if e = r.Store.DB.QueryRow("SELECT state,reason,round_number FROM harness_local_rounds WHERE task_id=? AND phase=? ORDER BY round_number DESC LIMIT 1", task, phase).Scan(&state, &reason, &round); e != nil || state != "ROLLED_BACK" || reason != "RESOURCE_CONFLICT" || round != 1 {
				t.Fatal("no actual successful-rollback evidence", state, reason, round, e)
			}
			if _, e = r.Store.QueryOriginalTurnCommit(ctx, scope, task); !errors.Is(e, sql.ErrNoRows) {
				t.Fatal("failed candidate has world receipt", e)
			}
		})
	}
}

func TestOriginalCommitUnknownNeverBecomesRollbackFromMissingReceipt(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	lost := errors.New("synthetic world COMMIT response lost after server rollback")
	r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		var services int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
			return e
		}
		if services == 0 {
			return tx.Commit()
		}
		if e := tx.Rollback(); e != nil {
			return e
		}
		return lost
	}
	_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("world outcome was not uncertain", e)
	}
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	br, q := actionTask(t, r, scope.ProductScope, instance, "unknown-competing-action")
	if _, e = commitAction(r, br, q); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("missing original receipt was converted into a fresh candidate transaction", e)
	}
	var state string
	if e = r.Store.DB.QueryRow("SELECT state FROM harness_local_rounds WHERE task_id=? AND phase='WORLD_COMMIT' ORDER BY round_number DESC LIMIT 1", task).Scan(&state); e != nil || state != "PENDING" {
		t.Fatal("unknown became known rollback", state, e)
	}
}

func TestCandidatePrepareUnknownNeverIssuesPermit(t *testing.T) {
	for _, phase := range []string{"APPROVAL", "WORLD_COMMIT"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if phase == "WORLD_COMMIT" {
				if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
					t.Fatal(e)
				}
			}
			r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
				if e := tx.Commit(); e != nil {
					return e
				}
				return errors.New("synthetic prepare ACK lost")
			}
			invoke := func() error {
				if phase == "APPROVAL" {
					return r.Store.ApproveOriginalTurn(ctx, lease, worker)
				}
				_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
				return e
			}
			if e := invoke(); !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("prepare ACK failure was not unknown", e)
			}
			r.Store.Commit = nil
			if e := invoke(); !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("pending prepare reconstructed a permit", e)
			}
			var pending, approvals, commits, calls, steps int
			if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=? AND state='PENDING'),(SELECT COUNT(*) FROM harness_candidate_approvals WHERE task_id=?),(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),t.calls_used,e.steps_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task, task).Scan(&pending, &approvals, &commits, &calls, &steps); e != nil {
				t.Fatal(e)
			}
			wantApproval, wantSteps := 0, 6
			if phase == "WORLD_COMMIT" {
				wantApproval, wantSteps = 1, 8
			}
			if pending != 1 || approvals != wantApproval || commits != 0 || calls != 1 || steps != wantSteps {
				t.Fatal(pending, approvals, commits, calls, steps)
			}
		})
	}
}

func TestCandidateApprovalReceiptReplayConsumesNoRound(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal("known approval replay failed", e)
	}
	var rounds, steps int
	if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=?),steps_used FROM harness_envelopes WHERE task_id=?", task, task).Scan(&rounds, &steps); e != nil || rounds != 1 || steps != 7 {
		t.Fatal(rounds, steps, e)
	}
}

func TestLocalFailureCannotForgeSemanticConflict(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var services int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
			return e
		}
		if services == 0 {
			return nil
		}
		return &store.CandidateResourceConflict{Kind: "item", ID: "caller-supplied", OriginalHash: "old", CurrentHash: "new"}
	}
	_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
	r.Store.BeforeCommit = nil
	if e == nil {
		t.Fatal("missing local fault")
	}
	var state, reason string
	var conflict []byte
	if e = r.Store.DB.QueryRow("SELECT state,reason,conflict_document FROM harness_local_rounds WHERE task_id=? AND phase='WORLD_COMMIT'", task).Scan(&state, &reason, &conflict); e != nil || state != "ROLLED_BACK" || reason != "LOCAL_FAILURE" || len(conflict) != 0 {
		t.Fatal("non-owning error forged recompute evidence", state, reason, string(conflict), e)
	}
}

func TestLocalRoundsStayWithinOriginalStepAndTimeBudget(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	var executeBefore, settleBefore string
	if e := r.Store.DB.QueryRow("SELECT CAST(execute_by AS CHAR),CAST(settle_by AS CHAR) FROM harness_envelopes WHERE task_id=?", task).Scan(&executeBefore, &settleBefore); e != nil {
		t.Fatal(e)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var services int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
			return e
		}
		if services == 0 {
			return nil
		}
		return errors.New("synthetic ordinary local rollback")
	}
	for i := 0; i < 7; i++ {
		if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e == nil {
			t.Fatal("local failure committed", i)
		}
	}
	r.Store.BeforeCommit = nil
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("exhausted pending outcome reopened", e)
	}
	var steps, calls, pending, rolled, conflicts, commits int
	var executeAfter, settleAfter string
	if e := r.Store.DB.QueryRow("SELECT e.steps_used,t.calls_used,CAST(e.execute_by AS CHAR),CAST(e.settle_by AS CHAR),(SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=? AND state='PENDING'),(SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=? AND state='ROLLED_BACK'),(SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=? AND reason='RESOURCE_CONFLICT'),(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task, task, task).Scan(&steps, &calls, &executeAfter, &settleAfter, &pending, &rolled, &conflicts, &commits); e != nil {
		t.Fatal(e)
	}
	if steps != 20 || calls != 1 || pending != 1 || rolled != 6 || conflicts != 0 || commits != 0 || executeAfter != executeBefore || settleAfter != settleBefore {
		t.Fatal("bounded original envelope changed", steps, calls, pending, rolled, conflicts, commits, executeAfter, settleAfter)
	}
}

func TestLaterPendingRoundSupersedesOldKnownRollback(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	br, q := actionTask(t, r, scope.ProductScope, instance, "round-history-conflict")
	if _, e = commitAction(r, br, q); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
		t.Fatal(e)
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic next prepare ACK lost")
	}
	e = r.Store.ApproveOriginalTurn(ctx, lease, worker)
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("old rollback reopened newer pending round", e)
	}
	var state, reason string
	var round uint64
	if e = r.Store.DB.QueryRow("SELECT state,reason,round_number FROM harness_local_rounds WHERE task_id=? AND phase='APPROVAL' ORDER BY round_number DESC LIMIT 1", task).Scan(&state, &reason, &round); e != nil || state != "PENDING" || reason != "" || round != 2 {
		t.Fatal(state, reason, round, e)
	}
}

func TestCandidateRollbackWithoutAcknowledgmentStaysPending(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
		var services int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
			return e
		}
		if services == 0 {
			return nil
		}
		// The server ended the Tx, but the fixed owning rollback receives
		// ErrTxDone rather than a successful rollback acknowledgment.
		if e := tx.Rollback(); e != nil {
			return e
		}
		return errors.New("synthetic rollback acknowledgment lost")
	}
	_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
	r.Store.BeforeCommit = nil
	if e == nil {
		t.Fatal("lost rollback outcome absent")
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal("rollback non-ack became proof", e)
	}
	var state, reason string
	if e = r.Store.DB.QueryRow("SELECT state,reason FROM harness_local_rounds WHERE task_id=? AND phase='WORLD_COMMIT'", task).Scan(&state, &reason); e != nil || state != "PENDING" || reason != "" {
		t.Fatal(state, reason, e)
	}
}

func TestCandidateApprovalUnknownQueriesExactCommittedPhase(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		var approvals int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_candidate_approvals WHERE task_id=?", task).Scan(&approvals); e != nil {
			return e
		}
		if e := tx.Commit(); e != nil {
			return e
		}
		if approvals == 0 {
			return nil
		}
		return errors.New("synthetic approval COMMIT ACK lost")
	}
	e := r.Store.ApproveOriginalTurn(ctx, lease, worker)
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal("committed approval phase not recoverable", e)
	}
	var rounds, steps int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=?),steps_used FROM harness_envelopes WHERE task_id=?", task, task).Scan(&rounds, &steps); e != nil || rounds != 1 || steps != 7 {
		t.Fatal(rounds, steps, e)
	}
}
