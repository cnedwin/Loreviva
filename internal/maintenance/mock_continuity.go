package maintenance

import (
	"context"
	"encoding/hex"
	"loreviva.local/loreviva/internal/domain"
)

type MockContinuity struct {
	Binding string
	Head    domain.MockHead
	Pending *domain.MockHead
}

// InitializeMockJournal is a fresh-install action only. Ordinary Open never
// recreates a missing independent authority or signs an existing journal tail.
func (c *Coordinator) InitializeMockJournal(ctx context.Context, binding string) error {
	if domain.ValidateID(binding) != nil {
		return ErrMaintenance
	}
	done, e := c.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := c.load()
	if e != nil {
		return e
	}
	if a.Mock != nil || a.Generation != 1 || a.Phase != "OPEN" || len(a.Participants) != 0 {
		return ErrMaintenance
	}
	a.Mock = &MockContinuity{Binding: binding, Head: domain.MockGenesis(a.Installation, binding)}
	return c.save(a)
}
func (p *Participant) MockIdentity(ctx context.Context) (string, uint64, error) {
	if e := p.check(ctx, MockWriter); e != nil {
		return "", 0, e
	}
	return p.coordinator.installation, p.generation, nil
}
func (p *Participant) MockContinuity(ctx context.Context, binding string) (MockContinuity, error) {
	ctx, release, e := p.Enter(ctx, MockWriter)
	if e != nil {
		return MockContinuity{}, e
	}
	defer release()
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return MockContinuity{}, e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation || a.Mock == nil || a.Mock.Binding != binding {
		return MockContinuity{}, ErrMaintenance
	}
	return *a.Mock, nil
}

// Reserve the exact next hash before the journal append. The independent
// coordinator can distinguish a known append gap from an untrusted old head.
func (p *Participant) BeginMockAppend(ctx context.Context, binding string, old, next domain.MockHead) error {
	ctx, release, e := p.Enter(ctx, MockWriter)
	if e != nil {
		return e
	}
	defer release()
	hash, decodeError := hex.DecodeString(next.Hash)
	if old.Sequence == ^uint64(0) || next.Sequence != old.Sequence+1 || decodeError != nil || len(hash) != 32 {
		return ErrMaintenance
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation || a.Mock == nil || a.Mock.Binding != binding || a.Mock.Head != old {
		return ErrMaintenance
	}
	if a.Mock.Pending != nil && *a.Mock.Pending != next {
		return ErrMaintenance
	}
	a.Mock.Pending = &next
	return p.coordinator.save(a)
}
func (p *Participant) ConfirmMockAppend(ctx context.Context, binding string, head domain.MockHead) error {
	ctx, release, e := p.Enter(ctx, MockWriter)
	if e != nil {
		return e
	}
	defer release()
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation || a.Mock == nil || a.Mock.Binding != binding {
		return ErrMaintenance
	}
	if a.Mock.Pending == nil {
		if a.Mock.Head == head {
			return nil
		}
		return ErrMaintenance
	}
	if *a.Mock.Pending != head {
		return ErrMaintenance
	}
	a.Mock.Head = head
	a.Mock.Pending = nil
	return p.coordinator.save(a)
}

// The fixed local participant mints this transient admission. It is never a
// caller-supplied worker name, boolean, persisted bearer token or public API.
func (p *Participant) EnterOriginalQuery(ctx context.Context) (context.Context, domain.QueryServiceIdentity, func(), error) {
	ctx, release, e := p.Enter(ctx, QueryWriter)
	if e != nil {
		return ctx, domain.QueryServiceIdentity{}, nil, e
	}
	return ctx, domain.QueryServiceIdentity{Installation: p.coordinator.installation, Generation: p.generation, ServiceID: "r0-mock-original-query", Action: "attempt.cost.query", Policy: "R0_ORIGINAL_MOCK_COST_V1"}, release, nil
}

func (p *Participant) EnterDeadlineSettlement(ctx context.Context) (context.Context, domain.SettlementServiceIdentity, func(), error) {
	ctx, release, e := p.Enter(ctx, DeadlineWriter)
	if e != nil {
		return ctx, domain.SettlementServiceIdentity{}, nil, e
	}
	return ctx, domain.SettlementServiceIdentity{Installation: p.coordinator.installation, Generation: p.generation, ServiceID: "r0-mock-settlement", Action: "attempt.cost.settle", Policy: "R0_BOUNDED_SETTLEMENT_V1"}, release, nil
}
