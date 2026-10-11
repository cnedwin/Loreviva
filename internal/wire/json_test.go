package wire

import (
	"bytes"
	"strings"
	"testing"
)

func TestBoundsRejectTinyExponentBombAndDeepOversize(t *testing.T) {
	for _, bad := range []string{`1e999999999`, `1e-999999999`, strings.Repeat("1", 1025), strings.Repeat("[", 66) + "0" + strings.Repeat("]", 66), `"` + strings.Repeat("a", MaxBytes) + `"`} {
		if _, e := Canonical([]byte(bad)); e == nil {
			t.Fatal("bounded work bypass")
		}
	}
	a, e := Canonical([]byte(`-0.000e2`))
	if e != nil || !bytes.Equal(a, []byte("0")) {
		t.Fatal("zero canonicalization", a, e)
	}
}
