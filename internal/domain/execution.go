package domain

import "errors"

type ExecutionLimits struct{ Calls, Steps, Seconds int }

// LimitsFor is the fixed R0 program contract, shared by intake and execution.
// Rehearsal's aggregate allowance never changes the foundational child limit.
func LimitsFor(kind string, n int) (ExecutionLimits, error) {
	switch kind {
	case "TURN", "CANDIDATE":
		if n == 0 {
			return ExecutionLimits{2, 20, 120}, nil
		}
	case "REHEARSAL":
		if n >= 1 && n <= 20 {
			return ExecutionLimits{2 * n, 2 + 20*n, 30 + 60*n}, nil
		}
	}
	return ExecutionLimits{}, errors.New("unsupported bounded execution kind or input count")
}
