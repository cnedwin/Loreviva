package harness

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// The synthetic adapter consumes exactly the actual canonical provider input.
// It has no database lookup or authority to invent missing resource identities.
func mockCandidate(canonical []byte) ([]byte, error) {
	var prompt struct {
		World        domain.WorldPerception
		Relationship []memory.MemoryRecord
		Turn         struct{ SourceID string }
		Route        struct{ Module string }
	}
	if json.Unmarshal(canonical, &prompt) != nil || prompt.World.ActorID == "" {
		return nil, store.ErrDenied
	}
	c := Candidate{Dialogue: "我听到了。", Actions: []world.ActionIntent{}, MemoryProposals: []memory.MemoryRecord{}}
	for _, n := range prompt.Relationship {
		if len(n.Sources) != 1 || n.Sources[0] != prompt.Turn.SourceID || n.ActorID != prompt.World.ActorID {
			continue
		}
		var input struct{ Kind, Text string }
		domain := memory.ActorBelief
		if prompt.Route.Module == "REHEARSAL" && n.Domain == memory.Sandbox {
			input.Kind, input.Text = "TEXT", n.Body
			domain = memory.Sandbox
		} else if json.Unmarshal([]byte(n.Body), &input) != nil {
			continue
		}
		if input.Kind != "TEXT" || input.Text != "Pass the sign and remember my request." {
			continue
		}
		target := ""
		for _, a := range prompt.World.Actors {
			if a.ID != prompt.World.ActorID {
				target = a.ID
				break
			}
		}
		if target == "" {
			break
		}
		for _, item := range prompt.World.Items {
			movable := false
			for _, cap := range item.Capabilities {
				if cap == "MOVE" {
					movable = true
				}
			}
			if !movable || item.CustodianID != prompt.World.ActorID || item.HolderID != "" {
				continue
			}
			c.Dialogue = "我已完成请求的操作。"
			c.Actions = append(c.Actions, world.ActionIntent{ID: content.Identity(n.ID, "move"), ActorID: prompt.World.ActorID, Kind: "MOVE", ItemID: item.ID, TargetID: target, DependsOn: []string{}})
			c.MemoryProposals = append(c.MemoryProposals, memory.MemoryRecord{ID: content.Identity(n.ID, "request-belief"), Kind: "EXPERIENCE", Body: "对方请求传递物品。", ActorID: n.ActorID, TargetKind: n.TargetKind, TargetID: n.TargetID, Domain: domain, Audience: memory.Private, Uses: []memory.Use{memory.PrivateResponse}, Parents: []string{n.ID}, Revision: 1})
			break
		}
		break
	}
	return json.Marshal(c)
}
