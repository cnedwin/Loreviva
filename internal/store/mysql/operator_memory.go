package mysql

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
)

type httpActionKey struct{}
type httpActionProof struct {
	store     *Store
	principal domain.OperatorPrincipal
	action    string
}

// AuthorizeHTTPAction mints an unexported proof only after validating the real
// private principal and current registry action. Wire fields cannot mint it.
func (s *Store) AuthorizeHTTPAction(ctx context.Context, scope domain.ProductScope, action string) (context.Context, error) {
	if e := s.CheckOperator(ctx, scope, action); e != nil {
		return ctx, e
	}
	p, e := s.CurrentOperator(ctx)
	if e != nil {
		return ctx, e
	}
	return context.WithValue(ctx, httpActionKey{}, httpActionProof{s, p, action}), nil
}

func (s *Store) memoryHTTPActions(ctx context.Context, scope domain.ProductScope, action string) ([]string, error) {
	proof, ok := ctx.Value(httpActionKey{}).(httpActionProof)
	if !ok {
		return nil, nil
	}
	p, e := s.CurrentOperator(ctx)
	if e != nil || proof.store != s || proof.principal != p || p.ProductScope != scope {
		return nil, ErrDenied
	}
	if proof.action == action {
		return []string{action}, nil
	}
	return []string{proof.action, action}, nil
}

// CheckMemoryCallerTx adds identity, and only for an actual HTTP entry, admin
// action authorization. Memory's independent self/use/source gates still run.
func CheckMemoryCallerTx(ctx context.Context, tx Tx, scope domain.ProductScope, action string) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore == nil {
		return ErrDenied
	}
	if handled, e := IntegrationGovernanceCallerTx(ctx, tx, domain.Scope{ProductScope: scope, WorldInstanceID: u.world}, action); handled { return e }
	p, e := currentOperatorTx(ctx, u.tx, u.operatorStore, scope)
	if e != nil || p.job != "" {
		return ErrDenied
	}
	for _, k := range operatorKeys(p.principal) {
		if !u.plan[string(lockBytes(k))] {
			return ErrReplan
		}
	}
	actions, e := u.operatorStore.memoryHTTPActions(ctx, scope, action)
	if e != nil {
		return e
	}
	for _, a := range actions {
		if e = operatorActionTx(ctx, u.tx, u.operatorStore, scope, a); e != nil {
			return e
		}
		u.memoryHTTPActions = append(u.memoryHTTPActions, a)
	}
	return nil
}

func (s *Store) BeginMemoryRead(ctx context.Context, scope domain.ProductScope, action, dataSubject string) (*OperatorRead, error) {
	actions, e := s.memoryHTTPActions(ctx, scope, action)
	if e != nil {
		return nil, e
	}
	return s.beginOperatorRead(ctx, scope, actions, LockKey{Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, dataSubject}})
}
