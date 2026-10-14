package mysql

import (
	"context"
	"database/sql"
	"loreviva.local/loreviva/internal/domain"
)

// OperatorRead is a short owning read with the same credential/grant locks as
// commands and settings writers. No external IO may run while it is held.
type OperatorRead struct {
	tx      *sql.Tx
	store   *Store
	scope   domain.ProductScope
	actions []string
}

func (r *OperatorRead) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return r.tx.QueryRowContext(ctx, q, args...)
}
func (r *OperatorRead) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return r.tx.QueryContext(ctx, q, args...)
}
func (r *OperatorRead) Close() { _ = r.tx.Rollback() }
// Finish rechecks the current identity, scope and requested actions before ending this read transaction with rollback; it does not commit business effects.
func (r *OperatorRead) Finish(ctx context.Context) error {
	if r.store.BeforeReadFinish != nil {
		if e := r.store.BeforeReadFinish(ctx, r.tx); e != nil {
			return e
		}
	}
	if _, e := currentOperatorTx(ctx, r.tx, r.store, r.scope); e != nil {
		return e
	}
	if e := operatorScopeTx(ctx, r.tx, r.scope); e != nil {
		return e
	}
	for _, a := range r.actions {
		if e := operatorActionTx(ctx, r.tx, r.store, r.scope, a); e != nil {
			return e
		}
	}
	return r.tx.Rollback()
}
func (s *Store) BeginOperatorRead(ctx context.Context, scope domain.ProductScope, actions ...string) (*OperatorRead, error) {
	if len(actions) == 0 {
		return nil, ErrDenied
	}
	return s.beginOperatorRead(ctx, scope, actions)
}
func (s *Store) beginOperatorRead(ctx context.Context, scope domain.ProductScope, actions []string, extra ...LockKey) (*OperatorRead, error) {
	p, e := s.CurrentOperator(ctx)
	if e != nil || p.ProductScope != scope {
		return nil, ErrDenied
	}
	keys := []LockKey{{Rank: 10, IDs: []string{scope.TenantID}}, {Rank: 20, IDs: []string{scope.TenantID, scope.ProductID}}, {Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}}
	keys = append(keys, extra...)
	keys, e = PlanLocks(append(keys, operatorKeys(p)...))
	if e != nil {
		return nil, e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	good := false
	defer func() {
		if !good {
			tx.Rollback()
		}
	}()
	principal := ctx.Value(operatorContextKey{}).(currentOperator)
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return nil, e
		}
		var id string
		switch k.Rank {
		case 10:
			e = tx.QueryRowContext(ctx, "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE", scope.TenantID).Scan(&id)
		case 20:
			e = tx.QueryRowContext(ctx, "SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE", scope.TenantID, scope.ProductID).Scan(&id)
		case 30:
			e = tx.QueryRowContext(ctx, "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2]).Scan(&id)
		case 15, 25:
			e = s.acquireOperatorKey(ctx, tx, BarrierRequest{operator: &principal}, k)
		}
		if e != nil {
			return nil, e
		}
	}
	if _, e = currentOperatorTx(ctx, tx, s, scope); e != nil {
		return nil, e
	}
	if e = operatorScopeTx(ctx, tx, scope); e != nil {
		return nil, e
	}
	for _, a := range actions {
		if e = operatorActionTx(ctx, tx, s, scope, a); e != nil {
			return nil, e
		}
	}
	good = true
	return &OperatorRead{tx, s, scope, append([]string(nil), actions...)}, nil
}
