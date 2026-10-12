package files

import (
	"context"
	"loreviva.local/loreviva/internal/domain"
	"reflect"
)

func (f *Store) candidateAssetTask(ctx context.Context, s domain.ProductScope, asset string) (string, error) {
	var count int
	var task string
	e := f.DB.DB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT task_id),COALESCE(MIN(task_id),'') FROM (SELECT task_id FROM harness_candidate_results WHERE tenant_id=? AND product_id=? AND asset_id=? UNION SELECT task_id FROM harness_candidate_commits WHERE tenant_id=? AND product_id=? AND asset_id=?) c", s.TenantID, s.ProductID, asset, s.TenantID, s.ProductID, asset).Scan(&count, &task)
	if e != nil {
		return "", e
	}
	if count > 1 {
		return "", ErrNotFound
	}
	return task, nil
}

// Fixed S55 bytes reader. Original 0 and optional repaired 1 remain actual
// governed dependencies, locked in that order outside database transactions.
func (f *Store) ReadApprovedDraftCandidate(ctx context.Context, scope domain.ProductScope, task string) ([]byte, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer release()
	before, e := f.DB.InspectApprovedDraftCandidate(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	if len(before.Results) < 1 || len(before.Results) > 2 {
		return nil, ErrNotFound
	}
	for _, source := range before.Sources {
		done, e := f.lockDerived(ctx, scope, source.Storage)
		if e != nil {
			return nil, e
		}
		defer done()
	}
	for _, m := range before.Results {
		done, e := f.lockDerived(ctx, scope, m.BodyStorage)
		if e != nil {
			return nil, e
		}
		defer done()
	}
	locked, e := f.DB.InspectApprovedDraftCandidate(ctx, scope, task)
	if e != nil || !reflect.DeepEqual(before, locked) {
		return nil, ErrNotFound
	}
	if e = f.checkCandidateSourceFiles(scope, before.Sources); e != nil {
		return nil, e
	}
	var body []byte
	for _, m := range before.Results {
		body, e = f.readCandidateBytes(scope, m)
		if e != nil {
			return nil, e
		}
	}
	after, e := f.DB.InspectApprovedDraftCandidate(ctx, scope, task)
	if e != nil || !reflect.DeepEqual(before, after) {
		return nil, ErrNotFound
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	return body, nil
}
