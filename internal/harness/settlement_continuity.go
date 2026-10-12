package harness

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/maintenance"
	"sync"
	"sync/atomic"
)

type settlementContinuity struct {
	owner        *MockJournal
	installation string
	generation   uint64
	head         domain.MockHead
	continuity   *maintenance.SettlementContinuity
	closed       atomic.Bool
	once         sync.Once
	close        func()
}

func (e *settlementContinuity) Close() { e.once.Do(func() { e.closed.Store(true); e.close() }) }

// Acquisition and journal verification precede SQL. The same real journal
// mutex remains held until the owning transaction has finished or is unknown.
func (j *MockJournal) ReadSettlementContinuity(ctx context.Context) (domain.SettlementContinuityEvidence, error) {
	ctx, release, e := j.participant.Enter(ctx, maintenance.DeadlineWriter)
	if e != nil {
		return nil, e
	}
	installation, generation, e := j.participant.CurrentInstallation(ctx)
	if e != nil {
		release()
		return nil, e
	}
	close, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		release()
		return nil, e
	}
	anchor, _, e := j.load(ctx)
	if e != nil {
		close()
		release()
		return nil, e
	}
	if anchor.Installation != installation {
		close()
		release()
		return nil, ErrMockContinuity
	}
	continuity, e := j.participant.HoldSettlementContinuity(ctx, j.binding, anchor.Head)
	if e != nil {
		close()
		release()
		return nil, e
	}
	return &settlementContinuity{owner: j, installation: anchor.Installation, generation: generation, head: anchor.Head, continuity: continuity, close: func() { continuity.Close(); close(); release() }}, nil
}

// No filesystem operation under SQL locks. Only this owner's still-live
// concrete held lease is accepted, never a caller's head, boolean or callback.
func (j *MockJournal) CheckSettlementContinuity(e domain.SettlementContinuityEvidence, id domain.SettlementServiceIdentity) (domain.MockHead, error) {
	actual, ok := e.(*settlementContinuity)
	if !ok || actual.owner != j || actual.closed.Load() || actual.installation != j.installation || actual.installation != id.Installation || actual.generation != id.Generation || id.ServiceID != "r0-mock-settlement" || id.Action != "attempt.cost.settle" || id.Policy != "R0_BOUNDED_SETTLEMENT_V1" {
		return domain.MockHead{}, ErrMockContinuity
	}
	if e := actual.continuity.Check(id, actual.head); e != nil {
		return domain.MockHead{}, e
	}
	return actual.head, nil
}
