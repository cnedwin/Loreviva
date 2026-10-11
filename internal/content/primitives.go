package content

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
)

type RuleDecision struct {
	Allowed bool
	Reason  string
}

// Actor slots are authoring identities. Task4 must supply these through its
// qualified runtime-actor mapping; this primitive grants no identity authority.
func EvaluateSignCustody(rule generated.RuleDefinition, actorSlot, targetID, trigger, custodianSlot string) RuleDecision {
	deny := RuleDecision{false, "Registered sign custody condition not satisfied."}
	if rule.Primitive != "SIGN_CUSTODY" || rule.Strength != "HARD" || rule.Trigger != trigger || actorSlot == "" || custodianSlot == "" || actorSlot != custodianSlot {
		return deny
	}
	var condition struct{ Op, SlotID string }
	var m map[string]string
	if e := json.Unmarshal(rule.Condition, &m); e != nil {
		return deny
	}
	condition.Op = m["op"]
	condition.SlotID = m["slot_id"]
	if condition.Op != "ACTOR_SLOT_EQUALS" || condition.SlotID != actorSlot {
		return deny
	}
	for _, raw := range rule.Targets {
		var ref map[string]string
		if e := json.Unmarshal(raw, &ref); e == nil && ref["kind"] == "LOCAL_DRAFT" && ref["object_id"] == targetID {
			if rule.Effect == "ALLOW" {
				return RuleDecision{true, rule.Reason}
			}
			return deny
		}
	}
	return deny
}
