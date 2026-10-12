package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
)

// The stage identity and hash are always derived here from actual provider
// bytes, with source binding before first write and the same storage mutex
// used by cleanup. No arbitrary file path or caller lineage is accepted.
func (f *Store) WriteCandidateResult(ctx context.Context, d store.DispatchRequest, body []byte) (string, string, error) {
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return "", "", e
	}
	defer release()
	if d.Slot == 0 {
		source, e := f.DB.CandidateOriginalSourceEvidence(ctx, d)
		if e != nil {
			return "", "", e
		}
		defer source.Close()
	}
	if d.Slot == 1 {
		original, e := f.DB.CandidateOriginalDispatch(ctx, d)
		if e != nil {
			return "", "", e
		}
		evidence, e := f.ReadPendingCandidateEvidence(ctx, original)
		if e != nil {
			return "", "", e
		}
		defer evidence.Close()
		d, e = f.DB.WithCandidateOriginalEvidence(d, evidence)
		if e != nil {
			return "", "", e
		}
	}
	stage, e := f.AllocateDerived(d.ProductScope, string(body))
	if e != nil {
		return "", "", e
	}
	if e = f.DB.BindCandidateResultStage(ctx, d, stage.key, stage.Hash); e != nil {
		return "", "", e
	}
	done, e := f.lockDerived(ctx, d.ProductScope, stage.key)
	if e != nil {
		return "", "", e
	}
	defer done()
	if e = f.DB.InspectCandidateResultStage(ctx, d, stage.key, stage.Hash); e != nil {
		return "", "", e
	}
	file, e := f.root.OpenFile(stage.relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return "", "", e
	}
	_, writeErr := file.Write(body)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return "", "", writeErr
	}
	if syncErr != nil {
		return "", "", syncErr
	}
	if closeErr != nil {
		return "", "", closeErr
	}
	if e = f.DB.InspectCandidateResultStage(ctx, d, stage.key, stage.Hash); e != nil {
		return "", "", e
	}
	return stage.key, stage.Hash, nil
}
func (f *Store) ReadPendingCandidateResult(ctx context.Context, d store.DispatchRequest) ([]byte, error) {
	evidence, e := f.ReadPendingCandidateEvidence(ctx, d)
	if e != nil {
		return nil, e
	}
	defer evidence.Close()
	return evidence.Bytes(), nil
}

func (f *Store) CandidateOriginalBytes(e store.OriginalResultEvidence, s domain.ProductScope, task, attempt string) ([]byte, domain.MockResultMetadata, error) {
	r, ok := e.(*originalEvidence)
	if !ok || r == nil || r.owner != f || r.closed.Load() {
		return nil, domain.MockResultMetadata{}, ErrNotFound
	}
	if r.original != nil {
		r = r.original
	}
	if r.owner != f || r.closed.Load() || r.metadata.Identity.Kind != "CANDIDATE" || r.metadata.Identity.Scope.ProductScope != s || r.metadata.Identity.Scope.WorldInstanceID != "" || r.metadata.Identity.TaskID != task || r.metadata.Identity.AttemptID != attempt {
		return nil, domain.MockResultMetadata{}, ErrNotFound
	}
	return r.Bytes(), r.metadata, nil
}
func (f *Store) ReadPendingCandidateEvidence(ctx context.Context, d store.DispatchRequest) (store.OriginalResultEvidence, error) {
	if d.Slot == 0 {
		return f.readPendingCandidateEvidence(ctx, d)
	}
	original, e := f.DB.CandidateOriginalDispatch(ctx, d)
	if e != nil {
		return nil, e
	}
	ev, e := f.readPendingCandidateEvidence(ctx, original)
	if e != nil {
		return nil, fmt.Errorf("candidate original dependency: %w", e)
	}
	d, e = f.DB.WithCandidateOriginalEvidence(d, ev)
	if e != nil {
		ev.Close()
		return nil, e
	}
	out, e := f.readPendingCandidateEvidence(ctx, d)
	if e != nil {
		ev.Close()
		return nil, e
	}
	actual := out.(*originalEvidence)
	actual.original = ev.(*originalEvidence)
	closeOwn := actual.release
	actual.release = func() { closeOwn(); ev.Close() }
	return actual, nil
}
func (f *Store) readPendingCandidateEvidence(ctx context.Context, d store.DispatchRequest) (store.OriginalResultEvidence, error) {
	if e := f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	keep := false
	ctx, release, e := f.DB.EnterOperation(ctx)
	if e != nil {
		return nil, e
	}
	defer func() {
		if !keep {
			release()
		}
	}()
	var source store.CandidateSourceEvidence
	if d.Slot == 0 {
		source, e = f.DB.CandidateOriginalSourceEvidence(ctx, d)
		if e != nil {
			return nil, e
		}
		defer func() {
			if !keep {
				source.Close()
			}
		}()
	}
	m, e := f.DB.PendingCandidateBody(ctx, d)
	if e != nil {
		return nil, fmt.Errorf("candidate pending metadata: %w", e)
	}
	done, e := f.lockDerived(ctx, d.ProductScope, m.BodyStorage)
	if e != nil {
		return nil, e
	}
	defer func() {
		if !keep {
			done()
		}
	}()
	before, e := f.DB.PendingCandidateBody(ctx, d)
	if e != nil || before != m {
		return nil, ErrNotFound
	}
	body, e := f.readCandidateBytes(d.ProductScope, m)
	if e != nil {
		return nil, e
	}
	after, e := f.DB.PendingCandidateBody(ctx, d)
	if e != nil || after != m {
		return nil, ErrNotFound
	}
	if e = f.checkResultGovernance(ctx); e != nil {
		return nil, e
	}
	keep = true
	return &originalEvidence{owner: f, body: body, metadata: m, release: func() {
		done()
		if source != nil {
			source.Close()
		}
		release()
	}}, nil
}
func (f *Store) readCandidateBytes(scope domain.ProductScope, m domain.MockResultMetadata) ([]byte, error) {
	p, e := f.path(scope, m.BodyStorage, ".part")
	if e != nil {
		return nil, e
	}
	in, e := f.root.Open(p)
	if e != nil {
		return nil, e
	}
	defer in.Close()
	body, e := io.ReadAll(io.LimitReader(in, MaxFileBytes+1))
	h := sha256.Sum256(body)
	if e != nil || len(body) > MaxFileBytes || !bytes.Equal([]byte(hex.EncodeToString(h[:])), []byte(m.BodyHash)) {
		return nil, ErrNotFound
	}
	return body, nil
}
