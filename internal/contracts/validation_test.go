package contracts_test

import (
	"bytes"
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"testing"
)

func TestCanonicalBoundaryValidatesExactStringsAndRejectsUnknownFields(t *testing.T) {
	valid := []byte(`{"release_id":"release","world_revision":"18446744073709551615","control_version":"1","privacy_epoch":"1","view_epoch":"1"}`)
	var versions generated.Versions
	if err := generated.Decode("Versions", valid, &versions); err != nil {
		t.Fatal(err)
	}
	if versions.WorldRevision != "18446744073709551615" {
		t.Fatal("exact integer coerced")
	}
	for _, input := range []string{
		`{"release_id":"release","world_revision":9007199254740992,"control_version":"1","privacy_epoch":"1","view_epoch":"1"}`,
		`{"release_id":"release","world_revision":"1","control_version":"1","privacy_epoch":"1","view_epoch":"1","extra":true}`,
		`{"release_id":"release","world_revision":"1","control_version":"1","privacy_epoch":"1"}`,
		`{"release_id":"release","world_revision":"1","control_version":"1","privacy_epoch":"1","view_epoch":"1"} {}`,
	} {
		if generated.Validate("Versions", []byte(input)) == nil {
			t.Fatal("invalid original wire JSON accepted")
		}
	}
}

func TestNamedRawJSONRoundTrips(t *testing.T) {
	segment := `{"segment_id":"s","speaker_actor_id":"actor","kind":"DIALOGUE","text":"hello","committed_event_ids":[]}`
	cases := []struct {
		name, wire string
		target     any
	}{
		{"TextSegment", segment, new(generated.TextSegment)},
		{"MemoryGovernanceRequest", `{"operation":"DELETE","memory_ids":["memory"],"expected_revisions":{"memory":"18446744073709551615"}}`, new(generated.MemoryGovernanceRequest)},
		{"ApprovedPacket", `{"packet_id":"packet","scope":{"tenant_id":"tenant","product_id":"product","world_instance_id":"world"},"turn_id":"turn","versions":{"release_id":"release","world_revision":"18446744073709551615","control_version":"1","privacy_epoch":"1","view_epoch":"1"},"text_segments":[` + segment + `],"actions":[{"action_id":"action","kind":"synthetic","committed_event_ids":["event"],"parameters":{"large_integer":900719925474099312345,"exact_decimal":0.123456789012345678901}}],"media":[],"audience":{"kind":"PUBLIC","subject_ids":[]}}`, new(generated.ApprovedPacket)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := []byte(tc.wire)
			if err := generated.Validate(tc.name, wire); err != nil {
				t.Fatalf("fixture invalid: %v", err)
			}
			if err := generated.Decode(tc.name, wire, tc.target); err != nil {
				t.Fatalf("valid DTO decode: %v", err)
			}
			encoded, err := json.Marshal(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if err := generated.Validate(tc.name, encoded); err != nil {
				t.Fatalf("roundtrip no longer follows Schema: %v; %s", err, encoded)
			}
			decode := func(data []byte) any {
				d := json.NewDecoder(bytes.NewReader(data))
				d.UseNumber()
				var v any
				if err := d.Decode(&v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			if !reflect.DeepEqual(decode(wire), decode(encoded)) {
				t.Fatalf("roundtrip changed JSON values or numeric precision: %s", encoded)
			}
		})
	}
}

func TestCanonicalBoundaryValidatesRawUnion(t *testing.T) {
	valid := []byte(`{"expected_versions":{"world_revision":"1","control_version":"1"},"primary_actor_id":"actor","input":{"kind":"TEXT","text":"hello"},"quote_id":"quote","requested_audience":"PRIVATE_RELATIONSHIP"}`)
	if err := generated.Validate("TurnRequest", valid); err != nil {
		t.Fatal(err)
	}
	invalid := []byte(`{"expected_versions":{"world_revision":"1","control_version":"1"},"primary_actor_id":"actor","input":{"kind":"TEXT","text":42},"quote_id":"quote","requested_audience":"PRIVATE_RELATIONSHIP"}`)
	if generated.Validate("TurnRequest", invalid) == nil {
		t.Fatal("malformed union accepted")
	}
}
