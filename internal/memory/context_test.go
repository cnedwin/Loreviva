package memory

import (
	"fmt"
	"testing"
	"time"
)

// Removing acquisition checks must disclose the private statement and fail.
func TestPermissionWithoutAcquisitionCannotRecall(t *testing.T) {
	a := fixture()
	a.Acquisitions = nil
	if got := a.Allowed("mia", Private, PrivateResponse, time.Now()); len(got) != 0 {
		t.Fatalf("permission alone disclosed %d records", len(got))
	}
}

// Dropping ancestor checks must expose a mixed public-labelled derivative.
func TestPrivatePreferenceAndDerivativesNeverPublic(t *testing.T) {
	a := fixture()
	a.Nodes["goal"] = MemoryRecord{ID: "goal", ActorID: "alex", Kind: "GOAL", Domain: ActorBelief, Body: "private goal", Parents: []string{"preference"}, Audience: Public, Uses: []Use{PublicResponse}, Revision: 1}
	if got := a.Allowed("alex", Public, PublicResponse, time.Now()); len(got) != 0 {
		t.Fatalf("private ancestry leaked: %+v", got)
	}
	if got := a.Allowed("alex", Private, PrivateResponse, time.Now()); len(got) != 1 {
		t.Fatalf("authorized private positive control: %d", len(got))
	}
}
func fixture() Aggregate {
	return Aggregate{
		Sources:      map[string]Source{"dm": {ID: "dm", Domain: PlayerStatement, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1}},
		Nodes:        map[string]MemoryRecord{"preference": {ID: "preference", ActorID: "alex", Kind: "PREFERENCE", Domain: PlayerStatement, Body: "player said tea", Sources: []string{"dm"}, Audience: Private, Uses: []Use{PrivateResponse, Relationship}, Revision: 1}},
		Grants:       []Grant{{ActorID: "alex", SourceID: "dm", Uses: []Use{PrivateResponse, Relationship}}, {ActorID: "mia", SourceID: "dm", Uses: []Use{PrivateResponse, Relationship}}},
		Acquisitions: []Acquisition{{ActorID: "alex", SourceID: "dm", Kind: Told, EvidenceID: "message-1"}},
	}
}

func TestRecallKeywordTimeFiltersAfterPermission(t *testing.T) {
	now := time.Now().UTC()
	a := Aggregate{Sources: map[string]Source{"s": {ID: "s", Body: "tea", Audience: Private, Uses: []Use{PrivateResponse}, OccurredAt: now}}, Nodes: map[string]MemoryRecord{"n": {ID: "n", ActorID: "alex", Body: "tea preferred", Audience: Private, Uses: []Use{PrivateResponse}, Sources: []string{"s"}}}, Grants: []Grant{{ActorID: "alex", SourceID: "s", Uses: []Use{PrivateResponse}}}, Acquisitions: []Acquisition{{ActorID: "alex", SourceID: "s", Kind: Told, EvidenceID: "e"}}}
	got, e := a.Retrieve("alex", Private, PrivateResponse, RecallQuery{Keywords: []string{"TEA"}, From: now.Add(-time.Second), To: now.Add(time.Second), Limit: 10}, now)
	if e != nil || len(got) != 1 {
		t.Fatal(got, e)
	}
	got, e = a.Retrieve("mia", Private, PrivateResponse, RecallQuery{Keywords: []string{"tea"}, Limit: 10}, now)
	if e != nil || len(got) != 0 {
		t.Fatal("relevance bypassed acquisition", got, e)
	}
	got, e = a.Retrieve("alex", Private, PrivateResponse, RecallQuery{From: now.Add(time.Second), To: now.Add(time.Hour), Limit: 10}, now)
	if e != nil || len(got) != 0 {
		t.Fatal("time filtering failed", got, e)
	}
}

func TestSharedSourceDoesNotTransferPrivateActorNode(t *testing.T) {
	a := fixture()
	a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: "mia", SourceID: "dm", Kind: Told, EvidenceID: "telling"})
	if got := a.Allowed("mia", Private, PrivateResponse, time.Now()); len(got) != 0 {
		t.Fatal("source acquisition copied Alex private interpretation", got)
	}
}
func TestDerivedClosureRefusesOversizedReferencesWithoutMutation(t *testing.T) {
	a := fixture()
	parent := a.Nodes["preference"]
	parent.Sources = nil
	for i := 0; i < 64; i++ {
		id := fmt.Sprintf("source%d", i)
		a.Sources[id] = a.Sources["dm"]
		a.Grants = append(a.Grants, Grant{ActorID: "alex", SourceID: id, Uses: []Use{PrivateResponse}})
		a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: "alex", SourceID: id, Kind: Told, EvidenceID: "e"})
		parent.Sources = append(parent.Sources, id)
	}
	a.Nodes[parent.ID] = parent
	before := len(a.Acquisitions)
	e := a.AddDerived(MemoryRecord{ID: "too-wide", ActorID: "alex", Domain: ActorBelief, Audience: Private, Uses: []Use{PrivateResponse}, Parents: []string{parent.ID}, Revision: 1}, time.Now())
	if e == nil {
		t.Fatal("source closure plus parent exceeded 64 references")
	}
	if _, ok := a.Nodes["too-wide"]; ok || len(a.Acquisitions) != before {
		t.Fatal("refused node mutated history")
	}
}
func TestLayeredSharedDAGHasBoundedTraversal(t *testing.T) {
	a := fixture()
	parents := []string{"preference"}
	for layer := 0; layer < 30; layer++ {
		next := []string{}
		for column := 0; column < 8; column++ {
			id := fmt.Sprintf("n%d_%d", layer, column)
			a.Nodes[id] = MemoryRecord{ID: id, ActorID: "alex", Kind: "GOAL", Domain: ActorBelief, Body: "goal", Parents: append([]string{}, parents...), Audience: Private, Uses: []Use{PrivateResponse}, Revision: 1}
			next = append(next, id)
		}
		parents = next
	}
	if got := a.Allowed("alex", Private, PrivateResponse, time.Now()); len(got) != 241 {
		t.Fatal("valid shared DAG lost nodes", len(got))
	}
	cycle := a.Nodes["preference"]
	cycle.Parents = parents
	a.Nodes["preference"] = cycle
	if got := a.Allowed("alex", Private, PrivateResponse, time.Now()); len(got) != 0 {
		t.Fatal("cycle admitted", len(got))
	}
}
