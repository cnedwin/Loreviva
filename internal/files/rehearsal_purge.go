package files

import (
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// Minted only after actual removal while the fixed owning storage lock and
// original operation admission remain live. No filesystem IO occurs in Check.
type rehearsalPurgeEvidence struct {
	owner *Store
	scope domain.ProductScope
	key   string
	claim store.CommandBarrierRequest
	live  bool
}

func (*rehearsalPurgeEvidence) PurgeEvidence() {}
func (f *Store) CheckRehearsalPurgeEvidence(e store.RehearsalPurgeEvidence, s domain.ProductScope, key string) (store.CommandBarrierRequest, error) {
	p, ok := e.(*rehearsalPurgeEvidence)
	if !ok || p == nil || p.owner != f || p.scope != s || p.key != key || !p.live || p.claim.Operation != "CLAIM_STAGE_DELETE" || p.claim.ProductScope != s {
		return store.CommandBarrierRequest{}, store.ErrDenied
	}
	return p.claim, nil
}
