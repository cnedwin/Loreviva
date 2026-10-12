package harness

import "testing"

func TestOriginalProgramRejectsAmbiguousOrIncompleteStructure(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"dialogue":"heard","actions":[],"memory_proposals":[]}`, "STRUCTURE_VALID"},
		{`{"dialogue":"heard","actions":[],"actions":[],"memory_proposals":[]}`, "STRUCTURE_INVALID"},
		{`{"dialogue":"heard","actions":null,"memory_proposals":[]}`, "STRUCTURE_INVALID"},
		{`{"dialogue":"heard","actions":[]}`, "STRUCTURE_INVALID"},
		{`{"dialogue":"heard","actions":[{"unknown":true}],"memory_proposals":[]}`, "STRUCTURE_INVALID"},
		{`{"refusal":"declined"}`, "REFUSAL"},
		{`{"dialogue":"heard","actions":[],"memory_proposals":[]} true`, "STRUCTURE_INVALID"},
	} {
		if got := (OriginalResultProgram{}).ValidateOriginalCandidate([]byte(tc.body)); got != tc.code {
			t.Fatalf("%s: %s != %s", tc.body, got, tc.code)
		}
	}
}
