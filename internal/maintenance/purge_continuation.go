package maintenance

import "context"

// Only the fixed Store purge finisher calls this; no new admission is minted.
// The original live shared lease prevents CLOSED/target replacement until ACK.
func (p *Participant) EnterRehearsalPurgeACK(ctx context.Context) (context.Context, func(), error) {
	token, ok := ctx.Value(permitKey{}).(*permit)
	if !ok || token.participant != p {
		return ctx, nil, ErrMaintenance
	}
	token.mu.Lock()
	defer token.mu.Unlock()
	if !token.live {
		return ctx, nil, ErrMaintenance
	}
	release, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return ctx, nil, e
	}
	defer release()
	a, e := p.coordinator.load()
	if e != nil {
		return ctx, nil, e
	}
	valid := a.Phase == "OPEN" && a.Generation == p.generation || a.Phase == "CLOSING" && p.generation != ^uint64(0) && a.Generation == p.generation+1 && a.Plan != ""
	r, ok := a.Participants[p.id]
	if !valid || !ok || r.Generation != p.generation || r.Incarnation != p.incarnation {
		return ctx, nil, ErrMaintenance
	}
	hasStore, hasFiles := false, false
	for _, w := range r.Writers {
		hasStore = hasStore || w == StoreWriter
		hasFiles = hasFiles || w == FileWriter
	}
	if !hasStore || !hasFiles {
		return ctx, nil, ErrMaintenance
	}
	return ctx, func() {}, nil
}
