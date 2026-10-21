package runtime

import (
	"context"
	"loreviva.local/loreviva/internal/generated"
)

// The Store owns the exact bearer-to-participant bridge and same-Tx intake.
func (r *Runtime) ControlIntegrationInstance(ctx context.Context, world, key string, input generated.ControlRequest) (generated.TaskReceipt, error) {
	return r.Store.ControlIntegrationInstance(ctx, world, key, input)
}
