package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"sync"
)

type candidateSourceEvidence struct {
	once    sync.Once
	release func()
}

func (e *candidateSourceEvidence) Close() { e.once.Do(e.release) }

func (f *Store) checkCandidateSourceFiles(scope domain.ProductScope, sources []store.CandidateSourceAsset) error {
	for _, source := range sources {
		path, e := f.path(scope, source.Storage, ".blob")
		if e != nil {
			return e
		}
		file, e := f.root.Open(path)
		if e != nil {
			return e
		}
		hash := sha256.New()
		n, readError := io.Copy(hash, io.LimitReader(file, MaxFileBytes+1))
		closeError := file.Close()
		if readError != nil {
			return readError
		}
		if closeError != nil {
			return closeError
		}
		if n > MaxFileBytes || hex.EncodeToString(hash.Sum(nil)) != source.Hash {
			return ErrNotFound
		}
	}
	return nil
}

// Source uploads are locked in fixed storage order before result0/result1.
// Bytes are verified and discarded; they are never injected into the prompt.
func (f *Store) ReadCandidateSourceEvidence(ctx context.Context, scope domain.ProductScope, m domain.DraftCandidateMaterial) (store.CandidateSourceEvidence, error) {
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
	before, e := f.DB.InspectCandidateSourceMaterial(ctx, scope, m)
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
	locked, e := f.DB.InspectCandidateSourceMaterial(ctx, scope, m)
	if e != nil || !reflect.DeepEqual(before, locked) {
		return nil, ErrNotFound
	}
	if e = f.checkCandidateSourceFiles(scope, before); e != nil {
		return nil, e
	}
	after, e := f.DB.InspectCandidateSourceMaterial(ctx, scope, m)
	if e != nil || !reflect.DeepEqual(before, after) {
		return nil, ErrNotFound
	}
	kept = true
	return &candidateSourceEvidence{release: closeAll}, nil
}
