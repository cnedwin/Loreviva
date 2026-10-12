package files

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

// ReadRehearsalChildResult keeps the original source/0/1 locks in that order.
// It is only a current root report read; it returns no child execution permit.
func (f *Store) ReadRehearsalChildResult(ctx context.Context, root domain.TaskLease, worker string, index int) ([]byte, domain.CandidateCommit, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	ctx, close, e := f.DB.EnterRehearsalResultRead(ctx, root, worker, index)
	if e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	defer close()
	c, e := f.DB.InspectRehearsalChildResult(ctx, root, worker, index)
	if e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	metas := []domain.MockResultMetadata{c.Metadata}
	if c.OriginalDependency != nil {
		if c.OriginalDependency.BodyStorage == c.Metadata.BodyStorage {
			return nil, domain.CandidateCommit{}, ErrNotFound
		}
		metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
	}
	for _, meta := range metas {
		done, e := f.lockDerived(ctx, root.ProductScope, meta.BodyStorage)
		if e != nil {
			return nil, domain.CandidateCommit{}, e
		}
		defer done()
	}
	before, e := f.DB.InspectRehearsalChildResult(ctx, root, worker, index)
	if e != nil || !reflect.DeepEqual(c, before) {
		return nil, domain.CandidateCommit{}, ErrNotFound
	}
	var body []byte
	for _, meta := range metas {
		body, e = f.readLockedResultBytes(root.ProductScope, meta)
		if e != nil {
			return nil, domain.CandidateCommit{}, e
		}
	}
	after, e := f.DB.InspectRehearsalChildResult(ctx, root, worker, index)
	if e != nil || !reflect.DeepEqual(c, after) {
		return nil, domain.CandidateCommit{}, ErrNotFound
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	return body, c, nil
}
