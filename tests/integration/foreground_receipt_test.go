package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func foregroundReceiptFixture(t *testing.T, kind string) (*app.Runtime, domain.MockRequestIdentity) {
	t.Helper()
	ctx := context.Background()
	if kind == "PRODUCT" {
		r, l, _, a := productSent(t)
		// Only setup generation enters the existing leased issuer; original query recovery below retains its necessary program context.
		issuer, e := r.Store.EnterDraftTaskIssuer(ctx, l, "candidate-worker")
		if e != nil {
			t.Fatal(e)
		}
		if e := r.Worker.Turns.GenerateDraftCandidate(issuer, a.OriginalSend()); e != nil {
			t.Fatal(e)
		}
		id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
		if e != nil {
			t.Fatal(e)
		}
		queryDue(t, r, l.TaskID)
		return r, id
	}
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	queryDue(t, r, task)
	return r, id
}

func TestForegroundReceiptWorkerRestartDoesNotQueryKnownOriginalAgain(t *testing.T) {
	ctx := context.Background()
	r, id := foregroundReceiptFixture(t, "WORLD")
	l, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("record original reply ACK lost")
	}
	_, e = r.Store.RecordOriginalQuery(ctx, l)
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	recovered := reopenRuntimeFixture(t, ctx, r)
	if _, e = recovered.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", id.TaskID); e != nil {
		t.Fatal(e)
	}
	if e = recovered.Worker.Once(ctx); e != nil {
		t.Fatal("resume known original outcome", e)
	}
	var used, calls int
	var state string
	if e = recovered.Store.DB.QueryRow("SELECT q.queries_used,t.calls_used,t.state FROM harness_original_queries q JOIN tasks t USING(tenant_id,product_id,task_id) WHERE task_id=? AND attempt_id=?", id.TaskID, id.AttemptID).Scan(&used, &calls, &state); e != nil || used != 1 || calls != 1 || state != "COMMITTED" {
		t.Fatal(used, calls, state, e)
	}
	if !bytes.Equal(before, journalBytes(t, recovered)) {
		t.Fatal("recovery generated provider request")
	}
}

func TestForegroundQueryReceiptRollbackAndCurrentMinimalRead(t *testing.T) {
	for _, phase := range []string{"BEGIN", "RECORD"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			r, id := foregroundReceiptFixture(t, "PRODUCT")
			rollback := func(ctx context.Context, tx *sql.Tx) error {
				if e := tx.Rollback(); e != nil {
					return e
				}
				return errors.New("synthetic rollback acknowledgement lost")
			}
			if phase == "BEGIN" {
				r.Store.Commit = rollback
			}
			l, e := r.Store.BeginOriginalQuery(ctx, id)
			r.Store.Commit = nil
			if phase == "RECORD" {
				if e != nil {
					t.Fatal(e)
				}
				r.Store.Commit = rollback
				_, e = r.Store.RecordOriginalQuery(ctx, l)
				r.Store.Commit = nil
			}
			if !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal(e)
			}
			receipt, e := r.Store.QueryOriginalQueryReceipt(ctx, id, l.OriginalQueryKey())
			if phase == "BEGIN" {
				if !errors.Is(e, sql.ErrNoRows) {
					t.Fatal("rolled back begin forged receipt", receipt, e)
				}
			} else if e != nil || !receipt.BeginCommitted || receipt.RecordCommitted {
				t.Fatal(receipt, e)
			}
			var used, records int
			var state string
			if e = r.Store.DB.QueryRow("SELECT queries_used,provider_state,(SELECT COUNT(*) FROM harness_original_query_records WHERE task_id=?) FROM harness_original_queries WHERE task_id=?", id.TaskID, id.TaskID).Scan(&used, &state, &records); e != nil {
				t.Fatal(e)
			}
			want := 0
			if phase == "RECORD" {
				want = 1
			}
			if used != want || records != want || state != "UNKNOWN" {
				t.Fatal("count/lease/record rollback diverged", used, records, state)
			}
		})
	}
	ctx := context.Background()
	r, id := foregroundReceiptFixture(t, "PRODUCT")
	l, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalQuery(ctx, l); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 181 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 61 SECOND),settle_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", id.TaskID); e != nil {
		t.Fatal(e)
	}
	receipt, e := r.Store.QueryOriginalQueryReceipt(ctx, id, l.OriginalQueryKey())
	if e != nil || !receipt.RecordCommitted {
		t.Fatal(receipt, e)
	}
	raw, _ := json.Marshal(receipt)
	for _, bad := range []string{"BodyStorage", "BodyHash", "BodyAvailable", "Canonical"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Fatal("receipt reveals body reference", bad)
		}
	}
	if _, e = r.Store.RecordOriginalQuery(ctx, l); e == nil {
		t.Fatal("receipt recreated IO lease")
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_query_services SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.QueryOriginalQueryReceipt(ctx, id, l.OriginalQueryKey()); e == nil {
		t.Fatal("disabled service read original receipt")
	}
}
func TestForegroundOriginalQueryCommitACKUsesExactReceipt(t *testing.T) {
	for _, kind := range []string{"WORLD", "PRODUCT"} {
		for _, phase := range []string{"BEGIN", "RECORD"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				ctx := context.Background()
				r, id := foregroundReceiptFixture(t, kind)
				lost := func(ctx context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("synthetic original query ACK lost")
				}
				if phase == "BEGIN" {
					r.Store.Commit = lost
				}
				l, e := r.Store.BeginOriginalQuery(ctx, id)
				r.Store.Commit = nil
				if phase == "BEGIN" {
					if !errors.Is(e, store.ErrCommitUnknown) || l.OriginalQueryKey() == "" {
						t.Fatal("unknown Begin must retain only exact safe key", e)
					}
					if _, e = r.Store.RecordOriginalQuery(ctx, l); e == nil {
						t.Fatal("unknown begin reconstructed IO permit")
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					r.Store.Commit = lost
					_, e = r.Store.RecordOriginalQuery(ctx, l)
					r.Store.Commit = nil
					if !errors.Is(e, store.ErrCommitUnknown) {
						t.Fatal("record must report unknown COMMIT", e)
					}
				}
				receipt, e := r.Store.QueryOriginalQueryReceipt(ctx, id, l.OriginalQueryKey())
				if e != nil || !receipt.BeginCommitted || receipt.RecordCommitted != (phase == "RECORD") || receipt.Kind != "FOREGROUND" || receipt.Ordinal != 1 {
					t.Fatal(receipt, e)
				}
				if _, e = r.Store.QueryOriginalQueryReceipt(ctx, id, "other-query"); e == nil {
					t.Fatal("unrelated receipt accepted")
				}
				var used int
				if e = r.Store.DB.QueryRow("SELECT queries_used FROM harness_original_queries WHERE task_id=? AND attempt_id=?", id.TaskID, id.AttemptID).Scan(&used); e != nil || used != 1 {
					t.Fatal(used, e)
				}
			})
		}
	}
}

func TestOriginalReplyReceiptHasIndependentOrdinalZero(t *testing.T) {
	for _, lostAt := range []int{1, 2} {
		t.Run([]string{"", "BEGIN", "RECORD"}[lostAt], func(t *testing.T) {
			r, l, _, a := productSent(t)
			ctx := runtimeContext(t, r)
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			id, e := r.Store.CandidateMockIdentity(ctx, l.ProductScope, l.TaskID)
			if e != nil {
				t.Fatal(e)
			}
			commits := 0
			r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
				if e := tx.Commit(); e != nil {
					return e
				}
				commits++
				if commits == lostAt {
					return errors.New("reply ACK lost")
				}
				return nil
			}
			_, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend())
			r.Store.Commit = nil
			if !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal(e)
			}
			var key string
			var used int
			if e = r.Store.DB.QueryRow("SELECT r.query_key,q.queries_used FROM harness_original_query_records r JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE r.task_id=? AND r.kind='REPLY' AND r.ordinal=0", id.TaskID).Scan(&key, &used); e != nil || used != 0 {
				t.Fatal(used, e)
			}
			receipt, e := r.Store.QueryOriginalQueryReceipt(ctx, id, key)
			if e != nil || receipt.Ordinal != 0 || receipt.Kind != "REPLY" || !receipt.BeginCommitted || receipt.RecordCommitted != (lostAt == 2) {
				t.Fatal(receipt, e)
			}
			if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e == nil {
				t.Fatal("unknown reply recreated permit")
			}
			if lostAt == 2 {
				m, e := r.Store.ReadOriginalQueryCompletion(ctx, id)
				if e != nil || m.State != "SUCCEEDED" {
					t.Fatal(m, e)
				}
			}
		})
	}
}

func TestOriginalQueryReceiptMissingHistoryCannotResetBudget(t *testing.T) {
	ctx := context.Background()
	r, id := foregroundReceiptFixture(t, "PRODUCT")
	l, e := r.Store.BeginOriginalQuery(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_original_queries SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),next_query_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", id.TaskID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM harness_original_query_records WHERE query_key=?", l.OriginalQueryKey()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.BeginOriginalQuery(ctx, id); e == nil {
		t.Fatal("missing trusted original receipt reset query sequence")
	}
	var used int
	if e = r.Store.DB.QueryRow("SELECT queries_used FROM harness_original_queries WHERE task_id=?", id.TaskID).Scan(&used); e != nil || used != 1 {
		t.Fatal(used, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 59 SECOND) WHERE task_id=?", id.TaskID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.BeginNecessaryCostQuery(ctx, id); e == nil {
		t.Fatal("necessary cost bypassed missing original foreground history")
	}
}
