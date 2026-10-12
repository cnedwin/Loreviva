package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestActualTurnAtomicZeroServiceCommit(t *testing.T) {
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(context.Background(), lease, worker); e != nil {
		t.Fatal(e)
	}
	receipt, e := r.Store.CommitOriginalTurn(context.Background(), lease, worker)
	if e != nil || !receipt.Committed {
		t.Fatal("approved actual provider result did not commit", receipt, e)
	}
	for table, wanted := range map[string]int{"approved_results": 1, "service_events": 1, "postings": 6, "outbox": 1, "events": 0} {
		var n int
		if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", task).Scan(&n); e != nil || n != wanted {
			t.Fatal(table, n, e)
		}
	}
	var state, fsm string
	var held, calls, steps int
	if e := r.Store.DB.QueryRow("SELECT t.state,h.fsm_state,t.calls_used,e.steps_used,(SELECT COUNT(*) FROM reservations WHERE task_id=? AND status='HELD') FROM tasks t JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task).Scan(&state, &fsm, &calls, &steps, &held); e != nil || state != "COMMITTED" || fsm != "COMMITTED" || calls != 1 || steps != 9 || held != 0 {
		t.Fatal(state, fsm, calls, steps, held, e)
	}
	again, e := r.Store.CommitOriginalTurn(context.Background(), lease, worker)
	if e != nil || again.Key != receipt.Key || !again.Committed {
		t.Fatal("original receipt replay", again, e)
	}
}

func TestActualOriginalEvidenceLocksBytesUntilCloseButDoesNotRetainPermission(t *testing.T) {
	testEvidenceGovernanceLocks(t, false)
}
func TestActualExtraEvidencePairBlocksCleanupUntilClose(t *testing.T) {
	testEvidenceGovernanceLocks(t, true)
}
func testEvidenceGovernanceLocks(t *testing.T, extra bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var r *app.Runtime
	var scope domain.Scope
	var task, actor string
	if !extra {
		r, scope, task, actor = originalResultFixture(t, false)
	}
	var attempt string
	var originalStorage string
	if extra {
		r, scope, task, _ = actionResultFixture(t, harness.MockMalformedOriginalResult)
		if e := r.Store.DB.QueryRow("SELECT actor_id FROM harness_turns WHERE task_id=?", task).Scan(&actor); e != nil {
			t.Fatal(e)
		}
		lease, worker := currentResultLease(t, task, scope, r.Store.DB)
		r.Worker.Turns.Fault = harness.MockDropOriginalReply
		if e := r.Worker.Turns.ExecuteRecovery(ctx, lease, worker); !errors.Is(e, harness.ErrMockReplyLost) {
			t.Fatal(e)
		}
		d, e := r.Store.RecoveryTurnDispatch(ctx, lease, worker)
		if e != nil {
			t.Fatal(e)
		}
		attempt = d.AttemptID
		id, e := r.Store.AttemptMockIdentity(ctx, scope, task, attempt)
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
		if e = r.Store.ValidateAttemptTurn(ctx, lease, worker, attempt); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.DB.QueryRow("SELECT s.storage_key FROM harness_result_sources s JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) WHERE s.task_id=? AND a.slot=0", task).Scan(&originalStorage); e != nil {
			t.Fatal(e)
		}
	} else {
		id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
		if e != nil {
			t.Fatal(e)
		}
		attempt = id.AttemptID
	}
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	evidence, e := r.Files.ReadAttemptEvidence(ctx, lease, worker, attempt)
	if e != nil {
		t.Fatal(e)
	}
	defer evidence.Close()
	var source string
	var raw []byte
	var a memory.Aggregate
	if e = r.Store.DB.QueryRow("SELECT input_source_id FROM harness_turns WHERE task_id=?", task).Scan(&source); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT document FROM memory_aggregates WHERE world_instance_id=? AND data_subject_id=?", scope.WorldInstanceID, scope.SubjectID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &a); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_govern=1 WHERE subject_id=?", scope.SubjectID); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"memory.govern", "memory.source.govern"} {
		if _, e = r.Store.DB.Exec("INSERT INTO memory_subject_permissions VALUES('demo-local','coffee','creator','creator',?,'PRIVATE_RESPONSE',DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 HOUR),1)", action); e != nil {
			t.Fatal(e)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, e := r.Memory.Govern(ctx, domain.RelationshipScope{Scope: scope, ActorInstanceID: actor, PlayerID: scope.SubjectID}, memory.MemoryGovernanceCommand{Key: "held-result-delete", Action: "DELETE", Selector: memory.Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{source}, Expected: map[string]uint64{source: a.Sources[source].Revision}}})
		done <- e
	}()
	finished := false
	defer func() {
		evidence.Close()
		cancel()
		if !finished {
			<-done
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var intent bool
		if e = r.Store.DB.QueryRow("SELECT delete_intent FROM memory_asset_lineage WHERE storage_key=?", evidence.Metadata().BodyStorage).Scan(&intent); e != nil {
			t.Fatal(e)
		}
		if intent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("governance metadata did not commit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The old evidence object has stable bytes but is not a new approval grant.
	if e = r.Store.ApproveAttemptTurn(ctx, lease, worker, attempt); e == nil {
		t.Fatal("deleted source approved via held evidence")
	}
	select {
	case e := <-done:
		finished = true
		t.Fatal("cleanup bypassed active evidence lock", e)
	case <-time.After(100 * time.Millisecond):
	}
	path := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, evidence.Metadata().BodyStorage+".part")
	if _, e = os.Stat(path); e != nil {
		t.Fatal("held original bytes were purged", e)
	}
	old := time.Now().Add(-2 * time.Hour)
	if _, e = r.Store.DB.Exec("UPDATE file_staging SET created_at=? WHERE storage_key=?", old, evidence.Metadata().BodyStorage); e != nil {
		t.Fatal(e)
	}
	if e = os.Chtimes(path, old, old); e != nil {
		t.Fatal(e)
	}
	var upper uint64
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id) FROM file_staging").Scan(&upper); e != nil {
		t.Fatal(e)
	}
	stageDone := make(chan error, 1)
	go func() {
		_, e := r.Files.CleanupPage(ctx, scope.ProductScope, time.Now().Add(-time.Hour), 0, upper)
		stageDone <- e
	}()
	stageFinished := false
	defer func() {
		evidence.Close()
		cancel()
		if !stageFinished {
			<-stageDone
		}
	}()
	select {
	case e := <-stageDone:
		stageFinished = true
		t.Fatal("bounded stage cleanup bypassed held evidence", e)
	case <-time.After(100 * time.Millisecond):
	}
	if _, e = os.Stat(path); e != nil {
		t.Fatal("bounded stage cleanup purged held bytes", e)
	}
	if extra {
		if _, err := os.Stat(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, originalStorage+".part")); err != nil {
			t.Fatal("locked original0 purged", err)
		}
	}
	evidence.Close()
	select {
	case e := <-done:
		finished = true
		if e != nil {
			t.Fatal("cleanup failed after evidence release", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup deadlocked after evidence release")
	}
	select {
	case e := <-stageDone:
		stageFinished = true
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stage cleanup deadlocked after evidence release")
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("governed bytes remain after real cleanup", e)
	}
	if extra {
		if _, err := os.Stat(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, originalStorage+".part")); !os.IsNotExist(err) {
			t.Fatal("original dependency remains after governed cleanup", err)
		}
	}
}

func TestActualCommittedTurnHasGovernedReadableBody(t *testing.T) {
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(context.Background(), lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(context.Background(), lease, worker); e != nil {
		t.Fatal(e)
	}
	body, committed, e := r.Files.ReadApprovedTurn(context.Background(), scope, task)
	if e != nil || len(body) == 0 || committed.ResultID == "" {
		t.Fatal("terminal current-authorized result not readable", len(body), committed.ResultID, e)
	}
	view, e := r.Turns.GetTurn(context.Background(), scope, task)
	if e != nil || len(view.ApprovedPackets) != 1 || view.Billing.ServiceSettlementState != "SETTLED" {
		t.Fatal("approved GetTurn projection", view, e)
	}
	var actor string
	if e = r.Store.DB.QueryRow("SELECT actor_id FROM harness_turns WHERE task_id=?", task).Scan(&actor); e != nil {
		t.Fatal(e)
	}
	if e = r.Memory.RecordStatement(context.Background(), memory.Namespace{Scope: scope, DataSubjectID: scope.SubjectID}, "later-source", "later-statement", actor, "another private statement", []memory.Use{memory.PrivateResponse}); e != nil {
		t.Fatal("legitimate later memory", e)
	}
	after, _, e := r.Files.ReadApprovedTurn(context.Background(), scope, task)
	if e != nil || !bytes.Equal(body, after) {
		t.Fatal("legitimate new graph incorrectly invalidated selected original closure", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE memory_subject_permissions SET enabled=0 WHERE action='memory.generation.use'"); e != nil {
		t.Fatal(e)
	}
	if body, _, e := r.Files.ReadApprovedTurn(context.Background(), scope, task); e == nil || len(body) != 0 {
		t.Fatal("current withdrawn permission leaked terminal bytes", e)
	}
}

func TestActualTurnCommitRollbackAndUnknownUseOriginalReceipt(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "all-writes-rollback", true: "committed-ack-lost"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, false)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal(e)
			}
			failure := errors.New("synthetic world commit transport fault")
			if unknown {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					var services int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM service_events WHERE task_id=?", task).Scan(&services); e != nil {
						return e
					}
					if services == 0 {
						return tx.Commit()
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					return failure
				}
			} else {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					var results, services, posts, outbox int
					if e := tx.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM approved_results WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?),(SELECT COUNT(*) FROM outbox WHERE task_id=?)", task, task, task, task).Scan(&results, &services, &posts, &outbox); e != nil {
						return e
					}
					if services == 0 {
						return nil
					} // Inject only after the real joint world writes.
					if results != 1 || services != 1 || posts != 6 || outbox != 1 {
						t.Errorf("fault must hit after all real writes: %d %d %d %d", results, services, posts, outbox)
					}
					return failure
				}
			}
			_, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			if e == nil || unknown && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("fault not observed", e)
			}
			var n, steps, calls int
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?),e.steps_used,t.calls_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task).Scan(&n, &steps, &calls); e != nil {
				t.Fatal(e)
			}
			if unknown {
				if n != 1 || steps != 9 || calls != 1 {
					t.Fatal(n, steps, calls)
				}
				var storage string
				if e = r.Store.DB.QueryRow("SELECT storage_key FROM harness_result_sources WHERE task_id=?", task).Scan(&storage); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".part")); e != nil {
					t.Fatal(e)
				}
				if receipt, e := r.Store.QueryOriginalTurnCommit(ctx, scope, task); e != nil || !receipt.Committed {
					t.Fatal("commit knowledge must survive missing narrative body", receipt, e)
				}
			} else {
				if n != 0 || steps != 9 || calls != 1 {
					t.Fatal(n, steps, calls)
				}
				for _, table := range []string{"approved_results", "service_events", "postings", "outbox", "harness_service_lines"} {
					var n int
					if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
						t.Fatal("partial world write survived rollback", table, n, e)
					}
				}
			}
			receipt, e := r.Store.CommitOriginalTurn(ctx, lease, worker)
			if e != nil || !receipt.Committed {
				t.Fatal("original retry/receipt", receipt, e)
			}
		})
	}
}

func TestActualTurnCommitRejectsOldFence(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.DB.Exec("UPDATE tasks SET fence=fence+1 WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e == nil {
		t.Fatal("old fence committed")
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_turn_commits WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestCommittedReaderRequiresCurrentNarrativeGenerationAndBody(t *testing.T) {
	for _, boundary := range []string{"generation", "retired", "missing-body", "wrong-owner"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, false)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal(e)
			}
			// These mutations are explicit negative barrier fixtures, not an implemented
			// ACCOUNTING_ONLY lifecycle or a claim that retirement proof exists yet.
			switch boundary {
			case "generation":
				if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET narrative_generation=narrative_generation+1 WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
			case "retired":
				if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET state='ACCOUNTING_ONLY' WHERE task_id=?", task); e != nil {
					t.Fatal(e)
				}
			case "missing-body":
				var storage string
				if e := r.Store.DB.QueryRow("SELECT storage_key FROM harness_result_sources WHERE task_id=?", task).Scan(&storage); e != nil {
					t.Fatal(e)
				}
				if e := os.Remove(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage+".part")); e != nil {
					t.Fatal(e)
				}
			case "wrong-owner":
				scope.SubjectID = "another-owner"
			}
			if b, _, e := r.Files.ReadApprovedTurn(ctx, scope, task); e == nil || len(b) != 0 {
				t.Fatal("terminal body crossed current boundary", boundary, e)
			}
		})
	}
}
