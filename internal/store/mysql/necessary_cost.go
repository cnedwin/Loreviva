package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"sync/atomic"
	"time"
)

type queryIOPermit struct{ taken atomic.Bool }

type NecessaryCostMetadata = domain.MockCostMetadata
type OriginalProviderCostEvidence interface {
	OriginalCostMetadata(context.Context, domain.MockRequestIdentity) (domain.MockCostMetadata, error)
}
type NecessaryCostReceipt struct {
	QueryKey                        string
	Ordinal                         int
	BeginCommitted, RecordCommitted bool
	Metadata                        NecessaryCostMetadata
}

func (l OriginalQueryLease) CostQueryKey() string { return l.queryKey }

func costQueryKey(id domain.MockRequestIdentity, n int) string {
	return turnIdentity(id.AuthorizationID, "necessary-cost", fmt.Sprint(n))
}
func (s *Store) costCommit(ctx context.Context, tx *sql.Tx) error {
	var e error
	if s.Commit != nil {
		e = s.Commit(ctx, tx)
	} else {
		e = tx.Commit()
	}
	if e != nil {
		return ErrCommitUnknown
	}
	return nil
}

// The caller never chooses a new identity, policy, budget or generation.
func (s *Store) BeginNecessaryCostQuery(ctx context.Context, id domain.MockRequestIdentity) (OriginalQueryLease, error) {
	var zero OriginalQueryLease
	if s.QueryAuthority == nil || id.Validate() != nil {
		return zero, ErrDenied
	}
	if _, ok := s.ProviderEvidence.(OriginalProviderCostEvidence); !ok {
		return zero, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return zero, e
	}
	defer release()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return zero, e
	}
	defer tx.Rollback()
	if e = lockOriginalQuery(ctx, tx, id, service); e != nil {
		return zero, e
	}
	var fence uint64
	var active, inTime, costKnown bool
	var foreground, costUsed int
	var costDue bool
	e = tx.QueryRowContext(ctx, "SELECT q.fence,COALESCE(q.lease_expires_at>UTC_TIMESTAMP(6),0),q.queries_used,q.cost_queries_used,COALESCE(q.next_cost_query_at<=UTC_TIMESTAMP(6),1),COALESCE(JSON_EXTRACT(q.metadata,'$.UsageKnown')=true AND JSON_EXTRACT(q.metadata,'$.NoFurtherCost')=true,0),e.execute_by>UTC_TIMESTAMP(6) FROM harness_original_queries q JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE q.tenant_id=? AND q.product_id=? AND q.task_id=? AND q.attempt_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&fence, &active, &foreground, &costUsed, &costDue, &costKnown, &inTime)
	if e != nil {
		return zero, e
	}
	if active || fence == ^uint64(0) || inTime && foreground < 3 || costUsed >= 3 || !costDue || costKnown {
		return zero, ErrDenied
	}
	if e = validateOriginalQueryHistory(ctx, tx, id, foreground); e != nil {
		return zero, e
	}
	// Reading the last original command receipt resolves any previous Begin/Record
	// ACK uncertainty before allocating a further bounded query. No permit is recreated.
	var n int
	var previousKey string
	var due bool
	var prior []byte
	e = tx.QueryRowContext(ctx, "SELECT ordinal,query_key,next_query_at<=UTC_TIMESTAMP(6),metadata FROM harness_necessary_cost_records WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? ORDER BY ordinal DESC LIMIT 1", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&n, &previousKey, &due, &prior)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return zero, e
	}
	if e == nil {
		if previousKey != costQueryKey(id, n) || !due || n >= 3 {
			return zero, ErrDenied
		}
		if len(prior) > 0 {
			var m NecessaryCostMetadata
			if json.Unmarshal(prior, &m) != nil || m.Identity != id {
				return zero, ErrDenied
			}
			if m.UsageKnown && m.NoFurtherCost {
				return zero, ErrDenied
			}
		}
	}
	if n != costUsed {
		return zero, ErrDenied
	}
	n++
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return zero, e
	}
	lease := OriginalQueryLease{identity: id, service: service, fence: fence + 1, owner: hex.EncodeToString(nonce[:]), cost: true, ioPermit: &queryIOPermit{}, queryKey: costQueryKey(id, n)}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_original_queries SET fence=?,lease_owner=?,lease_expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 5 SECOND),generation=?,cost_queries_used=cost_queries_used+1,next_cost_query_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND) WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", lease.fence, lease.owner, service.Generation, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID); e != nil {
		return zero, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_necessary_cost_records(tenant_id,product_id,task_id,attempt_id,ordinal,query_key,lease_fence,began_at,next_query_at) VALUES(?,?,?,?,?,?,?,UTC_TIMESTAMP(6),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 30 SECOND))", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, n, costQueryKey(id, n), lease.fence); e != nil {
		return zero, e
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return OriginalQueryLease{identity: id, queryKey: lease.queryKey}, e
	}
	return lease, nil
}
func validateCostReceipt(ctx context.Context, tx *sql.Tx, l OriginalQueryLease) (NecessaryCostReceipt, error) {
	var out NecessaryCostReceipt
	var raw []byte
	e := tx.QueryRowContext(ctx, "SELECT ordinal,query_key,metadata FROM harness_necessary_cost_records WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND lease_fence=?", l.identity.Scope.TenantID, l.identity.Scope.ProductID, l.identity.TaskID, l.identity.AttemptID, l.fence).Scan(&out.Ordinal, &out.QueryKey, &raw)
	if e != nil {
		return out, e
	}
	if out.QueryKey != costQueryKey(l.identity, out.Ordinal) {
		return out, ErrDenied
	}
	out.BeginCommitted = true
	if len(raw) > 0 {
		if json.Unmarshal(raw, &out.Metadata) != nil || out.Metadata.Identity != l.identity {
			return NecessaryCostReceipt{}, ErrDenied
		}
		out.RecordCommitted = true
	}
	return out, nil
}

// QueryNecessaryCostReceipt is a fixed current-service read of the exact durable
// original query command; it never mints a query lease or touches provider/body IO.
func (s *Store) QueryNecessaryCostReceipt(ctx context.Context, id domain.MockRequestIdentity, queryKey string) (NecessaryCostReceipt, error) {
	if s.QueryAuthority == nil {
		return NecessaryCostReceipt{}, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return NecessaryCostReceipt{}, e
	}
	defer release()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return NecessaryCostReceipt{}, e
	}
	defer tx.Rollback()
	if e = lockOriginalQuery(ctx, tx, id, service); e != nil {
		return NecessaryCostReceipt{}, e
	}
	var fence uint64
	if e = tx.QueryRowContext(ctx, "SELECT lease_fence FROM harness_necessary_cost_records WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND query_key=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, queryKey).Scan(&fence); e != nil {
		return NecessaryCostReceipt{}, e
	}
	return validateCostReceipt(ctx, tx, OriginalQueryLease{identity: id, fence: fence})
}
func (s *Store) RecordNecessaryCostQuery(ctx context.Context, l OriginalQueryLease) (out NecessaryCostMetadata, err error) {
	defer func() {
		if err != nil {
			out = NecessaryCostMetadata{}
		}
	}()
	provider, ok := s.ProviderEvidence.(OriginalProviderCostEvidence)
	if !ok || !l.cost || l.fence == 0 || l.owner == "" || s.QueryAuthority == nil || l.ioPermit == nil || !l.ioPermit.taken.CompareAndSwap(false, true) {
		return out, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	if service != l.service {
		return out, ErrDenied
	}
	check, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return out, e
	}
	if e = lockOriginalQuery(ctx, check, l.identity, service); e == nil {
		e = validateQueryLease(ctx, check, l)
	}
	var receipt NecessaryCostReceipt
	if e == nil {
		receipt, e = validateCostReceipt(ctx, check, l)
	}
	if rollbackError := check.Rollback(); e == nil && rollbackError != nil {
		e = rollbackError
	}
	if e != nil {
		return out, e
	}
	if receipt.RecordCommitted {
		return receipt.Metadata, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, e = provider.OriginalCostMetadata(queryCtx, l.identity)
	if e != nil {
		return out, e
	}
	if out.Identity != l.identity || (out.State != "UNKNOWN" && out.State != "SUCCEEDED") {
		return out, ErrDenied
	}
	if out.State == "SUCCEEDED" && !validOriginalUsageMetadata(out) {
		return out, ErrDenied
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if e = lockOriginalQuery(ctx, tx, l.identity, service); e != nil {
		return out, e
	}
	if e = validateQueryLease(ctx, tx, l); e != nil {
		return out, e
	}
	receipt, e = validateCostReceipt(ctx, tx, l)
	if e != nil {
		return out, e
	}
	if receipt.RecordCommitted {
		return receipt.Metadata, nil
	}
	raw, e := json.Marshal(out)
	if e != nil {
		return out, e
	}
	id := l.identity
	if _, e = tx.ExecContext(ctx, "UPDATE harness_necessary_cost_records SET metadata=?,recorded_at=UTC_TIMESTAMP(6) WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND lease_fence=? AND metadata IS NULL", raw, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, l.fence); e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_original_queries SET lease_expires_at=NULL,lease_owner=NULL WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID); e != nil {
		return out, e
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return out, e
	}
	return out, nil
}

// DueNecessaryCostQuery selects one original accepted purchase, never a new task.
func (s *Store) DueNecessaryCostQuery(ctx context.Context) (domain.MockRequestIdentity, error) {
	var id domain.MockRequestIdentity
	if s.QueryAuthority == nil {
		return id, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return id, e
	}
	defer release()
	var raw []byte
	e = s.DB.QueryRowContext(ctx, "SELECT q.request_identity FROM harness_original_queries q JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_query_services s ON s.installation_id=q.installation_id AND s.service_id=q.service_id JOIN memory_installation i ON i.installation_id=s.installation_id WHERE q.installation_id=? AND s.service_id=? AND s.action=? AND s.policy=? AND q.policy=s.policy AND s.enabled=1 AND i.generation=? AND q.provider_state IN ('UNKNOWN','SUCCEEDED') AND NOT COALESCE(JSON_EXTRACT(q.metadata,'$.UsageKnown')=true AND JSON_EXTRACT(q.metadata,'$.NoFurtherCost')=true,0) AND (q.queries_used=3 OR e.execute_by<=UTC_TIMESTAMP(6)) AND q.cost_queries_used<3 AND (q.next_cost_query_at IS NULL OR q.next_cost_query_at<=UTC_TIMESTAMP(6)) AND (q.lease_expires_at IS NULL OR q.lease_expires_at<=UTC_TIMESTAMP(6)) AND NOT EXISTS (SELECT 1 FROM harness_necessary_cost_records r WHERE r.tenant_id=q.tenant_id AND r.product_id=q.product_id AND r.task_id=q.task_id AND r.attempt_id=q.attempt_id AND JSON_EXTRACT(r.metadata,'$.UsageKnown')=true AND JSON_EXTRACT(r.metadata,'$.NoFurtherCost')=true) ORDER BY q.next_cost_query_at,q.tenant_id,q.product_id,q.task_id,q.attempt_id LIMIT 1", service.Installation, service.ServiceID, service.Action, service.Policy, service.Generation).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return id, ErrNoTask
	}
	if e != nil {
		return id, e
	}
	if json.Unmarshal(raw, &id) != nil {
		return id, ErrDenied
	}
	return id, nil
}
