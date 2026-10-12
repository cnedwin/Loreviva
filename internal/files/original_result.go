package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
)

type OriginalResultPolicy interface {
	CheckResultGovernance(context.Context) error
	AuthorizeAttemptResultWithOriginal(context.Context, domain.TaskLease, string, string, store.OriginalResultEvidence) (domain.MockResultMetadata, error)
	AuthorizeAttemptResult(context.Context, domain.TaskLease, string, string) (domain.MockResultMetadata, error)
	AuthorizeOriginalResult(context.Context, domain.TaskLease, string) (domain.MockResultMetadata, error)
	AuthorizeApprovedTurn(context.Context, domain.Scope, string) (domain.CandidateCommit, error)
}

// Called only outside SQL transactions, before and after actual result IO.
func (f *Store) checkResultGovernance(ctx context.Context) error {
	if f.OriginalResultPolicy == nil {
		return ErrNotFound
	}
	return f.OriginalResultPolicy.CheckResultGovernance(ctx)
}

func (f *Store) ReadApprovedTurn(ctx context.Context, scope domain.Scope, task string) ([]byte, domain.CandidateCommit, error) {
	if f.OriginalResultPolicy == nil {
		return nil, domain.CandidateCommit{}, ErrNotFound
	}
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	defer release()
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	approved, e := f.OriginalResultPolicy.AuthorizeApprovedTurn(ctx, scope, task)
	if e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	if approved.OriginalDependency != nil {
		original := *approved.OriginalDependency
		if original.BodyStorage == approved.Metadata.BodyStorage {
			return nil, domain.CandidateCommit{}, ErrNotFound
		}
		originalDone, e := f.lockDerived(ctx, scope.ProductScope, original.BodyStorage)
		if e != nil {
			return nil, domain.CandidateCommit{}, e
		}
		defer originalDone()
		current, e := f.OriginalResultPolicy.AuthorizeApprovedTurn(ctx, scope, task)
		if e != nil || !reflect.DeepEqual(current, approved) {
			return nil, domain.CandidateCommit{}, ErrNotFound
		}
		if _, e = f.readLockedResultBytes(scope.ProductScope, original); e != nil {
			return nil, domain.CandidateCommit{}, e
		}
	}
	done, e := f.lockDerived(ctx, scope.ProductScope, approved.Metadata.BodyStorage)
	if e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	defer done()
	before, e := f.OriginalResultPolicy.AuthorizeApprovedTurn(ctx, scope, task)
	if e != nil || !reflect.DeepEqual(before, approved) {
		return nil, domain.CandidateCommit{}, ErrNotFound
	}
	body, e := f.readLockedResultBytes(scope.ProductScope, approved.Metadata)
	if e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	after, e := f.OriginalResultPolicy.AuthorizeApprovedTurn(ctx, scope, task)
	if e != nil || !reflect.DeepEqual(after, approved) {
		return nil, domain.CandidateCommit{}, ErrNotFound
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, domain.CandidateCommit{}, e
	}
	return body, approved, nil
}

type originalEvidence struct {
	owner    *Store
	original *originalEvidence
	closed   atomic.Bool
	body     []byte
	metadata domain.MockResultMetadata
	release  func()
	once     sync.Once
}

func (r *originalEvidence) Bytes() []byte                       { return append([]byte(nil), r.body...) }
func (r *originalEvidence) Metadata() domain.MockResultMetadata { return r.metadata }
func (r *originalEvidence) Close()                              { r.once.Do(func() { r.closed.Store(true); r.release() }) }

// RecoveryOriginalBytes accepts only this Files instance's live, locked value.
// Caller-created implementations, copies, closed evidence and other tasks fail.
func (f *Store) RecoveryOriginalBytes(e store.OriginalResultEvidence, s domain.Scope, task, attempt string) ([]byte, domain.MockResultMetadata, error) {
	r, ok := e.(*originalEvidence)
	if !ok || r == nil || r.owner != f || r.closed.Load() {
		return nil, domain.MockResultMetadata{}, ErrNotFound
	}
	if r.original != nil {
		r = r.original
	}
	if r.owner != f || r.closed.Load() || r.metadata.Identity.Scope != s || r.metadata.Identity.TaskID != task || r.metadata.Identity.AttemptID != attempt {
		return nil, domain.MockResultMetadata{}, ErrNotFound
	}
	return r.Bytes(), r.metadata, nil
}

// ReadOriginalEvidence returns a transient opaque result through the installed
// owning adapter. Metadata/bytes are not a persistent authority or approval.
func (f *Store) ReadOriginalEvidence(ctx context.Context, lease domain.TaskLease, worker string) (store.OriginalResultEvidence, error) {
	if lease.WorldInstanceID == nil {
		return nil, ErrNotFound
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	id, e := f.DB.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e != nil {
		return nil, e
	}
	return f.ReadAttemptEvidence(ctx, lease, worker, id.AttemptID)
}
func (f *Store) ReadAttemptEvidence(ctx context.Context, lease domain.TaskLease, worker, attempt string) (store.OriginalResultEvidence, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	keep := false
	if f.OriginalResultPolicy == nil {
		return nil, ErrNotFound
	}
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer func() {
		if !keep {
			release()
		}
	}()
	if lease.WorldInstanceID == nil {
		return nil, ErrNotFound
	}
	scope := domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}
	id, e := f.DB.OriginalMockIdentity(ctx, scope, lease.TaskID)
	if e != nil {
		return nil, e
	}
	var original *originalEvidence
	if attempt != id.AttemptID {
		// Fixed ordering is original0 then extra1, independent of storage names.
		value, e := f.ReadAttemptEvidence(ctx, lease, worker, id.AttemptID)
		if e != nil {
			return nil, e
		}
		original = value.(*originalEvidence)
		defer func() {
			if !keep {
				original.Close()
			}
		}()
	}
	meta, e := f.authorizeAttemptEvidence(ctx, lease, worker, attempt, original)
	if e != nil {
		return nil, e
	}
	if original != nil && meta.BodyStorage == original.metadata.BodyStorage {
		return nil, ErrNotFound
	}
	done, e := f.lockDerived(ctx, lease.ProductScope, meta.BodyStorage)
	if e != nil {
		return nil, e
	}
	defer func() {
		if !keep {
			done()
		}
	}()
	before, e := f.authorizeAttemptEvidence(ctx, lease, worker, attempt, original)
	if e != nil || before != meta {
		return nil, ErrNotFound
	}
	body, e := f.readLockedResultBytes(lease.ProductScope, meta)
	if e != nil {
		return nil, e
	}
	after, e := f.authorizeAttemptEvidence(ctx, lease, worker, attempt, original)
	if e != nil || after != meta {
		return nil, ErrNotFound
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	keep = true
	return &originalEvidence{owner: f, original: original, body: body, metadata: meta, release: func() {
		done()
		if original != nil {
			original.Close()
		}
		release()
	}}, nil
}

func (f *Store) authorizeAttemptEvidence(ctx context.Context, lease domain.TaskLease, worker, attempt string, original *originalEvidence) (domain.MockResultMetadata, error) {
	if original == nil {
		return f.OriginalResultPolicy.AuthorizeAttemptResult(ctx, lease, worker, attempt)
	}
	return f.OriginalResultPolicy.AuthorizeAttemptResultWithOriginal(ctx, lease, worker, attempt, original)
}

// The caller must already own this exact storage lock and repeat its fixed
// original or approved policy before and after this bounded filesystem read.
func (f *Store) readLockedResultBytes(scope domain.ProductScope, meta domain.MockResultMetadata) ([]byte, error) {
	var body []byte
	for _, ext := range []string{".part", ".blob"} {
		path, e := f.path(scope, meta.BodyStorage, ext)
		if e != nil {
			return nil, e
		}
		info, e := f.root.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrNotFound
		}
		file, e := f.root.Open(path)
		if e != nil {
			return nil, e
		}
		body, e = io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
		file.Close()
		if e != nil || len(body) > MaxFileBytes {
			return nil, ErrNotFound
		}
		break
	}
	h := sha256.Sum256(body)
	if len(body) == 0 || hex.EncodeToString(h[:]) != meta.BodyHash {
		return nil, ErrNotFound
	}
	return body, nil
}
