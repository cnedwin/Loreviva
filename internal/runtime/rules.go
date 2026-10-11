package runtime

import (
	"loreviva.local/loreviva/internal/content"
	"strings"
)

type ActionIntent struct {
	ID, ActorID, Kind, ItemID, TargetID, Text string
	DependsOn                                 []string
}
type WorldState struct {
	Actors map[string]ActorState
	Items  map[string]ItemState
}
type ActionDecision struct {
	Accepted bool
	Items    map[string]ItemState
	Reason   string
}

func EvaluateActions(state WorldState, intents []ActionIntent) (ActionDecision, error) {
	deny := ActionDecision{Reason: "Registered action or prerequisite not satisfied."}
	if len(intents) == 0 || len(intents) > 32 {
		return deny, nil
	}
	next := map[string]ItemState{}
	for id, i := range state.Items {
		next[id] = i
	}
	done := map[string]ActionIntent{}
	for _, a := range intents {
		if a.ID == "" || a.ActorID == "" {
			return deny, nil
		}
		if _, exists := done[a.ID]; exists {
			return deny, nil
		}
		actor, ok := state.Actors[a.ActorID]
		if !ok {
			return deny, nil
		}
		item, ok := next[a.ItemID]
		if !ok {
			return deny, nil
		}
		for _, dep := range a.DependsOn {
			if _, ok := done[dep]; !ok {
				return deny, nil
			}
		}
		switch a.Kind {
		case "MOVE":
			if item.HolderID != "" {
				return deny, nil
			}
			if _, ok := state.Actors[a.TargetID]; !ok {
				return deny, nil
			}
			capability := false
			for _, c := range item.Definition.InteractionCapabilities {
				if c == "MOVE" {
					capability = true
				}
			}
			if !capability {
				return deny, nil
			}
			custodian, ok := state.Actors[item.CustodianID]
			if !ok {
				return deny, nil
			}
			allowed := false
			for _, rule := range item.Rules {
				if rule.Strength != "HARD" {
					continue
				}
				d := content.EvaluateSignCustody(rule, actor.Slot, item.DefinitionID, "MOVE", custodian.Slot)
				if !d.Allowed {
					return deny, nil
				}
				allowed = true
			}
			if !allowed {
				return deny, nil
			}
			item.HolderID = a.TargetID
			item.State = "held"
		case "WRITE_NOTE":
			// Only a dependent annotation of this actor's already-approved item move.
			prerequisite := false
			for _, dep := range a.DependsOn {
				p := done[dep]
				if p.Kind == "MOVE" && p.ActorID == a.ActorID && p.ItemID == a.ItemID {
					prerequisite = true
				}
			}
			if !prerequisite || len(a.Text) > 512 || strings.TrimSpace(a.Text) == "" {
				return deny, nil
			}
			item.Note = a.Text
		default:
			return deny, nil
		}
		next[a.ItemID] = item
		done[a.ID] = a
	}
	return ActionDecision{Accepted: true, Items: next}, nil
}
