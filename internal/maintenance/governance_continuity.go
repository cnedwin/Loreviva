package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
)

// Independent of paired DB/content files. It is not a world COMMIT or an OPEN seal.
type GovernanceHead struct {
	Sequence uint64
	Hash     string
}
type GovernanceContinuity struct {
	Binding, Ledger, Anchor string
	Head                    GovernanceHead
	Pending                 *GovernanceHead
}

func (c *Coordinator) InitializeGovernanceJournal(ctx context.Context, binding, ledger, anchor string) error {
	if domain.ValidateID(binding) != nil || domain.ValidateID(ledger) != nil || domain.ValidateID(anchor) != nil {
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
	if a.GovernanceContinuity != nil || a.Generation != 1 || a.Phase != "OPEN" || len(a.Participants) != 0 {
		return ErrMaintenance
	}
	b, _ := json.Marshal([]string{a.Installation, ledger, anchor})
	hash := sha256.Sum256(b)
	a.GovernanceContinuity = &GovernanceContinuity{Binding: binding, Ledger: ledger, Anchor: anchor, Head: GovernanceHead{Hash: hex.EncodeToString(hash[:])}}
	return c.save(a)
}

func (p *Participant) GovernanceJournalIdentity(ctx context.Context) (string, error) {
	if e := p.check(ctx, GovernanceWriter); e != nil {
		return "", e
	}
	return p.coordinator.installation, nil
}

// May inspect retained evidence while CLOSED for the existing exclusive replay
// path. This method does not mint admission or authorize any DB/body access.
func (p *Participant) GovernanceJournalContinuity(ctx context.Context, binding, ledger, anchor string) (GovernanceContinuity, error) {
	if p == nil || p.coordinator == nil {
		return GovernanceContinuity{}, ErrMaintenance
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return GovernanceContinuity{}, e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.GovernanceContinuity == nil {
		return GovernanceContinuity{}, ErrMaintenance
	}
	g := *a.GovernanceContinuity
	if g.Binding != binding || g.Ledger != ledger || g.Anchor != anchor {
		return GovernanceContinuity{}, ErrMaintenance
	}
	return g, nil
}
func (p *Participant) BeginGovernanceAppend(ctx context.Context, binding, ledger, anchor string, old, next GovernanceHead) error {
	ctx, leave, e := p.Enter(ctx, GovernanceWriter)
	if e != nil {
		return e
	}
	defer leave()
	h, e := hex.DecodeString(next.Hash)
	if e != nil || len(h) != 32 || old.Sequence == ^uint64(0) || next.Sequence != old.Sequence+1 {
		return ErrMaintenance
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation || a.GovernanceContinuity == nil {
		return ErrMaintenance
	}
	g := a.GovernanceContinuity
	if g.Binding != binding || g.Ledger != ledger || g.Anchor != anchor || g.Head != old || (g.Pending != nil && *g.Pending != next) {
		return ErrMaintenance
	}
	g.Pending = &next
	return p.coordinator.save(a)
}
func (p *Participant) ConfirmGovernanceAppend(ctx context.Context, binding, ledger, anchor string, head GovernanceHead) error {
	ctx, leave, e := p.Enter(ctx, GovernanceWriter)
	if e != nil {
		return e
	}
	defer leave()
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != p.generation || a.GovernanceContinuity == nil {
		return ErrMaintenance
	}
	g := a.GovernanceContinuity
	if g.Binding != binding || g.Ledger != ledger || g.Anchor != anchor {
		return ErrMaintenance
	}
	if g.Pending == nil {
		if g.Head == head {
			return nil
		}
		return ErrMaintenance
	}
	if *g.Pending != head || head.Sequence != g.Head.Sequence+1 {
		return ErrMaintenance
	}
	g.Head = head
	g.Pending = nil
	return p.coordinator.save(a)
}
