package delivery

import (
	"loreviva.local/loreviva/internal/wire"
	"strconv"
)

// Query parsing is closed and independent of cryptographic cursor validation.
// The sole source default is fifty; twenty is the separate SSE drain budget.
func ParseLimit(raw string) (uint16, error) {
	if raw == "" {
		return 50, nil
	}
	n, e := strconv.ParseUint(raw, 10, 16)
	if e != nil || n < 1 || n > 100 || strconv.FormatUint(n, 10) != raw {
		return 0, wire.ErrJSON
	}
	return uint16(n), nil
}
