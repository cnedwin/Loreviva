package integration

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProductCandidateActualProgramValidation(t *testing.T) {
	for _, code := range []string{"STRUCTURE_VALID", "STRUCTURE_INVALID"} {
		t.Run(code, func(t *testing.T) {
			r, l, _, a := productSent(t)
			ctx := runtimeContext(t, r)
			if code == "STRUCTURE_INVALID" {
				r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			}
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal("actual candidate program validation unavailable", e)
			}
			var got, state string
			var steps, calls, records int
			if e := r.Store.DB.QueryRow("SELECT v.validation_code,c.fsm_state,e.steps_used,t.calls_used,(SELECT COUNT(*) FROM harness_original_validations WHERE task_id=t.task_id) FROM harness_original_validations v JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) WHERE v.task_id=?", l.TaskID).Scan(&got, &state, &steps, &calls, &records); e != nil || got != code || state != "VALIDATED" || steps != 5 || calls != 1 || records != 1 {
				t.Fatal(got, state, steps, calls, records, e)
			}
			if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal("original validation replay", e)
			}
			if e := r.Store.DB.QueryRow("SELECT steps_used FROM harness_envelopes WHERE task_id=?", l.TaskID).Scan(&steps); e != nil || steps != 5 {
				t.Fatal("validation replay consumed step", steps, e)
			}
		})
	}
}

func TestProductCandidateLockedEvidenceAndActualSourceCleanup(t *testing.T) {
	r, l, d, a := productSent(t)
	ctx := runtimeContext(t, r)
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	evidence, e := r.Files.ReadPendingCandidateEvidence(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	defer evidence.Close()
	var draft string
	if e = r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	impact, e := r.Content.Impact(ctx, l.ProductScope, draft, 1, "DELETE", "candidate-lock-impact")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.Command(ctx, l.ProductScope, draft, 1, "DELETE", string(impact.ReportID), "", "", "candidate-lock-delete"); e != nil {
		t.Fatal(e)
	}
	var upper uint64
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id) FROM file_staging").Scan(&upper); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.DB.QueryRow("SELECT DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 24 HOUR)").Scan(&cutoff); e != nil {
		t.Fatal(e)
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		page, e := r.Files.CleanupPage(bounded, l.ProductScope, cutoff, 0, upper)
		if e == nil && page.Removed != 1 {
			e = errors.New("expected exactly original candidate purge")
		}
		done <- e
	}()
	select {
	case e := <-done:
		t.Fatal("cleanup bypassed live evidence lock", e)
	case <-time.After(100 * time.Millisecond):
	}
	file := filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, evidence.Metadata().BodyStorage+".part")
	if _, e = os.Stat(file); e != nil {
		t.Fatal("held bytes purged before evidence Close", e)
	}
	if e = r.Store.ValidateDraftCandidate(bounded, l, "candidate-worker"); e == nil {
		t.Fatal("deleted source authorized new validation")
	}
	evidence.Close()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal("cleanup did not drain", e)
		}
	case <-bounded.Done():
		t.Fatal("cleanup blocked with DB locks after release")
	}
	if _, e = os.Stat(file); !os.IsNotExist(e) {
		t.Fatal("source bytes retained after cleanup", e)
	}
}

func TestProductCandidateProgramRejectsActualUntrustedFields(t *testing.T) {
	cases := []struct {
		name  string
		fault harness.MockFault
		code  string
	}{
		{"duplicate", harness.MockDraftDuplicateField, "STRUCTURE_INVALID"},
		{"trailing", harness.MockDraftTrailingJSON, "STRUCTURE_INVALID"},
		{"unselected", harness.MockDraftUnselectedField, "STRUCTURE_INVALID"},
		{"wrong-type", harness.MockDraftInvalidFieldType, "STRUCTURE_INVALID"},
		{"overlong", harness.MockDraftOverlongField, "STRUCTURE_INVALID"},
		{"refusal", harness.MockRefuseOriginalResult, "REFUSAL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, l, _, a := productSent(t)
			ctx := runtimeContext(t, r)
			r.Worker.Turns.Fault = c.fault
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal(e)
			}
			var code string
			if e := r.Store.DB.QueryRow("SELECT validation_code FROM harness_original_validations WHERE task_id=?", l.TaskID).Scan(&code); e != nil || code != c.code {
				t.Fatal(code, e)
			}
			var extra int
			if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM attempts WHERE task_id=? AND slot=1", l.TaskID).Scan(&extra); e != nil || extra != 0 {
				t.Fatal("classification granted a repair", extra, e)
			}
		})
	}
}

func TestProductCandidateValidationAtomicOutcomeAndEvidenceClose(t *testing.T) {
	for _, phase := range []string{"ROLLBACK", "ACK_UNKNOWN", "WITHDRAW_BEFORE_COMMIT", "DELETE_INTENT_BEFORE_COMMIT"} {
		t.Run(phase, func(t *testing.T) {
			r, l, d, a := productSent(t)
			ctx := runtimeContext(t, r)
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			injected := errors.New("synthetic validation rollback")
			if phase == "ACK_UNKNOWN" {
				r.Store.Commit = func(ctx context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("validation ACK lost")
				}
			} else {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_original_validations WHERE task_id=?", l.TaskID).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return nil
					}
					if phase == "WITHDRAW_BEFORE_COMMIT" {
						_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'")
						return e
					}
					if phase == "DELETE_INTENT_BEFORE_COMMIT" {
						_, e := tx.ExecContext(ctx, "UPDATE harness_candidate_results SET delete_intent=1 WHERE task_id=?", l.TaskID)
						return e
					}
					return injected
				}
			}
			e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker")
			r.Store.BeforeCommit = nil
			r.Store.Commit = nil
			if e == nil {
				t.Fatal("validation ignored lost eligibility/transaction outcome")
			}
			if phase == "ACK_UNKNOWN" && !errors.Is(e, store.ErrCommitUnknown) {
				t.Fatal(e)
			}
			var records, steps int
			var state string
			if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_original_validations WHERE task_id=?),e.steps_used,c.fsm_state FROM harness_envelopes e JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) WHERE e.task_id=?", l.TaskID, l.TaskID).Scan(&records, &steps, &state); e != nil {
				t.Fatal(e)
			}
			if phase == "ACK_UNKNOWN" {
				if records != 1 || steps != 5 || state != "VALIDATED" {
					t.Fatal(records, steps, state)
				}
				receipt, e := r.Store.QueryDraftCandidateValidation(ctx, l.ProductScope, l.TaskID)
				if e != nil || !receipt.Committed {
					t.Fatal(receipt, e)
				}
			} else if records != 0 || steps != 3 || state != "SEND_MARKED" {
				t.Fatal("partial validation transaction", records, steps, state)
			}
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if _, e = r.Files.ReadPendingCandidateResult(bounded, d); e != nil {
				t.Fatal("evidence not released after Tx outcome", e)
			}
		})
	}
}
