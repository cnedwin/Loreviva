package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"math/big"
	"time"
)

type OriginalBillReceipt struct {
	Key      string
	Atoms    int64
	Conflict bool
}
type originalBillProvider interface {
	ReadOriginalBillEvidence(context.Context, domain.MockRequestIdentity, string) (domain.OriginalBillEvidence, error)
	CheckOriginalBillEvidence(domain.OriginalBillEvidence, domain.SettlementServiceIdentity) error
}

func ensureOriginalRisk(ctx context.Context, tx *sql.Tx, proof cutoffProof) error {
	for _, h := range proof.Holds {
		if h.Kind == "platform_risk" {
			_, e := tx.ExecContext(ctx, "INSERT INTO harness_risk_cases(tenant_id,product_id,task_id,risk_case_id,scope_key,account_id,reservation_id,original_cap) VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE risk_case_id=risk_case_id", proof.Scope.TenantID, proof.Scope.ProductID, proof.Task, turnIdentity(proof.Task, "risk"), h.ScopeKey, h.AccountID, h.ReservationID, h.Amount)
			return e
		}
	}
	return ErrConflict
}
func checkOriginalBillRequest(ctx context.Context, tx *sql.Tx, id domain.MockRequestIdentity, service domain.SettlementServiceIdentity) error {
	var raw []byte
	var installation string
	if e := tx.QueryRowContext(ctx, "SELECT request_identity,installation_id FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&raw, &installation); e != nil {
		return e
	}
	var saved domain.MockRequestIdentity
	if json.Unmarshal(raw, &saved) != nil || saved != id || installation != service.Installation {
		return ErrConflict
	}
	receipt, e := queryReceipt(ctx, tx, id.MarkerKey)
	if e != nil {
		return e
	}
	if receipt.task != id.TaskID || receipt.owner != id.Scope.SubjectID || receipt.phase != "SEND" || !settlementReceiptDimension(receipt, id.Scope) || receipt.tenant != id.Scope.TenantID || receipt.product != id.Scope.ProductID {
		return ErrConflict
	}
	// Rebuild only the immutable original dispatch receipt preimage from the
	// already locked attempt and its two actual supplier reservations. A SEND
	// label or matching task alone cannot prove that original purchase.
	d := DispatchRequest{BarrierRequest: BarrierRequest{ProductScope: id.Scope.ProductScope, OwnerSubjectID: id.Scope.SubjectID, TaskID: id.TaskID, Operation: "TURN_DISPATCH", CommandKey: id.MarkerKey, CommitKey: id.MarkerKey, phase: "SEND"}, AttemptID: id.AttemptID, AuthorizationID: id.AuthorizationID, Route: id.RouteID, RequestKey: id.RequestKey, RequestHash: mustArrayHash(id.RequestHash), HarnessQuoteID: id.QuoteID}
	if id.Kind == "CANDIDATE" {
		d.Operation = "CANDIDATE_DISPATCH"
	} else {
		d.WorldScope = &id.Scope
	}
	a, e := readAttempt(ctx, tx, d)
	if e != nil {
		return e
	}
	d.Slot = a.slot
	if a.slot > 1 || !a.matches(d) || !a.started.Valid || !a.marker.Valid || a.marker.String != id.MarkerKey {
		return ErrConflict
	}
	for _, kind := range []string{"supplier_global", "supplier_tenant"} {
		h := Reservation{Kind: kind, ReservationID: turnIdentity(id.AttemptID, "supplier", kind)}
		if e = tx.QueryRowContext(ctx, "SELECT account_id,scope_key,amount FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? AND kind=? AND reservation_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, kind, h.ReservationID).Scan(&h.AccountID, &h.ScopeKey, &h.Amount); e != nil {
			return e
		}
		d.Reservations = append(d.Reservations, h)
	}
	d.PayloadHash, e = dispatchFingerprint(d)
	if e != nil {
		return e
	}
	if !receipt.matches(d.BarrierRequest) {
		return ErrConflict
	}
	return nil
}

// RecordOriginalBillItem consumes one current independent provider item. The
// caller selects only the exact original identity/item, never price or evidence.
func (s *Store) RecordOriginalBillItem(ctx context.Context, id domain.MockRequestIdentity, item string) (OriginalBillReceipt, error) {
	var out OriginalBillReceipt
	provider, ok := s.ProviderEvidence.(originalBillProvider)
	if !ok || s.SettlementAuthority == nil || id.Validate() != nil || domain.ValidateID(item) != nil {
		return out, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, service, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	// First fixed DB boundary ends before any journal lock/file IO.
	pre, closePre, e := s.beginSettlementTx(ctx)
	if e != nil {
		return out, e
	}
	_, _, e = s.lockOriginalBillSettlement(ctx, pre, id, service, nil)
	if e == nil {
		e = checkOriginalBillRequest(ctx, pre, id, service)
	}
	knownHashes := map[string]bool{}
	if e == nil {
		rows, readError := pre.QueryContext(ctx, "SELECT item_hash FROM harness_bill_items WHERE tenant_id=? AND product_id=? AND attempt_id=? AND provider='r0-mock' AND provider_account='r0-mock-supplier' AND bill_item_id=? UNION ALL SELECT conflicting_hash FROM harness_bill_conflicts WHERE tenant_id=? AND product_id=? AND attempt_id=? AND provider='r0-mock' AND provider_account='r0-mock-supplier' AND bill_item_id=?", id.Scope.TenantID, id.Scope.ProductID, id.AttemptID, item, id.Scope.TenantID, id.Scope.ProductID, id.AttemptID, item)
		if readError != nil {
			e = readError
		} else {
			for rows.Next() {
				var hash []byte
				if e = rows.Scan(&hash); e != nil {
					break
				}
				knownHashes[hex.EncodeToString(hash)] = true
			}
			if e == nil {
				e = rows.Err()
			}
			rows.Close()
		}
	}
	rolled := pre.Rollback()
	closePre()
	if e != nil {
		return out, e
	}
	if rolled != nil {
		return out, rolled
	}
	evidence, e := provider.ReadOriginalBillEvidence(ctx, id, item)
	if e != nil {
		return out, e
	}
	if evidence == nil {
		return out, ErrDenied
	}
	defer evidence.Close()
	if e = provider.CheckOriginalBillEvidence(evidence, service); e != nil {
		return out, e
	}
	bill, head := evidence.Item(), evidence.Head()
	// Consume the earliest unrecorded actual provider version, not merely the
	// latest. Queued conflicts must never erase an original unprocessed invoice.
	for _, candidate := range evidence.Items() {
		_, h, e := harnessPayload(candidate)
		if e != nil {
			return out, e
		}
		if !knownHashes[hex.EncodeToString(h[:])] {
			bill = candidate
			break
		}
	}

	if bill.Identity != id || bill.ItemID != item || bill.Provider != "r0-mock" || bill.Account != "r0-mock-supplier" || bill.Unit != "MONEY" || bill.Currency != "syntheticUSD" || bill.Atoms < 0 || head.Sequence == 0 || len(head.Hash) != 64 {
		return out, ErrConflict
	}
	raw, hash, e := harnessPayload(bill)
	if e != nil {
		return out, e
	}
	headRaw, _ := json.Marshal(head)
	out = OriginalBillReceipt{Key: turnIdentity(id.AttemptID, bill.Provider, bill.Account, item, "bill"), Atoms: bill.Atoms}
	conflictKey := turnIdentity(out.Key, hex.EncodeToString(hash[:]), "conflict")
	tx, closeTx, e := s.beginSettlementTx(ctx)
	if e != nil {
		return out, e
	}
	defer closeTx()
	// Journal bytes/head remain locked through this second current DB boundary and
	// Commit attempt. No filesystem operation occurs while these SQL locks exist.
	extra := []LockKey{{Rank: 110, Kind: "commit_receipts", IDs: []string{out.Key}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{conflictKey}}}
	proof, state, e := s.lockOriginalBillSettlement(ctx, tx, id, service, extra)
	if e != nil {
		return out, e
	}
	if e = checkOriginalBillRequest(ctx, tx, id, service); e != nil {
		return out, e
	}
	if e = provider.CheckOriginalBillEvidence(evidence, service); e != nil {
		return out, e
	}
	if state != "CUTOFF" && state != "ACCOUNTING_ONLY" && state != "COMMITTED" {
		return out, ErrDenied
	}
	t, p, task := id.Scope.TenantID, id.Scope.ProductID, id.TaskID
	var originalHash []byte
	e = tx.QueryRowContext(ctx, "SELECT item_hash FROM harness_bill_items WHERE tenant_id=? AND product_id=? AND attempt_id=? AND provider=? AND provider_account=? AND bill_item_id=?", t, p, id.AttemptID, bill.Provider, bill.Account, item).Scan(&originalHash)
	if e == nil && bytes.Equal(originalHash, hash[:]) {
		receipt, e := queryReceipt(ctx, tx, out.Key)
		if e != nil || receipt.task != task || receipt.operation != "HARNESS_BILL_ITEM" {
			return out, ErrConflict
		}
		return out, nil
	}
	conflict := e == nil
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if conflict {
		out.Key = conflictKey
		out.Conflict = true
		if _, e = queryReceipt(ctx, tx, out.Key); e == nil {
			return out, ErrConflict
		} else if !errors.Is(e, sql.ErrNoRows) {
			return out, e
		}
	}
	// The live R0 quote is zero. Never reinterpret this as an externally supplied
	// nonzero tariff. Unexpected positive provider cost is full platform debt.
	var zero bool
	if e = tx.QueryRowContext(ctx, "SELECT document->'$.ServiceAtoms'=0 AND document->'$.SupplierAtoms'=0 FROM harness_quotes WHERE tenant_id=? AND product_id=? AND quote_id=?", t, p, id.QuoteID).Scan(&zero); e != nil {
		return out, e
	}
	if !zero {
		return out, ErrDenied
	}
	kind, world := settlementDimension(id.Scope)
	if _, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,?,?,?,'HARNESS_BILL_ITEM',?,?,'COMMIT')", out.Key, t, p, id.Scope.SubjectID, kind, world, task, out.Key, hash[:]); e != nil {
		return out, e
	}
	if conflict {
		_, e = tx.ExecContext(ctx, "INSERT INTO harness_bill_conflicts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", t, p, task, id.AttemptID, bill.Provider, bill.Account, item, originalHash, hash[:], raw, headRaw, out.Key)
		if e != nil {
			return out, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE harness_supplier_liabilities SET disputed=1,final_known=0 WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", t, p, task, id.AttemptID); e != nil {
			return out, e
		}
	} else {
		if _, e = tx.ExecContext(ctx, "INSERT INTO harness_bill_items VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", t, p, task, id.AttemptID, bill.Provider, bill.Account, item, hash[:], bill.Atoms, raw, headRaw, out.Key); e != nil {
			return out, e
		}
		// Exact integer aggregation may exceed int64 although each Scale8 item is
		// checked int64. DECIMAL(65,0) preserves full liability, never caps/truncates.
		var previous string
		var known, disputed bool
		e = tx.QueryRowContext(ctx, "SELECT CAST(total_atoms AS CHAR),final_known,disputed FROM harness_supplier_liabilities WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", t, p, task, id.AttemptID).Scan(&previous, &known, &disputed)
		if errors.Is(e, sql.ErrNoRows) {
			previous = "0"
		} else if e != nil {
			return out, e
		}
		total, ok := new(big.Int).SetString(previous, 10)
		if !ok {
			return out, ErrConflict
		}
		total.Add(total, big.NewInt(bill.Atoms))
		if len(total.String()) > 65 {
			return out, ErrConflict
		}
		final := bill.Final && bill.Atoms == 0 && total.Sign() == 0 && !disputed
		if _, e = tx.ExecContext(ctx, "INSERT INTO harness_supplier_liabilities(tenant_id,product_id,task_id,attempt_id,total_atoms,debt_atoms,final_known) VALUES(?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE total_atoms=VALUES(total_atoms),debt_atoms=VALUES(debt_atoms),final_known=VALUES(final_known)", t, p, task, id.AttemptID, total.String(), total.String(), final); e != nil {
			return out, e
		}
	}
	if e = ensureOriginalRisk(ctx, tx, proof); e != nil {
		return out, e
	}
	if bill.Atoms > 0 || conflict {
		reason := "LATE_OVER_CAP"
		if conflict {
			reason = "BILL_CONFLICT"
		}
		for _, h := range proof.Holds {
			if h.Purpose == id.AttemptID && (h.Kind == "supplier_global" || h.Kind == "supplier_tenant") {
				if _, e = tx.ExecContext(ctx, "INSERT INTO harness_account_blocks VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE reason=VALUES(reason)", h.Kind, h.ScopeKey, h.AccountID, reason); e != nil {
					return out, e
				}
			}
		}
	} else if bill.Final {
		var finalKnown bool
		if e = tx.QueryRowContext(ctx, "SELECT final_known AND disputed=0 FROM harness_supplier_liabilities WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", t, p, task, id.AttemptID).Scan(&finalKnown); e != nil {
			return out, e
		}
		if finalKnown {
			for _, h := range proof.Holds {
				if h.Purpose == id.AttemptID && (h.Kind == "supplier_global" || h.Kind == "supplier_tenant") && h.State == "HELD" {
					if _, e = tx.ExecContext(ctx, "UPDATE reservations SET status='SETTLED',revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
						return out, e
					}
					if _, e = tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND held>=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID, h.Amount); e != nil {
						return out, e
					}
				}
			}
		}
	}
	unresolved, total, e := originalRiskTotals(ctx, tx, proof)
	if e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_risk_cases SET cost_atoms=?,unresolved=? WHERE tenant_id=? AND product_id=? AND task_id=?", total, unresolved, t, p, proof.Task); e != nil {
		return out, e
	}
	if !unresolved {
		for _, h := range proof.Holds {
			if h.Kind == "platform_risk" && (h.State == "RISK" || state == "COMMITTED" && h.State == "HELD") {
				if _, e = tx.ExecContext(ctx, "UPDATE reservations SET status='RELEASED',revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
					return out, e
				}
				if _, e = tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND held>=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID, h.Amount); e != nil {
					return out, e
				}
			}
		}
	}
	// Freeze the legitimate post-accounting family view while every original
	// lock is still held. Finisher reads do not acquire a new lock plan.
	var familyHash [32]byte
	if proof.Task != id.TaskID {
		family, e := readRehearsalSettlement(ctx, tx, id.Scope.ProductScope, proof.Task, service)
		if e != nil {
			return out, e
		}
		_, familyHash, e = harnessPayload(family)
		if e != nil {
			return out, e
		}
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		return out, e
	}
	if e = checkOriginalBillRequest(ctx, tx, id, service); e != nil {
		return out, e
	}
	if e = provider.CheckOriginalBillEvidence(evidence, service); e != nil {
		return out, e
	}
	if proof.Task != id.TaskID {
		family, e := readRehearsalSettlement(ctx, tx, id.Scope.ProductScope, proof.Task, service)
		if e != nil {
			return out, e
		}
		_, current, e := harnessPayload(family)
		if e != nil {
			return out, e
		}
		if current != familyHash {
			return out, ErrConflict
		}
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return out, e
	}
	if conflict {
		return out, ErrConflict
	}
	return out, nil
}
func checkSupplierDebt(ctx context.Context, tx *sql.Tx, holds []Reservation) error {
	for _, h := range holds {
		if h.Kind != "supplier_global" && h.Kind != "supplier_tenant" {
			continue
		}
		var blocked bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_account_blocks WHERE kind=? AND scope_key=? AND account_id=?)", h.Kind, h.ScopeKey, h.AccountID).Scan(&blocked); e != nil {
			return e
		}
		if blocked {
			return ErrDenied
		}
	}
	return nil
}
