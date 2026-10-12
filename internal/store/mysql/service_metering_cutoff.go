package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
)

type meterReleaseProof struct {
	Original                                                   cutoffProof
	Service, Business, OriginalHash, Disposition, Installation string
	Generation                                                 uint64
}

// This proves an already-COMMITTED business and its original still-unsettled
// meter, never absence of a WORLD commit or rollback of an earlier transaction.
func (s *Store) cutoffCommittedMeter(ctx context.Context, tx *sql.Tx, p cutoffProof, id domain.SettlementServiceIdentity, evidence domain.SettlementContinuityEvidence) (HarnessCutoffResult, error) {
	out := HarnessCutoffResult{State: "COMMITTED", ReceiptKey: p.WorldReceipt}
	var service, business, state, status string
	var hash, raw []byte
	e := tx.QueryRowContext(ctx, "SELECT m.service_id,m.business_receipt,m.state,m.original_hash,m.disposition_proof,s.status FROM harness_service_metering m JOIN service_events s ON s.tenant_id=m.tenant_id AND s.product_id=m.product_id AND s.record_id=m.service_id WHERE m.tenant_id=? AND m.product_id=? AND m.task_id=? AND s.task_id=m.task_id AND s.owner_subject_id=?", p.Scope.TenantID, p.Scope.ProductID, p.Task, p.Scope.SubjectID).Scan(&service, &business, &state, &hash, &raw, &status)
	if e != nil {
		return out, e
	}
	if service != turnIdentity(p.Task, "service") || business != p.WorldReceipt {
		return out, ErrConflict
	}
	if state == "SETTLED" && status == "COMMITTED" {
		return out, nil
	}
	if state == "RELEASED" && status == "RELEASED" {
		_, e := checkMeterReleaseTx(ctx, tx, p.Scope.ProductScope, p.Task, service, business, hash, raw)
		return out, e
	}
	if state != "PENDING_VERIFICATION" || status != "PENDING" || p.CheckedAt.Before(p.SettleBy) {
		return out, ErrDenied
	}
	var unknown int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND service_id=? AND usage_state='UNKNOWN' AND input_units IS NULL AND output_units IS NULL", p.Scope.TenantID, p.Scope.ProductID, service).Scan(&unknown); e != nil {
		return out, e
	}
	if unknown == 0 {
		return out, ErrConflict
	}
	unresolved, total, e := originalRiskTotals(ctx, tx, p)
	if e != nil {
		return out, e
	}
	disposition := "ADJUSTED"
	if unresolved {
		disposition = "RISK_TRANSFERRED"
	}
	proof := meterReleaseProof{Original: p, Service: service, Business: business, OriginalHash: hex.EncodeToString(hash), Disposition: disposition, Installation: id.Installation, Generation: id.Generation}
	raw, h, e := harnessPayload(proof)
	if e != nil {
		return out, e
	}
	key := turnIdentity(p.Task, "meter-cutoff")
	kind, world := settlementDimension(p.Scope)
	if _, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,?,?,?,'HARNESS_METER_CUTOFF',?,?,'COMMIT')", key, p.Scope.TenantID, p.Scope.ProductID, p.Scope.SubjectID, kind, world, p.Task, key, h[:]); e != nil {
		return out, e
	}
	if unresolved {
		if e = ensureOriginalRisk(ctx, tx, p); e != nil {
			return out, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE harness_risk_cases SET cost_atoms=?,unresolved=1 WHERE tenant_id=? AND product_id=? AND task_id=?", total, p.Scope.TenantID, p.Scope.ProductID, p.Task); e != nil {
			return out, e
		}
	}
	for _, hold := range p.Holds {
		if hold.Purpose != "PARENT" {
			continue
		}
		if hold.State != "HELD" {
			if hold.Kind == "platform_risk" && hold.State == "RELEASED" && !unresolved {
				continue
			}
			return out, ErrConflict
		}
		target := "RELEASED"
		if hold.Kind == "platform_risk" && unresolved {
			target = "RISK"
		}
		if _, e = tx.ExecContext(ctx, "UPDATE reservations SET status=?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", target, hold.Kind, hold.ScopeKey, hold.AccountID, hold.ReservationID); e != nil {
			return out, e
		}
		if target == "RELEASED" {
			if _, e = tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND held>=?", hold.Amount, hold.Kind, hold.ScopeKey, hold.AccountID, hold.Amount); e != nil {
				return out, e
			}
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,?,?,1)", p.Scope.TenantID, p.Scope.ProductID, turnIdentity(p.Task, "meter-posting", hold.ReservationID), p.Task, p.Scope.SubjectID, hold.Kind, hold.ScopeKey, hold.AccountID, hold.Amount, target); e != nil {
			return out, e
		}
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_service_metering SET state='RELEASED',settlement_receipt=?,disposition_proof=? WHERE tenant_id=? AND product_id=? AND service_id=?", key, raw, p.Scope.TenantID, p.Scope.ProductID, service); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE service_events SET status='RELEASED',revision=revision+1 WHERE tenant_id=? AND product_id=? AND record_id=?", p.Scope.TenantID, p.Scope.ProductID, service); e != nil {
		return out, e
	}
	expected, e := meterDispositionSnapshot(ctx, tx, p, service)
	if e != nil {
		return out, e
	}
	var familyHash [32]byte
	if p.Scope.WorldInstanceID == "" {
		var rehearsal bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_admin_rehearsals WHERE tenant_id=? AND product_id=? AND task_id=?)", p.Scope.TenantID, p.Scope.ProductID, p.Task).Scan(&rehearsal); e != nil {
			return out, e
		}
		if rehearsal {
			family, e := readRehearsalSettlement(ctx, tx, p.Scope.ProductScope, p.Task, id)
			if e != nil {
				return out, e
			}
			_, familyHash, e = harnessPayload(family)
			if e != nil {
				return out, e
			}
		}
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = settlementServiceTx(ctx, tx, id); e != nil {
		return out, e
	}
	if e = checkMeterBusinessTx(ctx, tx, p); e != nil {
		return out, e
	}
	provider, ok := s.ProviderEvidence.(settlementContinuityOwner)
	if !ok {
		return out, ErrDenied
	}
	head, e := provider.CheckSettlementContinuity(evidence, id)
	if e != nil || p.MockHead == nil || head != *p.MockHead {
		return out, ErrDenied
	}
	actual, e := meterDispositionSnapshot(ctx, tx, p, service)
	if e != nil {
		return out, e
	}
	if actual != expected {
		return out, ErrConflict
	}
	if familyHash != ([32]byte{}) {
		family, e := readRehearsalSettlement(ctx, tx, p.Scope.ProductScope, p.Task, id)
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
	if _, e = checkMeterReleaseTx(ctx, tx, p.Scope.ProductScope, p.Task, service, business, hash, raw); e != nil {
		return out, e
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return out, e
	}
	return out, nil
}

func checkMeterReleaseTx(ctx context.Context, q rowQuerier, scope domain.ProductScope, task, service, business string, hash, raw []byte) (string, error) {
	var p meterReleaseProof
	if json.Unmarshal(raw, &p) != nil || p.Original.Scope.ProductScope != scope || p.Original.Task != task || p.Service != service || p.Business != business || p.Original.WorldReceipt != business || p.OriginalHash != hex.EncodeToString(hash) || p.Original.CheckedAt.Before(p.Original.SettleBy) || p.Installation == "" || p.Generation == 0 || p.Original.MockHead == nil || (p.Disposition != "ADJUSTED" && p.Disposition != "RISK_TRANSFERRED") {
		return "", ErrConflict
	}
	_, h, e := harnessPayload(p)
	if e != nil {
		return "", e
	}
	key := turnIdentity(task, "meter-cutoff")
	r, e := queryReceipt(ctx, q, key)
	if e != nil {
		return "", e
	}
	if !settlementReceiptScope(r, p.Original.Scope, task) || r.operation != "HARNESS_METER_CUTOFF" || r.command != key || !bytes.Equal(r.hash, h[:]) {
		return "", ErrConflict
	}
	var valid bool
	if e = q.QueryRowContext(ctx, "SELECT settlement_receipt=? AND state='RELEASED' FROM harness_service_metering WHERE tenant_id=? AND product_id=? AND task_id=? AND service_id=?", key, scope.TenantID, scope.ProductID, task, service).Scan(&valid); e != nil {
		return "", e
	}
	if !valid {
		return "", ErrConflict
	}
	return p.Disposition, nil
}
