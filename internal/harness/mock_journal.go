package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/wire"
	"sync"
	"sync/atomic"
)

var ErrMockContinuity = errors.New("original Mock identity or independent current head unproven")

type mockAnchor struct {
	Installation, Binding string
	Head                  domain.MockHead
}
type mockEntry struct {
	Sequence       uint64
	Previous, Hash string
	Result         domain.MockResultMetadata
	Bill           *domain.MockBillItem         `json:",omitempty"`
	Usage          *domain.MockUsageMeasurement `json:",omitempty"`
}
type MockJournal struct {
	root                  *localstate.Root
	installation, binding string
	participant           *maintenance.Participant
}

func mockHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func CreateMockJournal(dir, installation string) error {
	r, e := localstate.Open(dir, true)
	if e != nil {
		return e
	}
	binding := localstate.RootHash(dir)
	done, e := r.Lock(context.Background(), "state.lock")
	if e != nil {
		return e
	}
	defer done()
	if domain.ValidateID(installation) != nil {
		return ErrMockContinuity
	}
	a := mockAnchor{Installation: installation, Binding: binding, Head: domain.MockGenesis(installation, binding)}
	b, _ := json.Marshal(a)
	if e = r.Create("journal.jsonl", nil); e != nil {
		return e
	}
	return r.Create("anchor.json", b)
}
func OpenMockJournal(ctx context.Context, dir string, p *maintenance.Participant) (*MockJournal, error) {
	installation, _, e := p.MockIdentity(ctx)
	if e != nil {
		return nil, e
	}
	r, e := localstate.Open(dir, false)
	if e != nil {
		return nil, e
	}
	j := &MockJournal{root: r, installation: installation, binding: localstate.RootHash(dir), participant: p}
	if e = j.Ready(ctx); e != nil {
		return nil, e
	}
	return j, nil
}
func (j *MockJournal) Ready(ctx context.Context) error {
	ctx, release, e := j.participant.Enter(ctx, maintenance.MockWriter)
	if e != nil {
		return e
	}
	defer release()
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	_, _, e = j.load(ctx)
	return e
}
func (j *MockJournal) load(ctx context.Context) (mockAnchor, []mockEntry, error) {
	var a mockAnchor
	b, e := j.root.Read("anchor.json")
	if e != nil || json.Unmarshal(b, &a) != nil || a.Installation != j.installation || a.Binding != j.binding {
		return a, nil, ErrMockContinuity
	}
	continuity, e := j.participant.MockContinuity(ctx, j.binding)
	if e != nil {
		return a, nil, e
	}
	raw, e := j.root.Read("journal.jsonl")
	if e != nil || len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return a, nil, ErrMockContinuity
	}
	head := domain.MockGenesis(j.installation, j.binding)
	entries := []mockEntry{}
	original := map[string]domain.MockResultMetadata{}
	observed := map[string]domain.MockUsageMeasurement{}
	published := map[string]bool{}
	anchorMatched := a.Head == head
	trustedMatched := continuity.Head == head
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var entry mockEntry
		if json.Unmarshal(line, &entry) != nil {
			return a, nil, ErrMockContinuity
		}
		hash := entry.Hash
		entry.Hash = ""
		if entry.Sequence != head.Sequence+1 || entry.Previous != head.Hash || mockHash(entry) != hash {
			return a, nil, ErrMockContinuity
		}
		entry.Hash = hash
		id := entry.Result.Identity
		key := mockHash(id)
		if id.Validate() != nil || id.TaskID == "" || id.AttemptID == "" || id.RequestHash == "" || id.MarkerKey == "" {
			return a, nil, ErrMockContinuity
		}
		previous, exists := original[key]
		if entry.Usage != nil {
			m := *entry.Usage
			if entry.Bill != nil || !exists || previous.State != "SUCCEEDED" || previous.UsageKnown || m.Identity != id || !validMockUsage(m) {
				return a, nil, ErrMockContinuity
			}
			switch entry.Result.State {
			case "USAGE_OBSERVED":
				if _, ok := observed[key]; ok {
					return a, nil, ErrMockContinuity
				}
				observed[key] = m
			case "USAGE_PUBLISHED":
				if published[key] || observed[key] != m {
					return a, nil, ErrMockContinuity
				}
				published[key] = true
			default:
				return a, nil, ErrMockContinuity
			}
		} else if entry.Bill != nil {
			if !exists || entry.Result.State != "BILL" || entry.Bill.Identity != id || !validMockBill(*entry.Bill) {
				return a, nil, ErrMockContinuity
			}
		} else {
			switch entry.Result.State {
			case "UNKNOWN":
				if exists {
					return a, nil, ErrMockContinuity
				}
			case "SUCCEEDED":
				if !exists || previous.State != "UNKNOWN" || entry.Result.BodyHash == "" {
					return a, nil, ErrMockContinuity
				}
			default:
				return a, nil, ErrMockContinuity
			}
			original[key] = entry.Result
		}
		entries = append(entries, entry)
		head = domain.MockHead{Sequence: entry.Sequence, Hash: hash}
		if a.Head == head {
			anchorMatched = true
		}
		if continuity.Head == head {
			trustedMatched = true
		}
	}
	if !anchorMatched || !trustedMatched {
		return a, nil, ErrMockContinuity
	}
	if continuity.Pending == nil {
		if a.Head != head || continuity.Head != head {
			return a, nil, ErrMockContinuity
		}
	} else {
		// Only an independently reserved exact append may close an interrupted tail.
		// Missing append bytes are not recreated from the reservation or paired DB.
		if *continuity.Pending != head || head.Sequence != continuity.Head.Sequence+1 {
			return a, nil, ErrMockContinuity
		}
		a.Head = head
		b, _ = json.Marshal(a)
		if e = j.root.Replace("anchor.json", b); e != nil {
			return a, nil, e
		}
		if e = j.participant.ConfirmMockAppend(ctx, j.binding, head); e != nil {
			return a, nil, e
		}
	}
	return a, entries, nil
}
func (j *MockJournal) append(ctx context.Context, a mockAnchor, result domain.MockResultMetadata) error {
	return j.appendEntry(ctx, a, mockEntry{Result: result})
}
func (j *MockJournal) appendEntry(ctx context.Context, a mockAnchor, entry mockEntry) error {
	entry.Sequence = a.Head.Sequence + 1
	entry.Previous = a.Head.Hash
	entry.Hash = mockHash(entry)
	next := domain.MockHead{Sequence: entry.Sequence, Hash: entry.Hash}
	if e := j.participant.BeginMockAppend(ctx, j.binding, a.Head, next); e != nil {
		return e
	}
	b, _ := json.Marshal(entry)
	if e := j.root.Append("journal.jsonl", append(b, '\n')); e != nil {
		return e
	}
	a.Head = next
	b, _ = json.Marshal(a)
	if e := j.root.Replace("anchor.json", b); e != nil {
		return e
	}
	return j.participant.ConfirmMockAppend(ctx, j.binding, next)
}
func (j *MockJournal) begin(ctx context.Context, id domain.MockRequestIdentity) error {
	ctx, release, e := j.participant.Enter(ctx, maintenance.MockWriter)
	if e != nil {
		return e
	}
	defer release()
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, entries, e := j.load(ctx)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.Bill != nil || entry.Usage != nil {
			continue
		}
		if entry.Result.Identity.AttemptID == id.AttemptID {
			return ErrMockContinuity
		}
	}
	return j.append(ctx, a, domain.MockResultMetadata{Identity: id, State: "UNKNOWN"})
}
func (j *MockJournal) complete(ctx context.Context, result domain.MockResultMetadata) error {
	ctx, release, e := j.participant.Enter(ctx, maintenance.MockWriter)
	if e != nil {
		return e
	}
	defer release()
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, entries, e := j.load(ctx)
	if e != nil {
		return e
	}
	var current domain.MockResultMetadata
	for _, entry := range entries {
		if entry.Bill != nil || entry.Usage != nil {
			continue
		}
		if entry.Result.Identity == result.Identity {
			current = entry.Result
		}
	}
	if current.State != "UNKNOWN" || result.State != "SUCCEEDED" || result.BodyHash == "" {
		return ErrMockContinuity
	}
	if e = j.append(ctx, a, result); e != nil {
		return e
	}
	a, _, e = j.load(ctx)
	if e != nil {
		return e
	}
	bill := domain.MockBillItem{Identity: result.Identity, Provider: "r0-mock", Account: "r0-mock-supplier", ItemID: "original-charge", Unit: "MONEY", Currency: "syntheticUSD", Atoms: result.CostAtoms, Final: result.NoFurtherCost}
	return j.appendEntry(ctx, a, mockEntry{Result: domain.MockResultMetadata{Identity: result.Identity, State: "BILL"}, Bill: &bill})
}
func (j *MockJournal) OriginalMetadata(ctx context.Context, id domain.MockRequestIdentity) (domain.MockResultMetadata, error) {
	ctx, release, e := j.participant.Enter(ctx, maintenance.QueryWriter)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	defer release()
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	defer done()
	_, entries, e := j.load(ctx)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	out := domain.MockResultMetadata{Identity: id, State: "UNKNOWN"}
	for _, entry := range entries {
		if entry.Bill != nil || entry.Usage != nil {
			continue
		}
		if entry.Result.Identity.AttemptID == id.AttemptID {
			if entry.Result.Identity != id {
				return domain.MockResultMetadata{}, ErrMockContinuity
			}
			out = entry.Result
		}
	}
	return out, nil
}

func (j *MockJournal) OriginalCostMetadata(ctx context.Context, id domain.MockRequestIdentity) (domain.MockCostMetadata, error) {
	ctx, release, e := j.participant.Enter(ctx, maintenance.QueryWriter)
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	defer release()
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	defer done()
	// Chain verification reads the signed journal structure; the cost projection
	// never resolves or exposes its governed body reference.
	_, entries, e := j.load(ctx)
	if e != nil {
		return domain.MockCostMetadata{}, e
	}
	out := domain.MockCostMetadata{Identity: id, State: "UNKNOWN"}
	finalZero, nonzero, disputed := false, false, false
	items := map[string]string{}
	var measured *domain.MockUsageMeasurement
	for _, entry := range entries {
		if entry.Usage != nil {
			if entry.Result.Identity.AttemptID == id.AttemptID && entry.Result.State == "USAGE_PUBLISHED" {
				if entry.Result.Identity != id {
					return domain.MockCostMetadata{}, ErrMockContinuity
				}
				copy := *entry.Usage
				measured = &copy
			}
			continue
		}
		if entry.Bill != nil {
			b := entry.Bill
			if b.Identity.AttemptID == id.AttemptID {
				if b.Identity != id {
					return domain.MockCostMetadata{}, ErrMockContinuity
				}
				h := mockHash(*b)
				if old, ok := items[b.ItemID]; ok && old != h {
					disputed = true
				}
				items[b.ItemID] = h
				nonzero = nonzero || b.Atoms != 0
				finalZero = finalZero || (b.Final && b.Atoms == 0)
			}
			continue
		}
		m := entry.Result
		if m.Identity.AttemptID != id.AttemptID {
			continue
		}
		if m.Identity != id {
			return domain.MockCostMetadata{}, ErrMockContinuity
		}
		out = domain.MockCostMetadata{Identity: m.Identity, State: m.State, UsageKnown: m.UsageKnown, NoFurtherCost: m.NoFurtherCost, CostAtoms: m.CostAtoms, CostUnit: m.CostUnit, CostCurrency: m.CostCurrency, InputUnits: m.InputUnits, OutputUnits: m.OutputUnits}
	}
	if nonzero || disputed {
		out.NoFurtherCost = false
	} else if finalZero && out.State == "SUCCEEDED" {
		out.NoFurtherCost = true
	}
	if measured != nil && out.State == "SUCCEEDED" {
		out.UsageKnown = true
		out.InputUnits = measured.InputUnits
		out.OutputUnits = measured.OutputUnits
	}
	return out, nil
}

func validMockBill(b domain.MockBillItem) bool {
	return b.Identity.Validate() == nil && b.Provider == "r0-mock" && b.Account == "r0-mock-supplier" && domain.ValidateID(b.ItemID) == nil && b.Unit == "MONEY" && b.Currency == "syntheticUSD" && b.Atoms >= 0
}

type originalBillEvidence struct {
	owner      *MockJournal
	continuity *maintenance.SettlementContinuity
	closed     atomic.Bool
	items      []domain.MockBillItem
	item       domain.MockBillItem
	head       domain.MockHead
	close      func()
	once       sync.Once
}

func (e *originalBillEvidence) Items() []domain.MockBillItem {
	return append([]domain.MockBillItem(nil), e.items...)
}
func (e *originalBillEvidence) Item() domain.MockBillItem { return e.item }
func (e *originalBillEvidence) Head() domain.MockHead     { return e.head }
func (e *originalBillEvidence) Close()                    { e.once.Do(func() { e.closed.Store(true); e.close() }) }
func (j *MockJournal) ReadOriginalBillEvidence(ctx context.Context, id domain.MockRequestIdentity, item string) (domain.OriginalBillEvidence, error) {
	ctx, release, e := j.participant.Enter(ctx, maintenance.DeadlineWriter)
	if e != nil {
		return nil, e
	}
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		release()
		return nil, e
	}
	close := func() { done(); release() }
	a, entries, e := j.load(ctx)
	if e != nil {
		close()
		return nil, e
	}
	var found *domain.MockBillItem
	history := []domain.MockBillItem{}
	for _, entry := range entries {
		if entry.Bill == nil || entry.Result.Identity.AttemptID != id.AttemptID || entry.Bill.ItemID != item {
			continue
		}
		if entry.Result.Identity != id {
			close()
			return nil, ErrMockContinuity
		}
		b := *entry.Bill
		found = &b
		history = append(history, b)
	}
	if found == nil {
		close()
		return nil, ErrMockContinuity
	}
	continuity, e := j.participant.HoldSettlementContinuity(ctx, j.binding, a.Head)
	if e != nil {
		close()
		return nil, e
	}
	return &originalBillEvidence{owner: j, continuity: continuity, item: *found, items: history, head: a.Head, close: func() { continuity.Close(); close() }}, nil
}

func (j *MockJournal) CheckOriginalBillEvidence(e domain.OriginalBillEvidence, id domain.SettlementServiceIdentity) error {
	actual, ok := e.(*originalBillEvidence)
	if !ok || actual.owner != j || actual.closed.Load() {
		return ErrMockContinuity
	}
	return actual.continuity.Check(id, actual.head)
}

// Only fixed internal synthetic fault scenarios can append exceptional invoices.
// Existing request/result/bill entries remain immutable; query never creates one.
func (j *MockJournal) lateFaultBill(ctx context.Context, id domain.MockRequestIdentity, item string, atoms int64, final bool) error {
	ctx, release, e := j.participant.Enter(ctx, maintenance.MockWriter)
	if e != nil {
		return e
	}
	defer release()
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, entries, e := j.load(ctx)
	if e != nil {
		return e
	}
	known := false
	for _, entry := range entries {
		if entry.Result.Identity == id && entry.Bill == nil && entry.Result.State == "SUCCEEDED" {
			known = true
		}
	}
	if !known {
		return ErrMockContinuity
	}
	b := domain.MockBillItem{Identity: id, Provider: "r0-mock", Account: "r0-mock-supplier", ItemID: item, Atoms: atoms, Unit: "MONEY", Currency: "syntheticUSD", Final: final}
	return j.appendEntry(ctx, a, mockEntry{Result: domain.MockResultMetadata{Identity: id, State: "BILL"}, Bill: &b})
}

type originalBillScan struct {
	owner      *MockJournal
	continuity *maintenance.SettlementContinuity
	current    domain.MockHead
	closed     atomic.Bool
	start, end domain.MockHead
	refs       []domain.BillScanReference
	close      func()
	once       sync.Once
}

func (p *originalBillScan) Start() domain.MockHead { return p.start }
func (p *originalBillScan) End() domain.MockHead   { return p.end }
func (p *originalBillScan) References() []domain.BillScanReference {
	return append([]domain.BillScanReference(nil), p.refs...)
}
func (p *originalBillScan) Close() { p.once.Do(func() { p.closed.Store(true); p.close() }) }

// ScanOriginalBills verifies the supplied durable prefix against the independent
// current head. It only observes at most 100 actual journal positions.
func (j *MockJournal) ScanOriginalBills(ctx context.Context, from domain.MockHead) (domain.OriginalBillScan, error) {
	ctx, release, e := j.participant.Enter(ctx, maintenance.DeadlineWriter)
	if e != nil {
		return nil, e
	}
	done, e := j.root.Lock(ctx, "state.lock")
	if e != nil {
		release()
		return nil, e
	}
	close := func() { done(); release() }
	a, entries, e := j.load(ctx)
	if e != nil {
		close()
		return nil, e
	}
	genesis := domain.MockGenesis(j.installation, j.binding)
	if from.Sequence == 0 && from.Hash != genesis.Hash || from.Sequence > uint64(len(entries)) {
		close()
		return nil, ErrMockContinuity
	}
	if from.Sequence > 0 && entries[from.Sequence-1].Hash != from.Hash {
		close()
		return nil, ErrMockContinuity
	}
	continuity, e := j.participant.HoldSettlementContinuity(ctx, j.binding, a.Head)
	if e != nil {
		close()
		return nil, e
	}
	page := &originalBillScan{owner: j, continuity: continuity, current: a.Head, start: from, end: from, close: func() { continuity.Close(); close() }}
	for _, entry := range entries[from.Sequence:] {
		if entry.Sequence-from.Sequence > 100 {
			break
		}
		page.end = domain.MockHead{Sequence: entry.Sequence, Hash: entry.Hash}
		if entry.Bill != nil {
			page.refs = append(page.refs, domain.BillScanReference{Position: page.end, Identity: entry.Bill.Identity, ItemID: entry.Bill.ItemID, Digest: mockBillDigest(*entry.Bill)})
		}
	}
	return page, nil
}

func (j *MockJournal) CheckOriginalBillScan(page domain.OriginalBillScan, id domain.SettlementServiceIdentity) error {
	actual, ok := page.(*originalBillScan)
	if !ok || actual.owner != j || actual.closed.Load() {
		return ErrMockContinuity
	}
	return actual.continuity.Check(id, actual.current)
}

func mockBillDigest(b domain.MockBillItem) string {
	raw, _ := json.Marshal(b)
	canonical, _ := wire.Canonical(raw)
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:])
}
