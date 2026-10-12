package runtime

import (
	"encoding/json"
	"testing"
)

// Real EvaluateActions programs on two independent eligible items: an absent
// negative actor must not disappear merely because the item order changes.
func TestRehearsalCustodyIncompleteItemNeverBecomesPass(t *testing.T) {
	s := rulesFixture()
	alex, mia := s.Actors["alex"], s.Actors["mia"]
	alex.OriginalSetup.SceneRef = json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"scene-a"}`)
	mia.OriginalSetup.SceneRef = alex.OriginalSetup.SceneRef
	alone := alex
	alone.ID = "alone"
	alone.OriginalSetup.SceneRef = json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"scene-b"}`)
	complete := s.Items["sign"]
	incomplete := complete
	incomplete.ID = "sign-alone"
	incomplete.CustodianID = alone.ID
	for _, reverse := range []bool{false, true} {
		name := "incomplete-first"
		items := []ItemState{incomplete, complete}
		if reverse {
			name = "incomplete-last"
			items = []ItemState{complete, incomplete}
		}
		t.Run(name, func(t *testing.T) {
			init := Initialization{Actors: []ActorState{alex, mia, alone}, Items: items}
			got, e := rehearsalCustodyFinding(init)
			if e != nil || got != "NOT_RUN" {
				t.Fatal("missing negative case promoted", got, e)
			}
			if init.Items[0].HolderID != "" || init.Items[1].HolderID != "" {
				t.Fatal("program mutated input")
			}
		})
	}
}
