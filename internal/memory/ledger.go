package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

const MaxJournalBytes = 32 << 20
const CompletionAllowance = 512 << 10

var ErrPreservation = errors.New("independent preservation unconfirmed; query original task")

type Reservation struct {
	ID, RequestHash string
	Scope           []string
}

// Binding has no field for replacement bytes, reason, model traces or credentials.
type Binding struct {
	SelectorHash, SourceEpoch, Action, TargetKind, ReplacementID, ReplacementHash string
	Cutoff                                                                        uint64
	Targets                                                                       []string
	Uses                                                                          []Use
	Revisions                                                                     map[string]uint64
}
type Head struct {
	Installation, Ledger, Anchor string
	Sequence                     uint64
	Hash, Challenge              string
}
type LedgerEntry struct {
	Sequence             uint64
	PrevHash, Hash, Kind string
	Reservation          Reservation
	Binding              *Binding
}
type ledgerAnchor struct {
	Installation, Ledger, Anchor string
	Sequence                     uint64
	Hash                         string
	Pending                      map[string]string
}
type DeletionLedger struct {
	participant                  *maintenance.Participant
	binding                      string
	root                         *localstate.Root
	installation, ledger, anchor string
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func CreateLedger(dir, installation, ledger, anchor string) (*DeletionLedger, error) {
	l, e := ledgerRoot(dir, installation, ledger, anchor, true)
	if e != nil {
		return nil, e
	}
	release, e := l.root.Lock(context.Background(), "state.lock")
	if e != nil {
		return nil, e
	}
	defer release()
	a := ledgerAnchor{Installation: installation, Ledger: ledger, Anchor: anchor, Hash: digest([]string{installation, ledger, anchor}), Pending: map[string]string{}}
	b, _ := json.Marshal(a)
	if e = l.root.Create("journal.jsonl", nil); e != nil {
		return nil, e
	}
	if e = l.root.Create("anchor.json", b); e != nil {
		return nil, e
	}
	return l, nil
}
func ledgerRoot(dir, installation, ledger, anchor string, create bool) (*DeletionLedger, error) {
	for _, id := range []string{installation, ledger, anchor} {
		if domain.ValidateID(id) != nil {
			return nil, ErrPreservation
		}
	}
	r, e := localstate.Open(dir, create)
	if e != nil {
		return nil, e
	}
	return &DeletionLedger{root: r, installation: installation, ledger: ledger, anchor: anchor, binding: localstate.RootHash(dir)}, nil
}
func OpenLedger(dir, installation, ledger, anchor string) (*DeletionLedger, error) {
	l, e := ledgerRoot(dir, installation, ledger, anchor, false)
	if e != nil {
		return nil, e
	}
	_, e = l.LatestHead(context.Background(), "open")
	return l, e
}
func (l *DeletionLedger) load() (ledgerAnchor, []LedgerEntry, error) {
	return l.loadContext(context.Background())
}
func (l *DeletionLedger) loadContext(ctx context.Context) (ledgerAnchor, []LedgerEntry, error) {
	var a ledgerAnchor
	b, e := l.root.Read("anchor.json")
	if e != nil || json.Unmarshal(b, &a) != nil {
		return a, nil, ErrPreservation
	}
	if a.Installation != l.installation || a.Ledger != l.ledger || a.Anchor != l.anchor || a.Pending == nil {
		return a, nil, ErrPreservation
	}
	raw, e := l.root.Read("journal.jsonl")
	if e != nil || len(raw) > MaxJournalBytes {
		return a, nil, ErrPreservation
	}
	entries := []LedgerEntry{}
	head := digest([]string{l.installation, l.ledger, l.anchor})
	pending := map[string]string{}
	seen := map[string]LedgerEntry{}
	matched := a.Sequence == 0 && a.Hash == head && len(a.Pending) == 0
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return a, nil, ErrPreservation
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var v LedgerEntry
		if json.Unmarshal(line, &v) != nil {
			return a, nil, ErrPreservation
		}
		hash := v.Hash
		v.Hash = ""
		if v.Sequence != uint64(len(entries)+1) || v.PrevHash != head || digest(v) != hash {
			return a, nil, ErrPreservation
		}
		v.Hash = hash
		if v.Kind == "RESERVED" {
			if _, exists := seen[v.Reservation.ID]; exists {
				return a, nil, ErrPreservation
			}
			pending[v.Reservation.ID] = v.Reservation.RequestHash
			seen[v.Reservation.ID] = v
		} else if v.Kind == "APPENDED" || v.Kind == "REJECTED" {
			r, ok := seen[v.Reservation.ID]
			if !ok || digest(r.Reservation) != digest(v.Reservation) || pending[v.Reservation.ID] == "" || (v.Kind == "APPENDED" && v.Binding == nil) {
				return a, nil, ErrPreservation
			}
			delete(pending, v.Reservation.ID)
		} else {
			return a, nil, ErrPreservation
		}
		entries = append(entries, v)
		head = hash
		if a.Sequence == v.Sequence {
			if a.Hash != hash || digest(a.Pending) != digest(pending) {
				return a, nil, ErrPreservation
			}
			matched = true
		}
	}
	if !matched || a.Sequence > uint64(len(entries)) {
		return a, nil, ErrPreservation
	}
	if e = l.verifyContinuity(ctx, a, entries); e != nil {
		return a, nil, e
	}
	// Advance a lagging anchor only from a verified continuous journal tail.
	if a.Sequence < uint64(len(entries)) {
		a.Sequence = uint64(len(entries))
		a.Hash = head
		a.Pending = pending
		b, _ = json.Marshal(a)
		if e = l.root.Replace("anchor.json", b); e != nil {
			return a, nil, e
		}
	}
	return a, entries, nil
}
func (l *DeletionLedger) append(ctx context.Context, a ledgerAnchor, v LedgerEntry) error {
	current, readError := l.root.Read("journal.jsonl")
	if readError != nil {
		return readError
	}
	v.Sequence = a.Sequence + 1
	v.PrevHash = a.Hash
	v.Hash = ""
	v.Hash = digest(v)
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	size := len(b) + 1
	if v.Kind == "RESERVED" {
		if len(current)+size+(len(a.Pending)+1)*CompletionAllowance > MaxJournalBytes {
			return ErrPreservation
		}
	} else if size > CompletionAllowance {
		return ErrPreservation
	}
	if len(current)+size > MaxJournalBytes {
		return ErrPreservation
	}
	if l.participant != nil {
		if e = l.participant.BeginGovernanceAppend(ctx, l.binding, l.ledger, l.anchor, maintenance.GovernanceHead{Sequence: a.Sequence, Hash: a.Hash}, maintenance.GovernanceHead{Sequence: v.Sequence, Hash: v.Hash}); e != nil {
			return ErrPreservation
		}
	}
	if e = l.root.Append("journal.jsonl", append(b, '\n')); e != nil {
		return e
	}
	a.Sequence = v.Sequence
	a.Hash = v.Hash
	if v.Kind == "RESERVED" {
		a.Pending[v.Reservation.ID] = v.Reservation.RequestHash
	} else {
		delete(a.Pending, v.Reservation.ID)
	}
	b, _ = json.Marshal(a)
	if e = l.root.Replace("anchor.json", b); e != nil {
		return e
	}
	if l.participant != nil {
		if e = l.participant.ConfirmGovernanceAppend(ctx, l.binding, l.ledger, l.anchor, maintenance.GovernanceHead{Sequence: a.Sequence, Hash: a.Hash}); e != nil {
			return ErrPreservation
		}
	}
	return nil
}
func (l *DeletionLedger) ReserveOnce(ctx context.Context, r Reservation) error {
	_, e := l.reserve(ctx, r)
	return e
}
func (l *DeletionLedger) reserve(ctx context.Context, r Reservation) (bool, error) {
	if l.participant != nil {
		if _, err := l.participant.GovernanceJournalIdentity(ctx); err != nil {
			return false, err
		}
	}
	if l.participant != nil {
		var leave func()
		var err error
		ctx, leave, err = l.participant.Enter(ctx, maintenance.GovernanceWriter)
		if err != nil {
			return false, err
		}
		defer leave()
	}
	if domain.ValidateID(r.ID) != nil || r.RequestHash == "" || len(r.RequestHash) > 64 || len(r.Scope) != 5 {
		return false, ErrPreservation
	}
	for _, id := range r.Scope {
		if domain.ValidateID(id) != nil {
			return false, ErrPreservation
		}
	}
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return false, e
	}
	defer done()
	a, entries, e := l.loadContext(ctx)
	if e != nil {
		return false, e
	}
	for _, v := range entries {
		if v.Reservation.ID == r.ID && v.Kind == "REJECTED" {
			return false, ErrPreservation
		}
	}
	for _, v := range entries {
		if v.Reservation.ID == r.ID {
			if digest(v.Reservation) != digest(r) {
				return false, ErrPreservation
			}
			return false, nil
		}
	}
	e = l.append(ctx, a, LedgerEntry{Kind: "RESERVED", Reservation: r})
	return e == nil, e
}

func (l *DeletionLedger) AppendOnce(ctx context.Context, id string, b Binding) error {
	if l.participant != nil {
		if _, err := l.participant.GovernanceJournalIdentity(ctx); err != nil {
			return err
		}
	}
	if l.participant != nil {
		var leave func()
		var err error
		ctx, leave, err = l.participant.Enter(ctx, maintenance.GovernanceWriter)
		if err != nil {
			return err
		}
		defer leave()
	}
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, entries, e := l.loadContext(ctx)
	if e != nil {
		return e
	}
	var original *Reservation
	for _, v := range entries {
		if v.Reservation.ID == id {
			if v.Kind == "REJECTED" {
				return ErrPreservation
			}
			r := v.Reservation
			original = &r
			if v.Kind == "APPENDED" {
				if digest(v.Binding) != digest(b) {
					return ErrPreservation
				}
				return nil
			}
		}
	}
	if original == nil || b.SourceEpoch == "" || b.SelectorHash == "" {
		return ErrPreservation
	}
	return l.append(ctx, a, LedgerEntry{Kind: "APPENDED", Reservation: *original, Binding: &b})
}
func (l *DeletionLedger) LatestHead(ctx context.Context, challenge string) (Head, error) {
	if challenge == "" || len(challenge) > 128 {
		return Head{}, ErrPreservation
	}
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return Head{}, e
	}
	defer done()
	a, _, e := l.loadContext(ctx)
	return Head{a.Installation, a.Ledger, a.Anchor, a.Sequence, a.Hash, challenge}, e
}
func (l *DeletionLedger) VerifyHead(ctx context.Context, h Head) error {
	fresh, e := l.LatestHead(ctx, h.Challenge)
	if e != nil || fresh != h {
		return ErrPreservation
	}
	return nil
}
func (l *DeletionLedger) Ready(ctx context.Context) error {
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, _, e := l.loadContext(ctx)
	if e != nil {
		return e
	}
	if len(a.Pending) > 0 {
		return ErrPreservation
	}
	return nil
}
func (l *DeletionLedger) Lookup(ctx context.Context, id string) (LedgerEntry, error) {
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return LedgerEntry{}, e
	}
	defer done()
	_, entries, e := l.loadContext(ctx)
	if e != nil {
		return LedgerEntry{}, e
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Reservation.ID == id {
			return entries[i], nil
		}
	}
	return LedgerEntry{}, ErrPreservation
}
func (l *DeletionLedger) ReadAfter(ctx context.Context, h Head, after uint64, limit int) ([]LedgerEntry, error) {
	if limit < 1 || limit > 100 || after > h.Sequence {
		return nil, ErrPreservation
	}
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return nil, e
	}
	defer done()
	a, entries, e := l.loadContext(ctx)
	if e != nil || a.Sequence != h.Sequence || a.Hash != h.Hash || a.Installation != h.Installation || a.Ledger != h.Ledger || a.Anchor != h.Anchor {
		return nil, ErrPreservation
	}
	end := after + uint64(limit)
	if end > h.Sequence {
		end = h.Sequence
	}
	return append([]LedgerEntry{}, entries[after:end]...), nil
}

type intakePermit struct {
	ledger *DeletionLedger
	active bool
}

func (l *DeletionLedger) beginIntake(ctx context.Context) (*intakePermit, func(), error) {
	done, e := l.root.Lock(ctx, "governance.lock")
	if e != nil {
		return nil, nil, e
	}
	p := &intakePermit{l, true}
	return p, func() { p.active = false; done() }, nil
}
func (l *DeletionLedger) SealRejectedOnce(ctx context.Context, p *intakePermit, id string, proof *store.RejectionProof) error {
	if l.participant != nil {
		if _, err := l.participant.GovernanceJournalIdentity(ctx); err != nil {
			return err
		}
	}
	if l.participant != nil {
		var leave func()
		var err error
		ctx, leave, err = l.participant.Enter(ctx, maintenance.GovernanceWriter)
		if err != nil {
			return err
		}
		defer leave()
	}
	if p == nil || !p.active || p.ledger != l {
		return ErrPreservation
	}
	done, e := l.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, entries, e := l.loadContext(ctx)
	if e != nil {
		return e
	}
	var reservation *Reservation
	for _, entry := range entries {
		if entry.Reservation.ID != id {
			continue
		}
		if entry.Kind == "APPENDED" {
			return ErrPreservation
		}
		if entry.Kind == "REJECTED" {
			return nil
		}
		r := entry.Reservation
		reservation = &r
	}
	if reservation == nil || !proof.Matches(l.installation, id, reservation.RequestHash) {
		return ErrPreservation
	}
	return l.append(ctx, a, LedgerEntry{Kind: "REJECTED", Reservation: *reservation})
}

// ReplayDeny applies only retained negative evidence under the same local
// coordinator's opaque exclusive drain proof. It never authorizes reopening.
func (s *MemoryService) ReplayDeny(ctx context.Context, p *maintenance.ClosedPermit, h Head) error {
	installation, generation, e := p.MemoryReplayIdentity(ctx)
	if e != nil || installation != h.Installation || s.Ledger == nil {
		return ErrPreservation
	}
	if e = s.Ledger.Ready(ctx); e != nil {
		return e
	}
	if e = s.Ledger.VerifyHead(ctx, h); e != nil {
		return e
	}
	missing := false
	for after := uint64(0); after < h.Sequence; {
		entries, e := s.Ledger.ReadAfter(ctx, h, after, 100)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			after = entry.Sequence
			if entry.Kind != "APPENDED" {
				continue
			}
			scope := entry.Reservation.Scope
			if len(scope) != 5 {
				return ErrPreservation
			}
			n := Namespace{Scope: domain.Scope{ProductScope: domain.ProductScope{TenantID: scope[0], ProductID: scope[1], SubjectID: scope[4]}, WorldInstanceID: scope[2]}, DataSubjectID: scope[3]}
			if n.Validate() != nil {
				return ErrPreservation
			}
			var raw []byte
			if e = s.Store.DB.QueryRowContext(ctx, "SELECT document FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID).Scan(&raw); e != nil {
				return e
			}
			var a Aggregate
			if json.Unmarshal(raw, &a) != nil {
				return ErrPreservation
			}
			binding := entry.Binding
			if binding == nil {
				return ErrPreservation
			}
			needs := replayBlock(&a, *binding)
			missing = missing || needs
			a.Epoch = content.Identity("restored-source-epoch", installation, h.Hash, n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID)
			a.Clean()
			document, e := json.Marshal(a)
			if e != nil {
				return e
			}
			if _, _, e = p.MemoryReplayIdentity(ctx); e != nil {
				return e
			}
			if e = s.Ledger.VerifyHead(ctx, h); e != nil {
				return e
			}
			_, assets, e := s.assetLocks(ctx, n)
			if e != nil {
				return e
			}
			replayAssets := []store.MemoryReplayAsset{}
			for _, asset := range assets {
				var actor, node string
				if e = s.Store.DB.QueryRowContext(ctx, "SELECT actor_id,node_id FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND storage_key=?", n.TenantID, n.ProductID, asset.Storage).Scan(&actor, &node); e != nil {
					return e
				}
				permitted := false
				for _, v := range a.Allowed(actor, Private, PrivateResponse, time.Now()) {
					if v.ID == node {
						permitted = true
					}
				}
				var resultRaw []byte
				resultErr := s.Store.DB.QueryRowContext(ctx, "SELECT document FROM harness_result_sources WHERE tenant_id=? AND product_id=? AND storage_key=?", n.TenantID, n.ProductID, asset.Storage).Scan(&resultRaw)
				if resultErr == nil {
					permitted = resultSourcesAllowed(resultRaw, a, actor, time.Now())
				} else if !errors.Is(resultErr, sql.ErrNoRows) {
					return resultErr
				}
				replayAssets = append(replayAssets, store.MemoryReplayAsset{ID: asset.ID, Storage: asset.Storage, Delete: !permitted})
			}
			permit, e := p.MemoryStorePermit(ctx)
			if e != nil {
				return e
			}
			write := store.MemoryReplayWrite{Permit: permit, Assets: replayAssets, Scope: n.Scope, Subject: n.DataSubjectID, Installation: installation, Generation: generation, Task: entry.Reservation.ID, Sequence: entry.Sequence, Hash: entry.Hash, Expected: sha256.Sum256(raw), Document: document, NeedsReplacement: needs}
			if e = s.Store.ReplayMemoryDeny(ctx, write); e != nil {
				return e
			}
			for _, asset := range replayAssets {
				if asset.Delete {
					if s.Files == nil {
						return ErrPreservation
					}
					if e = s.Files.PurgeDerivedReplay(ctx, p, n.ProductScope, asset.Storage); e != nil {
						return e
					}
				}
			}
			if e = s.Store.ConfirmMemoryReplayCleanup(ctx, write); e != nil {
				return e
			}
		}
	}
	if missing {
		return ErrPreservation
	}
	return s.Ledger.VerifyHead(ctx, h)
}
func replayBlock(a *Aggregate, b Binding) bool {
	targets := map[string]bool{}
	for _, id := range b.Targets {
		targets[id] = true
	}
	nodes := map[string]bool{}
	if b.Action == "SET_USE_POLICY" {
		if b.TargetKind == "DISABLE" {
			for _, u := range b.Uses {
				if !contains(a.Disabled, u) {
					a.Disabled = append(a.Disabled, u)
				}
			}
			for id, n := range a.Nodes {
				if len(n.Parents) > 0 {
					for _, u := range b.Uses {
						if contains(n.Uses, u) {
							nodes[id] = true
						}
					}
				}
			}
		}
	} else {
		if b.TargetKind == "SOURCE" {
			for id := range targets {
				v, ok := a.Sources[id]
				if !ok {
					v = Source{ID: id, Blocked: true, Revision: 1, Epoch: b.SourceEpoch}
				}
				if b.Action == "REVOKE" && len(b.Uses) > 0 {
					v.Uses = removeUses(v.Uses, b.Uses)
				} else {
					v.Blocked = true
				}
				a.Sources[id] = v
			}
			for id, n := range a.Nodes {
				for _, source := range n.Sources {
					if targets[source] {
						nodes[id] = true
					}
				}
			}
		} else {
			for id := range targets {
				nodes[id] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for id, n := range a.Nodes {
			if nodes[id] {
				continue
			}
			for _, parent := range n.Parents {
				if nodes[parent] {
					nodes[id] = true
					changed = true
					break
				}
			}
		}
	}
	for id := range nodes {
		n, ok := a.Nodes[id]
		if !ok {
			n = MemoryRecord{ID: id, Blocked: true, Revision: 1}
		}
		if b.Action == "REVOKE" && len(b.Uses) > 0 {
			n.Uses = removeUses(n.Uses, b.Uses)
			if len(n.Uses) == 0 {
				n.Blocked = true
			}
		} else {
			n.Blocked = true
		}
		a.Nodes[id] = n
	}
	if b.ReplacementID != "" {
		v, ok := a.Nodes[b.ReplacementID]
		return !ok || digest(v.Body) != b.ReplacementHash
	}
	return false
}

type ReplayCandidate struct {
	head         Head
	installation string
	generation   uint64
	candidate    *maintenance.GovernanceCandidate
}

// SealReplayOpenCandidate proves only G replay completeness. There is no
// production positive reopen path until Task11 supplies independent C.
func (s *MemoryService) SealReplayOpenCandidate(ctx context.Context, p *maintenance.ClosedPermit) (*ReplayCandidate, error) {
	installation, generation, e := p.MemoryReplayIdentity(ctx)
	if e != nil {
		return nil, e
	}
	if e = s.Ledger.Ready(ctx); e != nil {
		return nil, e
	}
	var unpurged bool
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_asset_lineage WHERE delete_intent=1 AND purged=0)").Scan(&unpurged); e != nil || unpurged {
		return nil, ErrPreservation
	}
	h, e := s.Ledger.LatestHead(ctx, "replay-candidate")
	if e != nil || h.Installation != installation {
		return nil, ErrPreservation
	}
	for after := uint64(0); after < h.Sequence; {
		entries, e := s.Ledger.ReadAfter(ctx, h, after, 100)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			after = entry.Sequence
			if entry.Kind != "APPENDED" {
				continue
			}
			var hash, state string
			if e = s.Store.DB.QueryRowContext(ctx, "SELECT ledger_hash,state FROM memory_replay WHERE installation_id=? AND ledger_sequence=?", installation, entry.Sequence).Scan(&hash, &state); e != nil || hash != entry.Hash || state != "CONFIRMED" {
				return nil, ErrPreservation
			}
		}
	}
	if e = s.Ledger.VerifyHead(ctx, h); e != nil {
		return nil, e
	}
	candidate, e := p.SealGovernance(ctx, h.Ledger, h.Anchor, h.Hash, h.Sequence)
	if e != nil {
		return nil, e
	}
	return &ReplayCandidate{h, installation, generation, candidate}, nil
}

func (s *MemoryService) BindReplayOpenCandidate(ctx context.Context, p *maintenance.ClosedPermit, c *ReplayCandidate) (*store.MemoryCandidateReceipt, error) {
	if c == nil || s.Ledger == nil {
		return nil, ErrPreservation
	}
	if e := s.Ledger.VerifyHead(ctx, c.head); e != nil {
		return nil, e
	}
	b, e := p.GovernanceBinding(ctx, c.candidate)
	if e != nil {
		return nil, e
	}
	permit, e := p.MemoryStorePermit(ctx)
	if e != nil {
		return nil, e
	}
	return s.Store.BindMemoryGovernanceCandidate(ctx, permit, b)
}
func (s *MemoryService) ConfirmGovernanceReady(ctx context.Context, p *maintenance.ClosedPermit, c *ReplayCandidate, receipt *store.MemoryCandidateReceipt) error {
	if c == nil || s.Ledger == nil {
		return ErrPreservation
	}
	if e := s.Ledger.Ready(ctx); e != nil {
		return e
	}
	if e := s.Ledger.VerifyHead(ctx, c.head); e != nil {
		return e
	}
	return p.ConfirmGovernanceReady(ctx, c.candidate, receipt)
}
