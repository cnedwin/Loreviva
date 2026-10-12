package mysql

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

// Durable original attempt rows are the work set. No volatile cursor can skip an
// unpublished observation or an item whose failure record did not commit.
func (s *Store) RunOriginalServiceMetering(ctx context.Context) (int, int, error) {
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
	rows, e := s.DB.QueryContext(ctx, `SELECT q.request_identity FROM harness_attempt_metering a JOIN harness_service_metering m ON m.tenant_id=a.tenant_id AND m.product_id=a.product_id AND m.service_id=a.service_id JOIN harness_original_queries q ON q.tenant_id=a.tenant_id AND q.product_id=a.product_id AND q.task_id=a.task_id AND q.attempt_id=a.attempt_id JOIN harness_settlement_bindings b ON b.tenant_id=m.tenant_id AND b.product_id=m.product_id AND b.task_id=m.task_id JOIN harness_settlement_services s ON s.installation_id=b.installation_id AND s.service_id=b.service_id JOIN memory_installation i ON i.installation_id=s.installation_id WHERE a.usage_state='UNKNOWN' AND a.meter_next_run_at<=UTC_TIMESTAMP(6) AND m.state IN ('PENDING_VERIFICATION','RELEASED') AND b.installation_id=? AND b.service_id=? AND b.policy=? AND s.action=? AND s.enabled=1 AND i.generation=? ORDER BY a.meter_next_run_at,BINARY a.tenant_id,BINARY a.product_id,BINARY a.task_id,BINARY a.attempt_id LIMIT 100`, service.Installation, service.ServiceID, service.Policy, service.Action, service.Generation)
	if e != nil {
		return 0, 0, e
	}
	var items []domain.MockRequestIdentity
	for rows.Next() {
		var raw []byte
		var id domain.MockRequestIdentity
		if e = rows.Scan(&raw); e != nil {
			rows.Close()
			return 0, 0, e
		}
		if json.Unmarshal(raw, &id) != nil || id.Validate() != nil {
			rows.Close()
			return 0, 0, ErrConflict
		}
		items = append(items, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return 0, 0, e
	}
	done, failed := 0, 0
	for _, id := range items {
		if e = ctx.Err(); e != nil {
			return done, failed, e
		}
		item, close := context.WithTimeout(ctx, 5*time.Second)
		_, e = s.ReconcileOriginalServiceMetering(item, id)
		if e == nil {
			done++
			close()
			continue
		}
		failed++
		record := s.recordOriginalMeterRetry(item, id)
		close()
		if record != nil {
			return done, failed, record
		}
	}
	return done, failed, nil
}

func (s *Store) recordOriginalMeterRetry(ctx context.Context, id domain.MockRequestIdentity) error {
	ctx, service, release, e := s.SettlementAuthority.EnterDeadlineSettlement(ctx)
	if e != nil {
		return e
	}
	defer release()
	tx, closeTx, e := s.beginSettlementTx(ctx)
	if e != nil {
		return e
	}
	defer closeTx()
	if e = settlementServiceTx(ctx, tx, service); e != nil {
		return e
	}
	var raw []byte
	var valid bool
	e = tx.QueryRowContext(ctx, `SELECT q.request_identity,b.installation_id=? AND b.service_id=? AND b.policy=? AND q.marker_key=? AND t.owner_subject_id=? FROM harness_attempt_metering a JOIN harness_service_metering m ON m.tenant_id=a.tenant_id AND m.product_id=a.product_id AND m.service_id=a.service_id JOIN harness_settlement_bindings b ON b.tenant_id=m.tenant_id AND b.product_id=m.product_id AND b.task_id=m.task_id JOIN tasks t ON t.tenant_id=m.tenant_id AND t.product_id=m.product_id AND t.task_id=m.task_id JOIN harness_original_queries q ON q.tenant_id=a.tenant_id AND q.product_id=a.product_id AND q.task_id=a.task_id AND q.attempt_id=a.attempt_id WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.attempt_id=?`, service.Installation, service.ServiceID, service.Policy, id.MarkerKey, id.Scope.SubjectID, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&raw, &valid)
	if e != nil {
		return e
	}
	var saved domain.MockRequestIdentity
	if !valid || json.Unmarshal(raw, &saved) != nil || saved != id {
		return ErrDenied
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_attempt_metering SET meter_failures=meter_failures+1,meter_next_run_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND) WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND usage_state='UNKNOWN'", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID); e != nil {
		return e
	}
	return s.costCommit(ctx, tx)
}
