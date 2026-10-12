package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
)

type originalServiceMetering struct {
	request BarrierRequest
	service string
	costs   []domain.MockResultMetadata
}

func allOriginalUsageKnown(costs []domain.MockResultMetadata) bool {
	for _, m := range costs {
		if !m.UsageKnown {
			return false
		}
	}
	return len(costs) > 0
}

// Known service units settle only the original customer-side reservation.
// Supplier and platform risk remain held until the actual original liability
// is final; a successful narrative or authoritative units cannot prove that.
func originalCostHoldState(h Reservation, costs []domain.MockResultMetadata) string {
	if !allOriginalUsageKnown(costs) {
		return "HELD"
	}
	if h.Kind == "platform_risk" {
		for _, m := range costs {
			if !m.NoFurtherCost {
				return "HELD"
			}
		}
		return "RELEASED"
	}
	if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
		for _, m := range costs {
			if h.ReservationID == turnIdentity(m.Identity.AttemptID, "supplier", h.Kind) && m.NoFurtherCost {
				return "SETTLED"
			}
		}
		return "HELD"
	}
	return "SETTLED"
}

// Only the fixed business owners invoke this while holding their original
// root/tasks/attempts/accounts/holds/append locks. No caller-provided amounts.
func bindOriginalServiceMeteringTx(ctx context.Context, u *unit, r BarrierRequest, service string, costs []domain.MockResultMetadata) error {
	if len(costs) < 1 || len(costs) > 40 {
		return ErrDenied
	}
	state, status := "PENDING_VERIFICATION", "PENDING"
	if allOriginalUsageKnown(costs) {
		state, status = "SETTLED", "COMMITTED"
	}
	var quote string
	if e := u.tx.QueryRowContext(ctx, "SELECT quote_id FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&quote); e != nil {
		return e
	}
	original := make([]domain.MockCostMetadata, len(costs))
	for i, m := range costs {
		if m.State != "SUCCEEDED" || !validOriginalUsageMetadata(minimalOriginalMetadata(m)) {
			return ErrDenied
		}
		original[i] = minimalOriginalMetadata(m)
	}
	_, hash, e := harnessPayload(original)
	if e != nil {
		return e
	}
	if _, e = u.tx.ExecContext(ctx, "INSERT INTO service_events(tenant_id,product_id,record_id,task_id,owner_subject_id,status,revision) VALUES(?,?,?,?,?,?,1)", r.ProductScope.TenantID, r.ProductScope.ProductID, service, r.TaskID, r.OwnerSubjectID, status); e != nil {
		return e
	}
	if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_service_metering(tenant_id,product_id,task_id,service_id,quote_id,business_receipt,original_hash,state) VALUES(?,?,?,?,?,?,?,?)", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, service, quote, r.CommitKey, hash[:], state); e != nil {
		return e
	}
	for i, m := range costs {
		raw, h, e := harnessPayload(original[i])
		if e != nil {
			return e
		}
		var in, out any
		usage := "UNKNOWN"
		if m.UsageKnown {
			in, out, usage = m.InputUnits, m.OutputUnits, "AUTHORITATIVE"
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_attempt_metering(tenant_id,product_id,task_id,attempt_id,service_id,original_metadata,original_hash,usage_state,input_units,output_units,meter_next_run_at) VALUES(?,?,?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6))", r.ProductScope.TenantID, r.ProductScope.ProductID, m.Identity.TaskID, m.Identity.AttemptID, service, raw, h[:], usage, in, out); e != nil {
			return e
		}
		if !m.UsageKnown {
			continue
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_service_lines VALUES(?,?,?,?,?,?,?,?,0,'WE_CREDIT',0,'MONEY','syntheticUSD')", r.ProductScope.TenantID, r.ProductScope.ProductID, m.Identity.TaskID, service, m.Identity.AttemptID, m.Identity.QuoteID, m.InputUnits, m.OutputUnits); e != nil {
			return e
		}
	}
	u.serviceMetering = &originalServiceMetering{r, service, append([]domain.MockResultMetadata(nil), costs...)}
	return nil
}

// Finish-time proof for the exact current joint transaction, including explicit
// unknown rows. Absence of a service line alone never means unknown or zero.
func checkOriginalServiceMeteringTx(ctx context.Context, u *unit, s domain.ProductScope, task, service, receipt string, costs []domain.MockResultMetadata) error {
	expected := make([]domain.MockCostMetadata, len(costs))
	current := append([]domain.MockResultMetadata(nil), costs...)
	knownCount := 0
	for i, m := range costs {
		expected[i] = minimalOriginalMetadata(m)
		var e error
		current[i], e = resolvedOriginalUsageTx(ctx, u, s, service, m)
		if e != nil {
			return e
		}
		if current[i].UsageKnown {
			knownCount++
		}
	}
	_, hash, e := harnessPayload(expected)
	if e != nil {
		return e
	}
	status, state := "PENDING", "PENDING_VERIFICATION"
	if knownCount == len(costs) {
		status, state = "COMMITTED", "SETTLED"
	}
	var meterState string
	var releaseProof []byte
	if e = u.tx.QueryRowContext(ctx, "SELECT state,disposition_proof FROM harness_service_metering WHERE tenant_id=? AND product_id=? AND service_id=?", s.TenantID, s.ProductID, service).Scan(&meterState, &releaseProof); e != nil {
		return e
	}
	if meterState == "RELEASED" {
		if u.serviceMetering != nil {
			return ErrDenied
		}
		if _, e = checkMeterReleaseTx(ctx, u.tx, s, task, service, receipt, hash[:], releaseProof); e != nil {
			return e
		}
		status, state = "RELEASED", "RELEASED"
	}
	var valid bool
	if e = u.tx.QueryRowContext(ctx, "SELECT s.task_id=? AND s.owner_subject_id=? AND s.status=? AND m.task_id=s.task_id AND m.quote_id=e.quote_id AND m.business_receipt=? AND m.original_hash=? AND m.state=? FROM service_events s JOIN harness_service_metering m ON m.tenant_id=s.tenant_id AND m.product_id=s.product_id AND m.service_id=s.record_id JOIN harness_envelopes e ON e.tenant_id=m.tenant_id AND e.product_id=m.product_id AND e.task_id=m.task_id WHERE s.tenant_id=? AND s.product_id=? AND s.record_id=?", task, s.SubjectID, status, receipt, hash[:], state, s.TenantID, s.ProductID, service).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	if state == "SETTLED" && knownCount == len(costs) && !allOriginalUsageKnown(costs) {
		if e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_attempt_metering a WHERE a.tenant_id=m.tenant_id AND a.product_id=m.product_id AND a.service_id=m.service_id AND a.evidence_receipt=m.settlement_receipt) FROM harness_service_metering m WHERE m.tenant_id=? AND m.product_id=? AND m.service_id=?", s.TenantID, s.ProductID, service).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrDenied
		}
	}
	var n int
	if e = u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND service_id=?", s.TenantID, s.ProductID, service).Scan(&n); e != nil {
		return e
	}
	if n != len(costs) {
		return ErrDenied
	}
	if e = u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM harness_service_lines WHERE tenant_id=? AND product_id=? AND service_id=?", s.TenantID, s.ProductID, service).Scan(&n); e != nil {
		return e
	}
	if n != knownCount {
		return ErrDenied
	}
	for i, m := range costs {
		var raw, h []byte
		var usage string
		var in, out sql.NullInt64
		if e = u.tx.QueryRowContext(ctx, "SELECT original_metadata,original_hash,usage_state,input_units,output_units FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND service_id=?", s.TenantID, s.ProductID, m.Identity.TaskID, m.Identity.AttemptID, service).Scan(&raw, &h, &usage, &in, &out); e != nil {
			return e
		}
		var saved domain.MockCostMetadata
		_, want, _ := harnessPayload(expected[i])
		if json.Unmarshal(raw, &saved) != nil || saved != expected[i] || !bytes.Equal(h, want[:]) {
			return ErrDenied
		}
		measured := current[i]
		if !measured.UsageKnown {
			if usage != "UNKNOWN" || in.Valid || out.Valid {
				return ErrDenied
			}
			continue
		}
		if usage != "AUTHORITATIVE" || !in.Valid || !out.Valid || uint64(in.Int64) != measured.InputUnits || uint64(out.Int64) != measured.OutputUnits {
			return ErrDenied
		}
		if e = u.tx.QueryRowContext(ctx, "SELECT service_id=? AND quote_id=? AND input_units=? AND output_units=? AND service_atoms=0 AND supplier_atoms=0 AND service_unit='WE_CREDIT' AND supplier_unit='MONEY' AND supplier_currency='syntheticUSD' FROM harness_service_lines WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", service, m.Identity.QuoteID, measured.InputUnits, measured.OutputUnits, s.TenantID, s.ProductID, m.Identity.TaskID, m.Identity.AttemptID).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrDenied
		}
	}
	if u.serviceMetering != nil && (u.serviceMetering.request.Operation == "HARNESS_TURN_COMMIT" || u.serviceMetering.request.Operation == "CANDIDATE_COMMIT") {
		if e := checkJointServiceHoldsTx(ctx, u, u.serviceMetering); e != nil {
			return e
		}
	}
	return nil
}

// Fixed ordinary joint-commit finish check. Historical reads and later cost
// reconciliation have independent proofs and do not reuse initial hold states.
func checkJointServiceHoldsTx(ctx context.Context, u *unit, m *originalServiceMetering) error {
	r := m.request
	var project string
	if e := u.tx.QueryRowContext(ctx, "SELECT q.project_id FROM harness_envelopes e JOIN harness_quotes q USING(tenant_id,product_id,quote_id) WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&project); e != nil {
		return e
	}
	accounts := r0CandidateAccounts(r.ProductScope, project)
	if r.Operation == "HARNESS_TURN_COMMIT" {
		if r.WorldScope == nil {
			return ErrDenied
		}
		accounts = r0Accounts(*r.WorldScope, project)
	}
	var expected []Reservation
	for _, h := range accounts {
		if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
			for _, cost := range m.costs {
				copy := h
				copy.ReservationID = turnIdentity(cost.Identity.AttemptID, "supplier", h.Kind)
				expected = append(expected, copy)
			}
		} else {
			h.ReservationID = turnIdentity(r.TaskID, "parent", h.Kind)
			expected = append(expected, h)
		}
	}
	var count int
	if e := u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&count); e != nil {
		return e
	}
	if count != len(expected) {
		return ErrConflict
	}
	for _, h := range expected {
		if e := u.RequireLock(LockKey{Rank: 80, Kind: h.Kind, IDs: []string{h.ScopeKey, h.AccountID, h.ReservationID}}); e != nil {
			return e
		}
		var amount int64
		var state, purpose string
		if e := u.tx.QueryRowContext(ctx, "SELECT amount,status,purpose FROM reservations WHERE tenant_id=? AND product_id=? AND task_id=? AND kind=? AND scope_key=? AND account_id=? AND reservation_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID).Scan(&amount, &state, &purpose); e != nil {
			return e
		}
		wantPurpose := "PARENT"
		if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
			for _, cost := range m.costs {
				if h.ReservationID == turnIdentity(cost.Identity.AttemptID, "supplier", h.Kind) {
					wantPurpose = cost.Identity.AttemptID
				}
			}
		}
		wantState := originalCostHoldState(h, m.costs)
		if amount != h.Amount || state != wantState || purpose != wantPurpose {
			return ErrConflict
		}
		key := turnIdentity(r.TaskID, "posting", h.ReservationID)
		if wantState == "HELD" {
			if e := u.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM postings WHERE tenant_id=? AND product_id=? AND record_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, key).Scan(&count); e != nil {
				return e
			}
			if count != 0 {
				return ErrConflict
			}
		} else {
			var valid bool
			if e := u.tx.QueryRowContext(ctx, "SELECT task_id=? AND owner_subject_id=? AND kind=? AND scope_key=? AND account_id=? AND amount=0 AND status=? FROM postings WHERE tenant_id=? AND product_id=? AND record_id=?", r.TaskID, r.OwnerSubjectID, h.Kind, h.ScopeKey, h.AccountID, wantState, r.ProductScope.TenantID, r.ProductScope.ProductID, key).Scan(&valid); e != nil {
				return e
			}
			if !valid {
				return ErrConflict
			}
		}
	}
	return nil
}

// A later real metering receipt can refine units, never the immutable original
// result, body, finality, route or business outcome. Original joint finish stays
// strict and cannot manufacture a later refinement in that same transaction.
func resolvedOriginalUsageTx(ctx context.Context, u *unit, s domain.ProductScope, service string, m domain.MockResultMetadata) (domain.MockResultMetadata, error) {
	var usage string
	var in, out sql.NullInt64
	var key sql.NullString
	e := u.tx.QueryRowContext(ctx, "SELECT usage_state,input_units,output_units,evidence_receipt FROM harness_attempt_metering WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND service_id=?", s.TenantID, s.ProductID, m.Identity.TaskID, m.Identity.AttemptID, service).Scan(&usage, &in, &out, &key)
	if e != nil {
		return m, e
	}
	if m.UsageKnown || usage == "UNKNOWN" {
		if key.Valid {
			return m, ErrConflict
		}
		return m, nil
	}
	if u.serviceMetering != nil || usage != "AUTHORITATIVE" || !in.Valid || !out.Valid || in.Int64 <= 0 || in.Int64 > 16000 || out.Int64 <= 0 || out.Int64 > 4000 || !key.Valid || key.String != turnIdentity(m.Identity.AttemptID, "original-usage") {
		return m, ErrConflict
	}
	measured := domain.MockUsageMeasurement{Identity: m.Identity, Provider: "r0-mock", Account: "r0-mock-supplier", InputUnits: uint64(in.Int64), OutputUnits: uint64(out.Int64)}
	receipt, e := queryReceipt(ctx, u.tx, key.String)
	if e != nil {
		return m, e
	}
	if e = checkUsageReceipt(receipt, measured, key.String); e != nil {
		return m, e
	}
	m.UsageKnown, m.InputUnits, m.OutputUnits = true, measured.InputUnits, measured.OutputUnits
	return m, nil
}
