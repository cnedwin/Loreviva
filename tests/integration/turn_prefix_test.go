package integration

import (
	"context"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

// The fixture calls real typed stage ports. Production Worker.Once always runs
// the full workflow; there is no production pause switch or fabricated result.
func runOriginalPrefix(ctx context.Context, r *app.Runtime) error {
	lease, e := r.Store.ClaimRegistered(ctx, "m1-file-cleanup", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "TURN", Operation: "TURN"}})
	if e != nil {
		return e
	}
	d, e := r.Turns.PrepareOriginalTurn(ctx, lease.TaskLease, "m1-file-cleanup")
	if e != nil {
		return e
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		return e
	}
	d.CommandKey = content.Identity(lease.TaskID, "send")
	d.CommitKey = d.CommandKey
	auth, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		return e
	}
	if !auth.CanSend || auth.OriginalSend() == nil {
		return store.ErrDenied
	}
	if e = r.Worker.Turns.Generate(ctx, auth.OriginalSend()); e != nil {
		return e
	}
	if r.Worker.Turns.Fault == harness.MockDropOriginalReply {
		return harness.ErrMockReplyLost
	}
	if _, e = r.Store.ObserveOriginalReply(ctx, auth.OriginalSend()); e != nil {
		return e
	}
	return r.Store.ValidateOriginalTurn(ctx, lease.TaskLease, "m1-file-cleanup")
}
