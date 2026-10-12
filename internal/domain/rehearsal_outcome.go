package domain

import (
	"encoding/json"
	"errors"
)

// A report outcome never upgrades a refused program input into a world commit.
// The common fields are governed evidence, not approval or read capabilities.
type RehearsalNegativeOutcome struct {
	State, ReceiptKey, ValidationKey string
	Versions                         Versions
	NarrativeGeneration              uint64
	Metadata                         MockResultMetadata
	OriginalDependency               *MockResultMetadata
}

type RehearsalChildOutcome struct {
	World              *CandidateCommit
	Negative           *RehearsalNegativeOutcome
	ResultID           string
	Versions           Versions
	Metadata           MockResultMetadata
	OriginalDependency *MockResultMetadata
}

func RehearsalWorldOutcome(c CandidateCommit) RehearsalChildOutcome {
	return RehearsalChildOutcome{World: &c, ResultID: c.ResultID, Versions: c.Versions, Metadata: c.Metadata, OriginalDependency: c.OriginalDependency}
}
func RehearsalRejectedOutcome(n RehearsalNegativeOutcome) RehearsalChildOutcome {
	return RehearsalChildOutcome{Negative: &n, ResultID: n.ReceiptKey, Versions: n.Versions, Metadata: n.Metadata, OriginalDependency: n.OriginalDependency}
}

// Keep every existing committed-child evidence preimage byte-identical.
func (c RehearsalChildOutcome) MarshalJSON() ([]byte, error) {
	if c.World != nil && c.Negative == nil {
		return json.Marshal(c.World)
	}
	if c.Negative != nil && c.World == nil {
		return json.Marshal(struct{ Negative *RehearsalNegativeOutcome }{c.Negative})
	}
	return nil, errors.New("invalid rehearsal outcome branch")
}
func (c *RehearsalChildOutcome) UnmarshalJSON(raw []byte) error {
	var shape struct{ Negative *RehearsalNegativeOutcome }
	if e := json.Unmarshal(raw, &shape); e != nil {
		return e
	}
	if shape.Negative != nil {
		*c = RehearsalRejectedOutcome(*shape.Negative)
		return nil
	}
	var world CandidateCommit
	if e := json.Unmarshal(raw, &world); e != nil {
		return e
	}
	*c = RehearsalWorldOutcome(world)
	return nil
}
