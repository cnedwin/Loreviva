package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
)

func TestOriginalResultProgramPersistsImmutableValidation(t *testing.T) {
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ValidateOriginalTurn(context.Background(), lease, worker); e != nil {
		t.Fatal("actual original result did not enter program validation", e)
	}
	var state string
	var steps, calls int
	if e := r.Store.DB.QueryRow("SELECT h.fsm_state,e.steps_used,t.calls_used FROM harness_turns h JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) WHERE h.task_id=?", task).Scan(&state, &steps, &calls); e != nil || state != "VALIDATED" || steps != 5 || calls != 1 {
		t.Fatal(state, steps, calls, e)
	}
	var hash, code string
	if e := r.Store.DB.QueryRow("SELECT body_hash,validation_code FROM harness_original_validations WHERE task_id=?", task).Scan(&hash, &code); e != nil || len(hash) != 64 || code != "STRUCTURE_VALID" {
		t.Fatal(hash, code, e)
	}
	if e := r.Store.ValidateOriginalTurn(context.Background(), lease, worker); e != nil {
		t.Fatal("same original validation replay", e)
	}
	var after int
	if e := r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", task).Scan(&after); e != nil || after != steps {
		t.Fatal("validation replay consumed another step", after, e)
	}
}

func TestOriginalValidationTakeoverUsesReceiptWithoutProviderQuery(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	before := journalBytes(t, r)
	if _, e := r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", task); e != nil {
		t.Fatal(e)
	}
	if e := r.Worker.Once(ctx); e != nil {
		t.Fatal("takeover ignored original known validation", e)
	}
	var calls, steps, queries int
	if e := r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,q.queries_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_original_queries q USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task).Scan(&calls, &steps, &queries); e != nil || calls != 1 || steps != 9 || queries != 0 {
		t.Fatal(calls, steps, queries, e)
	}
	if !bytes.Equal(before, journalBytes(t, r)) {
		t.Fatal("takeover changed original provider journal")
	}
	if _, e := r.Store.QueryOriginalValidation(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
}

func TestOriginalResultProgramClassifiesOnlyRealFullProviderBytes(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		fault      harness.MockFault
	}{{"broken-json", "STRUCTURE_INVALID", harness.MockMalformedOriginalResult}, {"refusal", "REFUSAL", harness.MockRefuseOriginalResult}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, scope, turns, q, request := actualTurnFixture(t, true)
			quote, e := turns.QuoteTurn(ctx, scope, q, "classification-quote")
			if e != nil {
				t.Fatal(e)
			}
			request.QuoteID = quote.QuoteID
			accepted, e := turns.SubmitTurn(ctx, scope, request, "classification-turn")
			if e != nil {
				t.Fatal(e)
			}
			r.Worker.Turns.Fault = tc.fault
			if e = runOriginalPrefix(ctx, r); e != nil {
				t.Fatal(e)
			}
			var code, state string
			var calls int
			if e = r.Store.DB.QueryRow("SELECT v.validation_code,t.state,t.calls_used FROM harness_original_validations v JOIN tasks t USING(tenant_id,product_id,task_id) WHERE v.task_id=?", accepted.TurnID).Scan(&code, &state, &calls); e != nil || code != tc.code || state != "RUNNING" || calls != 1 {
				t.Fatal("structure classification fabricated approval, commit, or purchase", code, state, calls, e)
			}
		})
	}
}

func TestOriginalResultValidationRollbackAndCommitUnknown(t *testing.T) {
	for _, ackUnknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "committed-ack-lost"}[ackUnknown], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := originalResultFixture(t, true)
			queryDue(t, r, task)
			if e := r.Worker.QueryOriginalOnce(ctx); e != nil {
				t.Fatal(e)
			}
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			identity, e := r.Store.OriginalMockIdentity(ctx, scope, task)
			if e != nil {
				t.Fatal(e)
			}
			journalBefore := journalBytes(t, r)
			failure := errors.New("synthetic local validation transport failure")
			if ackUnknown {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return failure
				}
			} else {
				r.Store.BeforeCommit = func(context.Context, *sql.Tx) error { return failure }
			}
			e = r.Store.ValidateOriginalTurn(ctx, lease, worker)
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			if e == nil || ackUnknown && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal("missing actual transaction failure", e)
			}
			var count, steps int
			var state string
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_original_validations WHERE task_id=?),e.steps_used,h.fsm_state FROM harness_turns h JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE h.task_id=?", task, task).Scan(&count, &steps, &state); e != nil {
				t.Fatal(e)
			}
			var original store.CommitReceipt
			if ackUnknown {
				if count != 1 || steps != 5 || state != "VALIDATED" {
					t.Fatal(count, steps, state)
				}
				original, e = r.Store.QueryOriginalValidation(ctx, scope, task)
				if e != nil || !original.Committed || original.TaskID != task || original.OwnerSubjectID != scope.SubjectID || original.Key == "" {
					t.Fatal("original local validation receipt unavailable", original, e)
				}
			} else if count != 0 || steps != 3 || state != "SEND_MARKED" {
				t.Fatal("partial validation transaction survived rollback", count, steps, state)
			}
			// Current permitted real bytes allow idempotence after lost ACK and a
			// genuine retry after rollback, without another provider call.
			if e = r.Store.ValidateOriginalTurn(ctx, lease, worker); e != nil {
				t.Fatal("body-present original replay/retry failed", e)
			}
			known, e := r.Store.QueryOriginalValidation(ctx, scope, task)
			if e != nil || !known.Committed || known.TaskID != task || known.OwnerSubjectID != scope.SubjectID || known.Key == "" || ackUnknown && known != original {
				t.Fatal("retry did not preserve exact original receipt", known, e)
			}
			var receiptsBefore int
			if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM commit_receipts").Scan(&receiptsBefore); e != nil {
				t.Fatal(e)
			}
			if ackUnknown {
				var storage string
				if e = r.Store.DB.QueryRow("SELECT storage_key FROM harness_result_sources WHERE task_id=?", task).Scan(&storage); e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, storage)
				if e = os.Remove(path + ".part"); e != nil {
					t.Fatal(e)
				}
				if receipt, e := r.Store.QueryOriginalValidation(ctx, scope, task); e != nil || receipt != known {
					t.Fatal("local COMMIT knowledge improperly depended on surviving body", receipt, e)
				}
				if e = r.Store.ValidateOriginalTurn(ctx, lease, worker); !errors.Is(e, files.ErrNotFound) {
					t.Fatal("missing body permitted validation or changed the denial cause", e)
				}
				for _, ext := range []string{".part", ".blob"} {
					if _, e = os.Stat(path + ext); !os.IsNotExist(e) {
						t.Fatal("validation reconstructed an absent original body", ext, e)
					}
				}
			}
			var calls, receiptsAfter, validationReceipts int
			if e = r.Store.DB.QueryRow("SELECT e.steps_used,t.calls_used,h.fsm_state,(SELECT COUNT(*) FROM harness_original_validations WHERE task_id=?),(SELECT COUNT(*) FROM commit_receipts),(SELECT COUNT(*) FROM commit_receipts WHERE task_id=? AND operation='HARNESS_RESULT_VALIDATE' AND phase='COMMIT') FROM harness_envelopes e JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task).Scan(&steps, &calls, &state, &count, &receiptsAfter, &validationReceipts); e != nil || steps != 5 || calls != 1 || state != "VALIDATED" || count != 1 || validationReceipts != 1 || receiptsAfter != receiptsBefore {
				t.Fatal("recovery repeated purchase, validation steps or receipt", steps, calls, state, count, validationReceipts, receiptsAfter, e)
			}
			if receipt, e := r.Store.QueryOriginalValidation(ctx, scope, task); e != nil || receipt != known {
				t.Fatal("final metadata recovery changed original receipt", receipt, e)
			}
			if current, e := r.Store.OriginalMockIdentity(ctx, scope, task); e != nil || current != identity || !bytes.Equal(journalBefore, journalBytes(t, r)) {
				t.Fatal("recovery changed original attempt or provider history", e)
			}
		})
	}
}
