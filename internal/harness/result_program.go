package harness

import (
	"bytes"
	"encoding/json"
	"loreviva.local/loreviva/internal/memory"
	world "loreviva.local/loreviva/internal/runtime"
	"loreviva.local/loreviva/internal/wire"
	"unicode/utf8"
)

// Candidate is a program input, never an approved packet. Typed actions and
// proposals still require their owning modules' current transaction validation.
type Candidate struct {
	Dialogue        string                `json:"dialogue"`
	Actions         []world.ActionIntent  `json:"actions"`
	MemoryProposals []memory.MemoryRecord `json:"memory_proposals"`
}

type OriginalResultProgram struct{}

func (OriginalResultProgram) ValidateOriginalCandidate(raw []byte) string {
	v, e := wire.Parse(raw)
	if e != nil {
		return "STRUCTURE_INVALID"
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return "STRUCTURE_INVALID"
	}
	// An explicit provider refusal is never repair-eligible.
	if refusal, ok := obj["refusal"].(string); ok && refusal != "" {
		return "REFUSAL"
	}
	if len(obj) != 3 || obj["dialogue"] == nil || obj["actions"] == nil || obj["memory_proposals"] == nil {
		return "STRUCTURE_INVALID"
	}
	var c Candidate
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || c.Dialogue == "" || utf8.RuneCountInString(c.Dialogue) > 4000 || c.Actions == nil || c.MemoryProposals == nil || len(c.Actions) > 32 || len(c.MemoryProposals) > 32 {
		return "STRUCTURE_INVALID"
	}
	return "STRUCTURE_VALID"
}
