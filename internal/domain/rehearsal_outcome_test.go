package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestRehearsalWorldOutcomePreservesCommittedPreimage(t *testing.T) {
	for _, extra := range []bool{false, true} {
		original := CandidateCommit{NarrativeGeneration: 1, ResultID: "result", ReceiptKey: "receipt", EventIDs: []string{"event"}, MemoryIDs: []string{"memory"}}
		original.Metadata.BodyHash = "original-body-hash"
		if extra {
			dependency := original.Metadata
			original.OriginalDependency = &dependency
			original.Metadata.BodyHash = "extra-body-hash"
		}
		// This is the persisted pre-phase31 all-child receipt preimage shape.
		legacy, e := json.Marshal([]CandidateCommit{original})
		if e != nil {
			t.Fatal(e)
		}
		current, e := json.Marshal([]RehearsalChildOutcome{RehearsalWorldOutcome(original)})
		if e != nil || !bytes.Equal(legacy, current) {
			t.Fatal("committed receipt preimage changed", extra, e)
		}
		var decoded []RehearsalChildOutcome
		if e = json.Unmarshal(legacy, &decoded); e != nil || len(decoded) != 1 || decoded[0].Negative != nil || !reflect.DeepEqual(decoded[0].World, &original) {
			t.Fatal("legacy outcome changed", extra, e)
		}
	}
}
