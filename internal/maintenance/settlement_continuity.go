package maintenance

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"sync"
	"sync/atomic"
)

// SettlementContinuity pins the original OPEN installation and trusted Mock
// head while a bounded settlement transaction runs. Acquisition is before SQL,
// after the journal lock; inspection never performs filesystem IO.
type SettlementContinuity struct {
	installation string
	generation   uint64
	head         domain.MockHead
	closed       atomic.Bool
	once         sync.Once
	release      func()
}

func (l *SettlementContinuity) Close() { l.once.Do(func() { l.closed.Store(true); l.release() }) }
func (l *SettlementContinuity) Check(id domain.SettlementServiceIdentity, head domain.MockHead) error {
	if l == nil || l.closed.Load() || l.installation != id.Installation || l.generation != id.Generation || l.head != head || id.ServiceID != "r0-mock-settlement" || id.Action != "attempt.cost.settle" || id.Policy != "R0_BOUNDED_SETTLEMENT_V1" {
		return ErrMaintenance
	}
	return nil
}

func (p *Participant) HoldSettlementContinuity(ctx context.Context, binding string, head domain.MockHead) (*SettlementContinuity, error) {
	token, ok := ctx.Value(permitKey{}).(*permit)
	if !ok || token.participant != p {
		return nil, ErrMaintenance
	}
	token.mu.Lock()
	live := token.live
	token.mu.Unlock()
	if !live {
		return nil, ErrMaintenance
	}
	release, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return nil, e
	}
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation || a.Installation != p.coordinator.installation || a.Mock == nil || a.Mock.Binding != binding || a.Mock.Head != head || a.Mock.Pending != nil {
		release()
		return nil, ErrMaintenance
	}
	r, ok := a.Participants[p.id]
	allowed := false
	for _, w := range r.Writers {
		if w == DeadlineWriter {
			allowed = true
		}
	}
	if !ok || !allowed || r.Generation != p.generation || r.Incarnation != p.incarnation {
		release()
		return nil, ErrMaintenance
	}
	return &SettlementContinuity{installation: a.Installation, generation: a.Generation, head: head, release: release}, nil
}
