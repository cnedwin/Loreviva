package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
)

func OpenBoundLedger(ctx context.Context, dir, installation, ledger, anchor string, p *maintenance.Participant) (*DeletionLedger, error) {
	l, e := ledgerRoot(dir, installation, ledger, anchor, false)
	if e != nil {
		return nil, e
	}
	if e = l.BindContinuity(ctx, p); e != nil {
		return nil, e
	}
	return l, nil
}

// Bound once by actual Local.Open. It verifies existing trusted state; it does
// not initialize, replace, or sign an existing journal's current head.
func (l *DeletionLedger) BindContinuity(ctx context.Context, p *maintenance.Participant) error {
	if l == nil || p == nil || l.participant != nil {
		return ErrPreservation
	}
	id, e := p.GovernanceJournalIdentity(ctx)
	if e != nil || id != l.installation {
		return ErrPreservation
	}
	l.participant = p
	_, e = l.LatestHead(ctx, "bind-current-governance")
	return e
}
func (l *DeletionLedger) verifyContinuity(ctx context.Context, a ledgerAnchor, entries []LedgerEntry) error {
	// Standalone ledger primitive tests do not constitute a Local installation.
	// Local.Open always binds before exposing its ledger to Runtime.
	if l.participant == nil {
		return nil
	}
	g, e := l.participant.GovernanceJournalContinuity(ctx, l.binding, l.ledger, l.anchor)
	if e != nil {
		return ErrPreservation
	}
	actual := maintenance.GovernanceHead{Hash: digest([]string{l.installation, l.ledger, l.anchor})}
	matched := g.Head == actual
	for _, v := range entries {
		actual = maintenance.GovernanceHead{Sequence: v.Sequence, Hash: v.Hash}
		if actual == g.Head {
			matched = true
		}
	}
	if !matched {
		return ErrPreservation
	}
	if g.Pending == nil {
		if g.Head != actual {
			return ErrPreservation
		}
		return nil
	}
	// Only the exact independently reserved append with actual verified bytes
	// can finish an interrupted append. Missing bytes remain closed.
	if *g.Pending != actual || actual.Sequence != g.Head.Sequence+1 {
		return ErrPreservation
	}
	if e = l.participant.ConfirmGovernanceAppend(ctx, l.binding, l.ledger, l.anchor, actual); e != nil {
		return ErrPreservation
	}
	return nil
}

// ReadyDatabase compares the bounded independent G snapshot with exact retained
// application facts. G is held outside SQL row transactions; each lookup uses at
// most100 original IDs and never scans unbounded unrelated DB rows. The journal
// itself has the existing32MiB cap. No positive result is cached across reads.
func (l *DeletionLedger) ReadyDatabase(ctx context.Context, s *store.Store) error {
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, entries, e := l.loadContext(ctx)
	if e != nil {
		return e
	}
	if len(a.Pending) > 0 {
		return ErrPreservation
	}
	var selected []LedgerEntry
	for _, v := range entries {
		if v.Kind == "APPENDED" {
			selected = append(selected, v)
		}
	}
	for start := 0; start < len(selected); start += 100 {
		end := start + 100
		if end > len(selected) {
			end = len(selected)
		}
		want := map[string]LedgerEntry{}
		args := []any{}
		for _, v := range selected[start:end] {
			if len(v.Reservation.Scope) != 5 || v.Binding == nil {
				return ErrPreservation
			}
			want[v.Reservation.ID] = v
			args = append(args, v.Reservation.ID)
		}
		q := `SELECT g.task_id,g.tenant_id,g.product_id,g.world_instance_id,g.data_subject_id,g.owner_subject_id,g.request_hash,g.binding,g.preservation,g.cleanup,t.task_kind,j.operation,t.state,
ac.commit_key,ac.tenant_id,ac.product_id,ac.owner_subject_id,ac.scope_kind,ac.world_instance_id,ac.operation,ac.command_key,ac.payload_hash,ac.phase,
fc.commit_key,fc.tenant_id,fc.product_id,fc.owner_subject_id,fc.scope_kind,fc.world_instance_id,fc.task_id,fc.operation,fc.command_key,fc.payload_hash,fc.phase
FROM memory_governance g JOIN tasks t ON t.tenant_id=g.tenant_id AND t.product_id=g.product_id AND t.task_id=g.task_id
JOIN registered_jobs j ON j.tenant_id=t.tenant_id AND j.product_id=t.product_id AND j.task_id=t.task_id
JOIN commit_receipts ac ON ac.tenant_id=g.tenant_id AND ac.product_id=g.product_id AND ac.owner_subject_id=g.owner_subject_id AND ac.world_instance_id=g.world_instance_id AND ac.operation='MEMORY_GOVERNANCE_ACCEPT' AND ac.payload_hash=g.request_hash
JOIN commit_receipts fc ON fc.tenant_id=g.tenant_id AND fc.product_id=g.product_id AND fc.task_id=g.task_id AND fc.operation='MEMORY_GOVERNANCE'
WHERE g.task_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
		rows, e := s.DB.QueryContext(ctx, q, args...)
		if e != nil {
			return e
		}
		valid := true
		for rows.Next() {
			var id, tenant, product, world, subject, owner, preservation, cleanup, kind, operation, state string
			var request, raw, acceptHash, finishHash []byte
			var ak, at, ap, ao, ask, aw, aop, ack, aph, fk, ft, fp, fo, fsk, fw, ftask, fop, fck, fph string
			if e = rows.Scan(&id, &tenant, &product, &world, &subject, &owner, &request, &raw, &preservation, &cleanup, &kind, &operation, &state, &ak, &at, &ap, &ao, &ask, &aw, &aop, &ack, &acceptHash, &aph, &fk, &ft, &fp, &fo, &fsk, &fw, &ftask, &fop, &fck, &finishHash, &fph); e != nil {
				break
			}
			v, ok := want[id]
			var b Binding
			if !ok || json.Unmarshal(raw, &b) != nil {
				valid = false
				break
			}
			rawHash := sha256.Sum256(raw)
			sc := v.Reservation.Scope
			if tenant != sc[0] || product != sc[1] || world != sc[2] || subject != sc[3] || owner != sc[4] || hex.EncodeToString(request) != v.Reservation.RequestHash || digest(b) != digest(v.Binding) || preservation != "CONFIRMED" || cleanup != "LOCAL_COMPLETE" || kind != "GOVERNANCE" || operation != "MEMORY_GOVERNANCE" || state != "COMMITTED" || ak != content.Identity(id, "accept") || at != tenant || ap != product || ao != owner || ask != "WORLD" || aw != world || aop != "MEMORY_GOVERNANCE_ACCEPT" || aph != "COMMIT" || hex.EncodeToString(acceptHash) != v.Reservation.RequestHash || id != content.Identity(tenant, product, owner, world, "MEMORY_GOVERNANCE", ack) || fk != content.Identity(id, "preserved-clean") || ft != tenant || fp != product || fo != owner || fsk != "WORLD" || fw != world || ftask != id || fop != "MEMORY_GOVERNANCE" || fck != id || fph != "COMMIT" || hex.EncodeToString(finishHash) != hex.EncodeToString(rawHash[:]) {
				valid = false
				break
			}
			delete(want, id)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		if !valid || len(want) != 0 {
			return ErrPreservation
		}
	}
	return nil
}
