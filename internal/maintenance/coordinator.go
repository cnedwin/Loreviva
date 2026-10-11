package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/localstate"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sync"
)

type Writer string

const (
	APIWriter          Writer = "API"
	StoreWriter        Writer = "STORE"
	WorkerWriter       Writer = "WORKER"
	FileWriter         Writer = "FILES"
	GovernanceWriter   Writer = "GOVERNANCE"
	HarnessWriter      Writer = "HARNESS"
	MockWriter         Writer = "MOCK"
	QueryWriter        Writer = "QUERY_COST"
	CleanupWriter      Writer = "BODY_CLEANUP"
	DeadlineWriter     Writer = "DEADLINE"
	NotificationWriter Writer = "NOTIFICATIONS"
	ConfigWriter       Writer = "CONFIG"
)

var ErrMaintenance = errors.New("maintenance closed or participant generation retired")

type registration struct {
	Generation  uint64
	Incarnation string
	Writers     []Writer
}
type coordinatorState struct {
	Installation, Database, Files, Phase, Plan string
	Generation                                 uint64
	Participants                               map[string]registration
	Governance                                 *GovernanceSeal
}
type Coordinator struct {
	root                               *localstate.Root
	dir, installation, database, files string
}
type Participant struct {
	coordinator     *Coordinator
	id, incarnation string
	generation      uint64
	writers         []Writer
}
type permitKey struct{}
type permit struct {
	participant *Participant
	live        bool
	mu          sync.Mutex
}

func (c *Coordinator) Directory() string { return c.dir }
func coordinatorRoot(dir, installation, database, files string, create bool) (*Coordinator, error) {
	if domain.ValidateID(installation) != nil || database == "" || files == "" {
		return nil, ErrMaintenance
	}
	r, e := localstate.Open(dir, create)
	if e != nil {
		return nil, e
	}
	return &Coordinator{r, dir, installation, database, files}, nil
}

// Creation is an explicit fresh-install action. Runtime opens existing state;
// restoration can never call this to replace missing independent evidence.
func CreateCoordinator(dir, installation, database, files string) (*Coordinator, error) {
	c, e := coordinatorRoot(dir, installation, database, files, true)
	if e != nil {
		return nil, e
	}
	done, e := c.root.Lock(context.Background(), "state.lock")
	if e != nil {
		return nil, e
	}
	defer done()
	a := coordinatorState{Installation: installation, Database: database, Files: files, Phase: "OPEN", Generation: 1, Participants: map[string]registration{}}
	b, _ := json.Marshal(a)
	if e = c.root.Create("coordinator.json", b); e != nil {
		return nil, e
	}
	return c, nil
}
func OpenCoordinator(dir, installation, database, files string) (*Coordinator, error) {
	c, e := coordinatorRoot(dir, installation, database, files, false)
	if e != nil {
		return nil, e
	}
	done, e := c.root.Lock(context.Background(), "state.lock")
	if e != nil {
		return nil, e
	}
	defer done()
	_, e = c.load()
	return c, e
}
func (c *Coordinator) load() (coordinatorState, error) {
	var a coordinatorState
	b, e := c.root.Read("coordinator.json")
	if e != nil || json.Unmarshal(b, &a) != nil || a.Installation != c.installation || a.Database != c.database || a.Files != c.files || a.Generation == 0 || a.Participants == nil {
		return a, ErrMaintenance
	}
	switch a.Phase {
	case "OPEN", "CLOSING", "CLOSED":
	default:
		return a, ErrMaintenance
	}
	return a, nil
}
func (c *Coordinator) save(a coordinatorState) error {
	b, e := json.Marshal(a)
	if e != nil {
		return e
	}
	return c.root.Replace("coordinator.json", b)
}
func validWriter(w Writer) bool {
	switch w {
	case APIWriter, StoreWriter, WorkerWriter, FileWriter, GovernanceWriter, HarnessWriter, MockWriter, QueryWriter, CleanupWriter, DeadlineWriter, NotificationWriter, ConfigWriter:
		return true
	}
	return false
}
func (c *Coordinator) Register(ctx context.Context, id string, writers []Writer) (*Participant, error) {
	if domain.ValidateID(id) != nil || len(writers) == 0 || len(writers) > 12 {
		return nil, ErrMaintenance
	}
	for _, w := range writers {
		if !validWriter(w) {
			return nil, ErrMaintenance
		}
	}
	done, e := c.root.Lock(ctx, "state.lock")
	if e != nil {
		return nil, e
	}
	defer done()
	a, e := c.load()
	if e != nil || a.Phase != "OPEN" || len(a.Participants) >= 128 {
		return nil, ErrMaintenance
	}
	if _, exists := a.Participants[id]; exists {
		return nil, ErrMaintenance
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	p := &Participant{c, id, hex.EncodeToString(nonce[:]), a.Generation, append([]Writer(nil), writers...)}
	a.Participants[id] = registration{p.generation, p.incarnation, p.writers}
	if e = c.save(a); e != nil {
		return nil, e
	}
	return p, nil
}
func (p *Participant) check(ctx context.Context, w Writer) error {
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
	if !ok || r.Generation != p.generation || r.Incarnation != p.incarnation {
		return ErrMaintenance
	}
	for _, v := range r.Writers {
		if w == v {
			return nil
		}
	}
	return ErrMaintenance
}
func (p *Participant) Enter(ctx context.Context, w Writer) (context.Context, func(), error) {
	if existing, ok := ctx.Value(permitKey{}).(*permit); ok && existing.participant == p {
		existing.mu.Lock()
		live := existing.live
		existing.mu.Unlock()
		if !live {
			return ctx, nil, ErrMaintenance
		}
		if e := p.check(ctx, w); e != nil {
			return ctx, nil, e
		}
		return ctx, func() {}, nil
	}
	done, e := p.coordinator.root.Shared(ctx)
	if e != nil {
		return ctx, nil, e
	}
	if e = p.check(ctx, w); e != nil {
		done()
		return ctx, nil, e
	}
	token := &permit{participant: p, live: true}
	once := sync.Once{}
	release := func() { once.Do(func() { token.mu.Lock(); token.live = false; token.mu.Unlock(); done() }) }
	return context.WithValue(ctx, permitKey{}, token), release, nil
}
func (c *Coordinator) BeginClose(ctx context.Context, key string) error {
	if domain.ValidateIdempotencyKey(key) != nil {
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
	if a.Phase != "OPEN" {
		if a.Plan == key {
			return nil
		}
		return ErrMaintenance
	}
	if a.Generation == ^uint64(0) {
		return ErrMaintenance
	}
	a.Phase = "CLOSING"
	a.Plan = key
	a.Generation++
	return c.save(a)
}

// Exclusive admission proves all shared operation/stream handles have drained.
// Missing processes and elapsed leases are not used as proof.
func (c *Coordinator) Drain(ctx context.Context, key string) error {
	done, e := c.root.Lock(ctx, "operations.lock")
	if e != nil {
		return e
	}
	defer done()
	state, e := c.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer state()
	a, e := c.load()
	if e != nil || a.Plan != key || (a.Phase != "CLOSING" && a.Phase != "CLOSED") {
		return ErrMaintenance
	}
	a.Phase = "CLOSED"
	return c.save(a)
}

// EnterStore is the narrow Store interface; all SQL authority still belongs to
// the existing transaction barriers, not to this operation permit.
func (p *Participant) EnterStore(ctx context.Context) (context.Context, func(), error) {
	return p.Enter(ctx, StoreWriter)
}
func (p *Participant) Check(ctx context.Context) error { return p.check(ctx, StoreWriter) }
func (p *Participant) Close(ctx context.Context) error {
	// Called only after the executable's HTTP and worker servers have stopped.
	done, e := p.coordinator.root.Lock(ctx, "operations.lock")
	if e != nil {
		return e
	}
	defer done()
	state, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer state()
	a, e := p.coordinator.load()
	if e != nil {
		return e
	}
	r, ok := a.Participants[p.id]
	if !ok {
		return nil
	}
	if r.Incarnation != p.incarnation {
		return ErrMaintenance
	}
	delete(a.Participants, p.id)
	return p.coordinator.save(a)
}
func (p *Participant) CurrentInstallation(ctx context.Context) (string, uint64, error) {
	token, ok := ctx.Value(permitKey{}).(*permit)
	if !ok || token.participant != p {
		return "", 0, ErrMaintenance
	}
	token.mu.Lock()
	live := token.live
	token.mu.Unlock()
	if !live {
		return "", 0, ErrMaintenance
	}
	if e := p.check(ctx, StoreWriter); e != nil {
		return "", 0, e
	}
	return p.coordinator.installation, p.generation, nil
}

// ClosedPermit is issued only while the coordinator owns exclusive admission.
// It cannot authorize normal Store barriers or a positive reopen.
type ClosedPermit struct {
	admission   *localstate.ExclusiveAdmission
	coordinator *Coordinator
	plan        string
	generation  uint64
	active      bool
	mu          sync.Mutex
}

func (c *Coordinator) AcquireReplay(ctx context.Context, plan string) (*ClosedPermit, func(), error) {
	handle, e := c.root.ExclusiveAdmission(ctx)
	if e != nil {
		return nil, nil, e
	}
	done := handle.Close
	state, e := c.root.Lock(ctx, "state.lock")
	if e != nil {
		done()
		return nil, nil, e
	}
	a, e := c.load()
	state()
	if e != nil || a.Phase != "CLOSED" || a.Plan != plan {
		done()
		return nil, nil, ErrMaintenance
	}
	if e = handle.BindClosed(ctx, localstate.ClosedBinding{Installation: a.Installation, Database: a.Database, Files: a.Files, RootHash: localstate.RootHash(c.dir), Plan: plan, Generation: a.Generation}); e != nil {
		done()
		return nil, nil, e
	}
	p := &ClosedPermit{admission: handle, coordinator: c, plan: plan, generation: a.Generation, active: true}
	var once sync.Once
	return p, func() { once.Do(func() { p.mu.Lock(); p.active = false; p.mu.Unlock(); done() }) }, nil
}
func (p *ClosedPermit) MemoryReplayIdentity(ctx context.Context) (string, uint64, error) {
	if p == nil {
		return "", 0, ErrMaintenance
	}
	p.mu.Lock()
	active := p.active
	p.mu.Unlock()
	if !active {
		return "", 0, ErrMaintenance
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return "", 0, e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "CLOSED" || a.Plan != p.plan || a.Generation != p.generation {
		return "", 0, ErrMaintenance
	}
	return a.Installation, a.Generation, nil
}

func (c *Coordinator) CheckOpenGeneration(ctx context.Context, generation uint64) error {
	done, e := c.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := c.load()
	if e != nil || a.Phase != "OPEN" || a.Generation != generation {
		return ErrMaintenance
	}
	return nil
}

func (p *ClosedPermit) MemoryStorePermit(ctx context.Context) (*store.MemoryReplayPermit, error) {
	if _, _, e := p.MemoryReplayIdentity(ctx); e != nil {
		return nil, e
	}
	return store.IssueMemoryReplayPermit(p.admission)
}

// PinReplay keeps the exclusive OS admission handle alive across a fixed
// external file operation. It performs its state check before that operation.
func (p *ClosedPermit) PinReplay(ctx context.Context) (func(), error) {
	installation, generation, e := p.MemoryReplayIdentity(ctx)
	if e != nil {
		return nil, e
	}
	b, release, e := p.admission.Pin()
	if e != nil {
		return nil, e
	}
	if b.Installation != installation || b.Generation != generation {
		release()
		return nil, ErrMaintenance
	}
	return release, nil
}

type GovernanceSeal struct {
	store.MemoryCandidateBinding
	State string
}
type GovernanceCandidate struct {
	coordinator *Coordinator
	binding     store.MemoryCandidateBinding
}

func (p *ClosedPermit) SealGovernance(ctx context.Context, ledger, anchor, hash string, sequence uint64) (*GovernanceCandidate, error) {
	if domain.ValidateID(ledger) != nil || domain.ValidateID(anchor) != nil || len(hash) != 64 {
		return nil, ErrMaintenance
	}
	installation, generation, e := p.MemoryReplayIdentity(ctx)
	if e != nil {
		return nil, e
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return nil, e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "CLOSED" || a.Generation != generation || a.Plan != p.plan {
		return nil, ErrMaintenance
	}
	b := store.MemoryCandidateBinding{Installation: installation, Generation: generation, Ledger: ledger, Anchor: anchor, HeadHash: hash, Sequence: sequence, CommandKey: p.plan}
	if a.Governance != nil {
		b.CandidateID = a.Governance.CandidateID
		if a.Governance.MemoryCandidateBinding != b {
			return nil, ErrMaintenance
		}
		return &GovernanceCandidate{p.coordinator, b}, nil
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	b.CandidateID = hex.EncodeToString(nonce[:])
	a.Governance = &GovernanceSeal{b, "SEALED"}
	if e = p.coordinator.save(a); e != nil {
		return nil, e
	}
	return &GovernanceCandidate{p.coordinator, b}, nil
}
func (p *ClosedPermit) GovernanceBinding(ctx context.Context, c *GovernanceCandidate) (store.MemoryCandidateBinding, error) {
	if c == nil || p == nil || c.coordinator != p.coordinator {
		return store.MemoryCandidateBinding{}, ErrMaintenance
	}
	if _, _, e := p.MemoryReplayIdentity(ctx); e != nil {
		return store.MemoryCandidateBinding{}, e
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return store.MemoryCandidateBinding{}, e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "CLOSED" || a.Generation != p.generation || a.Plan != p.plan || a.Governance == nil || a.Governance.MemoryCandidateBinding != c.binding {
		return store.MemoryCandidateBinding{}, ErrMaintenance
	}
	return c.binding, nil
}
func (p *ClosedPermit) ConfirmGovernanceReady(ctx context.Context, c *GovernanceCandidate, receipt *store.MemoryCandidateReceipt) error {
	b, e := p.GovernanceBinding(ctx, c)
	if e != nil || !receipt.Matches(b) {
		return ErrMaintenance
	}
	done, e := p.coordinator.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	a, e := p.coordinator.load()
	if e != nil || a.Phase != "CLOSED" || a.Generation != p.generation || a.Plan != p.plan || a.Governance == nil || a.Governance.MemoryCandidateBinding != b {
		return ErrMaintenance
	}
	a.Governance.State = "G_READY"
	return p.coordinator.save(a)
}
