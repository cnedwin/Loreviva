package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sort"
	"testing"
	"time"
)

func TestCorrectionWinsWithoutErasingValidHistory(t *testing.T) {
	a := fixture()
	a.Nodes["independent"] = MemoryRecord{ID: "independent", Body: "independent valid history", Kind: "PROMISE", Domain: PlayerStatement, Audience: Private, Uses: []Use{PrivateResponse}, Sources: []string{"dm"}, Revision: 1}
	b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"preference"}, Expected: map[string]uint64{"preference": 1}}, "CORRECT", nil, "corrected", "updated preference")
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Targets) != 1 {
		t.Fatal(b)
	}
	if !a.Nodes["preference"].Blocked || a.Nodes["independent"].Blocked {
		t.Fatal("correction erased independent valid history")
	}
	if a.Nodes["corrected"].Body != "updated preference" {
		t.Fatal("replacement missing")
	}
}
func TestDeleteBlocksLateProposalAndClearsDerivedMedia(t *testing.T) {
	a := fixture()
	a.Nodes["media"] = MemoryRecord{ID: "media", Kind: "MEDIA", Domain: ActorBelief, Body: "private media reference", Parents: []string{"preference"}, Audience: Private, Uses: []Use{PrivateResponse}, Revision: 1}
	if _, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"preference"}, Expected: map[string]uint64{"preference": 1}}, "DELETE", nil, "", ""); e != nil {
		t.Fatal(e)
	}
	if e := a.AddDerived(MemoryRecord{ID: "late", ActorID: "alex", Kind: "GOAL", Domain: ActorBelief, Parents: []string{"media"}, Uses: []Use{PrivateResponse}, Audience: Private, Revision: 1}, time.Now()); e == nil {
		t.Fatal("late descendant admitted")
	}
	a.Clean()
	if a.Nodes["media"].Body != "" || a.Nodes["preference"].Body != "" {
		t.Fatal("blocked derived body retained")
	}
	if a.Sources["dm"].Blocked {
		t.Fatal("node deletion expanded upstream")
	}
}

func TestUsePolicyDisableAndEnableCannotReviveOldDerivative(t *testing.T) {
	a := fixture()
	a.PolicyRevision = 1
	a.Nodes["derived"] = MemoryRecord{ID: "derived", ActorID: "alex", Kind: "GOAL", Domain: ActorBelief, Body: "private goal", Parents: []string{"preference"}, Audience: Private, Uses: []Use{PrivateResponse}, Revision: 1}
	if e := a.SetUsePolicy(1, "DISABLE", []Use{PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	if len(a.Allowed("alex", Private, PrivateResponse, time.Now())) != 0 {
		t.Fatal("disabled use recalled")
	}
	if e := a.SetUsePolicy(2, "ENABLE", []Use{PrivateResponse}); e != nil {
		t.Fatal(e)
	}
	if !a.Nodes["derived"].Blocked {
		t.Fatal("ENABLE revived old derivative")
	}
	if len(a.Allowed("alex", Private, PrivateResponse, time.Now())) != 1 {
		t.Fatal("source permission did not survive policy enable")
	}
}

func TestSourceTimeRangeCutoffAndRestoredEpochRemainExact(t *testing.T) {
	a := fixture()
	a.Epoch = "restored"
	a.Intake = 2
	s := a.Sources["dm"]
	s.Epoch = "historical"
	s.Intake = 1
	s.OccurredAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.Sources["dm"] = s
	independent := s
	independent.ID = "independent"
	independent.Intake = 2
	independent.OccurredAt = s.OccurredAt.Add(time.Hour)
	a.Sources[independent.ID] = independent
	b, e := a.Block(Selector{Kind: "TIME_RANGE", TargetKind: "SOURCE", From: s.OccurredAt, To: s.OccurredAt.Add(time.Minute)}, "DELETE", nil, "", "")
	if e != nil || len(b.Targets) != 1 || b.Targets[0] != "dm" || b.Cutoff != 2 || a.Sources["independent"].Blocked {
		t.Fatal("time/source cutoff differs", b, e)
	}
	a.Sources["later"] = Source{ID: "later", Epoch: "restored", Intake: 3, Revision: 1}
	a.Intake = 3
	replayBlock(&a, b)
	if a.Sources["later"].Blocked {
		t.Fatal("resolved selector acquired a later source")
	}
	all, e := a.Block(Selector{Kind: "ALL_EXISTING", TargetKind: "SOURCE"}, "DELETE", nil, "", "")
	if e != nil || len(all.Targets) != 3 {
		t.Fatal("historical epoch excluded from ALL_EXISTING", all, e)
	}
}

// These tests exercise the private proof, not CLIENT-to-Admin authorization.
// Every mutation is produced by the real Aggregate implementation. No SQL,
// accepted task, Ledger entry, authenticated principal, or COMMIT is fabricated.
func integrationProofFixture() Aggregate {
	a := Aggregate{Sources: map[string]Source{}, Nodes: map[string]MemoryRecord{}, Epoch: "proof-epoch", Intake: 3, PolicyRevision: 1}
	for i, id := range []string{"a", "b", "c"} {
		sid := "source-" + id
		a.Sources[sid] = Source{ID: sid, Body: "source " + id, Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1, Epoch: a.Epoch, Intake: uint64(i + 1), OccurredAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
		a.Nodes[id] = MemoryRecord{ID: id, ActorID: "actor", TargetKind: "PLAYER", TargetID: "player", Kind: "BELIEF", Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Sources: []string{sid}, Body: "node " + id, Revision: 1}
	}
	return a
}
func copyIntegrationProof(t *testing.T, a Aggregate) Aggregate {
	t.Helper()
	raw, e := json.Marshal(a)
	if e != nil {
		t.Fatal(e)
	}
	var b Aggregate
	if e = json.Unmarshal(raw, &b); e != nil {
		t.Fatal(e)
	}
	return b
}
func proofBlock(t *testing.T, a *Aggregate, sel Selector, action string, uses []Use) Binding {
	t.Helper()
	b, e := a.Block(sel, action, uses, "", "")
	if e != nil {
		t.Fatal("real Aggregate.Block precondition", e)
	}
	a.Clean()
	return b
}
func TestIntegrationCurrentPolicySetProof(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		uses         [][]Use
	}{
		{"same-direction-insertion-order", "DISABLE", [][]Use{{Use("RESEARCH_OPT_IN")}, {Relationship}}},
		{"empty-enable-is-empty-set", "ENABLE", [][]Use{{Relationship}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := integrationProofFixture()
			a := copyIntegrationProof(t, base)
			bs := map[string]Binding{}
			for i, uses := range tc.uses {
				rev := a.PolicyRevision
				if e := a.SetUsePolicy(rev, tc.action, uses); e != nil {
					t.Fatal(e)
				}
				bs[string(rune('a'+i))] = Binding{Action: "SET_USE_POLICY", TargetKind: tc.action, Uses: uses, SelectorHash: digest(struct {
					Revision uint64
					Action   string
					Uses     []Use
				}{rev, tc.action, uses}), SourceEpoch: a.Epoch, Cutoff: a.Intake}
			}
			raw, _ := json.Marshal(a)
			if e := proveIntegrationAggregate(context.Background(), base, a, bs); e != nil {
				t.Errorf("legal policy set proof rejected: %v", e)
			}
			after, _ := json.Marshal(a)
			if string(raw) != string(after) {
				t.Fatal("proof mutated persisted representation")
			}
			bad := copyIntegrationProof(t, a)
			bad.Disabled = []Use{Relationship, Relationship}
			if proveIntegrationAggregate(context.Background(), base, bad, bs) == nil {
				t.Error("duplicate disabled purpose accepted")
			}
			bad = copyIntegrationProof(t, a)
			bad.Disabled = []Use{Use("UNKNOWN")}
			if proveIntegrationAggregate(context.Background(), base, bad, bs) == nil {
				t.Error("unknown disabled purpose accepted")
			}
			bad = copyIntegrationProof(t, a)
			bad.PolicyRevision++
			if proveIntegrationAggregate(context.Background(), base, bad, bs) == nil {
				t.Error("policy revision drift accepted")
			}
		})
	}
}
func TestIntegrationCurrentCrossSourceCutsProof(t *testing.T) {
	t.Run("deep-deterministic-source-history", func(t *testing.T) {
		base := integrationProofFixture()
		a := copyIntegrationProof(t, base)
		bs := map[string]Binding{}
		for i := 0; i < 1500; i++ {
			b := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{"source-a"}, Expected: map[string]uint64{"source-a": uint64(i + 1)}}, "REVOKE", []Use{Relationship})
			bs[fmt.Sprintf("source-%04d", i)] = b
		}
		raw, e := json.Marshal(bs)
		if e != nil || len(raw) >= MaxJournalBytes {
			t.Fatal("bounded pure history fixture", e, len(raw))
		}
		if e = proveIntegrationAggregate(context.Background(), base, a, bs); e != nil {
			t.Fatal("1500 actual transformations lost", e)
		}
	})
	t.Run("bounded-search-and-cancellation", func(t *testing.T) {
		a := integrationProofFixture()
		b := proofBlock(t, &a, Selector{Kind: "ALL_EXISTING", TargetKind: "SOURCE"}, "REVOKE", []Use{Relationship})
		bs := map[string]Binding{"source": b}
		if e := integrationSourceCutsBudget(context.Background(), bs, nil, nil, nil, 1); e == nil {
			t.Fatal("exhausted work budget minted proof")
		}
		if e := integrationSourceCutsBudget(context.Background(), bs, nil, nil, nil, MaxJournalBytes); e != nil {
			t.Fatal("same unranked history with available work", e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if e := integrationSourceCutsBudget(ctx, bs, nil, nil, nil, MaxJournalBytes); e != context.Canceled {
			t.Fatal("cancellation not observed", e)
		}
	})
	t.Run("source-before-cross-source-delete", func(t *testing.T) {
		base := integrationProofFixture()
		a := copyIntegrationProof(t, base)
		source := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{"source-a"}, Expected: map[string]uint64{"source-a": 1}}, "REVOKE", []Use{Relationship})
		del := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a", "b"}, Expected: map[string]uint64{"a": 2, "b": 1}}, "DELETE", nil)
		bs := map[string]Binding{"source": source, "delete": del}
		if e := proveIntegrationAggregate(context.Background(), base, a, bs); e != nil {
			t.Errorf("feasible source cut rejected: %v", e)
		}
		if a.Nodes["c"].Body != base.Nodes["c"].Body || a.Nodes["c"].Revision != 1 {
			t.Fatal("independent C changed")
		}
		bad := del
		bad.Revisions = map[string]uint64{"a": 3, "b": 1}
		if proveIntegrationAggregate(context.Background(), base, a, map[string]Binding{"source": source, "delete": bad}) == nil {
			t.Error("impossible source cut accepted")
		}
	})
	t.Run("direct-cross-source-cycle", func(t *testing.T) {
		base := integrationProofFixture()
		a := copyIntegrationProof(t, base)
		first := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a", "b"}, Expected: map[string]uint64{"a": 1, "b": 1}}, "REVOKE", []Use{Relationship})
		second := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a", "b"}, Expected: map[string]uint64{"a": 2, "b": 2}}, "DELETE", nil)
		if e := proveIntegrationAggregate(context.Background(), base, a, map[string]Binding{"first": first, "second": second}); e != nil {
			t.Fatal("legal direct order rejected", e)
		}
		first.Revisions = map[string]uint64{"a": 2, "b": 1}
		second.Revisions = map[string]uint64{"a": 1, "b": 2}
		if proveIntegrationAggregate(context.Background(), base, a, map[string]Binding{"first": first, "second": second}) == nil {
			t.Error("cross-source direct cycle accepted")
		}
		proven, e := integrationProvenComponents(context.Background(), base, a, map[string]Binding{"first": first, "second": second})
		if e != nil || len(proven) != 1 || proven[0] != "c" {
			t.Fatalf("independent C proof lost or A/B borrowed authority: %v %v", proven, e)
		}
	})
	t.Run("unranked-source-event-counts-once", func(t *testing.T) {
		base := integrationProofFixture()
		a := copyIntegrationProof(t, base)
		source := proofBlock(t, &a, Selector{Kind: "ALL_EXISTING", TargetKind: "SOURCE"}, "REVOKE", []Use{Relationship})
		if len(source.Revisions) != 0 {
			t.Fatal("ALL_EXISTING invented source ordering")
		}
		del := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a", "b"}, Expected: map[string]uint64{"a": 2, "b": 2}}, "DELETE", nil)
		if e := proveIntegrationAggregate(context.Background(), base, a, map[string]Binding{"source": source, "delete": del}); e != nil {
			t.Errorf("legal shared source event rejected: %v", e)
		}
		del.Revisions = map[string]uint64{"a": 1, "b": 2}
		if proveIntegrationAggregate(context.Background(), base, a, map[string]Binding{"source": source, "delete": del}) == nil {
			t.Error("inconsistent same-event cut accepted")
		}
	})
}

// These are pure structural/codec tests. The descriptive document values below
// do not mint Store authority, original receipts or CLIENT permissions.
func pureBirthSource(s Source) store.IntegrationBirthSource {
	raw, _ := json.Marshal(s)
	var fields store.IntegrationBirthSourceFields
	_ = json.Unmarshal(raw, &fields)
	return store.IntegrationBirthSource{IntegrationBirthSourceFields: fields, BodyRawSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(s.Body)))}
}
func pureBirthNode(n MemoryRecord) store.IntegrationBirthNode {
	raw, _ := json.Marshal(n)
	var fields store.IntegrationBirthNodeFields
	_ = json.Unmarshal(raw, &fields)
	return store.IntegrationBirthNode{IntegrationBirthNodeFields: fields, BodyStringJSONSHA256: digest(n.Body)}
}
func pureBirthEvidence(d store.IntegrationMemoryBirthDocument) store.IntegrationMemoryBirthEvidence {
	raw, _ := json.Marshal(d)
	return store.IntegrationMemoryBirthEvidence{Document: raw, DocumentSHA256: sha256.Sum256(raw)}
}
func pureBirthBase() Aggregate {
	a := integrationProofFixture()
	for _, id := range []string{"a", "b", "c"} {
		sid := "source-" + id
		a.Grants = append(a.Grants, Grant{ActorID: "actor", SourceID: sid, Uses: []Use{PrivateResponse, Relationship}})
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: "actor", SourceID: sid, Kind: Initial, EvidenceID: "creation"})
	}
	return a
}
func pureBirthInput(t *testing.T, a *Aggregate, revision uint64, task string) store.IntegrationMemoryBirthDocument {
	t.Helper()
	d := store.IntegrationMemoryBirthDocument{Format: "loreviva.integration-memory-birth.v1", Kind: "INPUT", TaskID: task, ActorID: "actor", DataSubjectID: "player", BeforeAggregateRevision: revision, AfterAggregateRevision: revision + 1, BeforeIntake: a.Intake, AfterIntake: a.Intake + 1, Epoch: a.Epoch, PolicyRevision: a.PolicyRevision, BeforeCommitment: integrationBirthProjection(integrationAbstract(*a)), ObservedAt: time.Date(2026, 10, 20, 1, 0, 0, 123000, time.UTC)}
	d.InputSourceID = content.Identity(task, "input")
	body := "pure input " + task
	id := content.Identity(d.InputSourceID, d.ActorID, "turn-input")
	a.Intake++
	source := Source{ID: d.InputSourceID, Body: body, Domain: PlayerStatement, Audience: Private, Uses: []Use{PrivateResponse}, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: d.ObservedAt}
	node := MemoryRecord{ID: id, Kind: "EXPERIENCE", ActorID: d.ActorID, TargetKind: "PLAYER", TargetID: d.DataSubjectID, Body: body, Domain: PlayerStatement, Audience: Private, Uses: []Use{PrivateResponse}, Sources: []string{source.ID}, Revision: 1}
	a.Sources[source.ID] = source
	a.Nodes[id] = node
	a.Grants = append(a.Grants, Grant{ActorID: d.ActorID, SourceID: source.ID, Uses: []Use{PrivateResponse}})
	a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: d.ActorID, SourceID: source.ID, Kind: Told, EvidenceID: task})
	d.SourcesBorn = []store.IntegrationBirthSource{pureBirthSource(source)}
	d.NodesBorn = []store.IntegrationBirthNode{pureBirthNode(node)}
	d.MemoryIDs = []string{id}
	d.InputHash = d.SourcesBorn[0].BodyRawSHA256
	d.AddedGrants = []store.IntegrationBirthGrant{{ActorID: d.ActorID, SourceID: source.ID, Uses: []string{"PRIVATE_RESPONSE"}}}
	d.AddedAcquisitions = []store.IntegrationBirthAcquisition{{ActorID: d.ActorID, SourceID: source.ID, Kind: "TOLD", EvidenceID: task}}
	d.AfterCommitment = integrationBirthProjection(integrationAbstract(*a))
	return d
}
func pureBirthDerived(t *testing.T, a *Aggregate, revision uint64, parents []string, count int) store.IntegrationMemoryBirthDocument {
	t.Helper()
	d := store.IntegrationMemoryBirthDocument{Format: "loreviva.integration-memory-birth.v1", Kind: "DERIVED", TaskID: "pure-task", ActorID: "actor", DataSubjectID: "player", BeforeAggregateRevision: revision, AfterAggregateRevision: revision, BeforeIntake: a.Intake, AfterIntake: a.Intake, Epoch: a.Epoch, PolicyRevision: a.PolicyRevision, BeforeCommitment: integrationBirthProjection(integrationAbstract(*a)), MemoryIDs: []string{}}
	sources := map[string]bool{}
	for _, id := range parents {
		n, ok := a.Nodes[id]
		if !ok {
			t.Fatal("pure actual parent")
		}
		d.ParentNodes = append(d.ParentNodes, pureBirthNode(n))
		for _, sid := range n.Sources {
			sources[sid] = true
		}
	}
	ids := []string{}
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d.ParentSources = append(d.ParentSources, pureBirthSource(a.Sources[id]))
	}
	if count > 0 {
		d.AfterAggregateRevision++
		d.ObservedAt = time.Date(2026, 10, 20, 1, 0, 1, 456000, time.UTC)
	}
	start := len(a.Acquisitions)
	for i := 0; i < count; i++ {
		n := MemoryRecord{ID: fmt.Sprintf("derived-%d", i), ActorID: "actor", Kind: "BELIEF", TargetKind: "PLAYER", TargetID: "player", Body: fmt.Sprintf("actual pure derived %d", i), Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse}, Parents: append([]string(nil), parents...), Revision: 1}
		if e := a.AddDerived(n, d.ObservedAt); e != nil {
			t.Fatal("real AddDerived", e)
		}
		d.NodesBorn = append(d.NodesBorn, pureBirthNode(a.Nodes[n.ID]))
		d.MemoryIDs = append(d.MemoryIDs, n.ID)
	}
	d.AddedAcquisitions = []store.IntegrationBirthAcquisition{}
	raw, _ := json.Marshal(a.Acquisitions[start:])
	_ = json.Unmarshal(raw, &d.AddedAcquisitions)
	d.AfterCommitment = integrationBirthProjection(integrationAbstract(*a))
	return d
}

// Tiny exhaustive reference: enumerate original apply/undo without any ordering
// reduction, then compare COMPLETE terminal states with the component plan.
// Explicit nil flags avoid JSON omitempty collapsing the relevant [] distinction.
func integrationComponentTerminalSet(t *testing.T, base Aggregate, events []integrationBirthEvent, prefix []int, reduced bool, fault func(*integrationBirthMachine)) map[string]bool {
	t.Helper()
	m := integrationBirthMachine{a: integrationAbstract(base), revision: 1, work: MaxJournalBytes, ctx: context.Background()}
	done := make([]bool, len(events))
	for _, i := range prefix {
		if _, e := m.apply(events[i], done); e != nil {
			t.Fatal("reference prefix", i, e)
		}
		done[i] = true
	}
	if fault != nil {
		fault(&m)
	}
	var plan *integrationComponentPlan
	if reduced {
		g, e := integrationDeleteGraph(&m, base, events)
		if e != nil {
			t.Fatal("reference graph", e)
		}
		var impossible bool
		plan, impossible, e = g.correctComponents(&m, events, done)
		if e != nil {
			t.Fatal("reference plan", e)
		}
		if impossible {
			return map[string]bool{}
		}
	}
	out := map[string]bool{}
	var visit func(int)
	visit = func(depth int) {
		if depth == len(events) {
			nilMedia := map[string]bool{}
			for id, n := range m.a.Nodes {
				nilMedia[id] = n.MediaRefs == nil
			}
			raw, e := json.Marshal(struct {
				Aggregate Aggregate
				Revision  uint64
				NilMedia  map[string]bool
			}{m.a, m.revision, nilMedia})
			if e != nil {
				t.Fatal(e)
			}
			out[string(raw)] = true
			return
		}
		component := -1
		if plan != nil {
			var e error
			component, e = plan.next(&m, done)
			if e != nil {
				t.Fatal("reference work", e)
			}
		}
		for i, event := range events {
			if done[i] || (component >= 0 && plan.events[i] != component) {
				continue
			}
			u, e := m.apply(event, done)
			if e == ErrInvalid {
				continue
			}
			if e != nil {
				t.Fatal("reference apply resource failure", e)
			}
			done[i] = true
			visit(depth + 1)
			done[i] = false
			m.undo(u)
		}
	}
	visit(len(prefix))
	return out
}

func integrationComponentDifferential(t *testing.T) {
	type op struct{ action, target string }
	cases := []struct {
		name    string
		ops     []op
		prefix  []int
		media   []string
		parents []string
		empty   bool
		fault   bool
	}{
		{"independent-correct", []op{{"CORRECT", "a"}, {"CORRECT", "b"}}, nil, nil, nil, false, false},
		{"mixed", []op{{"CORRECT", "a"}, {"DELETE", "b"}, {"CORRECT", "c"}}, nil, nil, nil, false, false},
		{"replacement-target", []op{{"CORRECT", "a"}, {"CORRECT", "@0"}, {"DELETE", "b"}}, nil, nil, nil, false, false},
		{"shared-descendant", []op{{"CORRECT", "a"}, {"CORRECT", "b"}}, nil, nil, []string{"a", "b"}, false, false},
		{"one-urgent", []op{{"CORRECT", "a"}, {"DELETE", "@0"}, {"CORRECT", "b"}}, []int{0, 2}, nil, nil, false, false},
		{"two-urgent", []op{{"CORRECT", "a"}, {"DELETE", "@0"}, {"CORRECT", "b"}, {"DELETE", "@2"}}, []int{0, 2, 4, 6}, nil, nil, true, false},
		{"accepted-not-cleaned", []op{{"CORRECT", "a"}, {"CORRECT", "b"}}, []int{0}, nil, nil, false, false},
		{"empty-media-not-nil", []op{{"CORRECT", "a"}, {"CORRECT", "b"}}, nil, []string{}, nil, false, false},
		{"media-value", []op{{"CORRECT", "a"}, {"CORRECT", "b"}}, nil, []string{"original-media"}, nil, false, false},
		{"media-inheritance-interleaving", []op{{"DELETE", "a"}, {"CORRECT", "a"}, {"DELETE", "b"}}, nil, []string{}, nil, false, false},
		{"unblocked-hash-mismatch", []op{{"CORRECT", "a"}, {"CORRECT", "b"}}, []int{0}, nil, nil, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := pureBirthBase()
			n := base.Nodes["a"]
			n.MediaRefs = tc.media
			base.Nodes["a"] = n
			if tc.parents != nil {
				n := base.Nodes["c"]
				n.Parents = tc.parents
				base.Nodes["c"] = n
			}
			actual := copyIntegrationProof(t, base)
			n = actual.Nodes["a"]
			n.MediaRefs = tc.media
			actual.Nodes["a"] = n
			events := []integrationBirthEvent{}
			replacements := map[string]string{}
			for i, operation := range tc.ops {
				target := operation.target
				if id, ok := replacements[target]; ok {
					target = id
				}
				task := fmt.Sprintf("exhaustive-%d", i)
				replacement, body := "", ""
				if operation.action == "CORRECT" {
					replacement = content.Identity(task, "replacement")
					body = "exact corrected " + task
					replacements[fmt.Sprintf("@%d", i)] = replacement
				}
				b, e := actual.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{target}, Expected: map[string]uint64{target: actual.Nodes[target].Revision}}, operation.action, nil, replacement, body)
				if e != nil {
					t.Fatal("actual Aggregate transformation", e)
				}
				accept := len(events)
				events = append(events, integrationBirthEvent{task: task, binding: &b, accept: -1}, integrationBirthEvent{task: task, binding: &b, cleanup: true, accept: accept})
				actual.Clean()
			}
			var fault func(*integrationBirthMachine)
			if tc.fault {
				fault = func(m *integrationBirthMachine) {
					id := events[0].binding.ReplacementID
					n := m.a.Nodes[id]
					n.Body = "wrong-abstract-hash"
					m.a.Nodes[id] = n
				}
			}
			want := integrationComponentTerminalSet(t, base, events, tc.prefix, false, fault)
			got := integrationComponentTerminalSet(t, base, events, tc.prefix, true, fault)
			if tc.empty != (len(want) == 0) {
				t.Fatal("reference reachability", len(want), tc.empty)
			}
			if tc.name == "media-inheritance-interleaving" && len(want) < 2 {
				t.Fatal("strict nil fallback must preserve multiple observable terminal states", len(want))
			}
			if len(want) != len(got) {
				t.Fatal("complete terminal set size", len(want), len(got))
			}
			for state := range want {
				if !got[state] {
					t.Fatal("component reduction lost or invented a terminal state")
				}
			}
			t.Logf("original apply exhaustive terminal set=%d matched reduced set", len(want))
			m := integrationBirthMachine{a: integrationAbstract(base), revision: 1, work: MaxJournalBytes, ctx: context.Background()}
			g, e := integrationDeleteGraph(&m, base, events)
			if e != nil {
				t.Fatal(e)
			}
			m.work = 0
			if _, _, e = g.correctComponents(&m, events, make([]bool, len(events))); e == nil {
				t.Fatal("plan ignored work budget")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			m.ctx = ctx
			m.work = MaxJournalBytes
			if _, _, e = g.correctComponents(&m, events, make([]bool, len(events))); e == nil {
				t.Fatal("plan ignored cancellation")
			}
		})
	}
}

func TestIntegrationTurnProducerProofReplay(t *testing.T) {
	t.Run("component-original-apply-differential", integrationComponentDifferential)
	base := pureBirthBase()
	actual := copyIntegrationProof(t, base)
	in := pureBirthInput(t, &actual, 1, "input-proof")
	derived := pureBirthDerived(t, &actual, 2, in.MemoryIDs, 1)
	evidence := []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(in), pureBirthEvidence(derived)}
	require := func(t *testing.T, a Aggregate, b map[string]Binding, ev []store.IntegrationMemoryBirthEvidence, revision uint64) {
		t.Helper()
		ids, e := integrationBirthReplay(context.Background(), copyIntegrationProof(t, base), a, b, ev, revision)
		if e != nil || len(ids) != len(a.Nodes) {
			t.Fatal("complete pure producer/transform proof", e, len(ids), len(a.Nodes))
		}
	}

	t.Run("actual-production-byte-guard-boundaries", func(t *testing.T) {
		// These are the exact writer/decoder/reader/replay arithmetic inputs, not
		// claims that an arbitrary giant JSON object is an admitted producer.
		const single uint64 = 16 << 20
		const total uint64 = 32 << 20
		for _, tc := range []struct {
			name             string
			used, size, want uint64
			ok               bool
		}{
			{"single-exact", 0, single, single, true}, {"single-plus-one", 0, single + 1, 0, false},
			{"aggregate-exact", single, single, total, true}, {"aggregate-plus-one", single + 1, single, 0, false},
			{"remaining-one", total - 1, 1, total, true}, {"no-remaining", total, 1, 0, false},
			{"zero-document", 0, 0, 0, false}, {"total-outside-domain", total + 1, 1, 0, false},
			{"max-size", 0, ^uint64(0), 0, false}, {"max-total", ^uint64(0), 1, 0, false},
			{"addition-would-wrap", ^uint64(0) - 7, 16, 0, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, e := store.IntegrationMemoryBirthBudget(tc.used, tc.size)
				if (e == nil) != tc.ok || got != tc.want {
					t.Fatal("production byte guard", got, e, tc)
				}
			})
		}
		sum, e := store.IntegrationMemoryBirthBudget(0, single)
		if e != nil {
			t.Fatal(e)
		}
		sum, e = store.IntegrationMemoryBirthBudget(sum, single)
		if e != nil || sum != total {
			t.Fatal("exact aggregate sequence", sum, e)
		}
		if _, e = store.IntegrationMemoryBirthBudget(sum, 1); e == nil {
			t.Fatal("overflowing next member admitted")
		}
	})
	t.Run("actual-input-and-AddDerived", func(t *testing.T) { require(t, actual, nil, evidence, 3) })
	t.Run("correct-cleared-chain-and-parent-delete", func(t *testing.T) {
		a := copyIntegrationProof(t, actual)
		bindings := map[string]Binding{}
		target := derived.MemoryIDs[0]
		for i := 0; i < 2; i++ {
			task := fmt.Sprintf("pure-correct-%d", i)
			id := content.Identity(task, "replacement")
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{target}, Expected: map[string]uint64{target: a.Nodes[target].Revision}}, "CORRECT", nil, id, "corrected pure body")
			if e != nil {
				t.Fatal(e)
			}
			bindings[task] = b
			a.Clean()
			target = id
		}
		require(t, a, bindings, evidence, 7)
		parent := in.MemoryIDs[0]
		b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{parent}, Expected: map[string]uint64{parent: a.Nodes[parent].Revision}}, "DELETE", nil, "", "")
		if e != nil {
			t.Fatal(e)
		}
		bindings["pure-parent-delete"] = b
		a.Clean()
		require(t, a, bindings, evidence, 9)
		if !a.Nodes[target].Blocked || a.Nodes[target].Body != "" {
			t.Fatal("real parent propagation")
		}
	})
	for _, kind := range []string{"source-body", "source-static", "node-body", "node-static", "missing-birth", "wrong-parent", "wrong-time"} {
		t.Run(kind, func(t *testing.T) {
			a := copyIntegrationProof(t, actual)
			ev := append([]store.IntegrationMemoryBirthEvidence(nil), evidence...)
			id := in.MemoryIDs[0]
			switch kind {
			case "source-body":
				v := a.Sources[in.InputSourceID]
				v.Body = "wrong"
				a.Sources[v.ID] = v
			case "source-static":
				v := a.Sources[in.InputSourceID]
				v.EstimatedTime = true
				a.Sources[v.ID] = v
			case "node-body":
				v := a.Nodes[id]
				v.Body = "wrong"
				a.Nodes[id] = v
			case "node-static":
				v := a.Nodes[id]
				v.TargetKind = "CHARACTER"
				a.Nodes[id] = v
			case "missing-birth":
				ev = ev[1:]
			case "wrong-parent":
				d := derived
				d.ParentNodes = append([]store.IntegrationBirthNode(nil), derived.ParentNodes...)
				d.ParentNodes[0].Revision++
				ev[1] = pureBirthEvidence(d)
			case "wrong-time":
				d := in
				d.ObservedAt = d.ObservedAt.Add(time.Second)
				ev[0] = pureBirthEvidence(d)
			}
			ids, e := integrationBirthReplay(context.Background(), copyIntegrationProof(t, base), a, nil, ev, 3)
			for _, got := range ids {
				if got == id || got == derived.MemoryIDs[0] {
					t.Fatal("tampered component proved", kind, e)
				}
			}
		})
	}
	t.Run("cancel-and-work-budget", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, e := integrationBirthReplay(ctx, base, actual, nil, evidence, 3); e == nil {
			t.Fatal("cancel ignored")
		}
		m := integrationBirthMachine{a: integrationAbstract(base), revision: 1, work: 0, ctx: context.Background()}
		if _, e := m.apply(integrationBirthEvent{birth: &in}, nil); e != store.ErrDenied {
			t.Fatal("explicit primitive-work exhaustion", e)
		}
	})
	t.Run("legal-1000-node-32-parent-32-derived-codec", func(t *testing.T) {
		b := pureBirthBase()
		template := b.Sources["source-a"]
		node := b.Nodes["a"]
		for i := 3; i < 32; i++ {
			sid := fmt.Sprintf("capacity-source-%d", i)
			nid := fmt.Sprintf("capacity-node-%d", i)
			v := template
			v.ID = sid
			v.Intake = uint64(i + 1)
			b.Sources[sid] = v
			n := node
			n.ID = nid
			n.Sources = []string{sid}
			b.Nodes[nid] = n
			b.Grants = append(b.Grants, Grant{ActorID: "actor", SourceID: sid, Uses: []Use{PrivateResponse, Relationship}})
			b.Acquisitions = append(b.Acquisitions, Acquisition{ActorID: "actor", SourceID: sid, Kind: Initial, EvidenceID: "creation"})
		}
		b.Intake = 32
		parents := []string{}
		for id := range b.Nodes {
			parents = append(parents, id)
		}
		sort.Strings(parents)
		for i := 32; i < 968; i++ {
			n := node
			n.ID = fmt.Sprintf("legal-base-node-%d", i)
			b.Nodes[n.ID] = n
		}
		a := copyIntegrationProof(t, b)
		d := pureBirthDerived(t, &a, 1, parents, 32)
		ev := pureBirthEvidence(d)
		if len(a.Acquisitions) != 1056 || len(a.Nodes) != 1000 || len(ev.Document) > 16<<20 {
			t.Fatal("real legal limits/capacity", len(a.Acquisitions), len(a.Nodes), len(ev.Document))
		}
		ids, e := integrationBirthReplay(context.Background(), b, a, nil, []store.IntegrationMemoryBirthEvidence{ev}, 2)
		if e != nil || len(ids) != 1000 {
			t.Fatal("legal capacity proof", e, len(ids))
		}
		t.Logf("pure codec actual bytes=%d parents=%d sourceParents=%d born=%d acquisitions=%d; not SQL writer capacity/performance", len(ev.Document), len(d.ParentNodes), len(d.ParentSources), len(d.NodesBorn), len(a.Acquisitions))
	})
	t.Run("ten-repeated-delete-unrelated-input", func(t *testing.T) {
		a := copyIntegrationProof(t, actual)
		bindings := map[string]Binding{}
		for i := 0; i < 10; i++ {
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a"}, Expected: map[string]uint64{"a": a.Nodes["a"].Revision}}, "DELETE", nil, "", "")
			if e != nil {
				t.Fatal(e)
			}
			bindings[fmt.Sprintf("repeat-delete-%02d", i)] = b
			a.Clean()
		}
		require(t, a, bindings, evidence, 23)
	})
	t.Run("correct-clean-before-independent-input-ten-old-deletes", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		bindings := map[string]Binding{}
		task := "prefix-correct"
		replacement := content.Identity(task, "replacement")
		b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a"}, Expected: map[string]uint64{"a": 1}}, "CORRECT", nil, replacement, "preserved correction")
		if e != nil {
			t.Fatal(e)
		}
		bindings[task] = b
		a.Clean()
		birth := pureBirthInput(t, &a, 3, "after-correct-input")
		for i := 0; i < 10; i++ {
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a"}, Expected: map[string]uint64{"a": a.Nodes["a"].Revision}}, "DELETE", nil, "", "")
			if e != nil {
				t.Fatal(e)
			}
			bindings[fmt.Sprintf("old-delete-%02d", i)] = b
			a.Clean()
		}
		require(t, a, bindings, []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(birth)}, 24)
	})

	t.Run("ten-independent-input-correct-components", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		ev := []store.IntegrationMemoryBirthEvidence{}
		targets := []string{}
		bindings := map[string]Binding{}
		for i := 0; i < 10; i++ {
			d := pureBirthInput(t, &a, uint64(i+1), fmt.Sprintf("correct-input-%02d", i))
			targets = append(targets, d.MemoryIDs[0])
			ev = append(ev, pureBirthEvidence(d))
		}
		require(t, a, nil, ev, 11)
		for i, id := range targets {
			task := fmt.Sprintf("independent-correct-%02d", i)
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{id}, Expected: map[string]uint64{id: 1}}, "CORRECT", nil, content.Identity(task, "replacement"), fmt.Sprintf("corrected input %d", i))
			if e != nil {
				t.Fatal("actual Aggregate CORRECT", e)
			}
			bindings[task] = b
			a.Clean()
		}
		require(t, a, bindings, ev, 31)
	})

	t.Run("ten-independent-input-delete-accepts", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		ev := []store.IntegrationMemoryBirthEvidence{}
		targets := []string{}
		bindings := map[string]Binding{}
		for i := 0; i < 10; i++ {
			d := pureBirthInput(t, &a, uint64(i+1), fmt.Sprintf("independent-input-%02d", i))
			targets = append(targets, d.MemoryIDs[0])
			ev = append(ev, pureBirthEvidence(d))
		}
		for i, id := range targets {
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{id}, Expected: map[string]uint64{id: 1}}, "DELETE", nil, "", "")
			if e != nil {
				t.Fatal(e)
			}
			bindings[fmt.Sprintf("independent-delete-%02d", i)] = b
			a.Clean()
		}
		require(t, a, bindings, ev, 31)
	})

	t.Run("tail-reduction-dependency-controls", func(t *testing.T) {
		del := Binding{Action: "DELETE", TargetKind: "MEMORY_NODE", Targets: []string{"a"}}
		correct := Binding{Action: "CORRECT", TargetKind: "MEMORY_NODE", Targets: []string{"a"}}
		for _, tc := range []struct {
			name        string
			event       integrationBirthEvent
			wantCleanup bool
		}{
			{"pending-birth", integrationBirthEvent{birth: &in}, false},
			{"pending-correct-accept", integrationBirthEvent{binding: &correct}, false},
			{"pending-correct-cleanup", integrationBirthEvent{binding: &correct, cleanup: true}, false},
			{"pending-source", integrationBirthEvent{binding: &Binding{Action: "DELETE", TargetKind: "SOURCE", Targets: []string{"source-a"}}}, true},
			{"pending-policy", integrationBirthEvent{binding: &Binding{Action: "SET_USE_POLICY", TargetKind: "DISABLE", Uses: []Use{Relationship}}}, true},
			{"pending-revoke", integrationBirthEvent{binding: &Binding{Action: "REVOKE", TargetKind: "MEMORY_NODE", Targets: []string{"b"}, Uses: []Use{Relationship}}}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cleanup, component := integrationBirthTail([]integrationBirthEvent{{binding: &del}, tc.event}, []bool{false, false})
				if cleanup != tc.wantCleanup || component {
					t.Fatal("dependent suffix reduced", cleanup, component)
				}
			})
		}
		multi := Binding{Action: "DELETE", TargetKind: "MEMORY_NODE", Targets: []string{"a", "b"}}
		if _, component := integrationBirthTail([]integrationBirthEvent{{binding: &multi}}, []bool{false}); !component {
			t.Fatal("closed multi-target tail classification")
		}
	})
	t.Run("cleanup-before-birth-commitment", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		b := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a"}, Expected: map[string]uint64{"a": 1}}, "DELETE", nil)
		d := pureBirthInput(t, &a, 3, "birth-after-clean")
		require(t, a, map[string]Binding{"delete-before-birth": b}, []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(d)}, 4)
		d.BeforeAggregateRevision = 2
		d.AfterAggregateRevision = 3
		if _, e := integrationBirthReplay(context.Background(), copyIntegrationProof(t, base), a, map[string]Binding{"delete-before-birth": b}, []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(d)}, 4); e == nil {
			t.Fatal("birth accepted before required cleanup")
		}
	})
	t.Run("shared-source-descendant-and-multitarget-order", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		d := pureBirthDerived(t, &a, 1, []string{"a", "b"}, 1)
		ev := []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(d)}
		bindings := map[string]Binding{}
		bindings["z-first"] = proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{"a", "b"}, Expected: map[string]uint64{"a": 1, "b": 1}}, "DELETE", nil)
		bindings["a-second"] = proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{d.MemoryIDs[0]}, Expected: map[string]uint64{d.MemoryIDs[0]: 2}}, "DELETE", nil)
		require(t, a, bindings, ev, 6)
		bad := bindings["z-first"]
		bad.Revisions = map[string]uint64{"a": 2, "b": 1}
		bindings["z-first"] = bad
		if _, e := integrationBirthReplay(context.Background(), copyIntegrationProof(t, base), a, bindings, ev, 6); e == nil {
			t.Fatal("overlapping impossible revision accepted")
		}
	})

	t.Run("completed-correct-chain-independent-old-replacements", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		bindings := map[string]Binding{}
		old := []string{}
		target := "a"
		for i := 0; i < 10; i++ {
			old = append(old, target)
			task := fmt.Sprintf("closed-chain-%02d", i)
			id := content.Identity(task, "replacement")
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{target}, Expected: map[string]uint64{target: a.Nodes[target].Revision}}, "CORRECT", nil, id, "next correction")
			if e != nil {
				t.Fatal(e)
			}
			bindings[task] = b
			a.Clean()
			target = id
		}
		birth := pureBirthInput(t, &a, 21, "after-complete-correction-chain")
		ev := []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(birth)}
		require(t, a, bindings, ev, 22)
		for i, id := range old {
			b := proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{id}, Expected: map[string]uint64{id: a.Nodes[id].Revision}}, "DELETE", nil)
			bindings[fmt.Sprintf("old-replacement-delete-%02d", i)] = b
		}
		require(t, a, bindings, ev, 42)
	})

	t.Run("ten-siblings-share-only-read-parent", func(t *testing.T) {
		a := copyIntegrationProof(t, base)
		d := pureBirthDerived(t, &a, 1, []string{"a"}, 10)
		bindings := map[string]Binding{}
		for i, id := range d.MemoryIDs {
			bindings[fmt.Sprintf("sibling-delete-%02d", i)] = proofBlock(t, &a, Selector{Kind: "IDS", TargetKind: "MEMORY_NODE", IDs: []string{id}, Expected: map[string]uint64{id: 1}}, "DELETE", nil)
		}
		require(t, a, bindings, []store.IntegrationMemoryBirthEvidence{pureBirthEvidence(d)}, 22)
	})
	t.Run("ten-equivalent-empty-batches", func(t *testing.T) {
		a := copyIntegrationProof(t, actual)
		ev := append([]store.IntegrationMemoryBirthEvidence(nil), evidence...)
		for i := 0; i < 10; i++ {
			d := pureBirthDerived(t, &a, 3, nil, 0)
			d.TaskID = fmt.Sprintf("empty-task-%02d", i)
			ev = append(ev, pureBirthEvidence(d))
		}
		require(t, a, nil, ev, 3)
	})
	t.Run("deterministic-1500-history", func(t *testing.T) {
		a := copyIntegrationProof(t, actual)
		bindings := map[string]Binding{}
		for i := 0; i < 1500; i++ {
			sid := "source-a"
			b, e := a.Block(Selector{Kind: "IDS", TargetKind: "SOURCE", IDs: []string{sid}, Expected: map[string]uint64{sid: a.Sources[sid].Revision}}, "REVOKE", []Use{Relationship}, "", "")
			if e != nil {
				t.Fatal(e)
			}
			bindings[fmt.Sprintf("deep-%04d", i)] = b
			a.Clean()
		}
		require(t, a, bindings, evidence, 3003)
	})
}
