package runtime

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// Evaluate only the actual frozen isolated initialization in a local copy. No
// action/annotation/event/memory is applied by this program.
func (r *Runtime) EvaluateRehearsalAssertionsTx(ctx context.Context, tx store.Tx, s domain.Scope) ([]domain.RehearsalAssertion, error) {
	basis, e := tx.RehearsalBasis(s)
	if e != nil {
		return nil, e
	}
	if basis == nil {
		return nil, store.ErrDenied
	}
	init, e := InitializationEvidence(ctx, tx, s)
	if e != nil {
		return nil, e
	}
	rights, duties, e := r.Content.RehearsalContentAssertionsTx(ctx, tx, s.ProductScope, *basis)
	if e != nil {
		return nil, e
	}
	sign, e := rehearsalCustodyFinding(init)
	if e != nil {
		return nil, e
	}
	out := []domain.RehearsalAssertion{}
	for _, kind := range basis.Request.Assertions {
		finding := domain.RehearsalAssertion{Kind: kind, State: "NOT_RUN"}
		switch kind {
		case "ISOLATED":
			finding.State = "PASS" // exact InitializationEvidence plus fixed Tx basis
		case "SIGN_CUSTODY":
			finding.State = sign
		case "RIGHTS":
			if rights {
				finding.State = "PASS"
			} else {
				finding.State = "FAIL"
			}
		case "DUTIES":
			if duties {
				finding.State = "PASS"
			} else {
				finding.State = "FAIL"
			}
		default:
			return nil, store.ErrDenied
		}
		out = append(out, finding)
	}
	return out, nil
}

func rehearsalCustodyFinding(init Initialization) (string, error) {
	state := WorldState{Actors: map[string]ActorState{}, Items: map[string]ItemState{}}
	for _, a := range init.Actors {
		state.Actors[a.ID] = a
	}
	for _, i := range init.Items {
		state.Items[i.ID] = i
	}
	sign := "NOT_RUN"
	incomplete := false
	for _, item := range init.Items {
		applicable := rehearsalCustodyApplies(item)
		if !applicable {
			continue
		}
		custodian, ok := state.Actors[item.CustodianID]
		if !ok {
			return "", store.ErrDenied
		}
		otherCount := 0
		for _, actor := range init.Actors {
			// Only an actual second actor in the same qualified scene is a
			// meaningful negative case. Cross-scene IDs are not invented.
			if actor.ID == custodian.ID || string(actor.OriginalSetup.SceneRef) != string(custodian.OriginalSetup.SceneRef) {
				continue
			}
			otherCount++
			positive, e := EvaluateActions(state, []ActionIntent{{ID: "rehearsal-sign-positive", ActorID: custodian.ID, Kind: "MOVE", ItemID: item.ID, TargetID: actor.ID}})
			if e != nil {
				return "", e
			}
			negative, e := EvaluateActions(state, []ActionIntent{{ID: "rehearsal-sign-negative", ActorID: actor.ID, Kind: "MOVE", ItemID: item.ID, TargetID: actor.ID}})
			if e != nil {
				return "", e
			}
			if !positive.Accepted || negative.Accepted {
				sign = "FAIL"
			} else if sign != "FAIL" {
				sign = "PASS"
			}
		}
		if otherCount == 0 && sign != "FAIL" {
			incomplete = true
		}
	}
	if incomplete && sign != "FAIL" {
		sign = "NOT_RUN"
	}
	return sign, nil
}

func rehearsalCustodyApplies(item ItemState) bool {
	for _, rule := range item.Rules {
		if rule.Primitive == "SIGN_CUSTODY" && rule.Strength == "HARD" && rule.Trigger == "MOVE" {
			return true
		}
	}
	return false
}
func (r *Runtime) PackageRehearsalRequiresSignTx(ctx context.Context, tx store.Tx, scope domain.Scope) (bool, error) {
	init, e := InitializationEvidence(ctx, tx, scope)
	if e != nil {
		return false, e
	}
	for _, item := range init.Items {
		if rehearsalCustodyApplies(item) {
			return true, nil
		}
	}
	return false, nil
}
