package contracts_test

import (
	"bytes"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"testing"
)

func TestOriginalWireAndExactCanonicalHash(t *testing.T) {
	for _, bad := range [][]byte{[]byte(`{"release_id":"r","release_id":"s"}`), []byte(`{"s":"\ud800"}`), []byte(`{"s":"\udc00"}`), []byte{'"', 0xff, '"'}, []byte(`{} {}`), []byte(`{"a":{"b":1,"b":2}}`)} {
		if _, e := wire.Canonical(bad); e == nil {
			t.Fatalf("unsafe wire accepted %q", bad)
		}
	}
	a, e := wire.Canonical([]byte(`{"z":[1.00,900719925474099312345,0.123456789012345678901],"a":"é"}`))
	if e != nil {
		t.Fatal(e)
	}
	b, e := wire.Canonical([]byte(`{"a":"é","z":[1e0,900719925474099312345.0,123456789012345678901e-21]}`))
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("equivalent exact numbers differ %s %s", a, b)
	}
	c, _ := wire.Canonical([]byte(`{"z":[900719925474099312345,1,0.123456789012345678901],"a":"é"}`))
	if bytes.Equal(a, c) {
		t.Fatal("array order collapsed")
	}
	if generated.Validate("Versions", []byte(`{"release_id":"r","world_revision":"1","control_version":"1","privacy_epoch":"1","view_epoch":"1","release_id":"s"}`)) == nil {
		t.Fatal("generated ingress bypassed duplicate keys")
	}
}
