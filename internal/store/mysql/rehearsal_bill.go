package mysql

import (
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

// Root attribution is selected only from a real immutable child registration,
// then proven under the complete original family's shared financial locks.
// The provider identity and bill receipt remain the original child's identity.
func (s *Store) lockOriginalBillSettlement(ctx context.Context, tx *sql.Tx, id domain.MockRequestIdentity, service domain.SettlementServiceIdentity, extra []LockKey) (cutoffProof, string, error) {
	var root string
	e := tx.QueryRowContext(ctx, "SELECT root_task_id FROM harness_rehearsal_children WHERE tenant_id=? AND product_id=? AND child_task_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID).Scan(&root)
	if errors.Is(e, sql.ErrNoRows) {
		return s.lockSettlement(ctx, tx, id.Scope, id.TaskID, service, extra)
	}
	if e != nil {
		return cutoffProof{}, "", e
	}
	p, e := s.lockRehearsalSettlement(ctx, tx, id.Scope.ProductScope, root, service, extra)
	if e != nil {
		return cutoffProof{}, "", e
	}
	found := false
	for _, c := range p.Children {
		if c.Task == id.TaskID && c.World == id.Scope.WorldInstanceID {
			for _, a := range c.Attempts {
				if a.Attempt == id.AttemptID && a.Marker == id.MarkerKey && a.Started {
					found = true
				}
			}
		}
	}
	if !found {
		return cutoffProof{}, "", ErrConflict
	}
	out := p.Root
	out.Holds = nil
	for _, h := range p.Holds {
		out.Holds = append(out.Holds, h.cutoffHold)
	}
	for _, c := range p.Children {
		out.Attempts = append(out.Attempts, c.Attempts...)
	}
	return out, p.State, nil
}

func originalRiskTotals(ctx context.Context, tx *sql.Tx, proof cutoffProof) (bool, string, error) {
	var unresolved bool
	var total string
	// Exact family membership was locked and verified above. Ordinary parents
	// have no registered children and retain their original one-task meaning.
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM attempts a LEFT JOIN harness_supplier_liabilities l USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND (a.task_id=? OR EXISTS(SELECT 1 FROM harness_rehearsal_children c WHERE c.tenant_id=a.tenant_id AND c.product_id=a.product_id AND c.child_task_id=a.task_id AND c.root_task_id=?)) AND a.send_started_at IS NOT NULL AND (l.final_known IS NULL OR l.final_known=0 OR l.disputed=1)),COALESCE((SELECT CAST(SUM(l.total_atoms) AS CHAR) FROM harness_supplier_liabilities l WHERE l.tenant_id=? AND l.product_id=? AND (l.task_id=? OR EXISTS(SELECT 1 FROM harness_rehearsal_children c WHERE c.tenant_id=l.tenant_id AND c.product_id=l.product_id AND c.child_task_id=l.task_id AND c.root_task_id=?))),'0')`, proof.Scope.TenantID, proof.Scope.ProductID, proof.Task, proof.Task, proof.Scope.TenantID, proof.Scope.ProductID, proof.Task, proof.Task).Scan(&unresolved, &total)
	return unresolved, total, e
}
