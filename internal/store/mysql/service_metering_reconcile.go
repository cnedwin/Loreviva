package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

type originalUsageProvider interface {
	ReadOriginalUsageEvidence(context.Context, domain.MockRequestIdentity) (domain.OriginalUsageEvidence, error)
	CheckOriginalUsageEvidence(domain.OriginalUsageEvidence, domain.SettlementServiceIdentity) error
}

// One original measurement, with no caller amounts, target account or new
// narrative authority. Independent provider IO finishes before SQL locking.
func (s *Store) ReconcileOriginalServiceMetering(ctx context.Context, id domain.MockRequestIdentity) (string, error) {
	key := turnIdentity(id.AttemptID, "original-usage")
	provider, ok := s.ProviderEvidence.(originalUsageProvider)
	if !ok || s.SettlementAuthority == nil || id.Validate() != nil {
		return key, ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, service, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return key, e
	}
	defer release()
	evidence, e := provider.ReadOriginalUsageEvidence(ctx, id)
	if e != nil {
		return key, e
	}
	defer evidence.Close()
	if e = provider.CheckOriginalUsageEvidence(evidence, service); e != nil {
		return key, e
	}
	m := evidence.Measurement()
	if m.Identity != id || m.Provider != "r0-mock" || m.Account != "r0-mock-supplier" || m.InputUnits == 0 || m.InputUnits > 16000 || m.OutputUnits == 0 || m.OutputUnits > 4000 {
		return key, ErrDenied
	}
	_, hash, e := harnessPayload(m)
	if e != nil {
		return key, e
	}
	tx, closeTx, e := s.beginSettlementTx(ctx)
	if e != nil {
		return key, e
	}
	defer closeTx()
	proof, state, e := s.lockOriginalBillSettlement(ctx, tx, id, service, []LockKey{{Rank: 110, Kind: "commit_receipts", IDs: []string{key}}})
	if e != nil {
		return key, e
	}
	if e = checkOriginalBillRequest(ctx, tx, id, service); e != nil {
		return key, e
	}
	if state != "COMMITTED" && state != "ACCOUNTING_ONLY" {
		return key, ErrDenied
	}
	var serviceID, business, meterState string
	if e = tx.QueryRowContext(ctx, "SELECT service_id,business_receipt,state FROM harness_service_metering WHERE tenant_id=? AND product_id=? AND task_id=?", id.Scope.TenantID, id.Scope.ProductID, proof.Task).Scan(&serviceID, &business, &meterState); e != nil {
		return key, e
	}
	if serviceID != turnIdentity(proof.Task, "service") || business != proof.WorldReceipt {
		return key, ErrConflict
	}
	if meterState == "RELEASED" {
		var raw, hash []byte
		if e = tx.QueryRowContext(ctx, "SELECT original_hash,disposition_proof FROM harness_service_metering WHERE tenant_id=? AND product_id=? AND service_id=?", id.Scope.TenantID, id.Scope.ProductID, serviceID).Scan(&hash, &raw); e != nil {
			return key, e
		}
		if _, e = checkMeterReleaseTx(ctx, tx, proof.Scope.ProductScope, proof.Task, serviceID, business, hash, raw); e != nil {
			return key, e
		}
	}
	original, e := queryReceipt(ctx, tx, business)
	if e != nil {
		return key, e
	}
	if !settlementReceiptScope(original, proof.Scope, proof.Task) || (original.operation != "HARNESS_TURN_COMMIT" && original.operation != "CANDIDATE_COMMIT" && original.operation != "REHEARSAL_REPORT_COMMIT") {
		return key, ErrConflict
	}
	if saved, e := queryReceipt(ctx, tx, key); e == nil {
		if e = checkUsageReceipt(saved, m, key); e != nil {
			return key, e
		}
		return key, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return key, e
	}
	// The later deadline disposition is a separate owning path. This branch may
	// settle only while the exact original customer reservations remain live.
	if meterState != "RELEASED" && (meterState != "PENDING_VERIFICATION" || !proof.CheckedAt.Before(proof.SettleBy)) {
		return key, ErrDenied
	}
	var originalRaw, originalHash []byte
	var usage string
	if e = tx.QueryRowContext(ctx, "SELECT original_metadata,original_hash,usage_state FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND service_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, serviceID).Scan(&originalRaw, &originalHash, &usage); e != nil {
		return key, e
	}
	originalMeta, e := decodeOriginalMeter(originalRaw, originalHash)
	if e != nil || originalMeta.Identity != id || originalMeta.UsageKnown || usage != "UNKNOWN" {
		return key, ErrConflict
	}
	var zero bool
	if e = tx.QueryRowContext(ctx, "SELECT document->'$.ServiceAtoms'=0 AND document->'$.SupplierAtoms'=0 FROM harness_quotes WHERE tenant_id=? AND product_id=? AND quote_id=?", id.Scope.TenantID, id.Scope.ProductID, id.QuoteID).Scan(&zero); e != nil {
		return key, e
	}
	if !zero {
		return key, ErrDenied
	}
	kind, world := settlementDimension(id.Scope)
	if _, e = tx.ExecContext(ctx, "INSERT INTO commit_receipts(commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase) VALUES(?,?,?,?,?,?,?,'HARNESS_ORIGINAL_USAGE',?,?,'COMMIT')", key, id.Scope.TenantID, id.Scope.ProductID, id.Scope.SubjectID, kind, world, id.TaskID, key, hash[:]); e != nil {
		return key, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_attempt_metering SET usage_state='AUTHORITATIVE',input_units=?,output_units=?,evidence_receipt=? WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND usage_state='UNKNOWN'", m.InputUnits, m.OutputUnits, key, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID); e != nil {
		return key, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_service_lines VALUES(?,?,?,?,?,?,?,?,0,'WE_CREDIT',0,'MONEY','syntheticUSD')", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, serviceID, id.AttemptID, id.QuoteID, m.InputUnits, m.OutputUnits); e != nil {
		return key, e
	}
	var unknown int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND service_id=? AND usage_state='UNKNOWN'", id.Scope.TenantID, id.Scope.ProductID, serviceID).Scan(&unknown); e != nil {
		return key, e
	}
	if unknown == 0 && meterState == "PENDING_VERIFICATION" {
		for _, h := range proof.Holds {
			if h.Purpose != "PARENT" || h.Kind == "platform_risk" {
				continue
			}
			if h.State != "HELD" || h.Amount != 0 {
				return key, ErrConflict
			}
			if _, e = tx.ExecContext(ctx, "UPDATE reservations SET status='SETTLED',revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
				return key, e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND held>=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID, h.Amount); e != nil {
				return key, e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,0,'SETTLED',1)", id.Scope.TenantID, id.Scope.ProductID, turnIdentity(proof.Task, "meter-posting", h.ReservationID), proof.Task, id.Scope.SubjectID, h.Kind, h.ScopeKey, h.AccountID); e != nil {
				return key, e
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE service_events SET status='COMMITTED',revision=revision+1 WHERE tenant_id=? AND product_id=? AND record_id=? AND status='PENDING'", id.Scope.TenantID, id.Scope.ProductID, serviceID); e != nil {
			return key, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE harness_service_metering SET state='SETTLED',settlement_receipt=? WHERE tenant_id=? AND product_id=? AND service_id=? AND state='PENDING_VERIFICATION'", key, id.Scope.TenantID, id.Scope.ProductID, serviceID); e != nil {
			return key, e
		}
	}
	expected, e := meterDispositionSnapshot(ctx, tx, proof, serviceID)
	if e != nil {
		return key, e
	}
	var familyHash [32]byte
	if proof.Task != id.TaskID {
		family, e := readRehearsalSettlement(ctx, tx, id.Scope.ProductScope, proof.Task, service)
		if e != nil {
			return key, e
		}
		_, familyHash, e = harnessPayload(family)
		if e != nil {
			return key, e
		}
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return key, e
		}
	}
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		return key, e
	}
	if e = checkOriginalBillRequest(ctx, tx, id, service); e != nil {
		return key, e
	}
	if e = checkMeterBusinessTx(ctx, tx, proof); e != nil {
		return key, e
	}
	if e = provider.CheckOriginalUsageEvidence(evidence, service); e != nil {
		return key, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return key, e
	}
	if meterState != "RELEASED" && !now.Before(proof.SettleBy) {
		return key, ErrDenied
	}
	if proof.Task != id.TaskID {
		family, e := readRehearsalSettlement(ctx, tx, id.Scope.ProductScope, proof.Task, service)
		if e != nil {
			return key, e
		}
		_, current, e := harnessPayload(family)
		if e != nil {
			return key, e
		}
		if current != familyHash {
			return key, ErrConflict
		}
	}
	actual, e := meterDispositionSnapshot(ctx, tx, proof, serviceID)
	if e != nil {
		return key, e
	}
	if actual != expected {
		return key, ErrConflict
	}
	receipt, e := queryReceipt(ctx, tx, key)
	if e != nil {
		return key, e
	}
	if e = checkUsageReceipt(receipt, m, key); e != nil {
		return key, e
	}
	return key, s.costCommit(ctx, tx)
}

func checkUsageReceipt(r receiptRow, m domain.MockUsageMeasurement, key string) error {
	_, hash, e := harnessPayload(m)
	if e != nil {
		return e
	}
	if !settlementReceiptScope(r, m.Identity.Scope, m.Identity.TaskID) || r.operation != "HARNESS_ORIGINAL_USAGE" || r.command != key || !bytes.Equal(r.hash, hash[:]) {
		return ErrConflict
	}
	return nil
}

func decodeOriginalMeter(raw, hash []byte) (domain.MockCostMetadata, error) {
	var m domain.MockCostMetadata
	if json.Unmarshal(raw, &m) != nil {
		return m, ErrConflict
	}
	_, h, e := harnessPayload(m)
	if e != nil {
		return m, e
	}
	if !bytes.Equal(hash, h[:]) || !validOriginalUsageMetadata(m) {
		return m, ErrConflict
	}
	return m, nil
}

// Closed inventory of this exact already-locked service, attempts and accounts.
// Used only inside original metering reconciliation, never a generic SQL port.
func meterDispositionSnapshot(ctx context.Context, tx *sql.Tx, p cutoffProof, service string) ([32]byte, error) {
	var data []string
	t, product := p.Scope.TenantID, p.Scope.ProductID
	for _, q := range []string{
		"SELECT CAST(JSON_ARRAY(s.task_id,s.owner_subject_id,s.status,m.quote_id,m.business_receipt,HEX(m.original_hash),m.state,m.settlement_receipt,CAST(m.disposition_proof AS CHAR),e.state,e.calls_allocated,e.steps_used,e.execute_by,e.settle_by) AS CHAR) FROM service_events s JOIN harness_service_metering m ON m.tenant_id=s.tenant_id AND m.product_id=s.product_id AND m.service_id=s.record_id JOIN harness_envelopes e ON e.tenant_id=m.tenant_id AND e.product_id=m.product_id AND e.task_id=m.task_id WHERE s.tenant_id=? AND s.product_id=? AND s.record_id=?",
		"SELECT CAST(JSON_ARRAY(task_id,attempt_id,CAST(original_metadata AS CHAR),HEX(original_hash),usage_state,input_units,output_units,evidence_receipt) AS CHAR) FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND service_id=? ORDER BY BINARY task_id,BINARY attempt_id",
		"SELECT CAST(JSON_ARRAY(task_id,attempt_id,quote_id,input_units,output_units,service_atoms,service_unit,supplier_atoms,supplier_unit,supplier_currency) AS CHAR) FROM harness_service_lines WHERE tenant_id=? AND product_id=? AND service_id=? ORDER BY BINARY task_id,BINARY attempt_id",
	} {
		rows, e := tx.QueryContext(ctx, q, t, product, service)
		if e != nil {
			return [32]byte{}, e
		}
		for rows.Next() {
			var v string
			if e = rows.Scan(&v); e != nil {
				rows.Close()
				return [32]byte{}, e
			}
			data = append(data, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return [32]byte{}, e
		}
	}
	for _, h := range p.Holds {
		var v string
		e := tx.QueryRowContext(ctx, "SELECT CAST(JSON_ARRAY(r.task_id,r.purpose,r.amount,r.status,r.revision,a.held,a.revision) AS CHAR) FROM reservations r JOIN budget_accounts a USING(kind,scope_key,account_id) WHERE r.kind=? AND r.scope_key=? AND r.account_id=? AND r.reservation_id=?", h.Kind, h.ScopeKey, h.AccountID, h.ReservationID).Scan(&v)
		if e != nil {
			return [32]byte{}, e
		}
		data = append(data, v)
		if e = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT CAST(JSON_ARRAY(task_id,owner_subject_id,kind,scope_key,account_id,amount,status) AS CHAR) FROM postings WHERE tenant_id=? AND product_id=? AND record_id=?),'NONE')", t, product, turnIdentity(p.Task, "meter-posting", h.ReservationID)).Scan(&v); e != nil {
			return [32]byte{}, e
		}
		data = append(data, v)
	}
	var risk string
	if e := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT CAST(JSON_ARRAY(scope_key,account_id,reservation_id,original_cap,CAST(cost_atoms AS CHAR),unresolved) AS CHAR) FROM harness_risk_cases WHERE tenant_id=? AND product_id=? AND task_id=?),'NONE')", t, product, p.Task).Scan(&risk); e != nil {
		return [32]byte{}, e
	}
	data = append(data, risk)
	_, hash, e := harnessPayload(data)
	return hash, e
}

func checkMeterBusinessTx(ctx context.Context, tx *sql.Tx, p cutoffProof) error {
	r, e := queryReceipt(ctx, tx, p.WorldReceipt)
	if e != nil {
		return e
	}
	if !settlementReceiptScope(r, p.Scope, p.Task) {
		return ErrConflict
	}
	var q string
	switch r.operation {
	case "HARNESS_TURN_COMMIT":
		q = "SELECT EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)"
	case "CANDIDATE_COMMIT":
		q = "SELECT EXISTS(SELECT 1 FROM harness_candidate_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)"
	case "REHEARSAL_REPORT_COMMIT":
		q = "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=? AND state='COMMITTED')"
	default:
		return ErrConflict
	}
	var valid bool
	if e = tx.QueryRowContext(ctx, q, p.Scope.TenantID, p.Scope.ProductID, p.Task, p.WorldReceipt).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return ErrConflict
	}
	return nil
}

func originalMeterUsageKnown(ctx context.Context, q rowQuerier, scope domain.Scope, task, attempt string) (bool, error) {
	var raw, hash []byte
	var usage string
	var in, out sql.NullInt64
	var key sql.NullString
	e := q.QueryRowContext(ctx, "SELECT original_metadata,original_hash,usage_state,input_units,output_units,evidence_receipt FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", scope.TenantID, scope.ProductID, task, attempt).Scan(&raw, &hash, &usage, &in, &out, &key)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	original, e := decodeOriginalMeter(raw, hash)
	if e != nil || original.Identity.Scope != scope || original.Identity.TaskID != task || original.Identity.AttemptID != attempt {
		return false, ErrConflict
	}
	if usage == "UNKNOWN" {
		if in.Valid || out.Valid || key.Valid {
			return false, ErrConflict
		}
		return false, nil
	}
	if usage != "AUTHORITATIVE" || !in.Valid || !out.Valid || in.Int64 <= 0 || in.Int64 > 16000 || out.Int64 <= 0 || out.Int64 > 4000 {
		return false, ErrConflict
	}
	if original.UsageKnown {
		return !key.Valid && uint64(in.Int64) == original.InputUnits && uint64(out.Int64) == original.OutputUnits, nil
	}
	if !key.Valid || key.String != turnIdentity(attempt, "original-usage") {
		return false, ErrConflict
	}
	r, e := queryReceipt(ctx, q, key.String)
	if e != nil {
		return false, e
	}
	m := domain.MockUsageMeasurement{Identity: original.Identity, Provider: "r0-mock", Account: "r0-mock-supplier", InputUnits: uint64(in.Int64), OutputUnits: uint64(out.Int64)}
	if e = checkUsageReceipt(r, m, key.String); e != nil {
		return false, e
	}
	return true, nil
}
