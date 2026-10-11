package runtime

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"testing"
)

func rulesFixture() WorldState {
	return WorldState{Actors: map[string]ActorState{"alex": {ID: "alex", Slot: "alex"}, "mia": {ID: "mia", Slot: "mia"}}, Items: map[string]ItemState{"sign": {ID: "sign", DefinitionID: "wooden-sign", CustodianID: "alex", State: "hanging", Definition: generated.PropDefinition{InteractionCapabilities: []string{"MOVE"}}, Rules: []generated.RuleDefinition{{Primitive: "SIGN_CUSTODY", Strength: "HARD", Trigger: "MOVE", Targets: []generated.DefinitionRef{json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"wooden-sign"}`)}, Condition: json.RawMessage(`{"op":"ACTOR_SLOT_EQUALS","slot_id":"alex"}`), Effect: "ALLOW"}}}}}
}
func TestMiaCannotTransferAlexCustody(t *testing.T) {
	s := rulesFixture()
	d, e := EvaluateActions(s, []ActionIntent{{ID: "move", ActorID: "mia", Kind: "MOVE", ItemID: "sign", TargetID: "mia"}})
	if e != nil || d.Accepted {
		t.Fatal("Mia speech cannot authorize Alex custody", d, e)
	}
}
func TestRegisteredCustodianMoveHasRealEffect(t *testing.T) {
	s := rulesFixture()
	d, e := EvaluateActions(s, []ActionIntent{{ID: "move", ActorID: "alex", Kind: "MOVE", ItemID: "sign", TargetID: "mia"}})
	if e != nil || !d.Accepted || d.Items["sign"].HolderID != "mia" || s.Items["sign"].HolderID != "" {
		t.Fatal("authorized move missing or evaluator mutates source", d, e)
	}
}
func TestDependentActionBundleRejectsAll(t *testing.T) {
	s := rulesFixture()
	d, e := EvaluateActions(s, []ActionIntent{{ID: "borrow", ActorID: "mia", Kind: "MOVE", ItemID: "sign", TargetID: "mia"}, {ID: "note", ActorID: "mia", Kind: "WRITE_NOTE", ItemID: "sign", DependsOn: []string{"borrow"}, Text: "borrowed"}})
	if e != nil || d.Accepted || len(d.Items) != 0 {
		t.Fatal("rejected prerequisite leaked partial effects", d, e)
	}
}
