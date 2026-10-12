// Package harness executes only the bounded, locally authorized R0 Mock route.
package harness

import (
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"math"
	"math/big"
)

const Scale int64 = 100000000

type Unit string

const (
	Money  Unit = "MONEY"
	Credit Unit = "WE_CREDIT"
)

type Rounding string

const (
	HalfEven Rounding = "SUM_THEN_HALF_EVEN"
	Ceiling  Rounding = "SUM_THEN_CEILING"
)

type Amount struct {
	Unit     Unit
	Currency string
	Atoms    int64
}
type PricedQuantity struct {
	Quantity int64
	Rate     Amount
}

var ErrBudget = errors.New("invalid, incompatible or overflowing bounded budget")

func validAmount(a Amount) bool {
	return a.Atoms >= 0 && (a.Unit == Money && a.Currency == "USD" || a.Unit == Credit && a.Currency == "")
}
func AddAmounts(a, b Amount) (Amount, error) {
	if !validAmount(a) || !validAmount(b) || a.Unit != b.Unit || a.Currency != b.Currency || a.Atoms > math.MaxInt64-b.Atoms {
		return Amount{}, ErrBudget
	}
	a.Atoms += b.Atoms
	return a, nil
}

// SumPriced keeps exact Scale16 products until the final aggregate rounding.
// The wide intermediate cannot wrap; every represented Scale8 amount remains
// checked int64. R0 USD is a synthetic cost label, never a live rate selection.
func SumPriced(lines []PricedQuantity, rounding Rounding) (Amount, error) {
	if len(lines) == 0 || len(lines) > 1000 || rounding != HalfEven && rounding != Ceiling {
		return Amount{}, ErrBudget
	}
	out := lines[0].Rate
	out.Atoms = 0
	sum := new(big.Int)
	for _, line := range lines {
		if line.Quantity < 0 || !validAmount(line.Rate) || line.Rate.Unit != out.Unit || line.Rate.Currency != out.Currency {
			return Amount{}, ErrBudget
		}
		sum.Add(sum, new(big.Int).Mul(big.NewInt(line.Quantity), big.NewInt(line.Rate.Atoms)))
	}
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(sum, big.NewInt(Scale), remainder)
	roundUp := false
	if rounding == Ceiling {
		roundUp = remainder.Sign() > 0
	} else {
		twice := new(big.Int).Lsh(remainder, 1)
		cmp := twice.Cmp(big.NewInt(Scale))
		roundUp = cmp > 0 || cmp == 0 && quotient.Bit(0) == 1
	}
	if roundUp {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return Amount{}, ErrBudget
	}
	out.Atoms = quotient.Int64()
	return out, nil
}

type ExecutionLimits = domain.ExecutionLimits

func LimitsFor(kind string, n int) (ExecutionLimits, error) { return domain.LimitsFor(kind, n) }
