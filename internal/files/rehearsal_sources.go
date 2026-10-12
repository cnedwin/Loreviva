package files

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
)

// Fixed authoring trial source owner. Never uses Candidate selection/assist,
// an arbitrary caller path or a generic source inspection callback.
func (f *Store) ReadRehearsalSourceEvidence(ctx context.Context, scope domain.ProductScope, m domain.RehearsalBinding, request generated.RehearsalRequest) (store.CandidateSourceEvidence, error) {
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	kept := false
	closes := []func(){release}
	closeAll := func() {
		for i := len(closes) - 1; i >= 0; i-- {
			closes[i]()
		}
	}
	defer func() {
		if !kept {
			closeAll()
		}
	}()
	before, e := f.DB.InspectRehearsalSourceMaterial(ctx, scope, m, request)
	if e != nil {
		return nil, e
	}
	for _, source := range before {
		done, e := f.lockDerived(ctx, scope, source.Storage)
		if e != nil {
			return nil, e
		}
		closes = append(closes, done)
	}
	locked, e := f.DB.InspectRehearsalSourceMaterial(ctx, scope, m, request)
	if e != nil || !reflect.DeepEqual(before, locked) {
		return nil, ErrNotFound
	}
	if e = f.checkCandidateSourceFiles(scope, before); e != nil {
		return nil, e
	}
	after, e := f.DB.InspectRehearsalSourceMaterial(ctx, scope, m, request)
	if e != nil || !reflect.DeepEqual(before, after) {
		return nil, ErrNotFound
	}
	kept = true
	return &candidateSourceEvidence{release: closeAll}, nil
}
