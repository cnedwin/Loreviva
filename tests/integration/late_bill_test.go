package integration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"testing"
	"time"
)

func TestOriginalZeroBillAfterCutoffSettlesSupplierWithoutCustomerRecoup(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	receipt, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	if e != nil {
		t.Fatal("actual original late bill unavailable", e)
	}
	again, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	if e != nil || again != receipt {
		t.Fatal(again, e)
	}
	var supplier, customer, service int
	if e = r.Store.DB.QueryRow("SELECT SUM(kind IN ('supplier_global','supplier_tenant') AND status='SETTLED'),SUM(kind IN ('customer','project','world') AND status='RELEASED'),(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM reservations WHERE task_id=?", task, task).Scan(&supplier, &customer, &service); e != nil || supplier != 2 || customer != 3 || service != 0 {
		t.Fatal(supplier, customer, service, e)
	}
}

func TestLateDistinctBillsPreserveFullDebtAndBlockBothSendBoundaries(t *testing.T) {
	ctx := context.Background()
	r, scope, turns, qr, request := actualTurnFixture(t, true)
	submit := func(key string) string {
		q, e := turns.QuoteTurn(ctx, scope, qr, key+"-quote")
		if e != nil {
			t.Fatal(e)
		}
		request.QuoteID = q.QuoteID
		a, e := turns.SubmitTurn(ctx, scope, request, key)
		if e != nil {
			t.Fatal(e)
		}
		return a.TurnID
	}
	task := submit("late-original")
	if e := runOriginalPrefix(ctx, r); e != nil {
		t.Fatal(e)
	}
	// Prepare two real other parents before any late obligation. One reaches
	// AUTHORIZE and must later fail StartSend; the other must fail AUTHORIZE.
	prepared := []store.DispatchRequest{}
	ids := []string{submit("late-unsent"), submit("late-unauthorized")}
	for _, id := range ids {
		lease, e := r.Store.ClaimRegistered(ctx, "late-test", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN"}})
		if e != nil || lease.TaskID != id {
			t.Fatal(lease, e)
		}
		d, e := turns.PrepareOriginalTurn(ctx, lease.TaskLease, "late-test")
		if e != nil {
			t.Fatal(e)
		}
		prepared = append(prepared, d)
	}
	if _, e := r.Store.AuthorizeDispatch(ctx, prepared[0]); e != nil {
		t.Fatal(e)
	}
	original, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalBillItem(ctx, original, "original-charge"); e != nil {
		t.Fatal(e)
	}
	old := journalBytes(t, r)
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, original); e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(journalBytes(t, r), old) {
		t.Fatal("late items rewrote original journal")
	}
	for _, item := range []string{"late-a", "late-b", "late-a"} {
		if _, e = r.Store.RecordOriginalBillItem(ctx, original, item); e != nil {
			t.Fatal(item, e)
		}
	}
	var amount, debt, risk string
	var items, blocks, customer, service int
	if e = r.Store.DB.QueryRow("SELECT CAST(l.total_atoms AS CHAR),CAST(l.debt_atoms AS CHAR),CAST(r.cost_atoms AS CHAR),(SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?),(SELECT COUNT(*) FROM harness_account_blocks),(SELECT COUNT(*) FROM reservations WHERE task_id=? AND kind IN ('customer','project','world') AND status='RELEASED'),(SELECT COUNT(*) FROM service_events WHERE task_id=?) FROM harness_supplier_liabilities l JOIN harness_risk_cases r USING(tenant_id,product_id,task_id) WHERE l.task_id=?", task, task, task, task).Scan(&amount, &debt, &risk, &items, &blocks, &customer, &service); e != nil || amount != "3" || debt != "3" || risk != "3" || items != 3 || blocks != 2 || customer != 3 || service != 0 {
		t.Fatal(amount, debt, risk, items, blocks, customer, service, e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, prepared[1]); e == nil {
		t.Fatal("new authorization ignored global/tenant debt")
	}
	d := prepared[0]
	d.CommandKey = content.Identity(d.TaskID, "send")
	d.CommitKey = d.CommandKey
	if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
		t.Fatal("already authorized new send ignored debt")
	}
	var negative, global, tenant int
	if e = r.Store.DB.QueryRow("SELECT SUM(balance<held),SUM(kind='supplier_global' AND scope_key='GLOBAL' AND tenant_id IS NULL),SUM(kind='supplier_tenant' AND tenant_id=?) FROM budget_accounts", scope.TenantID).Scan(&negative, &global, &tenant); e != nil || negative != 0 || global != 1 || tenant != 1 {
		t.Fatal(negative, global, tenant, e)
	}
	r.Worker.Turns.Fault = harness.MockLateConflictingBillItem
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, original); e != nil {
		t.Fatal(e)
	}
	receipt, e := r.Store.RecordOriginalBillItem(ctx, original, "late-a")
	if !errors.Is(e, store.ErrConflict) || !receipt.Conflict {
		t.Fatal(receipt, e)
	}
	again, e := r.Store.RecordOriginalBillItem(ctx, original, "late-a")
	if !errors.Is(e, store.ErrConflict) || again != receipt {
		t.Fatal(again, e)
	}
	var conflict int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_conflicts WHERE task_id=?", task).Scan(&conflict); e != nil || conflict != 1 {
		t.Fatal(conflict, e)
	}
	if e = r.Store.DB.QueryRow("SELECT CAST(total_atoms AS CHAR) FROM harness_supplier_liabilities WHERE task_id=?", task).Scan(&amount); e != nil || amount != "3" {
		t.Fatal("conflicting same item was silently billed twice", amount, e)
	}
}
func TestBillCommitAckUnknownReplaysOriginalReceipt(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	r.Store.Commit = func(_ context.Context, tx *sql.Tx) error {
		if e := tx.Commit(); e != nil {
			return e
		}
		return errors.New("synthetic bill ACK lost")
	}
	receipt, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	r.Store.Commit = nil
	if !errors.Is(e, store.ErrCommitUnknown) {
		t.Fatal(e)
	}
	again, e := r.Store.RecordOriginalBillItem(ctx, id, "original-charge")
	if e != nil || again != receipt {
		t.Fatal(again, receipt, e)
	}
	if _, e = r.Store.RecordOriginalBillItem(ctx, id, "nonexistent-item"); e == nil {
		t.Fatal("missing journal item fabricated zero invoice")
	}
}

func TestQueuedConflictingInvoiceCannotHideOriginalItem(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockLateConflictingBillItem
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	first, e := r.Store.RecordOriginalBillItem(ctx, id, "late-a")
	if e != nil || first.Atoms != 1 {
		t.Fatal("later same-item evidence hid the original invoice", first, e)
	}
	second, e := r.Store.RecordOriginalBillItem(ctx, id, "late-a")
	if !errors.Is(e, store.ErrConflict) || second.Atoms != 4 || !second.Conflict {
		t.Fatal(second, e)
	}
}

type disableSettlementAfterBillRead struct {
	*harness.MockJournal
	db *sql.DB
}

func (p disableSettlementAfterBillRead) ReadOriginalBillEvidence(ctx context.Context, id domain.MockRequestIdentity, item string) (domain.OriginalBillEvidence, error) {
	evidence, e := p.MockJournal.ReadOriginalBillEvidence(ctx, id, item)
	if e != nil {
		return nil, e
	}
	if _, e = p.db.ExecContext(ctx, "UPDATE harness_settlement_services SET enabled=0"); e != nil {
		evidence.Close()
		return nil, e
	}
	return evidence, nil
}
func TestBillCurrentAuthorityRepeatsAfterIndependentJournalRead(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, true)
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	expireCutoff(t, r.Store.DB, task)
	if _, e = r.Store.CutoffHarnessTask(ctx, scope, task); e != nil {
		t.Fatal(e)
	}
	r.Store.ProviderEvidence = disableSettlementAfterBillRead{r.Local.Mock, r.Store.DB}
	if _, e = r.Store.RecordOriginalBillItem(ctx, id, "original-charge"); e == nil {
		t.Fatal("withdrawal after journal IO was ignored")
	}
	r.Store.ProviderEvidence = r.Local.Mock
	var n int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_bill_items WHERE task_id=?", task).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_settlement_services SET enabled=1"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET enabled=0 WHERE subject_id=?", scope.SubjectID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalBillItem(ctx, id, "original-charge"); e != nil {
		t.Fatal("owner withdrawal blocked independently authorized original accounting", e)
	}
}

func TestLateBillAfterWorldCommitDoesNotSettleParentServiceAgain(t *testing.T) {
	ctx := context.Background()
	r, scope, task, _ := originalResultFixture(t, false)
	lease, worker := currentResultLease(t, task, scope, r.Store.DB)
	if e := r.Store.ApproveOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Store.CommitOriginalTurn(ctx, lease, worker); e != nil {
		t.Fatal(e)
	}
	id, e := r.Store.OriginalMockIdentity(ctx, scope, task)
	if e != nil {
		t.Fatal(e)
	}
	before := journalBytes(t, r)
	r.Worker.Turns.Fault = harness.MockLateTwoBillItems
	if e = r.Worker.Turns.DeliverLateMockBills(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RecordOriginalBillItem(ctx, id, "late-a"); e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(journalBytes(t, r), before) {
		t.Fatal("contradictory late cost rewrote known zero/no-further-cost history")
	}
	var services, lines, postings, cutoffs int
	var state, cost string
	var unresolved bool
	if e = r.Store.DB.QueryRow("SELECT e.state,(SELECT COUNT(*) FROM service_events WHERE task_id=?),(SELECT COUNT(*) FROM harness_service_lines WHERE task_id=?),(SELECT COUNT(*) FROM postings WHERE task_id=?),(SELECT COUNT(*) FROM harness_cutoffs WHERE task_id=?),CAST(r.cost_atoms AS CHAR),r.unresolved FROM harness_envelopes e JOIN harness_risk_cases r USING(tenant_id,product_id,task_id) WHERE e.task_id=?", task, task, task, task, task).Scan(&state, &services, &lines, &postings, &cutoffs, &cost, &unresolved); e != nil || state != "COMMITTED" || services != 1 || lines != 1 || postings != 6 || cutoffs != 0 || cost != "1" || !unresolved {
		t.Fatal(state, services, lines, postings, cutoffs, cost, unresolved, e)
	}
}
