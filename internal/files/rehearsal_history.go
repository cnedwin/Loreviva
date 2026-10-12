package files

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"path/filepath"
	"reflect"
	"strconv"
)

// Classification alone grants nothing: both callers must consume the fixed
// terminal inspection. A registered report never falls back to an upload.
func (f *Store) rehearsalReportAssetTask(ctx context.Context, scope domain.ProductScope, asset string) (string, error) {
	var task string
	err := f.DB.DB.QueryRowContext(ctx, "SELECT task_id FROM harness_rehearsal_reports WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, asset).Scan(&task)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return task, err
}

// Fixed S57 lifetime: sources -> each frozen child 0/1 -> original report.
// This returns a copy only after current proof and actual bytes agree twice.
func (f *Store) ReadCommittedRehearsalReport(ctx context.Context, scope domain.ProductScope, task string) ([]byte, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	ctx, done, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	closes := []func(){done}
	defer func() {
		for i := len(closes) - 1; i >= 0; i-- {
			closes[i]()
		}
	}()
	before, e := f.DB.InspectCommittedRehearsalReport(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	lock := func(key string) error {
		if seen[key] {
			return ErrNotFound
		}
		seen[key] = true
		close, e := f.lockDerived(ctx, scope, key)
		if e != nil {
			return e
		}
		closes = append(closes, close)
		return nil
	}
	for _, s := range before.Sources {
		if e = lock(s.Storage); e != nil {
			return nil, e
		}
	}
	for _, c := range before.Children {
		metas := []domain.MockResultMetadata{c.Metadata}
		if c.OriginalDependency != nil {
			metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
		}
		for _, m := range metas {
			if e = lock(m.BodyStorage); e != nil {
				return nil, e
			}
		}
	}
	if e = lock(before.Storage); e != nil {
		return nil, e
	}
	locked, e := f.DB.InspectCommittedRehearsalReport(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	if !reflect.DeepEqual(before, locked) {
		return nil, ErrNotFound
	}
	if e = f.checkCandidateSourceFiles(scope, before.Sources); e != nil {
		return nil, e
	}
	for _, c := range before.Children {
		metas := []domain.MockResultMetadata{c.Metadata}
		if c.OriginalDependency != nil {
			metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
		}
		for _, m := range metas {
			if _, e = f.readLockedResultBytes(scope, m); e != nil {
				return nil, e
			}
		}
	}
	if len(before.Storage) != 32 {
		return nil, ErrNotFound
	}
	if _, e = hex.DecodeString(before.Storage); e != nil {
		return nil, ErrNotFound
	}
	file, e := f.root.Open(filepath.Join(scope.TenantID, scope.ProductID, before.Storage+".part"))
	if e != nil {
		return nil, ErrNotFound
	}
	body, e := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	file.Close()
	if e != nil {
		return nil, e
	}
	h := sha256.Sum256(body)
	if hex.EncodeToString(h[:]) != before.Asset.Sha256 || strconv.Itoa(len(body)) != string(before.Asset.SizeBytes) {
		return nil, ErrNotFound
	}
	after, e := f.DB.InspectCommittedRehearsalReport(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	if !reflect.DeepEqual(before, after) {
		return nil, ErrNotFound
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	return body, nil
}
