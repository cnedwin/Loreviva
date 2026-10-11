package localstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// ExclusiveAdmission retains the actual OS handle. A zero value, another root,
// or a released handle cannot mint a live closed-generation proof.
type ExclusiveAdmission struct {
	root    *Root
	mu      sync.RWMutex
	live    bool
	release func()
	binding *ClosedBinding
}
type ClosedBinding struct {
	Installation, Database, Files, RootHash, Plan string
	Generation                                    uint64
}

func RootHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}
func (r *Root) ExclusiveAdmission(ctx context.Context) (*ExclusiveAdmission, error) {
	done, e := r.Lock(ctx, "operations.lock")
	if e != nil {
		return nil, e
	}
	return &ExclusiveAdmission{root: r, live: true, release: done}, nil
}
func (h *ExclusiveAdmission) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live {
		h.live = false
		h.release()
	}
}

// BindClosed checks physical provenance while the coordinator holds admission;
// it does not transition coordinator state or provide reopening authority.
func (h *ExclusiveAdmission) BindClosed(ctx context.Context, b ClosedBinding) error {
	if h == nil {
		return ErrClosed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.live || h.root == nil || b.RootHash != RootHash(h.root.dir) || b.Generation < 2 {
		return ErrClosed
	}
	done, e := h.root.Lock(ctx, "state.lock")
	if e != nil {
		return e
	}
	defer done()
	raw, e := h.root.Read("coordinator.json")
	if e != nil {
		return e
	}
	var state struct {
		Installation, Database, Files, Phase, Plan string
		Generation                                 uint64
	}
	if json.Unmarshal(raw, &state) != nil || state.Phase != "CLOSED" || state.Installation != b.Installation || state.Database != b.Database || state.Files != b.Files || state.Plan != b.Plan || state.Generation != b.Generation {
		return ErrClosed
	}
	if h.binding != nil && *h.binding != b {
		return ErrClosed
	}
	copy := b
	h.binding = &copy
	return nil
}

// Pin has no OS I/O. Its lifetime lock keeps exclusive admission alive for the
// complete SQL operation; release cannot race the transaction.
func (h *ExclusiveAdmission) Pin() (ClosedBinding, func(), error) {
	if h == nil {
		return ClosedBinding{}, nil, ErrClosed
	}
	h.mu.RLock()
	if !h.live || h.binding == nil {
		h.mu.RUnlock()
		return ClosedBinding{}, nil, ErrClosed
	}
	return *h.binding, h.mu.RUnlock, nil
}
