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
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func invalidCandidate(t *testing.T) (*app.Runtime, domain.TaskLease, store.DispatchRequest) {
	t.Helper()
	r, l, d, a := productSent(t)
	ctx := runtimeContext(t, r)
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	return r, l, d
}

func candidateOriginalSnapshot(t *testing.T, ctx context.Context, r *app.Runtime, d store.DispatchRequest) []byte {
	t.Helper()
	var attempt, validation, binding []byte
	if e := r.Store.DB.QueryRow("SELECT a.document,v.metadata,c.binding FROM harness_candidate_attempts a JOIN harness_original_validations v USING(tenant_id,product_id,task_id,attempt_id) JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) WHERE a.task_id=? AND a.attempt_id=?", d.TaskID, d.AttemptID).Scan(&attempt, &validation, &binding); e != nil {
		t.Fatal(e)
	}
	m, e := r.Store.PendingCandidateBody(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	body, e := r.Files.ReadPendingCandidateResult(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	out, _ := json.Marshal([]any{json.RawMessage(attempt), json.RawMessage(validation), json.RawMessage(binding), m, string(body)})
	return out
}

func TestProductCandidateRepairCompetitionAndOriginalImmutability(t *testing.T) {
	r, l, original := invalidCandidate(t)
	ctx := runtimeContext(t, r)
	before := candidateOriginalSnapshot(t, ctx, r, original)
	var execute, settle string
	if e := r.Store.DB.QueryRow("SELECT CAST(execute_by AS CHAR),CAST(settle_by AS CHAR) FROM harness_envelopes WHERE task_id=?", l.TaskID).Scan(&execute, &settle); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("shared extra winners", wins)
	}
	d, e := r.Store.ActiveDraftCandidateDispatch(ctx, l, "candidate-worker")
	if e != nil || d.Slot != 1 {
		t.Fatal(d, e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	// Inspect the exact immutable canonical supplied by the single-use permit.
	sent, m, e := a.OriginalSend().TakeCandidate(ctx, r.Store)
	if e != nil {
		t.Fatal(e)
	}
	var prompt map[string]json.RawMessage
	if json.Unmarshal(m.Canonical, &prompt) != nil {
		t.Fatal("canonical JSON")
	}
	var repair struct{ OriginalBody, OriginalAttempt, OriginalHash, Reason string }
	if json.Unmarshal(prompt["Recovery"], &repair) != nil || repair.OriginalBody != "{\"fields\":" || repair.OriginalAttempt != original.AttemptID || repair.Reason != "STRUCTURE_REPAIR" || sent.AttemptID != d.AttemptID {
		t.Fatal("actual UTF8 dependency", repair)
	}
	if bytes.Equal(before, candidateOriginalSnapshot(t, ctx, r, original)) == false {
		t.Fatal("original0 rewritten")
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, original); e == nil {
		t.Fatal("retired original authorization")
	}
	if _, e = r.Store.QueryDraftCandidateValidation(ctx, l.ProductScope, l.TaskID); e != nil {
		t.Fatal("explicit original receipt", e)
	}
	if _, e = r.Store.QueryDraftCandidateAttemptValidation(ctx, l.ProductScope, l.TaskID, d.AttemptID); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("original receipt confused with extra", e)
	}
	var gotExecute, gotSettle string
	var calls, steps int
	if e = r.Store.DB.QueryRow("SELECT CAST(e.execute_by AS CHAR),CAST(e.settle_by AS CHAR),t.calls_used,e.steps_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&gotExecute, &gotSettle, &calls, &steps); e != nil || gotExecute != execute || gotSettle != settle || calls != 2 || steps != 8 {
		t.Fatal(gotExecute, gotSettle, calls, steps, e)
	}
}

func TestProductCandidateRepairRejectsNonEvidence(t *testing.T) {
	for _, mode := range []string{"VALID", "REFUSAL", "NO_VALIDATION", "BODY_MISSING", "SOURCE_DELETED", "CANCELLED", "EXPIRED"} {
		t.Run(mode, func(t *testing.T) {
			r, l, d, a := productSent(t)
			ctx := runtimeContext(t, r)
			if mode == "REFUSAL" {
				r.Worker.Turns.Fault = harness.MockRefuseOriginalResult
			} else if mode != "VALID" {
				r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			}
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if mode != "NO_VALIDATION" {
				if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
					t.Fatal(e)
				}
			}
			switch mode {
			case "BODY_MISSING":
				m, e := r.Store.PendingCandidateBody(ctx, d)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, m.BodyStorage+".part")); e != nil {
					t.Fatal(e)
				}
			case "SOURCE_DELETED":
				var draft string
				if e := r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
					t.Fatal(e)
				}
				impact, e := r.Content.Impact(ctx, l.ProductScope, draft, 1, "DELETE", "repair-impact")
				if e != nil {
					t.Fatal(e)
				}
				if _, e = r.Content.Command(ctx, l.ProductScope, draft, 1, "DELETE", string(impact.ReportID), "", "", "repair-delete"); e != nil {
					t.Fatal(e)
				}
			case "CANCELLED":
				if _, e := r.Store.DB.Exec("UPDATE tasks SET state='CANCELLED' WHERE task_id=?", l.TaskID); e != nil {
					t.Fatal(e)
				}
			case "EXPIRED":
				if _, e := r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 121 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND),settle_by=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 59 SECOND) WHERE task_id=?", l.TaskID); e != nil {
					t.Fatal(e)
				}
			}
			before := journalBytes(t, r)
			if _, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker"); e == nil {
				t.Fatal("invalid repair evidence accepted")
			}
			if !bytes.Equal(before, journalBytes(t, r)) {
				t.Fatal("denial wrote J")
			}
			var approvals, calls int
			if e := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=t.task_id),calls_used FROM tasks t WHERE task_id=?", l.TaskID).Scan(&approvals, &calls); e != nil || approvals != 0 || calls != 1 {
				t.Fatal(approvals, calls, e)
			}
		})
	}
}

func TestProductCandidateRepairTwoBoundaries(t *testing.T) {
	for _, phase := range []string{"AUTHORIZE", "SEND", "CONSUME"} {
		for _, change := range []string{"ASSIST", "BODY", "VALIDATION"} {
			t.Run(phase+"_"+change, func(t *testing.T) {
				r, l, original := invalidCandidate(t)
				ctx := runtimeContext(t, r)
				d, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
				if e != nil {
					t.Fatal(e)
				}
				var permit *store.OriginalSendPermit
				if phase != "AUTHORIZE" {
					if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
						t.Fatal(e)
					}
					d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
					if e != nil {
						t.Fatal(e)
					}
				}
				if phase == "CONSUME" {
					a, err := r.Store.StartSend(ctx, d, d.AttemptID)
					if err != nil {
						t.Fatal(err)
					}
					permit = a.OriginalSend()
				}
				switch change {
				case "ASSIST":
					_, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'")
				case "VALIDATION":
					_, e = r.Store.DB.Exec("DELETE FROM harness_original_validations WHERE task_id=?", l.TaskID)
				case "BODY":
					var key string
					e = r.Store.DB.QueryRow("SELECT storage_key FROM harness_candidate_results WHERE task_id=? AND attempt_id=?", l.TaskID, original.AttemptID).Scan(&key)
					if e == nil {
						e = os.Remove(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, key+".part"))
					}
				}
				if e != nil {
					t.Fatal(e)
				}
				before := journalBytes(t, r)
				switch phase {
				case "AUTHORIZE":
					_, e = r.Store.AuthorizeDispatch(ctx, d)
				case "SEND":
					_, e = r.Store.StartSend(ctx, d, d.AttemptID)
				default:
					e = r.Worker.Turns.GenerateDraftCandidate(ctx, permit)
				}
				if e == nil {
					t.Fatal("lost dependency at actual boundary")
				}
				if !bytes.Equal(before, journalBytes(t, r)) {
					t.Fatal("denial generated provider request")
				}
			})
		}
	}
}

func TestProductCandidateRepairRestartKnownMarker(t *testing.T) {
	for _, mode := range []string{"UNSENT", "SEND_ACK_UNKNOWN", "LOST_REPLY"} {
		t.Run(mode, func(t *testing.T) {
			r, l, _ := invalidCandidate(t)
			ctx := runtimeContext(t, r)
			d, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
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
			if mode != "UNSENT" {
				if mode == "SEND_ACK_UNKNOWN" {
					r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
						if e := tx.Commit(); e != nil {
							return e
						}
						return errors.New("extra marker ACK lost")
					}
				}
				a, err := r.Store.StartSend(ctx, d, d.AttemptID)
				r.Store.Commit = nil
				if mode == "SEND_ACK_UNKNOWN" {
					if !errors.Is(err, store.ErrCommitUnknown) || a.OriginalSend() != nil {
						t.Fatal(a, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
						t.Fatal(e)
					}
				}
			}
			local, e := app.OpenLocal(ctx, r.Store, filepath.Dir(r.Local.Coordinator.Directory()), r.Files.RootPath)
			if e != nil {
				t.Fatal(e)
			}
			defer local.Participant.Close(ctx)
			fresh := app.NewRuntime(r.Store, r.Files, r.Auth)
			fresh.BindLocal(local)
			if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", l.TaskID); e != nil {
				t.Fatal(e)
			}
			next, e := r.Store.ClaimRegistered(ctx, "candidate-takeover", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE"}})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
				t.Fatal("old fence")
			}
			resumed, e := r.Store.PrepareDraftCandidateSend(ctx, next.TaskLease, "candidate-takeover")
			if e != nil || resumed.AttemptID != d.AttemptID || resumed.RequestHash != d.RequestHash || resumed.Slot != 1 {
				t.Fatal(resumed, e)
			}
			a, e := r.Store.StartSend(ctx, resumed, resumed.AttemptID)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "UNSENT" {
				if !a.CanSend {
					t.Fatal(a)
				}
				if e = fresh.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
					t.Fatal(e)
				}
			} else {
				if a.CanSend || !a.QueryOriginal || a.OriginalSend() != nil {
					t.Fatal("new purchase for original marker", a)
				}
				id, e := r.Store.CandidateAttemptMockIdentity(ctx, l.ProductScope, l.TaskID, d.AttemptID)
				if e != nil {
					t.Fatal(e)
				}
				queryDue(t, r, l.TaskID)
				q, e := r.Store.BeginOriginalQuery(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				m, e := r.Store.RecordOriginalQuery(ctx, q)
				if e != nil {
					t.Fatal(e)
				}
				if mode == "SEND_ACK_UNKNOWN" {
					if m.State != "UNKNOWN" {
						t.Fatal(m.State)
					}
				} else if m.State != "SUCCEEDED" {
					t.Fatal(m.State)
				}
			}
			if mode != "SEND_ACK_UNKNOWN" {
				if e = r.Store.ValidateDraftCandidate(ctx, next.TaskLease, "candidate-takeover"); e != nil {
					t.Fatal(e)
				}
			}
			var n int
			if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", l.TaskID).Scan(&n); e != nil || n != 2 {
				t.Fatal(n, e)
			}
			if _, e = r.Store.CandidateAttemptMockIdentity(ctx, l.ProductScope, l.TaskID, content.Identity(l.TaskID, "not-owned-attempt")); e == nil {
				t.Fatal("foreign attempt")
			}
		})
	}
}

func TestProductCandidateGenuineStructureRepair(t *testing.T) {
	r, l, _, a := productSent(t)
	ctx := runtimeContext(t, r)
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	d, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal("real malformed result did not authorize repair", e)
	}
	if d.Slot != 1 {
		t.Fatal("not shared extra", d.Slot)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal("repair authorization", e)
	}
	d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	a, e = r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil || !a.CanSend {
		t.Fatal("repair marker", e)
	}
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal("repair Generate", e)
	}
	if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal("repair validation", e)
	}
	var calls, steps, holds, validations int
	var state string
	if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,c.fsm_state,(SELECT COUNT(*) FROM reservations WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_original_validations WHERE task_id=t.task_id) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_admin_candidates c USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&calls, &steps, &state, &holds, &validations); e != nil || calls != 2 || steps != 10 || state != "VALIDATED" || holds != 7 || validations != 2 {
		t.Fatal(calls, steps, state, holds, validations, e)
	}
	zeroReceipt, e := r.Store.QueryDraftCandidateValidation(ctx, l.ProductScope, l.TaskID)
	if e != nil {
		t.Fatal(e)
	}
	oneReceipt, e := r.Store.QueryDraftCandidateAttemptValidation(ctx, l.ProductScope, l.TaskID, d.AttemptID)
	if e != nil || zeroReceipt.Key == oneReceipt.Key {
		t.Fatal("distinct validation receipts", e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_original_validations SET receipt_key=? WHERE task_id=? AND attempt_id=?", zeroReceipt.Key, l.TaskID, d.AttemptID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.QueryDraftCandidateAttemptValidation(ctx, l.ProductScope, l.TaskID, d.AttemptID); e == nil {
		t.Fatal("original0 receipt substituted for extra1 validation")
	}
}

func TestProductCandidateRepairApprovalAtomicEvidence(t *testing.T) {
	for _, mode := range []string{"ROLLBACK", "ACK_UNKNOWN", "VALIDATION_REMOVED", "WITHDRAW", "DELETE_INTENT"} {
		t.Run(mode, func(t *testing.T) {
			r, l, _, a := productSent(t)
			ctx := runtimeContext(t, r)
			r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if e := r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal(e)
			}
			if mode == "ACK_UNKNOWN" {
				r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
					if e := tx.Commit(); e != nil {
						return e
					}
					return errors.New("approval ACK lost")
				}
			} else {
				r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=?", l.TaskID).Scan(&n); e != nil {
						return e
					}
					if n == 0 {
						return nil
					}
					switch mode {
					case "VALIDATION_REMOVED":
						_, e := tx.ExecContext(ctx, "DELETE FROM harness_original_validations WHERE task_id=?", l.TaskID)
						return e
					case "WITHDRAW":
						_, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'")
						return e
					case "DELETE_INTENT":
						_, e := tx.ExecContext(ctx, "UPDATE harness_candidate_results SET delete_intent=1 WHERE task_id=?", l.TaskID)
						return e
					default:
						return errors.New("repair approval rollback")
					}
				}
			}
			d, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
			r.Store.Commit = nil
			r.Store.BeforeCommit = nil
			if e == nil || d.AttemptID != "" {
				t.Fatal("approval returned authority after failed final proof", mode, e)
			}
			var n, steps, calls int
			if err := r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=t.task_id),e.steps_used,t.calls_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&n, &steps, &calls); err != nil {
				t.Fatal(err)
			}
			if mode == "ACK_UNKNOWN" {
				if !errors.Is(e, store.ErrCommitUnknown) || n != 1 || steps != 6 || calls != 1 {
					t.Fatal(e, n, steps, calls)
				}
				resumed, err := r.Store.ActiveDraftCandidateDispatch(ctx, l, "candidate-worker")
				if err != nil || resumed.Slot != 1 {
					t.Fatal(resumed, err)
				}
			} else if n != 0 || steps != 5 || calls != 1 {
				t.Fatal("partial approval", n, steps, calls)
			}
		})
	}
}

func TestProductCandidateRepairCanonicalDependencyLimit(t *testing.T) {
	r, scope, release := runtimeFixtureWithConsent(t, false)
	ctx := runtimeContext(t, r)
	published, e := r.Content.GetRelease(ctx, scope, release)
	if e != nil {
		t.Fatal(e)
	}
	in := published.Input
	for i, raw := range in.Objects {
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			t.Fatal("fixture")
		}
		if object["kind"] == "CHARACTER" {
			in.RootObjectID = generated.Identity(object["object_id"].(string))
			lines := make([]string, 14)
			for j := range lines {
				lines[j] = strings.Repeat("\\", 500)
			}
			object["example_lines"] = lines
			in.Objects[i], _ = json.Marshal(object)
			break
		}
	}
	draft, e := r.Content.Create(ctx, scope, "CHARACTER", "", in, "repair-limit-draft")
	if e != nil {
		t.Fatal(e)
	}
	request := generated.CandidateRequest{ProductID: generated.Identity(scope.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "INTERACTION", ObjectID: in.RootObjectID, SelectedFields: []string{"example_lines"}, ApprovedCostUnits: "0"}
	accepted, e := r.Store.AcceptDraftCandidate(ctx, scope, string(draft.DraftID), "repair-limit-task", request)
	if e != nil {
		t.Fatal("valid original bounded intake", e)
	}
	claimed, e := r.Store.ClaimRegistered(ctx, "candidate-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE"}})
	if e != nil || claimed.TaskID != string(accepted.TaskID) {
		t.Fatal(claimed, e)
	}
	l := claimed.TaskLease
	d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
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
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockDraftOverlongField
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	m, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend())
	if e != nil || m.InputUnits <= 14000 || m.InputUnits > 16000 || m.OutputUnits < 2000 {
		t.Fatal(m.InputUnits, m.OutputUnits, e)
	}
	if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	before := candidateOriginalSnapshot(t, ctx, r, d)
	if _, e = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker"); e == nil {
		t.Fatal("oversize UTF8 original-body dependency bought extra")
	}
	if !bytes.Equal(before, candidateOriginalSnapshot(t, ctx, r, d)) {
		t.Fatal("overflow rewrote or truncated original")
	}
	var approvals, calls, steps int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=t.task_id),t.calls_used,e.steps_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&approvals, &calls, &steps); e != nil || approvals != 0 || calls != 1 || steps != 5 {
		t.Fatal(approvals, calls, steps, e)
	}
	t.Logf("actual original input=%d output=%d; full repair dependency rejected without allocation", m.InputUnits, m.OutputUnits)
}

func TestProductCandidateRepairOrderedEvidenceCleanup(t *testing.T) {
	r, l, original := invalidCandidate(t)
	ctx := runtimeContext(t, r)
	d, e := r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
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
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
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
	m0, e := r.Store.PendingCandidateBody(ctx, original)
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := r.Files.ReadPendingCandidateEvidence(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	defer evidence.Close()
	m1 := evidence.Metadata()
	var draft string
	if e = r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	impact, e := r.Content.Impact(ctx, l.ProductScope, draft, 1, "DELETE", "extra-cleanup-impact")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Content.Command(ctx, l.ProductScope, draft, 1, "DELETE", string(impact.ReportID), "", "", "extra-cleanup-delete"); e != nil {
		t.Fatal(e)
	}
	var upper uint64
	var cutoff time.Time
	if e = r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 24 HOUR) FROM file_staging").Scan(&upper, &cutoff); e != nil {
		t.Fatal(e)
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		page, e := r.Files.CleanupPage(bounded, l.ProductScope, cutoff, 0, upper)
		if e == nil && page.Removed != 2 {
			e = errors.New("expected exact two candidate bodies")
		}
		done <- e
	}()
	select {
	case e := <-done:
		t.Fatal("cleanup bypassed ordered held evidence", e)
	case <-time.After(100 * time.Millisecond):
	}
	for _, m := range []domain.MockResultMetadata{m0, m1} {
		if _, e = os.Stat(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, m.BodyStorage+".part")); e != nil {
			t.Fatal("purged held dependency", e)
		}
	}
	if e = r.Store.ValidateDraftCandidate(bounded, l, "candidate-worker"); e == nil {
		t.Fatal("deleted source allowed new validation")
	}
	evidence.Close()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	for _, m := range []domain.MockResultMetadata{m0, m1} {
		if _, e = os.Stat(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, m.BodyStorage+".part")); !os.IsNotExist(e) {
			t.Fatal("not purged", e)
		}
	}
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
		t.Fatal("deleted dependencies resurrected")
	}
}

func TestProductCandidateInvalidUTF8CannotBecomeRepairText(t *testing.T) {
	r, l, d, a := productSent(t)
	ctx := runtimeContext(t, r)
	r.Worker.Turns.Fault = harness.MockDraftInvalidUTF8
	if e := r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e == nil {
		t.Fatal("invalid UTF8 stored as rewritten text")
	}
	m, e := r.Store.ObserveOriginalReply(ctx, a.OriginalSend())
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256([]byte{0xff, 0xfe, 0x7b, 0x7d})
	if m.BodyAvailable || m.BodyHash != hex.EncodeToString(h[:]) || m.State != "SUCCEEDED" || !m.UsageKnown {
		t.Fatal("lost original provider evidence or normalized bytes", m.State, m.BodyAvailable, m.BodyHash)
	}
	if _, e = r.Files.ReadPendingCandidateResult(ctx, d); e == nil {
		t.Fatal("invented readable UTF8 body")
	}
	if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e == nil {
		t.Fatal("validation without governed bytes")
	}
	if _, e = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker"); e == nil {
		t.Fatal("purchased replacement text")
	}
	var calls, attempts, results, approvals int
	if e = r.Store.DB.QueryRow("SELECT t.calls_used,(SELECT COUNT(*) FROM attempts WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_candidate_results WHERE task_id=t.task_id),(SELECT COUNT(*) FROM harness_candidate_recoveries WHERE task_id=t.task_id) FROM tasks t WHERE task_id=?", l.TaskID).Scan(&calls, &attempts, &results, &approvals); e != nil || calls != 1 || attempts != 1 || results != 0 || approvals != 0 {
		t.Fatal(calls, attempts, results, approvals, e)
	}
}
