package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type candidateBusinessCommitter interface {
	CommitDraftCandidate(context.Context, domain.TaskLease, string) (store.CommitReceipt, error)
	QueryDraftCandidateCommit(context.Context, domain.ProductScope, string) (store.CommitReceipt, error)
}

func TestProductCandidateCommittedAssetGovernedCleanup(t *testing.T) {
	ctx := context.Background()
	r, l, _ := validCandidateBusiness(t, true)
	if _, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	var asset, draft string
	if e := r.Store.DB.QueryRow("SELECT c.asset_id,a.draft_id FROM harness_candidate_commits c JOIN harness_admin_candidates a USING(tenant_id,product_id,task_id) WHERE c.task_id=?", l.TaskID).Scan(&asset, &draft); e != nil {
		t.Fatal(e)
	}
	rows, e := r.Store.DB.Query("SELECT storage_key FROM harness_candidate_results WHERE task_id=? ORDER BY attempt_id", l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	keys := []string{}
	for rows.Next() {
		var key string
		if e = rows.Scan(&key); e != nil {
			t.Fatal(e)
		}
		keys = append(keys, key)
	}
	rows.Close()
	if len(keys) != 2 {
		t.Fatal(keys)
	}
	var upper uint64
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 24 HOUR) FROM file_staging").Scan(&upper, &cutoff); e != nil {
		t.Fatal(e)
	}
	page, e := r.Files.CleanupPage(ctx, l.ProductScope, cutoff, 0, upper)
	if e != nil || page.Removed != 0 {
		t.Fatal("committed original dependencies purged", page, e)
	}
	for _, key := range keys {
		if _, e = os.Stat(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, key+".part")); e != nil {
			t.Fatal(e)
		}
	}
	impact, e := r.Content.Impact(ctx, l.ProductScope, draft, 1, "DELETE", "candidate-committed-delete-impact")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.Command(ctx, l.ProductScope, draft, 1, "DELETE", string(impact.ReportID), "", "", "candidate-committed-delete"); e != nil {
		t.Fatal(e)
	}
	// A fresh durable cleanup command after actual deletion must retire both
	// references, including the original0 dependency, and the real asset.
	cutoff = cutoff.Add(time.Second)
	page, e = r.Files.CleanupPage(ctx, l.ProductScope, cutoff, 0, upper)
	if e != nil || page.Removed != 2 {
		t.Fatal("real derived result cleanup", page, e)
	}
	var state string
	if e = r.Store.DB.QueryRow("SELECT state FROM assets WHERE asset_id=?", asset).Scan(&state); e != nil || state != "DELETED" {
		t.Fatal("asset not retired", state, e)
	}
	for _, key := range keys {
		if _, e = os.Stat(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, key+".part")); !os.IsNotExist(e) {
			t.Fatal("deleted dependency restored", e)
		}
	}
	if receipt, e := r.Store.QueryDraftCandidateCommit(ctx, l.ProductScope, l.TaskID); e != nil || !receipt.Committed {
		t.Fatal("body deletion erased original COMMIT fact", receipt, e)
	}
}

func validCandidateBusiness(t *testing.T, repair bool) (*app.Runtime, domain.TaskLease, store.DispatchRequest) {
	t.Helper()
	ctx := context.Background()
	r, l, d, a := productSent(t)
	if repair {
		r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	}
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	if repair {
		var e error
		d, e = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
			t.Fatal(e)
		}
		d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
		if e != nil {
			t.Fatal(e)
		}
		a, e = r.Store.StartSend(ctx, d, d.AttemptID)
		if e != nil {
			t.Fatal(e)
		}
		if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
			t.Fatal(e)
		}
		if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
			t.Fatal(e)
		}
		if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
			t.Fatal(e)
		}
	}
	return r, l, d
}

func TestProductCandidateRepairBusinessAccounting(t *testing.T) {
	ctx := context.Background()
	r, l, d := validCandidateBusiness(t, true)
	var original, accepted, execute, settle string
	if e := r.Store.DB.QueryRow("SELECT CAST(a.document AS CHAR),CAST(e.accepted_at AS CHAR),CAST(e.execute_by AS CHAR),CAST(e.settle_by AS CHAR) FROM harness_candidate_attempts a JOIN attempts b USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE a.task_id=? AND b.slot=0", l.TaskID).Scan(&original, &accepted, &execute, &settle); e != nil {
		t.Fatal(e)
	}
	receipt, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker")
	if e != nil || !receipt.Committed {
		t.Fatal(receipt, e)
	}
	var raw []byte
	var steps, calls int
	if e = r.Store.DB.QueryRow("SELECT c.document,e.steps_used,t.calls_used FROM harness_candidate_commits c JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE c.task_id=?", l.TaskID).Scan(&raw, &steps, &calls); e != nil || steps != 14 || calls != 2 {
		t.Fatal(steps, calls, e)
	}
	var committed struct {
		Metadata           domain.MockResultMetadata
		OriginalDependency *domain.MockResultMetadata
		AssetID            string
	}
	if json.Unmarshal(raw, &committed) != nil || committed.Metadata.Identity.AttemptID != d.AttemptID || committed.OriginalDependency == nil || committed.OriginalDependency.Identity.AttemptID == d.AttemptID {
		t.Fatal("per-attempt provenance", string(raw))
	}
	for _, m := range []domain.MockResultMetadata{*committed.OriginalDependency, committed.Metadata} {
		var in, out uint64
		var service, quote, serviceUnit, costUnit, currency string
		var serviceAtoms, costAtoms int64
		if e = r.Store.DB.QueryRow("SELECT service_id,quote_id,input_units,output_units,service_atoms,service_unit,supplier_atoms,supplier_unit,supplier_currency FROM harness_service_lines WHERE task_id=? AND attempt_id=?", l.TaskID, m.Identity.AttemptID).Scan(&service, &quote, &in, &out, &serviceAtoms, &serviceUnit, &costAtoms, &costUnit, &currency); e != nil || quote != m.Identity.QuoteID || in != m.InputUnits || out != m.OutputUnits || serviceAtoms != 0 || costAtoms != 0 || serviceUnit != "WE_CREDIT" || costUnit != "MONEY" || currency != "syntheticUSD" {
			t.Fatal("actual attempt line", e, in, out)
		}
		for _, kind := range []string{"supplier_global", "supplier_tenant"} {
			var account, dimension, status, purpose string
			var amount int64
			if e = r.Store.DB.QueryRow("SELECT account_id,scope_key,status,purpose,amount FROM reservations WHERE task_id=? AND kind=? AND purpose=?", l.TaskID, kind, m.Identity.AttemptID).Scan(&account, &dimension, &status, &purpose, &amount); e != nil || account != "r0-mock-supplier" || status != "SETTLED" || amount != 0 || purpose != m.Identity.AttemptID {
				t.Fatal(kind, account, dimension, status, e)
			}
			if kind == "supplier_global" && dimension != "GLOBAL" || kind == "supplier_tenant" && dimension != l.TenantID {
				t.Fatal("two limits same original cost", dimension)
			}
		}
	}
	for _, kind := range []string{"customer", "project", "platform_risk"} {
		var n int
		var status string
		if e = r.Store.DB.QueryRow("SELECT COUNT(*),MIN(status) FROM reservations WHERE task_id=? AND kind=? AND purpose='PARENT'", l.TaskID, kind).Scan(&n, &status); e != nil || n != 1 {
			t.Fatal(kind, n, e)
		}
		want := "SETTLED"
		if kind == "platform_risk" {
			want = "RELEASED"
		}
		if status != want {
			t.Fatal(kind, status)
		}
	}
	var services, lines, postings int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?)", l.TaskID, l.TaskID, l.TaskID).Scan(&services, &lines, &postings); e != nil || services != 1 || lines != 2 || postings != 7 {
		t.Fatal(services, lines, postings, e)
	}
	var after, gotAccepted, gotExecute, gotSettle string
	if e = r.Store.DB.QueryRow("SELECT CAST(a.document AS CHAR),CAST(e.accepted_at AS CHAR),CAST(e.execute_by AS CHAR),CAST(e.settle_by AS CHAR) FROM harness_candidate_attempts a JOIN attempts b USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE a.task_id=? AND b.slot=0", l.TaskID).Scan(&after, &gotAccepted, &gotExecute, &gotSettle); e != nil || after != original || gotAccepted != accepted || gotExecute != execute || gotSettle != settle {
		t.Fatal("original immutable", e)
	}
	if asset, e := r.Files.Get(ctx, l.ProductScope, committed.AssetID); e != nil || asset.Sha256 != committed.Metadata.BodyHash {
		t.Fatal("fixed S55 candidate asset unavailable", e)
	}
}

func assertCandidateNotCommitted(t *testing.T, r *app.Runtime, l domain.TaskLease) {
	t.Helper()
	for _, table := range []string{"harness_candidate_commits", "service_events", "harness_service_lines", "postings"} {
		var n int
		if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", l.TaskID).Scan(&n); e != nil || n != 0 {
			t.Fatal("partial commit", table, n, e)
		}
	}
	var bad int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM reservations WHERE task_id=? AND status<>'HELD'", l.TaskID).Scan(&bad); e != nil || bad != 0 {
		t.Fatal("partial hold", bad, e)
	}
	var n int
	if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM assets WHERE document->>'$.name'='candidate-fields.json'").Scan(&n); e != nil || n != 0 {
		t.Fatal("partial asset", n, e)
	}
}

func TestProductCandidateBusinessAtomicBoundaries(t *testing.T) {
	for _, mode := range []string{"service_events", "harness_service_lines", "postings", "assets", "harness_candidate_results", "harness_candidate_commits", "harness_turn_steps", "WITHDRAW", "VALIDATION_REMOVED", "DELETE_INTENT", "COST_EVIDENCE", "OLD_FENCE", "ACK_UNKNOWN", "ACK_UNKNOWN_ROLLBACK"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, l, d := validCandidateBusiness(t, true)
			if mode == "ACK_UNKNOWN" || mode == "ACK_UNKNOWN_ROLLBACK" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if mode == "ACK_UNKNOWN_ROLLBACK" {
						if e := tx.Rollback(); e != nil {
							return e
						}
						return errors.New("rollback outcome ACK lost")
					}
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("candidate business ACK lost")
				}
			} else if mode == "WITHDRAW" || mode == "VALIDATION_REMOVED" || mode == "DELETE_INTENT" || mode == "COST_EVIDENCE" {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_candidate_commits WHERE task_id=?", l.TaskID).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return nil
					}
					switch mode {
					case "WITHDRAW":
						_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'")
						return e
					case "COST_EVIDENCE":
						_, e := tx.ExecContext(ctx, "UPDATE harness_original_queries SET metadata=JSON_SET(metadata,'$.NoFurtherCost',FALSE) WHERE task_id=? AND attempt_id=?", l.TaskID, d.AttemptID)
						return e
					case "VALIDATION_REMOVED":
						_, e := tx.ExecContext(ctx, "DELETE FROM harness_original_validations WHERE task_id=? AND attempt_id=?", l.TaskID, d.AttemptID)
						return e
					default:
						_, e := tx.ExecContext(ctx, "UPDATE harness_candidate_results SET delete_intent=1 WHERE task_id=?", l.TaskID)
						return e
					}
				}
			} else if mode == "OLD_FENCE" {
				l.Fence++
			} else {
				predicate := "task_id<>'" + l.TaskID + "'"
				if mode == "assets" {
					predicate = "document->>'$.name'<>'candidate-fields.json'"
				}
				if mode == "harness_turn_steps" {
					predicate = "task_id<>'" + l.TaskID + "' OR step_number<=10"
				}
				if mode == "harness_candidate_results" {
					predicate = "asset_id IS NULL"
				}
				if _, e := r.Store.DB.Exec("ALTER TABLE " + mode + " ADD CONSTRAINT candidate_atomic_failure CHECK (" + predicate + ")"); e != nil {
					t.Fatal(e)
				}
			}
			receipt, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker")
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			if e == nil || receipt.Committed {
				t.Fatal("failure returned committed", e, receipt)
			}
			if mode == "ACK_UNKNOWN" {
				if !errors.Is(e, store.ErrCommitUnknown) {
					t.Fatal(e)
				}
				known, err := r.Store.QueryDraftCandidateCommit(ctx, l.ProductScope, l.TaskID)
				if err != nil || !known.Committed {
					t.Fatal("exact receipt recovery", known, err)
				}
				if _, err = r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); err != nil {
					t.Fatal(err)
				}
				if _, err = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker"); err == nil {
					t.Fatal("unknown COMMIT bought again")
				}
				return
			}
			assertCandidateNotCommitted(t, r, l)
			if _, err := r.Store.QueryDraftCandidateCommit(ctx, l.ProductScope, l.TaskID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("rollback became commit", err)
			}
			if mode == "OLD_FENCE" {
				l.Fence--
			}
			switch mode {
			case "service_events", "harness_service_lines", "postings", "assets", "harness_candidate_results", "harness_candidate_commits", "harness_turn_steps":
				if _, err := r.Store.DB.Exec("ALTER TABLE " + mode + " DROP CHECK candidate_atomic_failure"); err != nil {
					t.Fatal(err)
				}
			}
			// Same original business command after its actual no-COMMIT outcome.
			// This also proves every failed path released both Files evidence locks.
			bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			if receipt, err := r.Store.CommitDraftCandidate(bounded, l, "candidate-worker"); err != nil || !receipt.Committed {
				t.Fatal("original business retry after actual rollback", receipt, err)
			}
		})
	}
}

func TestProductCandidateActualBusinessCommit(t *testing.T) {
	ctx := context.Background()
	r, l, _, a := productSent(t)
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	c, ok := any(r.Store).(candidateBusinessCommitter)
	if !ok {
		t.Fatal("real typed candidate business COMMIT unavailable")
	}
	receipt, e := c.CommitDraftCandidate(ctx, l, "candidate-worker")
	if e != nil || !receipt.Committed {
		t.Fatal(receipt, e)
	}
	var state, asset string
	var calls, steps int
	if e = r.Store.DB.QueryRow("SELECT t.state,c.asset_id,t.calls_used,e.steps_used FROM harness_candidate_commits c JOIN tasks t USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE c.task_id=?", l.TaskID).Scan(&state, &asset, &calls, &steps); e != nil || state != "COMMITTED" || asset == "" || calls != 1 || steps != 9 {
		t.Fatal(state, asset, calls, steps, e)
	}
	for table, want := range map[string]int{"service_events": 1, "harness_service_lines": 1, "postings": 5, "harness_candidate_commits": 1} {
		var got int
		if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE task_id=?", l.TaskID).Scan(&got); e != nil || got != want {
			t.Fatal(table, got, e)
		}
	}
	var count int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM assets WHERE asset_id=? AND state='READY'", asset).Scan(&count); e != nil || count != 1 {
		t.Fatal("real result asset", count, e)
	}
	again, e := c.CommitDraftCandidate(ctx, l, "candidate-worker")
	if e != nil || again.Key != receipt.Key {
		t.Fatal("stable replay", again, e)
	}
	if e = r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", l.TaskID).Scan(&steps); e != nil || steps != 9 {
		t.Fatal("replay consumed steps", steps, e)
	}
}
