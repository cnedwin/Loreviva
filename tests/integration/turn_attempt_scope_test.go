package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExactAttemptBodyAndValidationNeverFallBackToOriginal(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	reader, ok := any(r.Files).(interface {
		ReadAttemptEvidence(context.Context, domain.TaskLease, string, string) (store.OriginalResultEvidence, error)
	})
	if !ok {
		t.Fatal("no fixed attempt-scoped governed reader")
	}
	queries, ok := any(r.Store).(interface {
		QueryAttemptValidation(context.Context, domain.Scope, string, string) (store.CommitReceipt, error)
	})
	if !ok {
		t.Fatal("no exact attempt validation receipt query")
	}
	want, _, e := readOriginalResult(ctx, r.Files, lease, worker)
	if e != nil {
		t.Fatal(e)
	}
	evidence, e := reader.ReadAttemptEvidence(ctx, lease, worker, id.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	got, meta := evidence.Bytes(), evidence.Metadata()
	evidence.Close()
	if !bytes.Equal(want, got) || meta.Identity != id {
		t.Fatal("explicit original attempt differs")
	}
	if receipt, e := queries.QueryAttemptValidation(ctx, scope, task, id.AttemptID); e != nil || !receipt.Committed {
		t.Fatal(receipt, e)
	}
	for _, attempt := range []string{"absent-attempt", ""} {
		if body, e := reader.ReadAttemptEvidence(ctx, lease, worker, attempt); e == nil {
			body.Close()
			t.Fatal("missing attempt fell back to original", attempt)
		}
		if _, e := queries.QueryAttemptValidation(ctx, scope, task, attempt); e == nil {
			t.Fatal("missing validation attempt fell back", attempt)
		}
	}
}

func TestRecoveryChecksWholeOriginalCandidateAfterFirstConflict(t *testing.T) {
	for _, fault := range []harness.MockFault{0, harness.MockWrongRelationshipMemory} {
		t.Run(map[harness.MockFault]string{0: "valid-original", harness.MockWrongRelationshipMemory: "invalid-later-memory"}[fault], func(t *testing.T) {
			ctx := context.Background()
			r, scope, task, _ := actionResultFixture(t, fault)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			instance, e := r.World.GetInstance(ctx, scope)
			if e != nil {
				t.Fatal(e)
			}
			br, q := actionTask(t, r, scope.ProductScope, instance, "whole-recovery-conflict")
			if _, e = commitAction(r, br, q); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
				t.Fatal("not an actual first-resource conflict", e)
			}
			checker, ok := any(r.Store).(interface {
				InspectOriginalRecoveryCandidate(context.Context, domain.TaskLease, string) error
			})
			if !ok {
				t.Fatal("no fixed complete original-candidate recovery inspection")
			}
			e = checker.InspectOriginalRecoveryCandidate(ctx, lease, worker)
			if fault == 0 && e != nil {
				t.Fatal("legitimate original frozen-world action rejected", e)
			}
			if fault != 0 && !errors.Is(e, store.ErrDenied) {
				t.Fatal("later invalid memory hidden by first conflict", e)
			}
			var calls, steps, approvals int
			if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,(SELECT COUNT(*) FROM harness_candidate_approvals WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task).Scan(&calls, &steps, &approvals); e != nil || calls != 1 || steps != 7 || approvals != 0 {
				t.Fatal("inspection bought or committed", calls, steps, approvals, e)
			}
		})
	}
}

func TestExactAttemptApprovalAndCommitConsumeOnlyThatResult(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := actionResultFixture(t)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	p, ok := any(r.Store).(interface {
		ApproveAttemptTurn(context.Context, domain.TaskLease, string, string) error
		CommitAttemptTurn(context.Context, domain.TaskLease, string, string) (store.CommitReceipt, error)
	})
	if !ok {
		t.Fatal("no fixed attempt-scoped approval/world commit")
	}
	if e = p.ApproveAttemptTurn(ctx, lease, worker, "absent-attempt"); e == nil {
		t.Fatal("approval selected arbitrary result")
	}
	if _, e = p.CommitAttemptTurn(ctx, lease, worker, "absent-attempt"); e == nil {
		t.Fatal("commit selected arbitrary result")
	}
	if e = p.ApproveAttemptTurn(ctx, lease, worker, id.AttemptID); e != nil {
		t.Fatal(e)
	}
	if receipt, e := p.CommitAttemptTurn(ctx, lease, worker, id.AttemptID); e != nil || !receipt.Committed {
		t.Fatal(receipt, e)
	}
	if _, e = p.CommitAttemptTurn(ctx, lease, worker, "absent-attempt"); e == nil {
		t.Fatal("terminal receipt changed requested attempt")
	}
	if receipt, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil || !receipt.Committed {
		t.Fatal("original parent receipt replay", receipt, e)
	}
	if body, committed, e := r.Files.ReadApprovedTurn(ctx, scope, task); e != nil || len(body) == 0 || committed.Metadata.Identity != id {
		t.Fatal("wrong terminal attempt", committed.Metadata.Identity, e)
	}
}

func TestRealSharedExtraRepairAndRecomputeCommit(t *testing.T) {
	for _, reason := range []string{"STRUCTURE_REPAIR", "STATE_CONFLICT_RECOMPUTE"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			fault := harness.MockFault(0)
			if reason == "STRUCTURE_REPAIR" {
				fault = harness.MockMalformedOriginalResult
			}
			r, scope, task, _ := actionResultFixture(t, fault)
			lease, worker := currentResultLease(t, task, scope, r.Store.DB)
			original, e := r.Store.OriginalMockIdentity(ctx, scope, task)
			if e != nil {
				t.Fatal(e)
			}
			originalBody, originalMeta, e := readOriginalResult(ctx, r.Files, lease, worker)
			if e != nil {
				t.Fatal(e)
			}
			type held struct {
				Kind, Scope, Account, ID, Purpose string
				Amount                            int64
			}
			readHolds := func() map[string]held {
				t.Helper()
				rows, e := r.Store.DB.Query("SELECT kind,scope_key,account_id,reservation_id,purpose,amount FROM reservations WHERE task_id=? ORDER BY kind,scope_key,account_id,reservation_id", task)
				if e != nil {
					t.Fatal(e)
				}
				defer rows.Close()
				out := map[string]held{}
				for rows.Next() {
					var h held
					if e = rows.Scan(&h.Kind, &h.Scope, &h.Account, &h.ID, &h.Purpose, &h.Amount); e != nil {
						t.Fatal(e)
					}
					out[h.ID] = h
				}
				if e = rows.Err(); e != nil {
					t.Fatal(e)
				}
				return out
			}
			beforeHolds := readHolds()
			var beforeBinding, beforeSnapshot []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_context_bindings WHERE task_id=?", task).Scan(&beforeBinding); e != nil {
				t.Fatal(e)
			}
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=? AND attempt_id=?", task, original.AttemptID).Scan(&beforeSnapshot); e != nil {
				t.Fatal(e)
			}
			if reason == "STATE_CONFLICT_RECOMPUTE" {
				if e = r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
					t.Fatal(e)
				}
				instance, e := r.World.GetInstance(ctx, scope)
				if e != nil {
					t.Fatal(e)
				}
				br, q := actionTask(t, r, scope.ProductScope, instance, "real-extra-competing-action")
				if _, e = commitAction(r, br, q); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.CommitOriginalTurn(ctx, lease, worker); !errors.Is(e, store.ErrCandidateResourceConflict) {
					t.Fatal(e)
				}
			}
			x, ok := any(r.Worker.Turns).(interface {
				ExecuteRecovery(context.Context, domain.TaskLease, string) error
			})
			if !ok {
				t.Fatal("no real fixed recovery Generate/commit path")
			}
			r.Worker.Turns.Fault = 0
			if e = x.ExecuteRecovery(ctx, lease, worker); e != nil {
				t.Fatal("genuine approved recovery failed", e)
			}
			var calls, steps, attempts, services, lines, posts, holds int
			var state string
			if e = r.Store.DB.QueryRow("SELECT t.calls_used,e.steps_used,t.state,(SELECT COUNT(*) FROM attempts WHERE task_id=?),(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?),(SELECT COUNT(*) FROM reservations WHERE task_id=?) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", task, task, task, task, task, task).Scan(&calls, &steps, &state, &attempts, &services, &lines, &posts, &holds); e != nil {
				t.Fatal(e)
			}
			if calls != 2 || attempts != 2 || services != 1 || lines != 2 || posts != 8 || holds != 8 || state != "COMMITTED" || steps > 20 {
				t.Fatal("shared extra/service envelope incorrect", calls, steps, state, attempts, services, lines, posts, holds)
			}
			t.Logf("actual %s: calls=%d steps=%d service=%d lines=%d postings=%d holds=%d", reason, calls, steps, services, lines, posts, holds)
			var after []byte
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_context_bindings WHERE task_id=?", task).Scan(&after); e != nil || !bytes.Equal(after, beforeBinding) {
				t.Fatal("original binding changed", e)
			}
			if e = r.Store.DB.QueryRow("SELECT document FROM harness_attempt_contexts WHERE task_id=? AND attempt_id=?", task, original.AttemptID).Scan(&after); e != nil || !bytes.Equal(after, beforeSnapshot) {
				t.Fatal("original snapshot changed", e)
			}
			body, committed, e := r.Files.ReadApprovedTurn(ctx, scope, task)
			if e != nil || len(body) == 0 || committed.Metadata.Identity.AttemptID == original.AttemptID {
				t.Fatal("extra terminal result unavailable", e)
			}
			afterHolds := readHolds()
			supplierPurposes := map[string]map[string]bool{}
			for id, h := range afterHolds {
				if old, ok := beforeHolds[id]; ok && !reflect.DeepEqual(old, h) {
					t.Fatal("original hold identity/amount changed", old, h)
				}
				if h.Amount != 0 {
					t.Fatal("public R0 nonzero hold", h)
				}
				if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
					if h.Account != "r0-mock-supplier" || h.Kind == "supplier_global" && h.Scope != "GLOBAL" || h.Kind == "supplier_tenant" && h.Scope != scope.TenantID {
						t.Fatal("supplier dimension replaced", h)
					}
					if supplierPurposes[h.Purpose] == nil {
						supplierPurposes[h.Purpose] = map[string]bool{}
					}
					supplierPurposes[h.Purpose][h.Kind] = true
				} else if h.Purpose != "PARENT" {
					t.Fatal("parent hold moved to attempt", h)
				}
				var status, postStatus string
				var amount int64
				if e = r.Store.DB.QueryRow("SELECT status FROM reservations WHERE reservation_id=?", id).Scan(&status); e != nil {
					t.Fatal(e)
				}
				if e = r.Store.DB.QueryRow("SELECT amount,status FROM postings WHERE task_id=? AND record_id=?", task, content.Identity(task, "posting", id)).Scan(&amount, &postStatus); e != nil {
					t.Fatal(e)
				}
				want := "SETTLED"
				if h.Kind == "platform_risk" {
					want = "RELEASED"
					if h.Scope != "GLOBAL" || h.Account != "r0-mock-risk" {
						t.Fatal(h)
					}
				}
				if status != want || postStatus != want || amount != 0 {
					t.Fatal("actual per-hold settlement", h, status, postStatus, amount)
				}
			}
			if len(supplierPurposes) != 2 || len(supplierPurposes[original.AttemptID]) != 2 || len(supplierPurposes[committed.Metadata.Identity.AttemptID]) != 2 {
				t.Fatal("supplier attempts not distinct", supplierPurposes)
			}
			for _, m := range []domain.MockResultMetadata{originalMeta, committed.Metadata} {
				var input, output uint64
				var quote, serviceUnit, supplierUnit, currency string
				var serviceAmount, supplierAmount, allocation int64
				if e = r.Store.DB.QueryRow("SELECT l.quote_id,l.input_units,l.output_units,l.service_atoms,l.service_unit,l.supplier_atoms,l.supplier_unit,l.supplier_currency,a.supplier_atoms FROM harness_service_lines l JOIN harness_attempt_allocations a USING(tenant_id,product_id,task_id,attempt_id) WHERE l.task_id=? AND l.attempt_id=?", task, m.Identity.AttemptID).Scan(&quote, &input, &output, &serviceAmount, &serviceUnit, &supplierAmount, &supplierUnit, &currency, &allocation); e != nil {
					t.Fatal(e)
				}
				if quote != m.Identity.QuoteID || input != m.InputUnits || output != m.OutputUnits || input == 0 || output == 0 || serviceAmount != 0 || supplierAmount != 0 || allocation != 0 || serviceUnit != "WE_CREDIT" || supplierUnit != "MONEY" || currency != "syntheticUSD" {
					t.Fatal("actual attempt line not exact", fmt.Sprint(m.Identity.AttemptID, input, output, quote))
				}
			}
			live, e := r.Store.ProviderEvidence.OriginalMetadata(ctx, original)
			if e != nil || live != originalMeta {
				t.Fatal("original J identity/result mutated", e)
			}
			actual, e := os.ReadFile(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, originalMeta.BodyStorage+".part"))
			if e != nil || !bytes.Equal(actual, originalBody) {
				t.Fatal("original0 bytes mutated", e)
			}
			if e = x.ExecuteRecovery(ctx, lease, worker); e != nil {
				t.Fatal("original committed receipt replay", e)
			}
			if e = r.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", task).Scan(&calls); e != nil || calls != 2 {
				t.Fatal("replay purchased", calls, e)
			}
			var raw []byte
			var meta domain.MockResultMetadata
			if e = r.Store.DB.QueryRow("SELECT metadata FROM harness_original_queries WHERE task_id=? AND attempt_id=?", task, original.AttemptID).Scan(&raw); e != nil || json.Unmarshal(raw, &meta) != nil {
				t.Fatal(e)
			}
			if e = os.Remove(filepath.Join(r.Files.RootPath, scope.TenantID, scope.ProductID, meta.BodyStorage+".part")); e != nil {
				t.Fatal(e)
			}
			if body, _, e = r.Files.ReadApprovedTurn(ctx, scope, task); e == nil || len(body) != 0 {
				t.Fatal("extra terminal body lost real original dependency")
			}
		})
	}
}
