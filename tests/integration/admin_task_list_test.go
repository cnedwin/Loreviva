package integration

import (
	"context"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
)

func TestAdminTaskListWorldAccountingIndependentAxes(t *testing.T) {
	for _, mode := range []string{"KNOWN", "USAGE_UNKNOWN", "COST_NONFINAL", "LATE_CONTRADICTION"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, s, turns, qr, req := actualTurnFixture(t, true)
			quote, e := turns.QuoteTurn(ctx, s, qr, "axes-quote")
			if e != nil {
				t.Fatal(e)
			}
			req.QuoteID = quote.QuoteID
			a, e := turns.SubmitTurn(ctx, s, req, "axes-turn")
			if e != nil {
				t.Fatal(e)
			}
			if mode == "USAGE_UNKNOWN" {
				r.Worker.Turns.Fault = harness.MockUsagePending
			}
			if mode == "COST_NONFINAL" {
				r.Worker.Turns.Fault = harness.MockCostFinalityPending
			}
			if e = r.Worker.Once(ctx); e != nil {
				t.Fatal(e)
			}
			read := func() store.AdminTaskMetadata {
				t.Helper()
				p, e := r.Store.ListAdminTaskMetadata(ctx, s.ProductScope, store.AdminTaskSelection{ExactID: a.TurnID, Limit: 1})
				if e != nil || len(p.Items) != 1 {
					t.Fatal(p, e)
				}
				return p.Items[0]
			}
			item := read()
			want := store.AdminTaskAccounting{Service: "SETTLED", Supplier: "RECONCILED", Risk: "RESOLVED"}
			if mode == "USAGE_UNKNOWN" {
				want = store.AdminTaskAccounting{Service: "PENDING_VERIFICATION", Supplier: "PENDING_VERIFICATION", Risk: "RESERVED"}
			}
			if mode == "COST_NONFINAL" {
				want = store.AdminTaskAccounting{Service: "SETTLED", Supplier: "PENDING_VERIFICATION", Risk: "RESERVED"}
			}
			if item.State != "COMMITTED" || item.Accounting != want {
				t.Fatal(item, want)
			}
			if mode == "USAGE_UNKNOWN" {
				expireCutoff(t, r.Store.DB, a.TurnID)
				if _, e = r.Store.CutoffHarnessTask(ctx, s, a.TurnID); e != nil {
					t.Fatal(e)
				}
				item = read()
				if item.State != "COMMITTED" || item.Accounting.Service != "RISK_TRANSFERRED" || item.Accounting.Supplier != "PENDING_VERIFICATION" {
					t.Fatal("cutoff rewrote established business/unknown units", item)
				}
			}
			if mode == "LATE_CONTRADICTION" {
				id, e := r.Store.OriginalMockIdentity(ctx, s, a.TurnID)
				if e != nil {
					t.Fatal(e)
				}
				r.Worker.Turns.Fault = harness.MockLateTwoBillItems
				if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Store.RecordOriginalBillItem(ctx, id, "late-a"); e != nil {
					t.Fatal(e)
				}
				item = read()
				if item.State != "COMMITTED" || item.Accounting.Service != "SETTLED" || item.Accounting.Supplier != "PENDING_VERIFICATION" || item.Accounting.Risk != "DEBT_BLOCKED" {
					t.Fatal("late nonfinal obligation hidden by old no-further-cost", item)
				}
			}
		})
	}
}

func TestAdminTaskListHiddenPredecessorsAndBadPageProgress(t *testing.T) {
	ctx := context.Background()
	r, draft, req := candidateHTTPFixture(t)
	s := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	accepted, e := r.Store.AcceptDraftCandidate(ctx, s, draft, "visible-after-hidden", req)
	if e != nil {
		t.Fatal(e)
	}
	seed := func(id, kind, op string) {
		t.Helper()
		_, e = r.Store.DB.Exec("INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,next_run_at,task_kind) VALUES('demo-local','coffee',?,'creator','PRODUCT','ACCEPTED',UTC_TIMESTAMP(6),?)", id, kind)
		if e != nil {
			t.Fatal(e)
		}
		_, e = r.Store.DB.Exec("INSERT INTO registered_jobs VALUES('demo-local','coffee',?,?)", id, op)
		if e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 201; i++ {
		seed(fmt.Sprintf("000000-hidden-%03d", i), "GOVERNANCE", "MEMORY_GOVERNANCE")
	}
	p, e := r.Store.ListAdminTaskMetadata(ctx, s, store.AdminTaskSelection{Limit: 1})
	if e != nil || len(p.Items) != 1 || p.Items[0].ID != string(accepted.TaskID) || p.More {
		t.Fatal("hidden rows starved current own item", p, e)
	}
	// Malformed purported Candidate rows are not invented accepted-work proofs.
	seed("000000-bad-1", "ADMIN", "CANDIDATE")
	seed("000000-bad-2", "ADMIN", "CANDIDATE")
	p, e = r.Store.ListAdminTaskMetadata(ctx, s, store.AdminTaskSelection{Limit: 2})
	if e != nil || len(p.Items) != 0 || !p.More || p.Last != "000000-bad-2" {
		t.Fatal("bad page did not advance bounded checked keys", p, e)
	}
	p, e = r.Store.ListAdminTaskMetadata(ctx, s, store.AdminTaskSelection{Limit: 2, After: p.Last})
	if e != nil || len(p.Items) != 1 || p.Items[0].ID != string(accepted.TaskID) || p.More {
		t.Fatal("next empty-page continuation lost valid item", p, e)
	}
	if _, e = r.Store.DB.Exec("ALTER TABLE registered_jobs RENAME COLUMN operation TO unavailable_operation"); e != nil {
		t.Fatal(e)
	}
	defer r.Store.DB.Exec("ALTER TABLE registered_jobs RENAME COLUMN unavailable_operation TO operation")
	if _, e = r.Store.ListAdminTaskMetadata(ctx, s, store.AdminTaskSelection{Limit: 2}); e == nil {
		t.Fatal("SQL infrastructure failure treated as hidden item")
	}
}

func TestAdminTaskListRealWorldIntakeMetadata(t *testing.T) {
	ctx := context.Background()
	r, s, turns, q, req := actualTurnFixture(t, true)
	quote, e := turns.QuoteTurn(ctx, s, q, "list-world-quote")
	if e != nil {
		t.Fatal(e)
	}
	req.QuoteID = quote.QuoteID
	a, e := turns.SubmitTurn(ctx, s, req, "list-world-turn")
	if e != nil {
		t.Fatal(e)
	}
	p, e := r.Store.ListAdminTaskMetadata(ctx, s.ProductScope, store.AdminTaskSelection{ExactID: a.TurnID, Limit: 50})
	if e != nil || len(p.Items) != 1 {
		t.Fatal("actual WORLD absent", p, e)
	}
	item := p.Items[0]
	if item.World != s.WorldInstanceID || item.Kind != "TURN" || item.State != "ACCEPTED" || item.Accounting.Service != "RESERVED" || item.Accounting.Supplier != "NOT_APPLICABLE" || item.Accounting.Risk != "RESERVED" {
		t.Fatal(item)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_turns SET command_hash=REPEAT(CHAR(1),32) WHERE task_id=?", a.TurnID); e != nil {
		t.Fatal(e)
	}
	p, e = r.Store.ListAdminTaskMetadata(ctx, s.ProductScope, store.AdminTaskSelection{ExactID: a.TurnID, Limit: 50})
	if e != nil || len(p.Items) != 0 {
		t.Fatal("wrong original acceptance hash displayed", p, e)
	}
}

func TestAdminTaskListOwnMetadataWithoutBodyOrProcurement(t *testing.T) {
	ctx := context.Background()
	r, draft, request := candidateHTTPFixture(t)
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	receipt, e := r.Store.AcceptDraftCandidate(ctx, scope, draft, "listed-candidate", request)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action IN ('content.read','asset.read','content.assist')"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE subject_id='creator'"); e != nil {
		t.Fatal(e)
	}
	p, e := r.Store.ListAdminTaskMetadata(ctx, scope, store.AdminTaskSelection{ExactID: string(receipt.TaskID), Limit: 50})
	if e != nil || len(p.Items) != 1 || p.Items[0].ID != string(receipt.TaskID) || p.Items[0].Kind != "CANDIDATE" || p.Items[0].State != "ACCEPTED" {
		t.Fatal("own metadata blocked by unrelated procurement/body", p, e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='task.read'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ListAdminTaskMetadata(ctx, scope, store.AdminTaskSelection{Limit: 50}); e == nil {
		t.Fatal("task.read revoked")
	}
}
