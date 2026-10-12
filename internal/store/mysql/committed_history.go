package mysql

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"sort"
)

type committedTransition struct {
	PayloadHash                            [32]byte
	Task, Receipt, Operation, Action, Plan string
	Scope                                  domain.Scope
	Before                                 domain.Versions
	Event                                  struct{ Kind, Action, PlanID, FromRelease, TargetRelease, Policy string }
}

func (v committedTransition) locks(s domain.Scope) []LockKey {
	return []LockKey{{Rank: 50, IDs: []string{s.TenantID, s.ProductID, v.Task}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{v.Receipt}}, {Rank: 110, Kind: "events", IDs: []string{s.TenantID, s.ProductID, v.Receipt}}, {Rank: 110, Kind: "approved_results", IDs: []string{s.TenantID, s.ProductID, v.Receipt}}, {Rank: 110, Kind: "outbox", IDs: []string{s.TenantID, s.ProductID, v.Receipt}}}
}
func loadCommittedTransitions(ctx context.Context, q accountingQuery, s domain.Scope) ([]committedTransition, error) {
	rows, e := q.QueryContext(ctx, "SELECT t.task_id,c.commit_key,c.operation,r.document,d.document FROM tasks t JOIN runtime_tasks r USING(tenant_id,product_id,task_id) JOIN commit_receipts c ON c.tenant_id=t.tenant_id AND c.product_id=t.product_id AND c.task_id=t.task_id JOIN events e ON e.tenant_id=t.tenant_id AND e.product_id=t.product_id AND e.task_id=t.task_id AND e.record_id=c.commit_key JOIN runtime_events d ON d.tenant_id=e.tenant_id AND d.product_id=e.product_id AND d.record_id=e.record_id JOIN approved_results a ON a.tenant_id=e.tenant_id AND a.product_id=e.product_id AND a.task_id=e.task_id AND a.record_id=e.record_id JOIN outbox o ON o.tenant_id=e.tenant_id AND o.product_id=e.product_id AND o.task_id=e.task_id AND o.record_id=e.record_id WHERE t.tenant_id=? AND t.product_id=? AND t.world_instance_id=? AND t.owner_subject_id=? AND t.state='COMMITTED' AND e.status='COMMITTED' AND a.status='COMMITTED' AND c.operation IN ('INSTANCE_CONTROL','INSTANCE_MIGRATION') ORDER BY t.task_id LIMIT 201", s.TenantID, s.ProductID, s.WorldInstanceID, s.SubjectID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []committedTransition{}
	for rows.Next() {
		var v committedTransition
		var raw, event []byte
		if e = rows.Scan(&v.Task, &v.Receipt, &v.Operation, &raw, &event); e != nil {
			return nil, e
		}
		var p struct {
			Scope          domain.Scope
			Versions       domain.Versions
			Action, PlanID string
		}
		if json.Unmarshal(raw, &p) != nil || json.Unmarshal(event, &v.Event) != nil {
			return nil, ErrDenied
		}
		v.Scope, v.Before, v.Action, v.Plan = p.Scope, p.Versions, p.Action, p.PlanID
		v.PayloadHash = sha256.Sum256(raw)
		out = append(out, v)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(out) > 200 {
		return nil, ErrDenied
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before.ControlVersion < out[j].Before.ControlVersion })
	return out, nil
}
func (s *Store) planCommittedHistory(ctx context.Context, r *BarrierRequest) error {
	chain, e := loadCommittedTransitions(ctx, s.DB, *r.WorldScope)
	if e != nil {
		return e
	}
	for _, v := range chain {
		r.Locks = append(r.Locks, v.locks(*r.WorldScope)...)
	}
	r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(r.TaskID, "world-commit")}})
	rows, e := s.DB.QueryContext(ctx, "SELECT receipt_key FROM harness_task_retirements WHERE tenant_id=? AND product_id=? AND task_id=? UNION SELECT receipt_key FROM harness_world_seals WHERE tenant_id=? AND product_id=? AND world_instance_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.ProductScope.TenantID, r.ProductScope.ProductID, r.WorldScope.WorldInstanceID)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if e = rows.Scan(&key); e != nil {
			return e
		}
		r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{key}})
	}
	return rows.Err()
}
