package maintenance

import (
	"context"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// Covers the actual R0 owning modules. Shared admission is retained for the
// complete migration transaction; writer/installation checks precede DB locks.
func (p *Participant) EnterWorldAccounting(ctx context.Context) (context.Context, store.WorldAccountingIdentity, func(), error) {
	ctx, release, e := p.Enter(ctx, StoreWriter)
	if e != nil {
		return ctx, store.WorldAccountingIdentity{}, nil, e
	}
	for _, w := range []Writer{APIWriter, StoreWriter, WorkerWriter, FileWriter, GovernanceWriter, HarnessWriter, MockWriter, QueryWriter, CleanupWriter, DeadlineWriter} {
		if e = p.check(ctx, w); e != nil {
			release()
			return ctx, store.WorldAccountingIdentity{}, nil, e
		}
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		release()
		return ctx, store.WorldAccountingIdentity{}, nil, e
	}
	state, e := p.coordinator.load()
	done()
	if e != nil {
		release()
		return ctx, store.WorldAccountingIdentity{}, nil, e
	}
	for _, registration := range state.Participants {
		for _, w := range registration.Writers {
			if w == NotificationWriter || w == ConfigWriter {
				release()
				return ctx, store.WorldAccountingIdentity{}, nil, ErrMaintenance
			}
		}
	}
	return ctx, store.WorldAccountingIdentity{Installation: p.coordinator.installation, Generation: p.generation}, release, nil
}
