package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
)

// This private proof retains the fixed source -> child original/repair ->
// report lock order until the owning approval transaction has returned.
type packageRehearsalFiles struct {
	mu       sync.Mutex
	owner    *Store
	scope    domain.ProductScope
	task     string
	snapshot store.CommittedRehearsalReport
	closes   []func()
	closed   bool
}

func (p *packageRehearsalFiles) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	for i := len(p.closes) - 1; i >= 0; i-- {
		p.closes[i]()
	}
}
func (p *packageRehearsalFiles) read(ctx context.Context) ([]byte, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := p.owner.checkCandidateSourceFiles(p.scope, p.snapshot.Sources); e != nil {
		return nil, e
	}
	for _, c := range p.snapshot.Children {
		metas := []domain.MockResultMetadata{c.Metadata}
		if c.OriginalDependency != nil {
			metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
		}
		for _, m := range metas {
			if _, e := p.owner.readLockedResultBytes(p.scope, m); e != nil {
				return nil, e
			}
		}
	}
	name, e := p.owner.path(p.scope, p.snapshot.Storage, ".part")
	if e != nil {
		return nil, e
	}
	info, e := p.owner.root.Lstat(name)
	if e != nil || !info.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	file, e := p.owner.root.Open(filepath.Join(p.scope.TenantID, p.scope.ProductID, p.snapshot.Storage+".part"))
	if e != nil {
		return nil, ErrNotFound
	}
	body, e := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	file.Close()
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(body)
	if len(body) > MaxFileBytes || hex.EncodeToString(sum[:]) != p.snapshot.Asset.Sha256 || strconv.Itoa(len(body)) != string(p.snapshot.Asset.SizeBytes) {
		return nil, ErrNotFound
	}
	return body, nil
}

// No database or nested barrier is opened here. The original terminal owner
// supplies the current SQL projection inside the already locked transaction.
func (p *packageRehearsalFiles) CheckPackageApprovalFiles(ctx context.Context, scope domain.ProductScope, task string, current store.CommittedRehearsalReport) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.scope != scope || p.task != task || !reflect.DeepEqual(p.snapshot, current) {
		return ErrNotFound
	}
	_, e := p.read(ctx)
	return e
}
func (f *Store) HoldPackageApprovalFiles(ctx context.Context, scope domain.ProductScope, task string) (store.PackageApprovalFileProof, []byte, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, nil, e
	}
	ctx, done, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, nil, e
	}
	p := &packageRehearsalFiles{owner: f, scope: scope, task: task, closes: []func(){done}}
	kept := false
	defer func() {
		if !kept {
			p.Close()
		}
	}()
	p.snapshot, e = f.DB.InspectCommittedRehearsalReport(ctx, scope, task)
	if e != nil {
		return nil, nil, e
	}
	seen := map[string]bool{}
	lock := func(key string) error {
		if seen[key] {
			return ErrNotFound
		}
		seen[key] = true
		release, e := f.lockDerived(ctx, scope, key)
		if e != nil {
			return e
		}
		p.closes = append(p.closes, release)
		return nil
	}
	for _, source := range p.snapshot.Sources {
		if e = lock(source.Storage); e != nil {
			return nil, nil, e
		}
	}
	for _, child := range p.snapshot.Children {
		metas := []domain.MockResultMetadata{child.Metadata}
		if child.OriginalDependency != nil {
			metas = append([]domain.MockResultMetadata{*child.OriginalDependency}, metas...)
		}
		for _, m := range metas {
			if e = lock(m.BodyStorage); e != nil {
				return nil, nil, e
			}
		}
	}
	if e = lock(p.snapshot.Storage); e != nil {
		return nil, nil, e
	}
	current, e := f.DB.InspectCommittedRehearsalReport(ctx, scope, task)
	if e != nil {
		return nil, nil, e
	}
	if !reflect.DeepEqual(current, p.snapshot) {
		return nil, nil, ErrNotFound
	}
	body, e := p.read(ctx)
	if e != nil {
		return nil, nil, e
	}
	kept = true
	return p, body, nil
}
