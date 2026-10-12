package harness

import (
	"math"
	"testing"
)

func TestScale8AccumulatesBeforeHalfEvenAndCeiling(t *testing.T) {
	// Two half-atoms must become one atom; rounding each separately loses both.
	lines := []PricedQuantity{{Quantity: Scale / 2, Rate: Amount{Unit: Money, Currency: "USD", Atoms: 1}}, {Quantity: Scale / 2, Rate: Amount{Unit: Money, Currency: "USD", Atoms: 1}}}
	out, e := SumPriced(lines, HalfEven)
	if e != nil || out.Atoms != 1 {
		t.Fatalf("round summed exact costs once: %+v %v", out, e)
	}
	for _, tc := range []struct {
		rate  int64
		round Rounding
		want  int64
	}{{1, HalfEven, 0}, {3, HalfEven, 2}, {5, HalfEven, 2}, {1, Ceiling, 1}} {
		out, e = SumPriced([]PricedQuantity{{Quantity: Scale / 2, Rate: Amount{Unit: Money, Currency: "USD", Atoms: tc.rate}}}, tc.round)
		if e != nil || out.Atoms != tc.want {
			t.Fatalf("rate %d rounding %s: %+v %v", tc.rate, tc.round, out, e)
		}
	}
}
func TestScale8RejectsMixedUnitsOverflowAndInvalidRounding(t *testing.T) {
	usd := Amount{Unit: Money, Currency: "USD", Atoms: 1}
	credit := Amount{Unit: Credit, Atoms: 1}
	if _, e := AddAmounts(usd, credit); e == nil {
		t.Fatal("money and credits mixed")
	}
	if _, e := AddAmounts(Amount{Unit: Money, Currency: "USD", Atoms: math.MaxInt64}, usd); e == nil {
		t.Fatal("addition overflow")
	}
	if _, e := SumPriced([]PricedQuantity{{Quantity: math.MaxInt64, Rate: Amount{Unit: Money, Currency: "USD", Atoms: math.MaxInt64}}}, HalfEven); e == nil {
		t.Fatal("product overflow")
	}
	if _, e := SumPriced([]PricedQuantity{{Quantity: Scale, Rate: usd}}, "HALF_UP"); e == nil {
		t.Fatal("unsupported rounding")
	}
	if _, e := SumPriced([]PricedQuantity{{Quantity: -1, Rate: usd}}, HalfEven); e == nil {
		t.Fatal("negative quantity")
	}
	if _, e := AddAmounts(Amount{Unit: Credit, Currency: "USD"}, credit); e == nil {
		t.Fatal("credit mislabeled money")
	}
}
func TestFrozenExecutionLimits(t *testing.T) {
	for _, kind := range []string{"TURN", "CANDIDATE"} {
		v, e := LimitsFor(kind, 0)
		if e != nil || v.Calls != 2 || v.Steps != 20 || v.Seconds != 120 {
			t.Fatal(kind, v, e)
		}
	}
	for _, n := range []int{1, 20} {
		v, e := LimitsFor("REHEARSAL", n)
		if e != nil || v.Calls != 2*n || v.Steps != 2+20*n || v.Seconds != 30+60*n {
			t.Fatal(n, v, e)
		}
	}
	for _, n := range []int{-1, 0, 21} {
		if _, e := LimitsFor("REHEARSAL", n); e == nil {
			t.Fatal("unbounded rehearsal", n)
		}
	}
	if _, e := LimitsFor("TURN", 1); e == nil {
		t.Fatal("online child input budget")
	}
}
