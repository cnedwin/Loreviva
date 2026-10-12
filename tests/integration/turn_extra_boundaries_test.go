package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestRecoveryDispatchIdentityIsServerFrozen(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	d, e := r.Store.PrepareTurnRecovery(ctx, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	d.RequestKey = "caller-replaced-request"
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e == nil {
		t.Fatal("caller replaced the program-owned recovery request identity")
	}
}

func TestRecoverySharedSlotAndPurchaseBoundaries(t *testing.T) {
	for _, mode := range []string{"concurrent-plan", "missing-before-authorize", "missing-before-send", "withdraw-before-send", "stale-fence"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if mode == "concurrent-plan" {
				var wg sync.WaitGroup
				results := make(chan error, 2)
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); _, e := r.Store.PrepareTurnRecovery(ctx, lease, worker); results <- e }()
				}
				wg.Wait()
				close(results)
				success := 0
				for e := range results {
					if e == nil {
						success++
					}
				}
				if success != 1 {
					t.Fatal("shared decision count", success)
				}
				var n int
				if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_recovery_approvals WHERE task_id=?", task).Scan(&n); e != nil || n != 1 {
					t.Fatal(n, e)
				}
				return
			}
			d, e := r.Store.PrepareTurnRecovery(ctx, lease, worker)
			if e != nil {
				t.Fatal(e)
			}
			if mode != "missing-before-authorize" {
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
					t.Fatal(e)
				}
			}
			var raw []byte
			var plan domain.RecoveryPlan
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_recovery_approvals WHERE task_id=?", task).Scan(&raw); e != nil || json.Unmarshal(raw, &plan) != nil {
				t.Fatal(e)
			}
			if mode == "missing-before-authorize" || mode == "missing-before-send" {
				if e = os.Remove(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, plan.Original.BodyStorage+".part")); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "withdraw-before-send" {
				if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "stale-fence" {
				d.Fence--
			}
			if mode == "missing-before-authorize" {
				_, e = r.Store.AuthorizeDispatch(ctx, d)
			} else {
				d.CommandKey = "extra-boundary-send"
				d.CommitKey = d.CommandKey
				_, e = r.Store.StartSend(ctx, d, d.AttemptID)
			}
			if e == nil {
				t.Fatal("extra purchase boundary accepted", mode)
			}
			var sent int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=? AND slot=1 AND send_started_at IS NOT NULL", task).Scan(&sent); e != nil || sent != 0 {
				t.Fatal(sent, e)
			}
		})
	}
}

func TestExtraCanonicalContainsActualUTF8Original(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	original, _, e := readOriginalResult(ctx, r.Files, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	d, e := r.Store.PrepareTurnRecovery(ctx, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d.CommandKey = content.Identity(d.AttemptID, "send")
	d.CommitKey = d.CommandKey
	auth, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	_, material, e := auth.OriginalSend().Take(ctx, r.Store)
	if e != nil {
		t.Fatal(e)
	}
	var prompt struct {
		Recovery struct{ Reason, OriginalAttempt, OriginalBody string }
	}
	if json.Unmarshal(material.Canonical, &prompt) != nil || prompt.Recovery.Reason != "STRUCTURE_REPAIR" || prompt.Recovery.OriginalBody != string(original) {
		t.Fatal("original bytes were not actual UTF8 request content")
	}
	hash := sha256.Sum256(material.Canonical)
	if hex.EncodeToString(hash[:]) != material.Binding.RequestHash {
		t.Fatal("different canonical was bound")
	}
	again, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || again.CanSend || !again.QueryOriginal || again.OriginalSend() != nil {
		t.Fatal("existing marker granted Generate", e)
	}
}

func TestExtraLostReplyRestartUsesActiveOriginalRequest(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	original, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDropOriginalReply
	if e := r.Worker.Turns.ExecuteRecovery(ctx, lease, worker); !errors.Is(e, harness.ErrMockReplyLost) {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	var extraAttempt string
	if e = r.Store.DB.QueryRow("SELECT attempt_id FROM attempts WHERE task_id=? AND slot=1", task).Scan(&extraAttempt); e != nil {
		t.Fatal(e)
	}
	extra, e := r.Store.AttemptMockIdentity(ctx, scope, task, extraAttempt)
	if e != nil || extra.AttemptID == original.AttemptID || extra.RequestKey == original.RequestKey {
		t.Fatal("extra request did not retain its own original identity", e)
	}
	// Each actual request has begin/completion plus its independent bill record.
	lines := bytes.Split(bytes.TrimSuffix(before, []byte("\n")), []byte("\n"))
	if len(lines) != 6 || bytes.Contains(before, []byte("我听到了")) {
		t.Fatal("not exactly two metadata-only actual requests and bills")
	}
	for slot, identity := range []domain.MockRequestIdentity{original, extra} {
		for n, state := range []string{"UNKNOWN", "SUCCEEDED", "BILL"} {
			var entry struct {
				Result domain.MockResultMetadata
				Bill   *domain.MockBillItem
				Usage  *domain.MockUsageMeasurement
			}
			if e = json.Unmarshal(lines[slot*3+n], &entry); e != nil || entry.Result.State != state || entry.Result.Identity != identity || entry.Usage != nil {
				t.Fatal("journal changed original request identity or lifecycle", slot, n, e)
			}
			if state == "BILL" {
				if entry.Bill == nil || entry.Bill.Identity != identity || entry.Bill.ItemID != "original-charge" {
					t.Fatal("missing exact original bill", slot)
				}
			} else if entry.Bill != nil {
				t.Fatal("bill mislabeled as provider request", slot)
			}
		}
	}
	local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
	if e != nil {
		t.Fatal(e)
	}
	defer local.Participant.Close(ctx)
	recovered := app.NewRuntime(r.Store, r.Files, r.Auth)
	recovered.BindLocal(local)
	if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, task)
	if e = recovered.Worker.Once(ctx); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("restart purchased another request")
	}
	var state string
	var calls int
	if e = r.Store.DB.QueryRow("SELECT state,calls_used FROM tasks WHERE task_id=?", task).Scan(&state, &calls); e != nil || state != "COMMITTED" || calls != 2 {
		t.Fatal("restart failed to continue actual active1", state, calls, e)
	}
	if _, _, e = recovered.Files.ReadApprovedTurn(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); e == nil {
		t.Fatal("retired original candidate or old fence approved")
	}
}

func TestRecoveryRejectsIncompleteOrUnknownOriginal(t *testing.T) {
	for _, mode := range []string{"invalid-later-memory", "unknown-commit", "refusal"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			fault := harness.MockFault(0)
			if mode == "invalid-later-memory" {
				fault = harness.MockWrongRelationshipMemory
			}
			if mode == "refusal" {
				fault = harness.MockRefuseOriginalResult
			}
			r, scope, task, _ := actionResultFixture(t, fault)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if mode == "unknown-commit" {
				if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
					t.Fatal(e)
				}
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return tx.Commit()
					}
					if e := tx.Rollback(); e != nil {
						return e
					}
					return errors.New("lost COMMIT outcome")
				}
				_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
				r.Store.Commit = nil
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
			}
			if mode != "refusal" {
				instance, e := r.World.GetInstance(ctx, scope)
				if e != nil {
					t.Fatal(e)
				}
				br, q := actionTask(t, r, scope.ProductScope, instance, "negative-recovery-action")
				if _, e = commitAction(r, br, q); e != nil {
					t.Fatal(e)
				}
				if mode == "invalid-later-memory" {
					if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
						t.Fatal(e)
					}
				}
			}
			if _, e := r.Store.PrepareTurnRecovery(ctx, lease, worker); e == nil {
				t.Fatal("invalid original got extra", mode)
			}
			var n, calls int
			if e := r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_recovery_approvals WHERE task_id=?) FROM tasks WHERE task_id=?", task, task).Scan(&calls, &n); e != nil || calls != 1 || n != 0 {
				t.Fatal(calls, n, e)
			}
		})
	}
}

func TestExtraJointCommitRollbackAndLostACK(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-rollback", true: "committed-lost-ACK"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			var before []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&before); e != nil {
				t.Fatal(e)
			}
			r.Worker.Turns.Fault = 0
			fault := errors.New("extra joint commit transport failure")
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				var services, events, lines, posts, outbox int
				if e := tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM events WHERE task_id=?),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?),(SELECT COUNT(*) FROM outbox WHERE task_id=?)", task, task, task, task, task).Scan(&services, &events, &lines, &posts, &outbox); e != nil {
					return e
				}
				if services == 0 {
					return nil
				}
				if services != 1 || events != 1 || lines != 2 || posts != 8 || outbox != 1 {
					t.Errorf("not the complete actual joint transaction: %d/%d/%d/%d/%d", services, events, lines, posts, outbox)
				}
				if !unknown {
					return fault
				}
				return nil
			}
			if unknown {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&n); e != nil {
						return e
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					if n > 0 {
						return fault
					}
					return nil
				}
			}
			e := r.Worker.Turns.ExecuteRecovery(ctx, lease, worker)
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if e == nil || unknown && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal(e)
			}
			if !unknown {
				for _, table := range []string{"events", "approved_results", "service_events", "harness_service_lines", "postings", "outbox", "harness_turn_commits"} {
					var n int
					if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
						t.Fatal("partial write survived", table, n, e)
					}
				}
				var after []byte
				if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&after); e != nil || !bytes.Equal(before, after) {
					t.Fatal("memory survived rollback", e)
				}
			} else {
				if receipt, e := r.Store.QueryOriginalTurnCommit(ctx, scope, task); e != nil || !receipt.Committed {
					t.Fatal(receipt, e)
				}
			}
			journal := journalBytes(t, r)
			if e = r.Worker.Turns.ExecuteRecovery(ctx, lease, worker); e != nil {
				t.Fatal("same extra continuation", e)
			}
			if !bytes.Equal(journal, journalBytes(t, r)) {
				t.Fatal("local recovery repurchased")
			}
			if body, _, e := r.Files.ReadApprovedTurn(ctx, scope, task); e != nil || len(body) == 0 {
				t.Fatal("committed extra unreadable", e)
			}
		})
	}
}

func TestExtraResultNeverReadsWithoutOriginalBytes(t *testing.T) {
	for _, mode := range []string{"missing", "hash-mismatch", "permission-withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			r.Worker.Turns.Fault = harness.MockDropOriginalReply
			if e := r.Worker.Turns.ExecuteRecovery(ctx, lease, worker); !errors.Is(e, harness.ErrMockReplyLost) {
				t.Fatal(e)
			}
			var raw []byte
			var plan domain.RecoveryPlan
			if e := r.Store.DB.QueryRow("SELECT document FROM harness_recovery_approvals WHERE task_id=?", task).Scan(&raw); e != nil || json.Unmarshal(raw, &plan) != nil {
				t.Fatal(e)
			}
			id, e := r.Store.AttemptMockIdentity(ctx, scope, task, plan.AttemptID)
			if e != nil {
				t.Fatal(e)
			}
			queryDue(t, r, task)
			q, e := r.Store.BeginOriginalQuery(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.RecordOriginalQuery(ctx, q); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, plan.Original.BodyStorage+".part")
			if mode == "missing" {
				e = os.Remove(path)
			} else if mode == "hash-mismatch" {
				e = os.WriteFile(path, []byte("synthetic tampered body"), 0600)
			} else {
				_, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'")
			}
			if e != nil {
				t.Fatal(e)
			}
			if evidence, e := r.Files.ReadAttemptEvidence(ctx, lease, worker, plan.AttemptID); e == nil {
				evidence.Close()
				t.Fatal("extra body ignored original dependency")
			}
			if e = r.Store.ValidateAttemptTurn(ctx, lease, worker, plan.AttemptID); e == nil {
				t.Fatal("invalid dependent result validated")
			}
			var calls, validated int
			if e = r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_original_validations WHERE task_id=? AND attempt_id=?) FROM tasks WHERE task_id=?", task, plan.AttemptID, task).Scan(&calls, &validated); e != nil || calls != 2 || validated != 0 {
				t.Fatal(calls, validated, e)
			}
		})
	}
}

func TestExtraMarkedUnsentAndUnknownACKRestartNeverGenerates(t *testing.T) {
	for _, lostACK := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-marker-unsent", true: "marker-committed-ACK-lost"}[lostACK], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t, harness.MockMalformedOriginalResult)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			d, e := r.Store.PrepareTurnRecovery(ctx, lease, worker)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d.CommandKey = content.Identity(task, "extra-send")
			d.CommitKey = d.CommandKey
			if lostACK {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic send marker committed ACK lost")
				}
			}
			auth, e := r.Store.StartSend(ctx, d, d.AttemptID)
			r.Store.Commit = nil
			if lostACK {
				if !errors.Is(e, store.ErrCommitUnknown) || auth.CanSend || auth.OriginalSend() != nil {
					t.Fatal("unknown marker exposed fresh permit", auth.CanSend, e)
				}
			} else if e != nil || !auth.CanSend {
				t.Fatal(e)
			}
			// Deliberately never consume the known permit: durable marker alone only permits query.
			before := journalBytes(t, r)
			local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
			if e != nil {
				t.Fatal(e)
			}
			defer local.Participant.Close(ctx)
			recovered := app.NewRuntime(r.Store, r.Files, r.Auth)
			recovered.BindLocal(local)
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
				t.Fatal(e)
			}
			queryDue(t, r, task)
			if e = recovered.Worker.Once(ctx); e == nil {
				t.Fatal("provider UNKNOWN was falsely completed")
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("known marker caused Generate on restart")
			}
			var state, provider string
			var calls, queries, commits int
			if e = r.Store.DB.QueryRow("SELECT t.state,t.calls_used,q.provider_state,q.queries_used,(SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?) FROM tasks t JOIN attempts a USING(tenant_id,product_id,task_id) JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE t.task_id=? AND a.slot=1", task, task).Scan(&state, &calls, &provider, &queries, &commits); e != nil {
				t.Fatal(e)
			}
			if state != "RUNNING" || calls != 2 || provider != "UNKNOWN" || queries != 1 || commits != 0 {
				t.Fatal(state, calls, provider, queries, commits)
			}
		})
	}
}

func TestExtraCanonicalCannotExceedOriginalQuotedInput(t *testing.T) {
	ctx := context.Background()
	baseline, scope, task, _ := originalResultFixture(t, false)
	id, e := baseline.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	m, e := baseline.Worker.Turns.Journal.OriginalMetadata(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	bound := m.InputUnits + 20
	r, s, turns, q, request := actualTurnFixture(t, true)
	q.InputUpperBound = uintString(bound)
	quote, e := turns.QuoteTurn(ctx, s, q, "tight-quote")
	if e != nil {
		t.Fatal(e)
	}
	request.QuoteID = quote.QuoteID
	accepted, e := turns.SubmitTurn(ctx, s, request, "tight-turn")
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = runOriginalPrefix(ctx, r); e != nil {
		t.Fatal("original must fit actual quote", e)
	}
	lease, worker := currentResultLease(t, accepted.TurnID, s, r.Store.DB)
	before := journalBytes(t, r)
	if _, e = r.Store.PrepareTurnRecovery(ctx, lease, worker); e == nil {
		t.Fatal("Recovery bytes exceeded original quoted input")
	}
	var calls, plans int
	if e = r.Store.DB.QueryRow("SELECT calls_used,(SELECT COUNT(*) FROM harness_recovery_approvals WHERE task_id=?) FROM tasks WHERE task_id=?", accepted.TurnID, accepted.TurnID).Scan(&calls, &plans); e != nil || calls != 1 || plans != 0 {
		t.Fatal(calls, plans, e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("oversize recovery purchased")
	}
}
func uintString(n uint64) string { return strconv.FormatUint(n, 10) }

func TestRecoveryRetiresOriginalCandidateBeforeExtraSend(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	instance, e := r.World.GetInstance(ctx, scope)
	if e != nil {
		t.Fatal(e)
	}
	br, q := actionTask(t, r, scope.ProductScope, instance, "retired-candidate-conflict")
	if _, e = commitAction(r, br, q); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
		t.Fatal(e)
	}
	if _, e = r.Store.PrepareTurnRecovery(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	var stepsBefore, roundsBefore int
	if e = r.Store.DB.QueryRow("SELECT steps_used,(SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=?) FROM harness_envelopes WHERE task_id=?", task, task).Scan(&stepsBefore, &roundsBefore); e != nil {
		t.Fatal(e)
	}
	// The still-current lease must not revive candidate0 even before slot1 is sent.
	if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrDenied) {
		t.Fatal("old approval revived", e)
	}
	if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrDenied) {
		t.Fatal("old commit revived", e)
	}
	var stepsAfter, roundsAfter, calls int
	if e = r.Store.DB.QueryRow("SELECT e.steps_used,t.calls_used,(SELECT COUNT(*) FROM harness_local_rounds WHERE task_id=?) FROM harness_envelopes e JOIN tasks t USING(tenant_id,product_id,task_id) WHERE e.task_id=?", task, task).Scan(&stepsAfter, &calls, &roundsAfter); e != nil || stepsAfter != stepsBefore || roundsAfter != roundsBefore || calls != 1 {
		t.Fatal(stepsBefore, stepsAfter, roundsBefore, roundsAfter, calls, e)
	}
}
