package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	worldrt "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"sort"
	"strconv"
	"time"
)

type IntegrationMemoryPage struct {
	Scope        domain.Scope
	Aggregate    Aggregate
	Nodes        []MemoryRecord
	Epoch, View  uint64
	BodyReadable bool
	Next         string
}

// One original read owns every item in the page and its final watermark.
// The adapter never joins independent reads into an invented snapshot.
func (s *MemoryService) ListIntegrationMetadata(ctx context.Context, world, id, cursor, profile string, limit int, body bool) (IntegrationMemoryPage, error) {
	out := IntegrationMemoryPage{Nodes: []MemoryRecord{}}
	if e := s.BindIntegrationGovernance(); e != nil {
		return out, e
	}
	if limit < 1 || limit > 100 || (id != "" && (domain.ValidateID(id) != nil || cursor != "")) {
		return out, ErrInvalid
	}
	page, e := s.Store.BeginIntegrationMemoryPage(ctx, world, body)
	if e != nil {
		return out, e
	}
	defer page.Close()
	read := page.Read()
	after, _, view, e := s.Store.IntegrationMemoryPageCursor(ctx, page, cursor, profile, "")
	if e != nil {
		return out, e
	}
	if json.Unmarshal(read.MetadataBody(), &out.Aggregate) != nil || len(out.Aggregate.Nodes) > 1000 {
		return IntegrationMemoryPage{}, store.ErrDenied
	}
	out.Scope = read.Scope()
	out.Epoch = read.Epoch()
	out.View = view
	out.BodyReadable = false

	ids := []string{}
	for key := range out.Aggregate.Nodes {
		ids = append(ids, key)
	}
	sort.Strings(ids)
	more := false
	for _, key := range ids {
		if key <= after || (id != "" && key != id) {
			continue
		}
		node := out.Aggregate.Nodes[key]
		if node.ID != key || node.TargetID != out.Scope.SubjectID || node.Audience != Private || len(node.Sources) == 0 {
			continue
		}
		allowed := false
		for _, v := range out.Aggregate.Allowed(node.ActorID, Private, PrivateResponse, read.Now()) {
			allowed = allowed || v.ID == key
		}
		if !allowed {
			continue
		}
		sourceValid := true
		for _, sid := range node.Sources {
			source := out.Aggregate.Sources[sid]
			found := false
			if source.ID != sid || source.Revision == 0 || (source.Domain != WorldFact && source.Domain != ActorBelief && source.Domain != PlayerStatement) {
				sourceValid = false
				break
			}
			for _, a := range out.Aggregate.Acquisitions {
				if a.ActorID == node.ActorID && a.SourceID == sid && a.EvidenceID != "" && (a.Kind == Initial || a.Kind == Witnessed || a.Kind == Told || a.Kind == Inferred) {
					found = true
				}
			}
			sourceValid = sourceValid && found
		}
		if !sourceValid {
			continue
		}
		if len(out.Nodes) == limit {
			more = true
			break
		}
		if !body {
			node = MemoryRecord{ID: node.ID, Kind: node.Kind, ActorID: node.ActorID, TargetID: node.TargetID, Revision: node.Revision, Audience: Private, Sources: append([]string(nil), node.Sources...), Uses: []Use{PrivateResponse}}
		}
		out.Nodes = append(out.Nodes, node)
	}
	if id != "" && len(out.Nodes) != 1 {
		return IntegrationMemoryPage{}, store.ErrDenied
	}
	if more {
		_, out.Next, _, e = s.Store.IntegrationMemoryPageCursor(ctx, page, cursor, profile, out.Nodes[len(out.Nodes)-1].ID)
		if e != nil {
			return IntegrationMemoryPage{}, e
		}
	}
	selected := make([]string, 0, len(out.Nodes))
	for _, node := range out.Nodes {
		selected = append(selected, node.ID)
	}
	if e = read.SelectItems(selected); e != nil {
		return IntegrationMemoryPage{}, e
	}
	if body {
		if json.Unmarshal(read.Body(), &out.Aggregate) != nil {
			return IntegrationMemoryPage{}, store.ErrDenied
		}
		for i, id := range selected {
			out.Nodes[i] = out.Aggregate.Nodes[id]
		}
	}
	if e = s.Store.FinishIntegrationMemoryPage(ctx, read, view); e != nil {
		return IntegrationMemoryPage{}, e
	}
	out.BodyReadable = read.FinalBodyReadable()
	return out, nil
}

// Default metadata retains the schema-required real source/acquisition identity.
// PRIVATE_RESPONSE describes the material; it does not grant positive use.
func (s *MemoryService) ReadIntegrationMemoryMetadata(ctx context.Context, world, id string) (Aggregate, MemoryRecord, uint64, error) {
	var empty Aggregate
	var zero MemoryRecord
	if domain.ValidateID(id) != nil {
		return empty, zero, 0, store.ErrDenied
	}
	if e := s.BindIntegrationGovernance(); e != nil {
		return empty, zero, 0, e
	}
	read, e := s.Store.BeginIntegrationMemoryMetadata(ctx, world)
	if e != nil {
		return empty, zero, 0, e
	}
	defer read.Close()
	var aggregate Aggregate
	if json.Unmarshal(read.Body(), &aggregate) != nil {
		return empty, zero, 0, store.ErrDenied
	}
	node, ok := aggregate.Nodes[id]
	if !ok || node.ID != id || node.TargetID != read.Scope().SubjectID || node.Audience != Private || len(node.Sources) == 0 {
		return empty, zero, 0, store.ErrDenied
	}
	allowed := false
	for _, candidate := range aggregate.Allowed(node.ActorID, Private, PrivateResponse, read.Now()) {
		allowed = allowed || candidate.ID == id
	}
	if !allowed {
		return empty, zero, 0, store.ErrDenied
	}
	safe := MemoryRecord{ID: node.ID, Kind: node.Kind, ActorID: node.ActorID, TargetID: node.TargetID, Revision: node.Revision, Audience: Private, Sources: append([]string(nil), node.Sources...), Uses: []Use{PrivateResponse}}
	metadata := Aggregate{Sources: map[string]Source{}}
	for _, id := range node.Sources {
		source := aggregate.Sources[id]
		if source.ID != id || source.Revision == 0 || (source.Domain != WorldFact && source.Domain != ActorBelief && source.Domain != PlayerStatement) {
			return empty, zero, 0, store.ErrDenied
		}
		metadata.Sources[id] = Source{ID: id, Domain: source.Domain, Revision: source.Revision}
		found := false
		for _, acq := range aggregate.Acquisitions {
			if acq.SourceID == id && acq.ActorID == node.ActorID && acq.EvidenceID != "" && (acq.Kind == Initial || acq.Kind == Witnessed || acq.Kind == Told || acq.Kind == Inferred) {
				metadata.Acquisitions = append(metadata.Acquisitions, acq)
				found = true
			}
		}
		if !found {
			return empty, zero, 0, store.ErrDenied
		}
	}
	if e = read.SelectItems([]string{id}); e != nil {
		return empty, zero, 0, e
	}
	if e = read.Finish(ctx); e != nil {
		return empty, zero, 0, e
	}
	return metadata, safe, read.Epoch(), nil
}

// CLIENT body reads require both present use and the original published
// lineage. Neither a PRIVATE label nor a copied scope is an authority proof.
func (s *MemoryService) ReadIntegrationMemory(ctx context.Context, world, id string) (Aggregate, MemoryRecord, uint64, error) {
	var empty Aggregate
	var zero MemoryRecord
	if domain.ValidateID(id) != nil {
		return empty, zero, 0, store.ErrDenied
	}
	if e := s.BindIntegrationGovernance(); e != nil {
		return empty, zero, 0, e
	}
	read, e := s.Store.BeginIntegrationMemoryRead(ctx, world)
	if e != nil {
		return empty, zero, 0, e
	}
	defer read.Close()
	scope := read.Scope()
	var a Aggregate
	if json.Unmarshal(read.MetadataBody(), &a) != nil {
		return empty, zero, 0, store.ErrDenied
	}
	node, ok := a.Nodes[id]
	if !ok || node.TargetID != scope.SubjectID {
		return empty, zero, 0, store.ErrDenied
	}
	allowed := false
	for _, v := range a.Allowed(node.ActorID, Private, PrivateResponse, read.Now()) {
		if v.ID == id {
			allowed = true
		}
	}
	if !allowed {
		return empty, zero, 0, store.ErrDenied
	}
	if e = read.SelectItems([]string{id}); e != nil {
		return empty, zero, 0, e
	}
	a = Aggregate{}
	if json.Unmarshal(read.Body(), &a) != nil {
		return empty, zero, 0, store.ErrDenied
	}
	node = a.Nodes[id]
	if e = read.Finish(ctx); e != nil {
		return empty, zero, 0, e
	}
	return a, node, read.Epoch(), nil
}

// The installed owner alone receives full proof facts. All Ledger calls occur
// with no SQL transaction, row iterator, or memory lock held.
func (s *MemoryService) PrepareIntegrationMemoryProof(ctx context.Context, r *store.IntegrationMemoryRead) ([]byte, error) {
	owner, e := s.integrationGovernanceOwner(ctx)
	if e != nil {
		return nil, e
	}
	f, e := s.Store.IntegrationMemoryProofInput(owner, r)
	if e != nil {
		return nil, e
	}
	head, e := s.Ledger.LatestHead(ctx, "integration-current-read")
	if e != nil {
		return nil, e
	}
	scope := []string{f.Scope.TenantID, f.Scope.ProductID, f.Scope.WorldInstanceID, f.Scope.SubjectID, f.Scope.SubjectID}
	entries := map[string]LedgerEntry{}
	after := uint64(0)
	for after < head.Sequence {
		page, e := s.Ledger.ReadAfter(ctx, head, after, 100)
		if e != nil || len(page) == 0 {
			return nil, store.ErrDenied
		}
		for _, entry := range page {
			if entry.Sequence != after+1 {
				return nil, store.ErrDenied
			}
			after = entry.Sequence
			if reflect.DeepEqual(entry.Reservation.Scope, scope) {
				entries[entry.Reservation.ID] = entry
			}
		}
	}
	if e = s.Ledger.VerifyHead(ctx, head); e != nil {
		return nil, e
	}
	history, e := s.Store.PlanIntegrationMemoryHistory(ctx, owner, r, len(entries))
	if e != nil {
		return nil, e
	}
	mutations := map[string]Binding{}
	for _, row := range history {
		entry, ok := entries[row.Task]
		if !ok || entry.Kind != "APPENDED" || entry.Binding == nil || entry.Reservation.RequestHash != row.Hash {
			return nil, store.ErrDenied
		}
		var b Binding
		if json.Unmarshal(row.Binding, &b) != nil || !reflect.DeepEqual(b, *entry.Binding) {
			return nil, store.ErrDenied
		}
		mutations[row.Task] = b
	}
	for id, entry := range entries {
		if entry.Kind == "APPENDED" {
			if _, ok := mutations[id]; !ok {
				return nil, store.ErrDenied
			}
		}
	}
	f.Births, e = s.Store.PlanIntegrationMemoryBirths(ctx, owner, r)
	if e != nil {
		return nil, e
	}
	ids, e := proveIntegrationCurrent(ctx, f, mutations)
	if e != nil {
		return nil, e
	}
	if e = s.Store.SealIntegrationMemoryComponents(owner, r, ids); e != nil {
		return nil, e
	}
	if e = s.Ledger.VerifyHead(ctx, head); e != nil {
		return nil, e
	}
	return json.Marshal(head)
}
func (s *MemoryService) FinishIntegrationMemoryProof(ctx context.Context, r *store.IntegrationMemoryRead, raw []byte) error {
	owner, e := s.integrationGovernanceOwner(ctx)
	if e != nil {
		return e
	}
	if e = s.Store.CheckIntegrationMemoryProofFinal(owner, r); e != nil {
		return e
	}
	var h Head
	if json.Unmarshal(raw, &h) != nil {
		return store.ErrDenied
	}
	return s.Ledger.VerifyHead(ctx, h)
}

// Reconstruct only the already-published initial family. Publication objects
// were validated by Store in the short SQL phase, never fetched here.
func integrationReadBase(f store.IntegrationMemoryProofFacts) (Aggregate, error) {
	n := Namespace{Scope: f.Scope, DataSubjectID: f.Scope.SubjectID}
	a := emptyAggregate(n)
	if len(f.Publications) == 0 || f.Lineage == nil {
		return a, store.ErrDenied
	}
	expected, e := content.InspectIntegrationPublishedLineage(f.Publications[0], f.Publications[1:], f.Scope, f.Scope, f.Lineage.Customer, f.Scope.WorldInstanceID)
	if e != nil || !reflect.DeepEqual(expected, f.Lineage) {
		return a, store.ErrDenied
	}
	var init worldrt.Initialization
	if json.Unmarshal(f.Initial, &init) != nil || !reflect.DeepEqual(init.IntegrationInitialLineage, f.Lineage) {
		return a, store.ErrDenied
	}
	add := func(id, actor, kind, body string) {
		if _, ok := a.Sources[id]; !ok {
			a.Intake++
			a.Sources[id] = Source{ID: id, Body: body, Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, EstimatedTime: true}
		}
		nid := content.Identity(id, actor, "initial")
		if _, ok := a.Nodes[nid]; ok {
			return
		}
		a.Grants = append(a.Grants, Grant{ActorID: actor, SourceID: id, Uses: []Use{PrivateResponse, Relationship}})
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: actor, SourceID: id, Kind: Initial, EvidenceID: init.CreationCommandID})
		a.Nodes[nid] = MemoryRecord{ID: nid, ActorID: actor, TargetKind: "PLAYER", TargetID: n.SubjectID, Kind: kind, Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Sources: []string{id}, Body: body, Revision: 1}
	}
	for _, actor := range init.Actors {
		for i, goal := range append(append([]string{}, actor.OriginalSetup.InitialGoals...), actor.OriginalCharacter.LongTermGoals...) {
			add(content.Identity(init.CreationCommandID, actor.ID, "goal", strconv.Itoa(i)), actor.ID, "GOAL", goal)
		}
		var owner struct {
			Kind    string `json:"kind"`
			Release string `json:"release_id"`
		}
		if json.Unmarshal(actor.OwnerReference, &owner) != nil {
			return a, store.ErrDenied
		}
		cr := init.ReleaseID
		if owner.Kind == "PINNED_DEFINITION" {
			cr = owner.Release
		} else if owner.Kind != "LOCAL_DRAFT" {
			return a, store.ErrDenied
		}
		type ref struct {
			Raw   generated.DefinitionRef
			Local string
		}
		refs := []ref{}
		for _, r := range actor.OriginalSetup.KnowledgeRefs {
			refs = append(refs, ref{r, init.ReleaseID})
		}
		for _, r := range actor.OriginalCharacter.InitialKnowledgeRefs {
			refs = append(refs, ref{r, cr})
		}
		for _, r := range refs {
			var q struct {
				Kind    string `json:"kind"`
				Release string `json:"release_id"`
				ID      string `json:"object_id"`
				Version string `json:"version"`
				Hash    string `json:"content_hash"`
			}
			if json.Unmarshal(r.Raw, &q) != nil {
				return a, store.ErrDenied
			}
			release := r.Local
			if q.Kind == "PINNED_DEFINITION" {
				release = q.Release
			} else if q.Kind != "LOCAL_DRAFT" {
				return a, store.ErrDenied
			}
			found := false
			for _, pub := range f.Publications {
				if string(pub.ReleaseID) != release {
					continue
				}
				for _, object := range pub.Input.Objects {
					var h struct {
						ID      string `json:"object_id"`
						Version string `json:"version"`
					}
					if json.Unmarshal(object, &h) != nil {
						return a, store.ErrDenied
					}
					if h.ID != q.ID {
						continue
					}
					var knowledge generated.KnowledgeDefinition
					if generated.Decode("KnowledgeDefinition", object, &knowledge) != nil {
						return a, store.ErrDenied
					}
					if q.Kind == "PINNED_DEFINITION" {
						var v any
						if json.Unmarshal(object, &v) != nil {
							return a, store.ErrDenied
						}
						hash, e := content.Hash(v)
						if e != nil || hash != q.Hash || h.Version != q.Version {
							return a, store.ErrDenied
						}
					}
					if found {
						return a, store.ErrDenied
					}
					found = true
					add(content.Identity("initial-knowledge", release, h.ID, h.Version), actor.ID, "BELIEF", knowledge.Text)
				}
			}
			if !found {
				return a, store.ErrDenied
			}
		}
	}
	if e = checkIntegrationLineage(a, f.Lineage, n); e != nil {
		return a, e
	}
	return a, nil
}

type integrationNodeEffect struct {
	Task    string
	Binding Binding
}

func proveIntegrationCurrent(ctx context.Context, f store.IntegrationMemoryProofFacts, mutations map[string]Binding) ([]string, error) {
	base, e := integrationReadBase(f)
	if e != nil {
		return nil, e
	}
	var actual Aggregate
	if json.Unmarshal(f.Document, &actual) != nil {
		return nil, store.ErrDenied
	}
	if len(f.Births) > 0 {
		return integrationBirthReplay(ctx, base, actual, mutations, f.Births, f.AggregateRevision)
	}
	return integrationProvenComponents(ctx, base, actual, mutations)
}

// Pure structural proof; the production caller first validates publication and initial lineage.
func proveIntegrationAggregate(ctx context.Context, base, actual Aggregate, mutations map[string]Binding) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if len(actual.Nodes) > MaxNodes || len(actual.Sources) != len(base.Sources) || actual.Epoch != base.Epoch || actual.Intake != base.Intake || !reflect.DeepEqual(actual.Grants, base.Grants) || !reflect.DeepEqual(actual.Acquisitions, base.Acquisitions) {
		return store.ErrDenied
	}
	direct := map[string][]integrationNodeEffect{}
	sources := map[string][]integrationNodeEffect{}
	creators := map[string]integrationNodeEffect{}
	policies := []Binding{}
	for task, b := range mutations {
		if b.SourceEpoch != base.Epoch || b.Cutoff != base.Intake || len(b.SelectorHash) != 64 {
			return store.ErrDenied
		}
		if b.Action == "SET_USE_POLICY" {
			policies = append(policies, b)
			continue
		}
		if b.Action != "DELETE" && b.Action != "REVOKE" && b.Action != "CORRECT" {
			return store.ErrDenied
		}
		if len(b.Uses) > 0 && !validUses(b.Uses) {
			return store.ErrDenied
		}
		if b.Action != "CORRECT" && (b.ReplacementID != "" || b.ReplacementHash != "") {
			return store.ErrDenied
		}
		if b.Action == "CORRECT" {
			if b.TargetKind != "MEMORY_NODE" || len(b.Targets) != 1 || b.ReplacementID != content.Identity(task, "replacement") || len(b.ReplacementHash) != 64 {
				return store.ErrDenied
			}
			if _, ok := creators[b.ReplacementID]; ok {
				return store.ErrDenied
			}
			creators[b.ReplacementID] = integrationNodeEffect{task, b}
		}
		seen := map[string]bool{}
		for _, id := range b.Targets {
			if seen[id] {
				return store.ErrDenied
			}
			seen[id] = true
			effect := integrationNodeEffect{task, b}
			switch b.TargetKind {
			case "SOURCE":
				if _, ok := base.Sources[id]; !ok || b.Action == "CORRECT" {
					return store.ErrDenied
				}
				sources[id] = append(sources[id], effect)
			case "MEMORY_NODE":
				if b.Revisions[id] == 0 || len(b.Revisions) != len(b.Targets) {
					return store.ErrDenied
				}
				direct[id] = append(direct[id], effect)
			default:
				return store.ErrDenied
			}
		}
	}
	if len(actual.Nodes) != len(base.Nodes)+len(creators) {
		return store.ErrDenied
	}
	for id, list := range direct {
		if _, ok := actual.Nodes[id]; !ok {
			return store.ErrDenied
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Binding.Revisions[id] < list[j].Binding.Revisions[id] })
		for i := 1; i < len(list); i++ {
			if list[i-1].Binding.Revisions[id] == list[i].Binding.Revisions[id] {
				return store.ErrDenied
			}
		}
		direct[id] = list
	}
	// The old policy format does not encode a total order. Same-direction sets
	// commute; mixed ENABLE/DISABLE cannot be used as positive evidence here.
	direction := ""
	disabled := append([]Use(nil), base.Disabled...)
	for _, b := range policies {
		if !validUses(b.Uses) || (b.TargetKind != "ENABLE" && b.TargetKind != "DISABLE") {
			return store.ErrDenied
		}
		if direction != "" && direction != b.TargetKind {
			return store.ErrDenied
		}
		direction = b.TargetKind
		if direction == "DISABLE" {
			for _, u := range b.Uses {
				if !contains(disabled, u) {
					disabled = append(disabled, u)
				}
			}
		} else {
			disabled = removeUses(disabled, b.Uses)
		}
	}
	expectedSet, ok := integrationPurposeSet(disabled)
	if !ok {
		return store.ErrDenied
	}
	actualSet, ok := integrationPurposeSet(actual.Disabled)
	if !ok {
		return store.ErrDenied
	}
	if actual.PolicyRevision != base.PolicyRevision+uint64(len(policies)) || !reflect.DeepEqual(actualSet, expectedSet) {
		return store.ErrDenied
	}
	sourceRemoved := map[string][]Use{}
	unreadable := map[string]bool{}
	for id, original := range base.Sources {
		want := original
		for _, effect := range sources[id] {
			b := effect.Binding
			if b.Action == "REVOKE" && len(b.Uses) > 0 {
				want.Uses = removeUses(want.Uses, b.Uses)
				sourceRemoved[id] = append(sourceRemoved[id], b.Uses...)
			} else {
				want.Blocked = true
			}
			if want.Revision == ^uint64(0) {
				return store.ErrDenied
			}
			want.Revision++
		}
		got, ok := actual.Sources[id]
		if !ok || got.OccurredAt.IsZero() {
			return store.ErrDenied
		}
		want.OccurredAt = got.OccurredAt
		if want.Blocked {
			want.Body = ""
		}
		// Current source bytes, not just a reconstructed copy, must match the
		// published commitment while readable. All other Source fields are exact.
		if !want.Blocked {
			sum := sha256.Sum256([]byte(got.Body))
			orig := sha256.Sum256([]byte(original.Body))
			if sum != orig {
				return store.ErrDenied
			}
		}
		if !reflect.DeepEqual(got, want) {
			return store.ErrDenied
		}
		unreadable[id] = want.Blocked || !contains(want.Uses, PrivateResponse)
	}
	type nodeProof struct {
		birth  uint64
		record MemoryRecord
		hash   string
	}
	proven := map[string]nodeProof{}
	for id, n := range base.Nodes {
		proven[id] = nodeProof{0, n, digest(n.Body)}
	}
	for len(proven) < len(actual.Nodes) {
		progress := false
		for id, producer := range creators {
			if _, ok := proven[id]; ok {
				continue
			}
			parentID := producer.Binding.Targets[0]
			parent, ok := proven[parentID]
			if !ok {
				continue
			}
			n := parent.record
			if len(n.Sources) != 1 || len(n.Parents) != 0 || len(n.MediaRefs) != 0 || !n.ExpiresAt.IsZero() {
				return store.ErrDenied
			}
			before := uint64(0)
			found := false
			for _, event := range direct[parentID] {
				if event.Task == producer.Task {
					found = true
					break
				}
				before++
				if event.Binding.Action == "REVOKE" && len(event.Binding.Uses) > 0 {
					n.Uses = removeUses(n.Uses, event.Binding.Uses)
				}
			}
			rev := producer.Binding.Revisions[parentID]
			if !found || rev <= before {
				return store.ErrDenied
			}
			delta := rev - 1 - before
			birth := parent.birth + delta
			if birth < parent.birth || birth > uint64(len(sources[n.Sources[0]])) {
				return store.ErrDenied
			}
			n.ID = id
			n.Corrects = parentID
			n.Revision = 1
			n.Blocked = false
			n.Body = ""
			if n.Domain == WorldFact {
				return store.ErrDenied
			}
			proven[id] = nodeProof{birth, n, producer.Binding.ReplacementHash}
			progress = true
		}
		if !progress {
			return store.ErrDenied
		}
	}
	// Every multi-target command must have one feasible source-count cut. This
	// family has one source per node. A witness must satisfy all cuts and
	// producer/direct dependencies; Ledger append order supplies no constraint.
	taskCuts := map[string]map[string]uint64{}
	for id, p := range proven {
		want := p.record
		got, ok := actual.Nodes[id]
		if !ok || len(want.Sources) != 1 || len(want.Parents) != 0 || len(want.MediaRefs) != 0 || !want.ExpiresAt.IsZero() {
			return store.ErrDenied
		}
		sid := want.Sources[0]
		total := uint64(len(sources[sid]))
		last := p.birth
		for i, event := range direct[id] {
			rev := event.Binding.Revisions[id]
			if rev <= uint64(i) {
				return store.ErrDenied
			}
			cut := p.birth + rev - 1 - uint64(i)
			if cut < p.birth || cut < last || cut > total {
				return store.ErrDenied
			}
			last = cut
			cuts := taskCuts[event.Task]
			if cuts == nil {
				cuts = map[string]uint64{}
				taskCuts[event.Task] = cuts
			}
			if previous, ok := cuts[sid]; ok && previous != cut {
				return store.ErrDenied
			}
			cuts[sid] = cut
			b := event.Binding
			if b.Action == "REVOKE" && len(b.Uses) > 0 {
				want.Uses = removeUses(want.Uses, b.Uses)
				if len(want.Uses) == 0 {
					want.Blocked = true
				}
			} else {
				want.Blocked = true
			}
		}
		want.Uses = removeUses(want.Uses, sourceRemoved[sid])
		if len(want.Uses) == 0 {
			want.Blocked = true
		}
		want.Revision = 1 + uint64(len(direct[id])) + total - p.birth
		if unreadable[sid] {
			// Denied components cannot create body authority. Their invariant and
			// revision proof remains exact; historical block/reset order is not minted.
			want.Blocked = got.Blocked
		}
		want.Body = got.Body
		if got.Blocked {
			if got.Body != "" {
				return store.ErrDenied
			}
		} else if !unreadable[sid] && digest(got.Body) != p.hash {
			return store.ErrDenied
		}
		if !reflect.DeepEqual(got, want) {
			return store.ErrDenied
		}
	}
	return integrationSourceCuts(ctx, mutations, direct, creators, taskCuts)
}

// Comparison copies only: persisted order and JSON/digest bytes are untouched.
func integrationPurposeSet(v []Use) ([]Use, bool) {
	if len(v) > 0 && !validUses(v) {
		return nil, false
	}
	out := append([]Use{}, v...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, true
}

// Find a feasible order, not the original unrecorded order. Source IDS revisions
// constrain their own source counts; ALL_EXISTING/TIME_RANGE do not invent ranks.
// A single source event increments every selected source once. Direct commands
// occupy exact cuts and preserve per-node and CORRECT producer dependencies.
// The original read's deadline bounds ambiguous search; exhaustion is no proof.

func integrationSourceCuts(ctx context.Context, mutations map[string]Binding, direct map[string][]integrationNodeEffect, creators map[string]integrationNodeEffect, cuts map[string]map[string]uint64) error {
	return integrationSourceCutsBudget(ctx, mutations, direct, creators, cuts, MaxJournalBytes)
}

// The budget counts primitive search work, not bytes of allocated memory.
// It is tied to the existing journal bound; no history is truncated. Graph,
// counters, undo log and explicit frames are linear in supplied tasks/edges.
func integrationSourceCutsBudget(ctx context.Context, mutations map[string]Binding, direct map[string][]integrationNodeEffect, creators map[string]integrationNodeEffect, cuts map[string]map[string]uint64, budget int) error {
	work := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if budget <= 0 {
			return store.ErrDenied
		}
		budget--
		return nil
	}
	ids := []string{}
	deps := map[string]map[string]bool{}
	sourceCuts := map[string]map[string]uint64{}
	sourceTotal := map[string]int{}
	ranks := map[string]map[uint64]string{}
	ranked := true
	for id, b := range mutations {
		if e := work(); e != nil {
			return e
		}
		if b.Action == "SET_USE_POLICY" {
			continue
		}
		ids = append(ids, id)
		deps[id] = map[string]bool{}
		if b.TargetKind == "SOURCE" {
			sc := map[string]uint64{}
			if len(b.Revisions) > 0 && len(b.Revisions) != len(b.Targets) {
				return store.ErrDenied
			}
			for _, sid := range b.Targets {
				if e := work(); e != nil {
					return e
				}
				sourceTotal[sid]++
				if len(b.Revisions) == 0 {
					ranked = false
					continue
				}
				rev := b.Revisions[sid]
				if rev == 0 {
					return store.ErrDenied
				}
				sc[sid] = rev - 1
				if ranks[sid] == nil {
					ranks[sid] = map[uint64]string{}
				}
				if _, ok := ranks[sid][rev-1]; ok {
					return store.ErrDenied
				}
				ranks[sid][rev-1] = id
			}
			sourceCuts[id] = sc
		}
	}
	sort.Strings(ids)
	for node, list := range direct {
		for i, event := range list {
			if e := work(); e != nil {
				return e
			}
			if i > 0 {
				deps[event.Task][list[i-1].Task] = true
			}
			if creator, ok := creators[node]; ok {
				deps[event.Task][creator.Task] = true
			}
		}
	}
	if ranked {
		// Exact IDS source ranks yield a graph, not a search tree. A deep lawful
		// single-source history is processed iteratively with O(tasks+edges) memory.
		for sid, total := range sourceTotal {
			for rank := 0; rank < total; rank++ {
				if e := work(); e != nil {
					return e
				}
				id, ok := ranks[sid][uint64(rank)]
				if !ok {
					return store.ErrDenied
				}
				if rank > 0 {
					deps[id][ranks[sid][uint64(rank-1)]] = true
				}
			}
		}
		for id, requirements := range cuts {
			for sid, n := range requirements {
				if e := work(); e != nil {
					return e
				}
				total := uint64(sourceTotal[sid])
				if n > total {
					return store.ErrDenied
				}
				if n > 0 {
					deps[id][ranks[sid][n-1]] = true
				}
				if n < total {
					deps[ranks[sid][n]][id] = true
				}
			}
		}
		degree := map[string]int{}
		next := map[string][]string{}
		queue := []string{}
		for _, id := range ids {
			degree[id] = len(deps[id])
			if degree[id] == 0 {
				queue = append(queue, id)
			}
			for dep := range deps[id] {
				if e := work(); e != nil {
					return e
				}
				if _, ok := deps[dep]; !ok {
					return store.ErrDenied
				}
				next[dep] = append(next[dep], id)
			}
		}
		count := 0
		for len(queue) > 0 {
			if e := work(); e != nil {
				return e
			}
			id := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			count++
			for _, child := range next[id] {
				if e := work(); e != nil {
					return e
				}
				degree[child]--
				if degree[child] == 0 {
					queue = append(queue, child)
				}
			}
		}
		if count != len(ids) {
			return store.ErrDenied
		}
		return nil
	}
	// Unranked source operations use one mutable state with explicit undo; there
	// are no recursive map copies or N-byte memo keys retained at each depth.
	done := map[string]bool{}
	counts := map[string]uint64{}
	undo := []string{}
	type frame struct {
		base, next int
		entered    bool
	}
	stack := []frame{{}}
	apply := func(id string) {
		done[id] = true
		undo = append(undo, id)
		if mutations[id].TargetKind == "SOURCE" {
			for _, sid := range mutations[id].Targets {
				counts[sid]++
			}
		}
	}
	rewind := func(mark int) {
		for len(undo) > mark {
			id := undo[len(undo)-1]
			undo = undo[:len(undo)-1]
			delete(done, id)
			if mutations[id].TargetKind == "SOURCE" {
				for _, sid := range mutations[id].Targets {
					counts[sid]--
				}
			}
		}
	}
	ready := func(id string, required map[string]uint64) (bool, error) {
		for dep := range deps[id] {
			if e := work(); e != nil {
				return false, e
			}
			if !done[dep] {
				return false, nil
			}
		}
		for sid, n := range required {
			if e := work(); e != nil {
				return false, e
			}
			if counts[sid] != n {
				return false, nil
			}
		}
		return true, nil
	}
	for len(stack) > 0 {
		if e := work(); e != nil {
			return e
		}
		top := &stack[len(stack)-1]
		if !top.entered {
			top.entered = true
			for changed := true; changed; {
				changed = false
				for _, id := range ids {
					if e := work(); e != nil {
						return e
					}
					if done[id] || mutations[id].TargetKind == "SOURCE" {
						continue
					}
					ok, e := ready(id, cuts[id])
					if e != nil {
						return e
					}
					if ok {
						apply(id)
						changed = true
					}
				}
			}
			impossible := false
			for _, id := range ids {
				if e := work(); e != nil {
					return e
				}
				if done[id] {
					continue
				}
				required := cuts[id]
				if mutations[id].TargetKind == "SOURCE" {
					required = sourceCuts[id]
				}
				for sid, n := range required {
					if e := work(); e != nil {
						return e
					}
					if counts[sid] > n {
						impossible = true
					}
				}
			}
			if impossible {
				rewind(top.base)
				stack = stack[:len(stack)-1]
				continue
			}
			if len(done) == len(ids) {
				return nil
			}
		}
		advanced := false
		for top.next < len(ids) {
			if e := work(); e != nil {
				return e
			}
			id := ids[top.next]
			top.next++
			if done[id] || mutations[id].TargetKind != "SOURCE" {
				continue
			}
			ok, e := ready(id, sourceCuts[id])
			if e != nil {
				return e
			}
			if !ok {
				continue
			}
			mark := len(undo)
			apply(id)
			stack = append(stack, frame{base: mark})
			advanced = true
			break
		}
		if !advanced {
			rewind(top.base)
			stack = stack[:len(stack)-1]
		}
	}
	return store.ErrDenied
}

// Completeness and creator membership are global. Only an independently closed
// source/component may survive another component's unprovable history.
func integrationProvenComponents(ctx context.Context, base, actual Aggregate, mutations map[string]Binding) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if len(actual.Nodes) > MaxNodes || len(actual.Sources) != len(base.Sources) || actual.Epoch != base.Epoch || actual.Intake != base.Intake || !reflect.DeepEqual(actual.Grants, base.Grants) || !reflect.DeepEqual(actual.Acquisitions, base.Acquisitions) {
		return nil, store.ErrDenied
	}
	roots := map[string]string{}
	nodeSource := map[string]string{}
	creators := map[string]Binding{}
	for sid := range base.Sources {
		if _, ok := actual.Sources[sid]; !ok {
			return nil, store.ErrDenied
		}
		roots[sid] = sid
	}
	for id, n := range base.Nodes {
		if len(n.Sources) != 1 || len(n.Parents) != 0 {
			return nil, store.ErrDenied
		}
		if _, ok := roots[n.Sources[0]]; !ok {
			return nil, store.ErrDenied
		}
		nodeSource[id] = n.Sources[0]
	}
	for task, b := range mutations {
		if b.SourceEpoch != base.Epoch || b.Cutoff != base.Intake || len(b.SelectorHash) != 64 {
			return nil, store.ErrDenied
		}
		if b.Action == "SET_USE_POLICY" {
			if !validUses(b.Uses) || (b.TargetKind != "ENABLE" && b.TargetKind != "DISABLE") {
				return nil, store.ErrDenied
			}
			continue
		}
		if b.Action != "DELETE" && b.Action != "REVOKE" && b.Action != "CORRECT" {
			return nil, store.ErrDenied
		}
		if b.TargetKind != "SOURCE" && b.TargetKind != "MEMORY_NODE" {
			return nil, store.ErrDenied
		}
		if len(b.Uses) > 0 && !validUses(b.Uses) {
			return nil, store.ErrDenied
		}
		seen := map[string]bool{}
		for _, id := range b.Targets {
			if seen[id] {
				return nil, store.ErrDenied
			}
			seen[id] = true
		}
		if b.Action == "CORRECT" {
			if b.TargetKind != "MEMORY_NODE" || len(b.Targets) != 1 || b.ReplacementID != content.Identity(task, "replacement") || len(b.ReplacementHash) != 64 {
				return nil, store.ErrDenied
			}
			if _, ok := nodeSource[b.ReplacementID]; ok {
				return nil, store.ErrDenied
			}
			if _, ok := creators[b.ReplacementID]; ok {
				return nil, store.ErrDenied
			}
			creators[b.ReplacementID] = b
		} else if b.ReplacementID != "" || b.ReplacementHash != "" {
			return nil, store.ErrDenied
		}
	}
	for len(nodeSource) < len(base.Nodes)+len(creators) {
		progress := false
		for id, b := range creators {
			if _, ok := nodeSource[id]; ok {
				continue
			}
			if sid, ok := nodeSource[b.Targets[0]]; ok {
				nodeSource[id] = sid
				progress = true
			}
		}
		if !progress {
			return nil, store.ErrDenied
		}
	}
	if len(actual.Nodes) != len(nodeSource) {
		return nil, store.ErrDenied
	}
	for id := range actual.Nodes {
		if _, ok := nodeSource[id]; !ok {
			return nil, store.ErrDenied
		}
	}
	var root func(string) string
	root = func(id string) string {
		if roots[id] != id {
			roots[id] = root(roots[id])
		}
		return roots[id]
	}
	group := func(b Binding) ([]string, error) {
		out := []string{}
		for _, id := range b.Targets {
			sid := id
			if b.TargetKind == "MEMORY_NODE" {
				var ok bool
				sid, ok = nodeSource[id]
				if !ok {
					return nil, store.ErrDenied
				}
			}
			if _, ok := roots[sid]; !ok {
				return nil, store.ErrDenied
			}
			out = append(out, sid)
		}
		return out, nil
	}
	for _, b := range mutations {
		if b.Action == "SET_USE_POLICY" {
			continue
		}
		ss, e := group(b)
		if e != nil {
			return nil, e
		}
		for i := 1; i < len(ss); i++ {
			roots[root(ss[i])] = root(ss[0])
		}
	}
	groups := map[string]bool{}
	for sid := range roots {
		groups[root(sid)] = true
	}
	ids := []string{}
	for component := range groups {
		b, a := base, actual
		b.Sources = map[string]Source{}
		a.Sources = map[string]Source{}
		b.Nodes = map[string]MemoryRecord{}
		a.Nodes = map[string]MemoryRecord{}
		for sid, s := range base.Sources {
			if root(sid) == component {
				b.Sources[sid] = s
				a.Sources[sid] = actual.Sources[sid]
			}
		}
		for id, sid := range nodeSource {
			if root(sid) != component {
				continue
			}
			a.Nodes[id] = actual.Nodes[id]
			if n, ok := base.Nodes[id]; ok {
				b.Nodes[id] = n
			}
		}
		bs := map[string]Binding{}
		for task, mutation := range mutations {
			if mutation.Action == "SET_USE_POLICY" {
				bs[task] = mutation
				continue
			}
			ss, _ := group(mutation)
			if len(ss) == 0 || root(ss[0]) == component {
				bs[task] = mutation
			}
		}
		if e := proveIntegrationAggregate(ctx, b, a, bs); e == nil {
			for id := range a.Nodes {
				ids = append(ids, id)
			}
		} else {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Birth replay uses commitments as abstract Body values. No deleted plaintext
// is recovered. Empty plaintext is represented by the empty sentinel so the
// existing source/use/parent predicates retain their real empty-body meaning.
func integrationAbstract(a Aggregate) Aggregate {
	out := Aggregate{Sources: map[string]Source{}, Nodes: map[string]MemoryRecord{}, Grants: append([]Grant(nil), a.Grants...), Acquisitions: append([]Acquisition(nil), a.Acquisitions...), Disabled: append([]Use(nil), a.Disabled...), Epoch: a.Epoch, Intake: a.Intake, PolicyRevision: a.PolicyRevision}
	for id, v := range a.Sources {
		if v.Body != "" {
			h := sha256.Sum256([]byte(v.Body))
			v.Body = fmt.Sprintf("%x", h)
		}
		out.Sources[id] = v
	}
	for id, v := range a.Nodes {
		if v.Body != "" {
			v.Body = digest(v.Body)
		}
		out.Nodes[id] = v
	}
	return out
}
func integrationBirthSource(v store.IntegrationBirthSource) Source {
	raw, _ := json.Marshal(v.IntegrationBirthSourceFields)
	var out Source
	_ = json.Unmarshal(raw, &out)
	out.Body = v.BodyRawSHA256
	if out.Body == fmt.Sprintf("%x", sha256.Sum256(nil)) {
		out.Body = ""
	}
	return out
}
func integrationBirthNode(v store.IntegrationBirthNode) MemoryRecord {
	raw, _ := json.Marshal(v.IntegrationBirthNodeFields)
	var out MemoryRecord
	_ = json.Unmarshal(raw, &out)
	out.Body = v.BodyStringJSONSHA256
	if out.Body == digest("") {
		out.Body = ""
	}
	return out
}
func integrationBirthProjection(a Aggregate) string {
	type projection struct {
		Sources                map[string]store.IntegrationBirthSource
		Nodes                  map[string]store.IntegrationBirthNode
		Grants                 []Grant
		Acquisitions           []Acquisition
		Disabled               []Use
		Epoch                  string
		Intake, PolicyRevision uint64
	}
	p := projection{map[string]store.IntegrationBirthSource{}, map[string]store.IntegrationBirthNode{}, a.Grants, a.Acquisitions, a.Disabled, a.Epoch, a.Intake, a.PolicyRevision}
	for id, v := range a.Sources {
		raw, _ := json.Marshal(v)
		var fields store.IntegrationBirthSourceFields
		_ = json.Unmarshal(raw, &fields)
		h := v.Body
		if h == "" {
			h = fmt.Sprintf("%x", sha256.Sum256(nil))
		}
		p.Sources[id] = store.IntegrationBirthSource{IntegrationBirthSourceFields: fields, BodyRawSHA256: h}
	}
	for id, v := range a.Nodes {
		raw, _ := json.Marshal(v)
		var fields store.IntegrationBirthNodeFields
		_ = json.Unmarshal(raw, &fields)
		h := v.Body
		if h == "" {
			h = digest("")
		}
		p.Nodes[id] = store.IntegrationBirthNode{IntegrationBirthNodeFields: fields, BodyStringJSONSHA256: h}
	}
	return digest(p)
}

type integrationBirthEvent struct {
	task    string
	binding *Binding
	birth   *store.IntegrationMemoryBirthDocument
	cleanup bool
	accept  int
}
type integrationBirthUndo struct {
	sources                  map[string]*Source
	nodes                    map[string]*MemoryRecord
	grants, acquisitions     int
	disabled                 []Use
	intake, policy, revision uint64
}
type integrationBirthMachine struct {
	a              Aggregate
	revision       uint64
	work, retained int
	ctx            context.Context
}

func (m *integrationBirthMachine) spend(n int) error {
	if n < 0 || m.work < n || m.ctx.Err() != nil {
		return store.ErrDenied
	}
	m.work -= n
	return nil
}
func (m *integrationBirthMachine) undo(u *integrationBirthUndo) {
	for id, v := range u.sources {
		if v == nil {
			delete(m.a.Sources, id)
		} else {
			m.a.Sources[id] = *v
		}
	}
	for id, v := range u.nodes {
		if v == nil {
			delete(m.a.Nodes, id)
		} else {
			m.a.Nodes[id] = *v
		}
	}
	m.a.Grants = m.a.Grants[:u.grants]
	m.a.Acquisitions = m.a.Acquisitions[:u.acquisitions]
	m.a.Disabled = u.disabled
	m.a.Intake = u.intake
	m.a.PolicyRevision = u.policy
	m.revision = u.revision
	m.retained -= len(u.nodes) + len(u.sources)
}
func (m *integrationBirthMachine) apply(e integrationBirthEvent, done []bool) (undo *integrationBirthUndo, err error) {
	u := &integrationBirthUndo{map[string]*Source{}, map[string]*MemoryRecord{}, len(m.a.Grants), len(m.a.Acquisitions), append([]Use(nil), m.a.Disabled...), m.a.Intake, m.a.PolicyRevision, m.revision}
	putSource := func(id string, v Source) {
		if _, ok := u.sources[id]; !ok {
			if old, exists := m.a.Sources[id]; exists {
				copy := old
				u.sources[id] = &copy
			} else {
				u.sources[id] = nil
			}
			m.retained++
		}
		m.a.Sources[id] = v
	}
	putNode := func(id string, v MemoryRecord) {
		if _, ok := u.nodes[id]; !ok {
			if old, exists := m.a.Nodes[id]; exists {
				copy := old
				u.nodes[id] = &copy
			} else {
				u.nodes[id] = nil
			}
			m.retained++
		}
		m.a.Nodes[id] = v
	}
	good := false
	defer func() {
		if !good {
			m.undo(u)
		}
	}()
	if m.spend(1) != nil {
		return nil, store.ErrDenied
	}
	if d := e.birth; d != nil {
		if m.revision != d.BeforeAggregateRevision || m.a.Intake != d.BeforeIntake || m.a.Epoch != d.Epoch || m.a.PolicyRevision != d.PolicyRevision || !integrationBirthDisabledEqual(m.a.Disabled, d.Disabled) {
			return nil, ErrInvalid
		}
		if m.spend(len(m.a.Nodes)+len(m.a.Sources)) != nil {
			return nil, store.ErrDenied
		}
		if integrationBirthProjection(m.a) != d.BeforeCommitment {
			return nil, ErrInvalid
		}
		if d.Kind == "INPUT" {
			if len(d.SourcesBorn) != 1 || len(d.NodesBorn) != 1 || len(d.MemoryIDs) != 1 || d.BeforeIntake == ^uint64(0) || d.AfterIntake != d.BeforeIntake+1 || d.AfterAggregateRevision != d.BeforeAggregateRevision+1 || d.BeforeAggregateRevision == ^uint64(0) || len(d.AddedGrants) != 1 || len(d.AddedAcquisitions) != 1 {
				return nil, ErrInvalid
			}
			s := integrationBirthSource(d.SourcesBorn[0])
			n := integrationBirthNode(d.NodesBorn[0])
			if _, ok := m.a.Sources[s.ID]; ok {
				return nil, ErrInvalid
			}
			if _, ok := m.a.Nodes[n.ID]; ok {
				return nil, ErrInvalid
			}
			if s.ID != d.InputSourceID || s.ID == "" || s.Body == "" || d.SourcesBorn[0].BodyRawSHA256 != d.InputHash || s.Domain != PlayerStatement || s.Audience != Private || !reflect.DeepEqual(s.Uses, []Use{PrivateResponse}) || s.Revision != 1 || s.Blocked || s.Epoch != m.a.Epoch || s.Intake != d.AfterIntake || !s.OccurredAt.Equal(d.ObservedAt) || s.EstimatedTime {
				return nil, ErrInvalid
			}
			if n.ID != content.Identity(s.ID, d.ActorID, "turn-input") || d.MemoryIDs[0] != n.ID || n.Kind != "EXPERIENCE" || n.ActorID != d.ActorID || n.TargetKind != "PLAYER" || n.TargetID != d.DataSubjectID || n.Corrects != "" || n.Domain != PlayerStatement || n.Audience != Private || !reflect.DeepEqual(n.Uses, []Use{PrivateResponse}) || !reflect.DeepEqual(n.Sources, []string{s.ID}) || len(n.Parents) != 0 || len(n.MediaRefs) != 0 || n.Revision != 1 || n.Blocked || !n.ExpiresAt.IsZero() || n.Body == "" {
				return nil, ErrInvalid
			}
			wantGrant := store.IntegrationBirthGrant{ActorID: d.ActorID, SourceID: s.ID, Uses: []string{"PRIVATE_RESPONSE"}}
			if !reflect.DeepEqual(d.AddedGrants[0], wantGrant) || d.AddedAcquisitions[0] != (store.IntegrationBirthAcquisition{ActorID: d.ActorID, SourceID: s.ID, Kind: "TOLD", EvidenceID: d.TaskID}) {
				return nil, ErrInvalid
			}
			putSource(s.ID, s)
			putNode(n.ID, n)
			m.a.Intake = d.AfterIntake
		} else if d.Kind == "DERIVED" {
			if d.BeforeIntake != d.AfterIntake || len(d.SourcesBorn) != 0 || len(d.AddedGrants) != 0 || len(d.NodesBorn) != len(d.MemoryIDs) || len(d.MemoryIDs) > 32 {
				return nil, ErrInvalid
			}
			if len(d.MemoryIDs) == 0 {
				if d.AfterAggregateRevision != d.BeforeAggregateRevision || !d.ObservedAt.IsZero() {
					return nil, ErrInvalid
				}
			} else if d.BeforeAggregateRevision == ^uint64(0) || d.AfterAggregateRevision != d.BeforeAggregateRevision+1 || d.ObservedAt.IsZero() {
				return nil, ErrInvalid
			}
			for _, v := range d.ParentSources {
				if !reflect.DeepEqual(m.a.Sources[v.ID], integrationBirthSource(v)) {
					return nil, ErrInvalid
				}
			}
			for _, v := range d.ParentNodes {
				if !reflect.DeepEqual(m.a.Nodes[v.ID], integrationBirthNode(v)) {
					return nil, ErrInvalid
				}
			}
			acqStart := len(m.a.Acquisitions)
			for i, v := range d.NodesBorn {
				n := integrationBirthNode(v)
				if n.ID != d.MemoryIDs[i] || n.Body == "" || n.Corrects != "" || n.Blocked || m.spend(len(m.a.Nodes)+len(m.a.Sources)) != nil {
					return nil, ErrInvalid
				}
				if _, exists := m.a.Nodes[n.ID]; exists {
					return nil, ErrInvalid
				}
				// AddDerived itself validates current parent/use/source closure at the
				// original captured producer time. Only abstract commitments are passed.
				if e := m.a.AddDerived(n, d.ObservedAt); e != nil {
					return nil, ErrInvalid
				}
				got := m.a.Nodes[n.ID]
				delete(m.a.Nodes, n.ID)
				putNode(n.ID, got)
				if !reflect.DeepEqual(got, n) {
					return nil, ErrInvalid
				}
			}
			raw, _ := json.Marshal(m.a.Acquisitions[acqStart:])
			var actual []store.IntegrationBirthAcquisition
			_ = json.Unmarshal(raw, &actual)
			if !reflect.DeepEqual(actual, d.AddedAcquisitions) {
				return nil, ErrInvalid
			}
			// Acquisitions were already appended by the actual pure producer below.
		} else {
			return nil, ErrInvalid
		}
		for _, v := range d.AddedGrants {
			uses := []Use{}
			for _, x := range v.Uses {
				uses = append(uses, Use(x))
			}
			m.a.Grants = append(m.a.Grants, Grant{v.ActorID, v.SourceID, uses, v.Revoked, v.ExpiresAt})
		}
		if d.Kind == "INPUT" {
			for _, v := range d.AddedAcquisitions {
				m.a.Acquisitions = append(m.a.Acquisitions, Acquisition{v.ActorID, v.SourceID, AcquisitionKind(v.Kind), v.EvidenceID})
			}
		}
		m.revision = d.AfterAggregateRevision
		if integrationBirthProjection(m.a) != d.AfterCommitment {
			return nil, ErrInvalid
		}
	} else {
		b := e.binding
		if b == nil || b.SourceEpoch != m.a.Epoch {
			return nil, ErrInvalid
		}
		if e.cleanup {
			if e.accept < 0 || !done[e.accept] {
				return nil, ErrInvalid
			}
			if b.ReplacementID != "" && m.a.Nodes[b.ReplacementID].Body != b.ReplacementHash {
				return nil, ErrInvalid
			}
			for id, n := range m.a.Nodes {
				if m.spend(1) != nil {
					return nil, store.ErrDenied
				}
				if n.Blocked {
					n.Body = ""
					n.MediaRefs = nil
					putNode(id, n)
				}
			}
			for id, s := range m.a.Sources {
				if m.spend(1) != nil {
					return nil, store.ErrDenied
				}
				if s.Blocked {
					s.Body = ""
					putSource(id, s)
				}
			}
		} else {
			if b.Cutoff != m.a.Intake || len(b.SelectorHash) != 64 {
				return nil, ErrInvalid
			}
			if b.Action == "SET_USE_POLICY" {
				if !validUses(b.Uses) || m.a.PolicyRevision == ^uint64(0) || (b.TargetKind != "ENABLE" && b.TargetKind != "DISABLE") {
					return nil, ErrInvalid
				}
				if b.TargetKind == "DISABLE" {
					for _, v := range b.Uses {
						if !contains(m.a.Disabled, v) {
							m.a.Disabled = append(m.a.Disabled, v)
						}
					}
					for id, n := range m.a.Nodes {
						if m.spend(1) != nil {
							return nil, store.ErrDenied
						}
						if len(n.Parents) == 0 {
							continue
						}
						for _, v := range b.Uses {
							if contains(n.Uses, v) {
								if n.Revision == ^uint64(0) {
									return nil, ErrInvalid
								}
								n.Blocked = true
								n.Revision++
								putNode(id, n)
								break
							}
						}
					}
				} else {
					m.a.Disabled = removeUses(m.a.Disabled, b.Uses)
				}
				m.a.PolicyRevision++
			} else {
				if b.Action != "DELETE" && b.Action != "REVOKE" && b.Action != "CORRECT" {
					return nil, ErrInvalid
				}
				if len(b.Uses) > 0 && !validUses(b.Uses) {
					return nil, ErrInvalid
				}
				selected := map[string]bool{}
				sourceIDs := map[string]bool{}
				blocked := map[string]bool{}
				for _, id := range b.Targets {
					if selected[id] {
						return nil, ErrInvalid
					}
					selected[id] = true
					if b.TargetKind == "SOURCE" {
						s, ok := m.a.Sources[id]
						if !ok || s.Intake > b.Cutoff {
							return nil, ErrInvalid
						}
						if len(b.Revisions) > 0 && b.Revisions[id] != s.Revision {
							return nil, ErrInvalid
						}
						sourceIDs[id] = true
					} else if b.TargetKind == "MEMORY_NODE" {
						n, ok := m.a.Nodes[id]
						if !ok || b.Revisions[id] != n.Revision {
							return nil, ErrInvalid
						}
						blocked[id] = true
					} else {
						return nil, ErrInvalid
					}
				}
				if len(b.Revisions) > 0 && len(b.Revisions) != len(selected) {
					return nil, ErrInvalid
				}
				if b.Action == "CORRECT" {
					if b.TargetKind != "MEMORY_NODE" || len(b.Targets) != 1 || b.ReplacementID != content.Identity(e.task, "replacement") || len(b.ReplacementHash) != 64 || m.a.Nodes[b.Targets[0]].Domain == WorldFact {
						return nil, ErrInvalid
					}
					if _, ok := m.a.Nodes[b.ReplacementID]; ok {
						return nil, ErrInvalid
					}
				} else if b.ReplacementID != "" || b.ReplacementHash != "" {
					return nil, ErrInvalid
				}
				for id, n := range m.a.Nodes {
					if m.spend(1) != nil {
						return nil, store.ErrDenied
					}
					for _, sid := range n.Sources {
						if sourceIDs[sid] {
							blocked[id] = true
						}
					}
				}
				for change := true; change; {
					change = false
					for id, n := range m.a.Nodes {
						if m.spend(1) != nil {
							return nil, store.ErrDenied
						}
						if blocked[id] {
							continue
						}
						for _, parent := range n.Parents {
							if blocked[parent] {
								blocked[id] = true
								change = true
								break
							}
						}
					}
				}
				for id := range sourceIDs {
					s := m.a.Sources[id]
					if s.Revision == ^uint64(0) {
						return nil, ErrInvalid
					}
					if b.Action == "REVOKE" && len(b.Uses) > 0 {
						s.Uses = removeUses(s.Uses, b.Uses)
					} else {
						s.Blocked = true
					}
					s.Revision++
					putSource(id, s)
				}
				for id := range blocked {
					n := m.a.Nodes[id]
					if n.Revision == ^uint64(0) {
						return nil, ErrInvalid
					}
					if b.Action == "REVOKE" && len(b.Uses) > 0 {
						n.Uses = removeUses(n.Uses, b.Uses)
						if len(n.Uses) == 0 {
							n.Blocked = true
						}
					} else {
						n.Blocked = true
					}
					n.Revision++
					putNode(id, n)
				}
				if b.Action == "CORRECT" {
					n := m.a.Nodes[b.Targets[0]]
					n.ID = b.ReplacementID
					n.Body = b.ReplacementHash
					n.Blocked = false
					n.Revision = 1
					n.Corrects = b.Targets[0]
					putNode(n.ID, n)
				}
			}
		}
		if m.revision == ^uint64(0) {
			return nil, ErrInvalid
		}
		m.revision++
	}
	// Undo entries are bounded separately from primitive search work. Exceeding
	// the budget means no proof, never a truncated governance history.
	if m.retained > MaxJournalBytes/1024 || len(m.a.Nodes) > MaxNodes || len(m.a.Sources) > MaxSources || len(m.a.Grants) > MaxGrants || len(m.a.Acquisitions) > MaxAcquisitions {
		return nil, store.ErrDenied
	}
	good = true
	return u, nil
}
func integrationProofSourceEqual(a, b Source) bool {
	as, ok := integrationPurposeSet(a.Uses)
	if !ok {
		return false
	}
	bs, ok := integrationPurposeSet(b.Uses)
	if !ok {
		return false
	}
	a.Uses = as
	b.Uses = bs
	return reflect.DeepEqual(a, b)
}
func integrationProofNodeEqual(a, b MemoryRecord) bool {
	as, ok := integrationPurposeSet(a.Uses)
	if !ok {
		return false
	}
	bs, ok := integrationPurposeSet(b.Uses)
	if !ok {
		return false
	}
	a.Uses = as
	b.Uses = bs
	a.Sources = append([]string(nil), a.Sources...)
	b.Sources = append([]string(nil), b.Sources...)
	sort.Strings(a.Sources)
	sort.Strings(b.Sources)
	return reflect.DeepEqual(a, b)
}
func integrationBirthReplay(ctx context.Context, base, actual Aggregate, mutations map[string]Binding, evidence []store.IntegrationMemoryBirthEvidence, finalRevision uint64) ([]string, error) {
	if len(actual.Nodes) > MaxNodes || len(actual.Sources) > MaxSources || len(evidence) > 2*MaxSources {
		return nil, store.ErrDenied
	}
	for id, s := range base.Sources {
		got, ok := actual.Sources[id]
		if !ok || got.OccurredAt.IsZero() {
			return nil, store.ErrDenied
		}
		s.OccurredAt = got.OccurredAt
		base.Sources[id] = s
	}
	base = integrationAbstract(base)
	actual = integrationAbstract(actual)
	events := []integrationBirthEvent{}
	members := map[string]bool{}
	sources := map[string]bool{}
	parent := map[string]string{}
	root := func(id string) string {
		for parent[id] != "" && parent[id] != id {
			id = parent[id]
		}
		return id
	}
	join := func(a, b string) {
		a = root(a)
		b = root(b)
		if a != b {
			parent[a] = b
		}
	}
	connect := func(n MemoryRecord) {
		for _, sid := range n.Sources {
			join("n:"+n.ID, "s:"+sid)
		}
		for _, id := range n.Parents {
			join("n:"+n.ID, "n:"+id)
		}
		if n.Corrects != "" {
			join("n:"+n.ID, "n:"+n.Corrects)
		}
	}
	for id, n := range base.Nodes {
		members[id] = true
		connect(n)
	}
	for id := range base.Sources {
		sources[id] = true
	}
	var total uint64
	for _, ev := range evidence {
		var budgetError error
		total, budgetError = store.IntegrationMemoryBirthBudget(total, uint64(len(ev.Document)))
		if budgetError != nil || sha256.Sum256(ev.Document) != ev.DocumentSHA256 {
			return nil, store.ErrDenied
		}
		var d store.IntegrationMemoryBirthDocument
		if json.Unmarshal(ev.Document, &d) != nil || d.Format != "loreviva.integration-memory-birth.v1" {
			return nil, store.ErrDenied
		}
		copy := d
		events = append(events, integrationBirthEvent{task: d.TaskID, birth: &copy})
		for _, s := range d.SourcesBorn {
			if sources[s.ID] {
				return nil, store.ErrDenied
			}
			sources[s.ID] = true
		}
		for _, n := range d.NodesBorn {
			if members[n.ID] {
				return nil, store.ErrDenied
			}
			members[n.ID] = true
			connect(integrationBirthNode(n))
		}
	}
	ids := []string{}
	for id := range mutations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, task := range ids {
		b := mutations[task]
		if b.SourceEpoch != base.Epoch {
			return nil, store.ErrDenied
		}
		copy := b
		accept := len(events)
		events = append(events, integrationBirthEvent{task: task, binding: &copy, accept: -1}, integrationBirthEvent{task: task, binding: &copy, cleanup: true, accept: accept})
		if b.ReplacementID != "" {
			if members[b.ReplacementID] || len(b.Targets) != 1 {
				return nil, store.ErrDenied
			}
			members[b.ReplacementID] = true
			join("n:"+b.ReplacementID, "n:"+b.Targets[0])
		}
		target := ""
		for _, id := range b.Targets {
			v := "n:" + id
			if b.TargetKind == "SOURCE" {
				v = "s:" + id
			}
			if target != "" {
				join(target, v)
			}
			target = v
		}
		if b.Action == "SET_USE_POLICY" {
			for id, n := range actual.Nodes {
				if len(n.Parents) > 0 {
					if target != "" {
						join(target, "n:"+id)
					}
					target = "n:" + id
				}
			}
		}
	}
	if len(members) != len(actual.Nodes) || len(sources) != len(actual.Sources) {
		return nil, store.ErrDenied
	}
	for id, n := range actual.Nodes {
		if !members[id] || n.ID != id {
			return nil, store.ErrDenied
		}
		for _, sid := range n.Sources {
			if !sources[sid] {
				return nil, store.ErrDenied
			}
		}
		for _, pid := range n.Parents {
			if !members[pid] {
				return nil, store.ErrDenied
			}
		}
	}
	for id, s := range actual.Sources {
		if !sources[id] || s.ID != id {
			return nil, store.ErrDenied
		}
	}
	// Identical empty DERIVED transitions do not modify any aggregate field or
	// row revision. Validate each document through apply, but admit only one
	// order for this exact structural no-op class. Original task/receipt proof
	// remains independently checked by Store; this is not receipt deduplication.
	emptyPrevious := map[int]int{}
	emptyLast := map[string]int{}
	for i, ev := range events {
		d := ev.birth
		if d == nil || d.Kind != "DERIVED" || len(d.MemoryIDs) != 0 || len(d.NodesBorn) != 0 || len(d.SourcesBorn) != 0 || len(d.AddedGrants) != 0 || len(d.AddedAcquisitions) != 0 || !d.ObservedAt.IsZero() || d.BeforeAggregateRevision != d.AfterAggregateRevision || d.BeforeIntake != d.AfterIntake || d.BeforeCommitment != d.AfterCommitment {
			continue
		}
		group := digest(struct {
			Revision, Intake, Policy      uint64
			Epoch, Before, Actor, Subject string
			Disabled                      []string
			Sources                       []store.IntegrationBirthSource
			Nodes                         []store.IntegrationBirthNode
		}{d.BeforeAggregateRevision, d.BeforeIntake, d.PolicyRevision, d.Epoch, d.BeforeCommitment, d.ActorID, d.DataSubjectID, d.Disabled, d.ParentSources, d.ParentNodes})
		if previous, ok := emptyLast[group]; ok {
			emptyPrevious[i] = previous
		}
		emptyLast[group] = i
	}
	// Every possible complete replay must agree. No branch is preferred because
	// it happens to match the mutable current aggregate.
	m := integrationBirthMachine{a: base, revision: 1, work: MaxJournalBytes, ctx: ctx}
	writes, graphError := integrationDeleteGraph(&m, base, events)
	if graphError != nil {
		return nil, graphError
	}
	done := make([]bool, len(events))
	proven := map[string]bool{}
	solutions := 0
	type frame struct {
		next, chosen int
		undo         *integrationBirthUndo
		components   *integrationComponentPlan
		planned      bool
	}
	stack := []frame{{chosen: -1}}
	for len(stack) > 0 {
		if m.spend(1) != nil {
			return nil, fmt.Errorf("%w: replay work=%d retained=%d depth=%d", store.ErrDenied, m.work, m.retained, len(stack))
		}
		f := &stack[len(stack)-1]
		// Row revisions only increase. An unconsumed birth whose original before
		// revision is already behind this state can never become eligible again.
		// Prune the impossible branch, rather than exploring all its cleanups.
		missedBirth := false
		for i, ev := range events {
			if ev.birth != nil && !done[i] && ev.birth.BeforeAggregateRevision < m.revision {
				missedBirth = true
				break
			}
		}
		if missedBirth {
			if f.undo != nil {
				m.undo(f.undo)
				done[f.chosen] = false
			}
			stack = stack[:len(stack)-1]
			continue
		}
		if len(stack) == len(events)+1 {
			if m.spend(len(m.a.Nodes)+len(m.a.Sources)+len(m.a.Grants)+len(m.a.Acquisitions)+1) != nil {
				return nil, store.ErrDenied
			}
			if m.revision == finalRevision {
				solutions++
				valid := map[string]bool{}
				for id := range members {
					valid[root("n:"+id)] = true
				}
				x, xok := integrationPurposeSet(m.a.Disabled)
				y, yok := integrationPurposeSet(actual.Disabled)
				header := m.a.Intake == actual.Intake && m.a.Epoch == actual.Epoch && m.a.PolicyRevision == actual.PolicyRevision && xok && yok && reflect.DeepEqual(x, y) && reflect.DeepEqual(m.a.Grants, actual.Grants) && reflect.DeepEqual(m.a.Acquisitions, actual.Acquisitions)
				for id, s := range m.a.Sources {
					if !integrationProofSourceEqual(s, actual.Sources[id]) {
						valid[root("s:"+id)] = false
					}
				}
				for id, n := range m.a.Nodes {
					if !integrationProofNodeEqual(n, actual.Nodes[id]) {
						valid[root("n:"+id)] = false
					}
				}
				for id := range members {
					ok := header && valid[root("n:"+id)]
					if solutions == 1 {
						proven[id] = ok
					} else {
						proven[id] = proven[id] && ok
					}
				}
			}
			if f.undo != nil {
				m.undo(f.undo)
				done[f.chosen] = false
			}
			stack = stack[:len(stack)-1]
			continue
		}
		// A component plan is frozen at its first eligible suffix and shared by
		// descendant DFS frames. Do not split its dependencies after one acceptance
		// runs, or allocate another event map at every recursive depth.
		if writes.hasCorrect && !f.planned {
			var impossible bool
			var err error
			f.components, impossible, err = writes.correctComponents(&m, events, done)
			if err != nil {
				return nil, err
			}
			f.planned = true
			if impossible {
				if f.undo != nil {
					m.undo(f.undo)
					done[f.chosen] = false
				}
				stack = stack[:len(stack)-1]
				continue
			}
		}
		component := -1
		if f.components != nil {
			var err error
			component, err = f.components.next(&m, done)
			if err != nil {
				return nil, err
			}
		}
		// Only a closed suffix can move cleanup to the end: every birth and both
		// phases of every CORRECT must already be consumed. A completed historical
		// CORRECT is harmless; a pending CORRECT cleanup still reads its replacement
		// commitment and must never be moved past a later delete or cleanup.
		if m.spend(len(events)) != nil {
			return nil, store.ErrDenied
		}
		tailCleanup, deleteTail := integrationBirthTail(events, done)
		if f.components != nil {
			// Actual cleanup remains global and retains its replacement guard. It
			// must be allowed inside this component before other components accept.
			tailCleanup, deleteTail = false, false
		}
		deleteComponent := -1
		if deleteTail {
			var e error
			deleteComponent, e = writes.selectComponent(&m, events, done)
			if e != nil {
				return nil, e
			}
		}
		pendingAccept := false
		firstCleanup := -1
		if tailCleanup {
			for i, e := range events {
				if done[i] {
					continue
				}
				if !e.cleanup {
					pendingAccept = true
				} else if firstCleanup < 0 {
					firstCleanup = i
				}
			}
		}
		advanced := false
		for f.next < len(events) {
			i := f.next
			f.next++
			if done[i] {
				continue
			}
			if m.spend(1) != nil {
				return nil, fmt.Errorf("%w: candidate work=%d depth=%d", store.ErrDenied, m.work, len(stack))
			}
			ev := events[i]
			if component >= 0 && f.components.events[i] != component {
				continue
			}
			if tailCleanup && ev.cleanup && (pendingAccept || i != firstCleanup) {
				continue
			}
			if deleteComponent >= 0 && !ev.cleanup && writes.root(writes.index[ev.binding.Targets[0]]) != deleteComponent {
				continue
			}
			// Reject an ineligible deterministic successor before allocating undo maps.
			if ev.birth != nil && ev.birth.BeforeAggregateRevision != m.revision {
				continue
			}
			if previous, ok := emptyPrevious[i]; ok && !done[previous] {
				continue
			}
			if ev.cleanup && (ev.accept < 0 || !done[ev.accept]) {
				continue
			}
			if ev.binding != nil && !ev.cleanup {
				eligible := ev.binding.Cutoff == m.a.Intake
				for id, revision := range ev.binding.Revisions {
					if ev.binding.TargetKind == "SOURCE" {
						eligible = eligible && m.a.Sources[id].Revision == revision
					} else {
						eligible = eligible && m.a.Nodes[id].Revision == revision
					}
				}
				if !eligible {
					continue
				}
			}
			u, e := m.apply(ev, done)
			if e == store.ErrDenied {
				return nil, fmt.Errorf("%w: transition work=%d retained=%d depth=%d", e, m.work, m.retained, len(stack))
			}
			if e != nil {
				continue
			}
			done[i] = true
			stack = append(stack, frame{chosen: i, undo: u, components: f.components, planned: f.components != nil})
			advanced = true
			break
		}
		if advanced {
			continue
		}
		if f.undo != nil {
			m.undo(f.undo)
			done[f.chosen] = false
		}
		stack = stack[:len(stack)-1]
	}
	if solutions == 0 {
		return nil, fmt.Errorf("%w: no complete replay work=%d", store.ErrDenied, m.work)
	}
	out := []string{}
	for id, ok := range proven {
		if ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// integrationBirthTail permits normalization only after every birth and both
// CORRECT phases. Cleanup reads a CORRECT replacement, so an unconsumed cleanup
// is a dependency even if its acceptance already ran. Other acceptance kinds
// retain the general search. The caller charges the complete event scan.
func integrationBirthTail(events []integrationBirthEvent, done []bool) (bool, bool) {
	cleanup, deletes := true, true
	for i, e := range events {
		if done[i] {
			continue
		}
		if e.birth != nil || e.binding == nil || e.binding.Action == "CORRECT" {
			cleanup = false
			deletes = false
			continue
		}
		if !e.cleanup && (e.binding.Action != "DELETE" || e.binding.TargetKind != "MEMORY_NODE" || len(e.binding.Targets) == 0) {
			deletes = false
		}
	}
	return cleanup, cleanup && deletes
}

// This graph is separate from the deliberately conservative final authority
// components. It is built from the verified base and retained birth declarations,
// with CORRECT inheriting its creator's immutable Parents. Mutable current SQL
// nodes cannot supply a dependency graph that authorizes their own proof.
// Only Parents propagates DELETE. Sources and Corrects are read-only here: two
// siblings or old replacements need not be dependent merely for sharing them.
// Graph storage is O(MaxNodes * MaxReferences); one visited/queue/first-writer/DSU
// scratch set is reused, never one descendant set per event or recursion frame.
// All graph construction and traversal spend the original search-work budget.
type integrationDeleteWrites struct {
	hasCorrect                 bool
	index                      map[string]int
	children                   [][]int
	parent, owner, seen, queue []int
	selected                   []bool
}

func integrationDeleteGraph(m *integrationBirthMachine, base Aggregate, events []integrationBirthEvent) (*integrationDeleteWrites, error) {
	hasCorrect := false
	parents := map[string][]string{}
	creators := map[string]string{}
	for id, n := range base.Nodes {
		parents[id] = n.Parents
	}
	for _, e := range events {
		if m.spend(1) != nil {
			return nil, store.ErrDenied
		}
		hasCorrect = hasCorrect || (e.binding != nil && e.binding.Action == "CORRECT")
		if e.birth != nil {
			for _, n := range e.birth.NodesBorn {
				parents[n.ID] = n.Parents
			}
		}
		if e.binding != nil && !e.cleanup && e.binding.ReplacementID != "" {
			if len(e.binding.Targets) != 1 {
				return nil, store.ErrDenied
			}
			creators[e.binding.ReplacementID] = e.binding.Targets[0]
		}
	}
	for len(creators) > 0 {
		progress := false
		for id, creator := range creators {
			if m.spend(1) != nil {
				return nil, store.ErrDenied
			}
			if p, ok := parents[creator]; ok {
				parents[id] = p
				delete(creators, id)
				progress = true
			}
		}
		if !progress {
			return nil, store.ErrDenied
		}
	}
	if len(parents) > MaxNodes {
		return nil, store.ErrDenied
	}
	ids := make([]string, 0, len(parents))
	for id := range parents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	n := len(ids)
	g := &integrationDeleteWrites{hasCorrect: hasCorrect, index: map[string]int{}, children: make([][]int, n), parent: make([]int, n), owner: make([]int, n), seen: make([]int, n), selected: make([]bool, n), queue: make([]int, 0, n)}
	for i, id := range ids {
		g.index[id] = i
	}
	for id, ps := range parents {
		if len(ps) > MaxReferences || m.spend(1+len(ps)) != nil {
			return nil, store.ErrDenied
		}
		for _, p := range ps {
			i, ok := g.index[p]
			if !ok {
				return nil, store.ErrDenied
			}
			g.children[i] = append(g.children[i], g.index[id])
		}
	}
	return g, nil
}
func (g *integrationDeleteWrites) root(i int) int {
	for g.parent[i] != i {
		g.parent[i] = g.parent[g.parent[i]]
		i = g.parent[i]
	}
	return i
}
func (g *integrationDeleteWrites) join(a, b int) {
	a = g.root(a)
	b = g.root(b)
	if a < b {
		g.parent[b] = a
	} else {
		g.parent[a] = b
	}
}
func (g *integrationDeleteWrites) selectComponent(m *integrationBirthMachine, events []integrationBirthEvent, done []bool) (int, error) {
	if m.spend(len(g.parent)) != nil {
		return -1, store.ErrDenied
	}
	for i := range g.parent {
		g.parent[i] = i
		g.owner[i] = -1
		g.seen[i] = 0
		g.selected[i] = false
	}
	for i, e := range events {
		if m.spend(1) != nil {
			return -1, store.ErrDenied
		}
		if done[i] || e.cleanup {
			continue
		}
		b := e.binding
		if b == nil || b.Action != "DELETE" || b.TargetKind != "MEMORY_NODE" || len(b.Targets) == 0 {
			return -1, store.ErrDenied
		}
		first := -1
		for _, id := range b.Targets {
			if m.spend(1) != nil {
				return -1, store.ErrDenied
			}
			v, ok := g.index[id]
			if !ok {
				return -1, store.ErrDenied
			}
			g.selected[v] = true
			if first < 0 {
				first = v
			} else {
				g.join(first, v)
			}
		}
	}
	// Each selected anchor writes itself and its directed descendants. Joining
	// only overlapping write sets also covers selected-revision reads (a subset
	// of those writes). A shared read-only ancestor alone does not create overlap.
	for anchor, selected := range g.selected {
		if !selected {
			continue
		}
		g.queue = g.queue[:0]
		g.queue = append(g.queue, anchor)
		g.seen[anchor] = anchor + 1
		for head := 0; head < len(g.queue); head++ {
			v := g.queue[head]
			if m.spend(1+len(g.children[v])) != nil {
				return -1, store.ErrDenied
			}
			if g.owner[v] < 0 {
				g.owner[v] = anchor
			} else {
				g.join(anchor, g.owner[v])
			}
			for _, child := range g.children[v] {
				if g.seen[child] != anchor+1 {
					g.seen[child] = anchor + 1
					g.queue = append(g.queue, child)
				}
			}
		}
	}
	first := -1
	for i, selected := range g.selected {
		if selected {
			r := g.root(i)
			if first < 0 || r < first {
				first = r
			}
		}
	}
	// Across these components, DELETE cannot change another guard or write set.
	// Intake/policy/grants/acquisitions stay fixed; row increments are additive,
	// with no remaining birth to observe them. Cleanup is deferred and every
	// original receipt still applies once. Within a component all feasible orders
	// remain. Choosing a component is never choosing the first SQL-matching leaf.
	return first, nil
}

// integrationComponentPlan serializes complete independent local trajectories,
// not individual CORRECT events. Each local order and every complete final
// result still participates in the original proof intersection.
type integrationComponentPlan struct {
	events []int
	urgent int
}

func (p *integrationComponentPlan) next(m *integrationBirthMachine, done []bool) (int, error) {
	first := -1
	urgent := false
	for i, component := range p.events {
		if m.spend(1) != nil {
			return -1, store.ErrDenied
		}
		if done[i] || component < 0 {
			continue
		}
		if component == p.urgent {
			urgent = true
		}
		if first < 0 || component < first {
			first = component
		}
	}
	if urgent {
		return p.urgent, nil
	}
	return first, nil
}

// correctComponents is a closed-suffix reduction. Every birth must be consumed;
// SOURCE, policy and other actions retain the original search. The immutable
// graph includes all real potential replacements, but only remaining operations
// contribute conflicts: a completed creator is not a permanent union.
//
// Global Clean is NOT replaced by component-local cleanup. Projecting a legal
// trace removes foreign erasures, which cannot break revision/creation checks or
// a previously successful replacement-hash check. CORRECT inherits MediaRefs,
// so this argument requires strictly nil media on each remaining creator chain.
// Empty-but-nonnil slices use the original search, as do missing unknown targets.
//
// At the cut, an accepted pending CORRECT whose replacement is already blocked
// must clean before ANY global Clean erases its hash. Its component goes first.
// Two such guards cannot both survive the first Clean, even in one component.
// After a component completes, foreign erasure cannot harm the other components'
// cut-point guards; all future replacement/blocking dependencies are local.
// Every blocked object is eventually cleared by its real acceptance's cleanup.
// Thus serialized representatives retain all feasible final safety results.
func (g *integrationDeleteWrites) correctComponents(m *integrationBirthMachine, events []integrationBirthEvent, done []bool) (*integrationComponentPlan, bool, error) {
	hasCorrect := false
	creators := map[string]string{}
	for i, e := range events {
		if m.spend(1) != nil {
			return nil, false, store.ErrDenied
		}
		if done[i] {
			continue
		}
		b := e.binding
		if e.birth != nil || b == nil || b.TargetKind != "MEMORY_NODE" ||
			(b.Action != "DELETE" && b.Action != "CORRECT") || len(b.Targets) == 0 || len(b.Uses) != 0 {
			return nil, false, nil
		}
		if b.Action == "CORRECT" {
			if len(b.Targets) != 1 || b.ReplacementID == "" {
				return nil, false, nil
			}
			hasCorrect = true
			if !e.cleanup {
				creators[b.ReplacementID] = b.Targets[0]
			}
		}
	}
	if !hasCorrect {
		return nil, false, nil
	}
	for _, target := range creators {
		// Resolve an as-yet-unborn target only through an actual remaining
		// creator declaration; never use a missing map entry's nil zero value.
		for steps := 0; ; steps++ {
			if m.spend(1) != nil {
				return nil, false, store.ErrDenied
			}
			if n, ok := m.a.Nodes[target]; ok {
				if n.MediaRefs != nil {
					return nil, false, nil
				}
				break
			}
			next, ok := creators[target]
			if !ok || steps >= MaxNodes {
				return nil, false, nil
			}
			target = next
		}
	}
	if m.spend(len(g.parent)+len(events)) != nil {
		return nil, false, store.ErrDenied
	}
	for i := range g.parent {
		g.parent[i], g.owner[i], g.seen[i], g.selected[i] = i, -1, 0, false
	}
	anchors := make([]int, len(events))
	for i := range anchors {
		anchors[i] = -1
	}
	urgentEvent := -1
	for i, e := range events {
		if m.spend(1) != nil {
			return nil, false, store.ErrDenied
		}
		if done[i] {
			continue
		}
		b := e.binding
		first := -1
		add := func(id string) bool {
			v, ok := g.index[id]
			if !ok {
				return false
			}
			g.selected[v] = true
			if first < 0 {
				first = v
			} else {
				g.join(first, v)
			}
			return true
		}
		for _, id := range b.Targets {
			if m.spend(1) != nil {
				return nil, false, store.ErrDenied
			}
			if !add(id) {
				return nil, false, nil
			}
		}
		if b.ReplacementID != "" && !add(b.ReplacementID) {
			return nil, false, nil
		}
		anchors[i] = first
		if e.cleanup && b.Action == "CORRECT" && e.accept >= 0 && done[e.accept] {
			n, ok := m.a.Nodes[b.ReplacementID]
			if !ok {
				return nil, true, nil
			}
			if n.Blocked {
				if n.Body != b.ReplacementHash || urgentEvent >= 0 {
					return nil, true, nil
				}
				urgentEvent = i
			}
			// An unblocked mismatch is still rejected by the original apply
			// guard. Classification never manufactures the expected hash.
		}
	}
	// Replacement existence/hash reads and creator field reads are included as
	// anchors along with actual selected writes. Potential descendant overlap is
	// conservative. Reuse one queue/visited/first-writer array, not a set per event.
	for anchor, selected := range g.selected {
		if !selected {
			continue
		}
		g.queue = append(g.queue[:0], anchor)
		g.seen[anchor] = anchor + 1
		for head := 0; head < len(g.queue); head++ {
			v := g.queue[head]
			if m.spend(1+len(g.children[v])) != nil {
				return nil, false, store.ErrDenied
			}
			if g.owner[v] < 0 {
				g.owner[v] = anchor
			} else {
				g.join(anchor, g.owner[v])
			}
			for _, child := range g.children[v] {
				if g.seen[child] != anchor+1 {
					g.seen[child] = anchor + 1
					g.queue = append(g.queue, child)
				}
			}
		}
	}
	p := &integrationComponentPlan{events: anchors, urgent: -1}
	for i, anchor := range anchors {
		if anchor >= 0 {
			p.events[i] = g.root(anchor)
		}
	}
	if urgentEvent >= 0 {
		p.urgent = p.events[urgentEvent]
	}
	return p, false, nil
}

func integrationBirthDisabledEqual(have []Use, want []string) bool {
	converted := make([]Use, len(want))
	for i, v := range want {
		converted[i] = Use(v)
	}
	a, ok := integrationPurposeSet(have)
	if !ok {
		return false
	}
	b, ok := integrationPurposeSet(converted)
	return ok && reflect.DeepEqual(a, b)
}
