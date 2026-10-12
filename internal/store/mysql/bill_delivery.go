package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

type originalBillScanner interface {
	ScanOriginalBills(context.Context, domain.MockHead) (domain.OriginalBillScan, error)
	CheckOriginalBillScan(domain.OriginalBillScan, domain.SettlementServiceIdentity) error
}
type billScanReceipt struct {
	Start, End domain.MockHead
	References []domain.BillScanReference
}

// scanOriginalBills has no financial effect. One bounded page and its exact
// receipt/cursor are committed together; unknown ACK is recovered by this same
// original receipt before the next page can be observed.
func (s *Store) scanOriginalBills(ctx context.Context, service domain.SettlementServiceIdentity) error {
	provider, ok := s.ProviderEvidence.(originalBillScanner)
	if !ok {
		return ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return e
	}
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		tx.Rollback()
		return e
	}
	var from domain.MockHead
	var prior string
	e = tx.QueryRowContext(ctx, "SELECT sequence_number,journal_hash,receipt_key FROM harness_bill_cursor WHERE installation_id=? FOR UPDATE", service.Installation).Scan(&from.Sequence, &from.Hash, &prior)
	if e == nil && from.Sequence > 0 {
		var raw []byte
		var receipt billScanReceipt
		e = tx.QueryRowContext(ctx, "SELECT document FROM harness_bill_scan_receipts WHERE installation_id=? AND receipt_key=?", service.Installation, prior).Scan(&raw)
		if e == nil && (json.Unmarshal(raw, &receipt) != nil || receipt.End != from) {
			e = ErrConflict
		}
	}
	rolled := tx.Rollback()
	if e != nil {
		return e
	}
	if rolled != nil {
		return rolled
	}
	page, e := provider.ScanOriginalBills(ctx, from)
	if e != nil {
		return e
	}
	if page == nil {
		return ErrDenied
	}
	defer page.Close()
	if e = provider.CheckOriginalBillScan(page, service); e != nil {
		return e
	}
	end, refs := page.End(), page.References()
	if page.Start() != from || end.Sequence < from.Sequence || end.Sequence-from.Sequence > 100 || len(refs) > 100 || len(end.Hash) != 64 {
		return ErrConflict
	}
	if end == from {
		return nil
	}
	previous := from.Sequence
	for _, ref := range refs {
		if ref.Position.Sequence <= previous || ref.Position.Sequence > end.Sequence || len(ref.Position.Hash) != 64 || ref.Identity.Validate() != nil || domain.ValidateID(ref.ItemID) != nil || len(ref.Digest) != 64 {
			return ErrConflict
		}
		previous = ref.Position.Sequence
	}
	receipt := billScanReceipt{Start: from, End: end, References: refs}
	raw, hash, e := harnessPayload(receipt)
	if e != nil {
		return e
	}
	key := turnIdentity(service.Installation, "bill-scan", end.Hash)
	tx, e = s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		return e
	}
	var current domain.MockHead
	if e = tx.QueryRowContext(ctx, "SELECT sequence_number,journal_hash FROM harness_bill_cursor WHERE installation_id=? FOR UPDATE", service.Installation).Scan(&current.Sequence, &current.Hash); e != nil {
		return e
	}
	if current != from {
		var stored []byte
		if current == end && tx.QueryRowContext(ctx, "SELECT document FROM harness_bill_scan_receipts WHERE installation_id=? AND receipt_key=?", service.Installation, key).Scan(&stored) == nil {
			var saved billScanReceipt
			if json.Unmarshal(stored, &saved) == nil {
				_, savedHash, _ := harnessPayload(saved)
				if savedHash == hash {
					return nil
				}
			}
		}
		return ErrConflict
	}
	for _, ref := range refs {
		identity, _ := json.Marshal(ref.Identity)
		if _, e = tx.ExecContext(ctx, "INSERT INTO harness_bill_work(installation_id,sequence_number,journal_hash,request_identity,bill_item_id,item_hash,next_run_at) VALUES(?,?,?,?,?,?,UTC_TIMESTAMP(6))", service.Installation, ref.Position.Sequence, ref.Position.Hash, identity, ref.ItemID, ref.Digest); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_bill_scan_receipts VALUES(?,?,?,?)", service.Installation, key, service.Generation, raw); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_bill_cursor SET sequence_number=?,journal_hash=?,receipt_key=? WHERE installation_id=?", end.Sequence, end.Hash, key, service.Installation); e != nil {
		return e
	}
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		return e
	}
	if e = provider.CheckOriginalBillScan(page, service); e != nil {
		return e
	}
	return s.costCommit(ctx, tx)
}

// RunOriginalBills shares one 30 second budget for scan and delivery. An append
// cursor discovers late invoices on old attempts without rescanning task age.
func (s *Store) RunOriginalBills(ctx context.Context) (int, int, error) {
	if s.SettlementAuthority == nil {
		return 0, 0, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx, service, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return 0, 0, e
	}
	defer release()
	if e = s.scanOriginalBills(ctx, service); e != nil {
		return 0, 0, e
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT sequence_number,request_identity,bill_item_id,item_hash FROM harness_bill_work WHERE installation_id=? AND done=0 AND next_run_at<=UTC_TIMESTAMP(6) ORDER BY next_run_at,sequence_number LIMIT 100", service.Installation)
	if e != nil {
		return 0, 0, e
	}
	type work struct {
		sequence     uint64
		id           domain.MockRequestIdentity
		item, digest string
	}
	items := []work{}
	for rows.Next() {
		var w work
		var raw []byte
		if e = rows.Scan(&w.sequence, &raw, &w.item, &w.digest); e != nil {
			break
		}
		if e = json.Unmarshal(raw, &w.id); e != nil {
			break
		}
		items = append(items, w)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return 0, 0, e
	}
	done, failed := 0, 0
	for _, w := range items {
		if e = ctx.Err(); e != nil {
			return done, failed, e
		}
		itemCtx, itemCancel := context.WithTimeout(ctx, 5*time.Second)
		_, recordError := s.RecordOriginalBillItem(itemCtx, w.id, w.item)
		// Even a successful older version is not this observed version's receipt.
		// ACK unknown and conflicts are resolved only by the exact immutable digest.
		complete, e := s.recordBillDelivery(itemCtx, service, w.sequence, w.id, w.item, w.digest, recordError)
		itemCancel()
		if e != nil {
			return done, failed, e
		}
		if complete {
			done++
		} else {
			failed++
		}
	}
	return done, failed, nil
}
func (s *Store) recordBillDelivery(ctx context.Context, service domain.SettlementServiceIdentity, sequence uint64, id domain.MockRequestIdentity, item, digest string, attemptError error) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		return false, e
	}
	var saved, dig string
	var identity []byte
	if e = tx.QueryRowContext(ctx, "SELECT request_identity,bill_item_id,item_hash FROM harness_bill_work WHERE installation_id=? AND sequence_number=? FOR UPDATE", service.Installation, sequence).Scan(&identity, &saved, &dig); e != nil {
		return false, e
	}
	var original domain.MockRequestIdentity
	if json.Unmarshal(identity, &original) != nil || original != id || saved != item || dig != digest {
		return false, ErrConflict
	}
	// These immutable records were written only after the complete owning boundary.
	// A failed/unknown call is never presumed complete merely from its return code.
	var receiptKey string
	e = tx.QueryRowContext(ctx, "SELECT receipt_key FROM harness_bill_items WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND provider='r0-mock' AND provider_account='r0-mock-supplier' AND bill_item_id=? AND item_hash=UNHEX(?) UNION ALL SELECT receipt_key FROM harness_bill_conflicts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND provider='r0-mock' AND provider_account='r0-mock-supplier' AND bill_item_id=? AND conflicting_hash=UNHEX(?) LIMIT 1", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, item, digest, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, item, digest).Scan(&receiptKey)
	complete := e == nil && (attemptError == nil || errors.Is(attemptError, ErrCommitUnknown) || errors.Is(attemptError, ErrConflict))
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return false, e
	}
	if complete {
		receipt, e := queryReceipt(ctx, tx, receiptKey)
		if e != nil || !settlementReceiptScope(receipt, id.Scope, id.TaskID) || receipt.operation != "HARNESS_BILL_ITEM" || hex.EncodeToString(receipt.hash) != digest {
			return false, ErrConflict
		}
		_, e = tx.ExecContext(ctx, "UPDATE harness_bill_work SET done=1,receipt_key=? WHERE installation_id=? AND sequence_number=?", receiptKey, service.Installation, sequence)
		if e != nil {
			return false, e
		}
	} else {
		_, e = tx.ExecContext(ctx, "UPDATE harness_bill_work SET failures=failures+1,next_run_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND) WHERE installation_id=? AND sequence_number=? AND done=0", service.Installation, sequence)
		if e != nil {
			return false, e
		}
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return false, e
	}
	return complete, nil
}
