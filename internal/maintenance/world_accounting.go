package maintenance

import (
	"context"
	"loreviva.local/loreviva/internal/localstate"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
)

// A notification registration is valid only as the complete known delivery
// owner group. Legacy registrations retain their original non-delivery rules.
func deliveryManifest(writers []Writer) bool {
	if len(writers) != 12 {
		return false
	}
	seen := map[Writer]bool{}
	for _, w := range writers {
		if !validWriter(w) || seen[w] {
			return false
		}
		seen[w] = true
	}
	return len(seen) == 12
}
func (p *Participant) checkDeliveryManifest(ctx context.Context) error {
	if p == nil || p.coordinator == nil || !deliveryManifest(p.writers) {
		return ErrMaintenance
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation {
		return ErrMaintenance
	}
	r, ok := a.Participants[p.id]
	if !ok || r.Generation != p.generation || r.Incarnation != p.incarnation || !reflect.DeepEqual(r.Writers, p.writers) || !deliveryManifest(r.Writers) {
		return ErrMaintenance
	}
	return nil
}

// EnterDelivery always owns a new actual parent handle. Existing or released
// permit contexts cannot be borrowed as a new IO stage, even on the same C.
func (p *Participant) EnterDelivery(ctx context.Context, s *store.Store) (context.Context, func(), error) {
	if ctx.Value(permitKey{}) != nil {
		return ctx, nil, ErrMaintenance
	}
	next, release, e := p.Enter(ctx, APIWriter)
	if e != nil {
		return ctx, nil, e
	}
	if e = p.CheckDelivery(next, s); e != nil {
		release()
		return ctx, nil, e
	}
	return next, release, nil
}
func (p *Participant) CheckDelivery(ctx context.Context, s *store.Store) error {
	if p == nil || s == nil || s.Guard != p || s.ConfigGuard != p || s.WorldAccountingAuthority != p {
		return ErrMaintenance
	}
	id, g, e := p.CurrentInstallation(ctx)
	if e != nil {
		return e
	}
	if e = p.checkDeliveryManifest(ctx); e != nil {
		return e
	}
	var actual, db, files, root string
	var generation uint64
	e = s.DB.QueryRowContext(ctx, "SELECT installation_id,generation,database_binding,files_binding,coordinator_binding FROM memory_installation WHERE singleton=1").Scan(&actual, &generation, &db, &files, &root)
	if e != nil || actual != id || generation != g || db != p.coordinator.database || files != p.coordinator.files || root != localstate.RootHash(p.coordinator.dir) {
		return ErrMaintenance
	}
	return nil
}

// Covers the actual R0 owning modules. Shared admission is retained for the
// complete migration transaction; writer/installation checks precede DB locks.
func (p *Participant) EnterWorldAccounting(ctx context.Context) (context.Context, store.WorldAccountingIdentity, func(), error) {
	ctx, release, e := p.Enter(ctx, StoreWriter)
	if e != nil {
		return ctx, store.WorldAccountingIdentity{}, nil, e
	}
	for _, w := range []Writer{APIWriter, StoreWriter, WorkerWriter, FileWriter, GovernanceWriter, HarnessWriter, MockWriter, QueryWriter, CleanupWriter, DeadlineWriter, ConfigWriter} {
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
	if e != nil || state.Phase != "OPEN" || state.Generation != p.generation {
		release()
		return ctx, store.WorldAccountingIdentity{}, nil, ErrMaintenance
	}
	// CONFIG shares this installation's actual admission handle. A second API
	// or worker has its own incarnation; it need not borrow this participant's
	// nonce. Every registration must nevertheless belong to the live generation.
	if _, _, e = p.CurrentInstallation(ctx); e != nil {
		release()
		return ctx, store.WorldAccountingIdentity{}, nil, e
	}
	for _, registration := range state.Participants {
		if registration.Generation != p.generation || registration.Incarnation == "" || len(registration.Writers) == 0 {
			release()
			return ctx, store.WorldAccountingIdentity{}, nil, ErrMaintenance
		}
		for _, w := range registration.Writers {
			if !validWriter(w) || (w == NotificationWriter && !deliveryManifest(registration.Writers)) {
				release()
				return ctx, store.WorldAccountingIdentity{}, nil, ErrMaintenance
			}
		}
	}
	return ctx, store.WorldAccountingIdentity{Installation: p.coordinator.installation, Generation: p.generation}, release, nil
}
