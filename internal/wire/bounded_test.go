package wire

import (
	"bytes"
	"strings"
	"testing"
)

func TestBoundedJSONPreservesStrictDefaults(t *testing.T) {
	large := []byte(`{"data":"` + strings.Repeat("a", MaxBytes) + `"}`)
	if _, err := Parse(large); err == nil {
		t.Fatal("ordinary wire ceiling was relaxed")
	}
	if _, err := Canonical(large); err == nil {
		t.Fatal("ordinary canonical ceiling was relaxed")
	}
	if _, err := ParseBounded(large, int64(len(large))); err != nil {
		t.Fatal("explicit exact ceiling refused", err)
	}
	if _, err := CanonicalBounded(large, int64(len(large))); err != nil {
		t.Fatal("explicit canonical ceiling refused", err)
	}
	for _, limit := range []int64{-1, 0, int64(len(large) - 1)} {
		if _, err := ParseBounded(large, limit); err == nil {
			t.Fatal("invalid/overrun ceiling accepted")
		}
		if _, err := CanonicalBounded(large, limit); err == nil {
			t.Fatal("invalid canonical ceiling accepted")
		}
	}
	for _, raw := range [][]byte{[]byte(`{"a":1,"\u0061":2}`), []byte(`{"nested":{"x":1,"x":2}}`), []byte(`"\ud800"`), []byte(`{} true`), {34, 255, 34}, []byte(strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66))} {
		if _, err := ParseBounded(raw, 8<<20); err == nil {
			t.Fatalf("strict parsing weakened for %q", raw)
		}
	}
	got, err := CanonicalBounded([]byte(`{"z":1.00,"a":-0.000e2}`), 128)
	if err != nil || !bytes.Equal(got, []byte(`{"a":0,"z":1}`)) {
		t.Fatal("canonical number/key algorithm changed", string(got), err)
	}
}
