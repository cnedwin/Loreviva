package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"loreviva.local/loreviva/internal/localstate"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// All authority stays on original objects. Exported facts and file names are
// data, never constructors for a claim, evidence, or a finalization reservation.
type RuntimeMediaStage struct {
	self                                   *RuntimeMediaStage
	owner                                  *Store
	claim                                  *store.RuntimeMediaClaim
	plan                                   store.RuntimeMediaStagePlan
	relative, original                     string
	originalInfo                           os.FileInfo
	handle                                 *os.File
	info                                   os.FileInfo
	unlock                                 func()
	mu                                     sync.Mutex
	closed, closeRequested, pinned, sealed bool
	evidence                               *RuntimeMediaEvidence
	fileCalls, bytesRead                   atomic.Uint64
}

// Observation is read-only accounting, never a file pin or authorization.
// FileCalls counts explicit owned file API calls, not operating-system syscalls.
type RuntimeMediaIOObservation struct{ FileCalls, BytesRead uint64 }

func (s *RuntimeMediaStage) ObserveIO() (RuntimeMediaIOObservation, error) {
	if s == nil || s.self != s {
		return RuntimeMediaIOObservation{}, store.ErrDenied
	}
	return RuntimeMediaIOObservation{s.fileCalls.Load(), s.bytesRead.Load()}, nil
}

type runtimeMediaReader struct{ stage *RuntimeMediaStage }

func (r runtimeMediaReader) Read(p []byte) (int, error) {
	r.stage.fileCalls.Add(1)
	n, e := r.stage.handle.Read(p)
	r.stage.bytesRead.Add(uint64(n))
	return n, e
}

type RuntimeMediaEvidence struct {
	self  *RuntimeMediaEvidence
	stage *RuntimeMediaStage
	facts store.RuntimeMediaFileFacts
}
type RuntimeMediaFinalPin struct {
	self     *RuntimeMediaFinalPin
	evidence *RuntimeMediaEvidence
	live     bool
}

func (f *Store) RuntimeMediaFileStore() *store.Store {
	if f == nil {
		return nil
	}
	return f.DB
}
func (e *RuntimeMediaEvidence) RuntimeMediaEvidenceStore() *store.Store {
	if e == nil || e.self != e || e.stage == nil {
		return nil
	}
	return e.stage.owner.DB
}
func (p *RuntimeMediaFinalPin) RuntimeMediaFinalPinStore() *store.Store {
	if p == nil || p.self != p || p.evidence == nil {
		return nil
	}
	return p.evidence.RuntimeMediaEvidenceStore()
}

// A has no textual input. This deterministic PCM tone is a synthetic artifact,
// not speech, a model call, or a representation of the approved private body.
func runtimeTone() []byte {
	b := make([]byte, 160044)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:8], 160036)
	copy(b[8:12], "WAVE")
	copy(b[12:16], "fmt ")
	binary.LittleEndian.PutUint32(b[16:20], 16)
	binary.LittleEndian.PutUint16(b[20:22], 1)
	binary.LittleEndian.PutUint16(b[22:24], 1)
	binary.LittleEndian.PutUint32(b[24:28], 16000)
	binary.LittleEndian.PutUint32(b[28:32], 32000)
	binary.LittleEndian.PutUint16(b[32:34], 2)
	binary.LittleEndian.PutUint16(b[34:36], 16)
	copy(b[36:40], "data")
	binary.LittleEndian.PutUint32(b[40:44], 160000)
	for i := 0; i < 80000; i++ {
		v := int16(4000)
		if (i/40)%2 != 0 {
			v = -v
		}
		binary.LittleEndian.PutUint16(b[44+i*2:], uint16(v))
	}
	return b
}
func (f *Store) StageRuntimeTone(ctx context.Context, c *store.RuntimeMediaClaim) (out *RuntimeMediaStage, err error) {
	// This short SQL check must finish before the first Files metadata operation.
	plan, e := f.DB.RuntimeMediaStagePlan(ctx, f, c)
	if e != nil {
		return nil, e
	}
	dir, e := f.productDir(plan.Scope)
	if e != nil {
		return nil, e
	}
	// Media identities are full SHA256 hex, distinct from ordinary upload's
	// 16-byte storage keys. Reserve this dedicated namespace with its full key.
	id, e := hex.DecodeString(plan.StageStorage)
	if e != nil || len(id) != 32 || hex.EncodeToString(id) != plan.StageStorage {
		return nil, ErrMetadata
	}
	reservation, e := localstate.Open(filepath.Join(f.RootPath, dir, "runtime-media-locks", plan.StageStorage), true)
	if e != nil {
		return nil, e
	}
	unlock, e := reservation.Lock(ctx, "operations.lock")
	if e != nil {
		return nil, e
	}
	// The reservation serializes the actual producer with terminal purge. Recheck
	// after acquiring it, close SQL, then begin native metadata/write operations.
	latest, err := f.DB.RuntimeMediaStagePlan(ctx, f, c)
	if err != nil || latest != plan {
		unlock()
		if err != nil {
			return nil, err
		}
		return nil, store.ErrDenied
	}
	s := &RuntimeMediaStage{owner: f, claim: c, plan: plan, unlock: unlock}
	s.self = s
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	for _, ext := range []string{".part", ".blob"} {
		original, e := f.path(plan.Scope, plan.OriginalStorage, ext)
		if e != nil {
			return nil, e
		}
		s.fileCalls.Add(1)
		info, e := f.root.Lstat(original)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > MaxFileBytes {
			return nil, ErrNotFound
		}
		// The original has no stored byte length. This is a metadata observation,
		// not a historical hash or equal-size tamper claim; no body is opened.
		s.original = original
		s.originalInfo = info
		break
	}
	if s.originalInfo == nil {
		return nil, ErrNotFound
	}
	mediaDir := dir + "/runtime-media"
	s.fileCalls.Add(1)
	if e = f.root.Mkdir(mediaDir, 0700); e != nil && !os.IsExist(e) {
		return nil, e
	}
	s.fileCalls.Add(1)
	info, e := f.root.Lstat(mediaDir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrMetadata
	}
	s.relative = mediaDir + "/" + plan.StageStorage + ".wav"
	s.fileCalls.Add(1)
	w, e := f.root.OpenFile(s.relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	body := runtimeTone()
	s.fileCalls.Add(1)
	n, we := w.Write(body)
	s.fileCalls.Add(1)
	se := w.Sync()
	s.fileCalls.Add(1)
	ce := w.Close()
	if we != nil {
		return nil, we
	}
	if se != nil {
		return nil, se
	}
	if ce != nil {
		return nil, ce
	}
	if n != len(body) {
		return nil, io.ErrShortWrite
	}
	s.fileCalls.Add(1)
	s.handle, e = f.root.Open(s.relative)
	if e != nil {
		return nil, e
	}
	s.fileCalls.Add(1)
	s.info, e = s.handle.Stat()
	if e != nil {
		return nil, e
	}
	return s, nil
}
func (f *Store) SealRuntimeMediaStage(ctx context.Context, c *store.RuntimeMediaClaim, s *RuntimeMediaStage) (*RuntimeMediaEvidence, error) {
	if s == nil || s.self != s || s.owner != f || s.claim != c {
		return nil, store.ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.closeRequested || s.pinned || s.sealed || s.handle == nil {
		return nil, store.ErrDenied
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s.fileCalls.Add(1)
	original, e := f.root.Lstat(s.original)
	if e != nil || !original.Mode().IsRegular() || original.Mode()&os.ModeSymlink != 0 || !os.SameFile(original, s.originalInfo) || original.Size() != s.originalInfo.Size() {
		return nil, ErrNotFound
	}
	s.fileCalls.Add(1)
	current, e := f.root.Lstat(s.relative)
	if e != nil || !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, s.info) || current.Size() != 160044 {
		return nil, ErrMetadata
	}
	s.fileCalls.Add(1)
	if _, e = s.handle.Seek(0, io.SeekStart); e != nil {
		return nil, e
	}
	body, e := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: runtimeMediaReader{s}}, 160045))
	if e != nil || !bytes.Equal(body, runtimeTone()) {
		return nil, ErrMetadata
	}
	s.fileCalls.Add(1)
	after, e := s.handle.Stat()
	if e != nil || !os.SameFile(after, s.info) || after.Size() != 160044 {
		return nil, ErrMetadata
	}
	hash := sha256.Sum256(body)
	ev := &RuntimeMediaEvidence{stage: s, facts: store.RuntimeMediaFileFacts{Asset: s.plan.Asset, Storage: s.plan.StageStorage, SHA256: hex.EncodeToString(hash[:]), NativeIdentity: "same-file:" + s.plan.StageStorage, Bytes: 160044, Fence: s.plan.Fence}}
	ev.self = ev
	s.evidence = ev
	s.sealed = true
	return ev, nil
}
func (f *Store) AcquireMediaFinalEvidence(c *store.RuntimeMediaClaim, e *RuntimeMediaEvidence) (*RuntimeMediaFinalPin, error) {
	if e == nil || e.self != e || e.stage == nil {
		return nil, store.ErrDenied
	}
	s := e.stage
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.self != s || s.owner != f || s.claim != c || s.evidence != e || !s.sealed || s.pinned || s.closed || s.closeRequested {
		return nil, store.ErrDenied
	}
	p := &RuntimeMediaFinalPin{evidence: e, live: true}
	p.self = p
	s.pinned = true
	return p, nil
}

// P2 is pure memory. A reentrant Close only requests closure, invalidating the
// commit check while leaving the actual pinned native handle for P3 to return.
func (f *Store) CheckMediaFinalEvidencePure(c *store.RuntimeMediaClaim, v store.RuntimeMediaFinalPin) (store.RuntimeMediaFileFacts, error) {
	p, ok := v.(*RuntimeMediaFinalPin)
	if !ok || p == nil || p.self != p || p.evidence == nil || p.evidence.self != p.evidence {
		return store.RuntimeMediaFileFacts{}, store.ErrDenied
	}
	e := p.evidence
	s := e.stage
	if s == nil {
		return store.RuntimeMediaFileFacts{}, store.ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.self != s || s.owner != f || s.claim != c || s.evidence != e || !s.sealed || !s.pinned || !p.live || s.closed || s.closeRequested || s.handle == nil {
		return store.RuntimeMediaFileFacts{}, store.ErrDenied
	}
	return e.facts, nil
}
func (f *Store) CompleteMediaFinalEvidence(ctx context.Context, c *store.RuntimeMediaClaim, p *RuntimeMediaFinalPin) error {
	if p == nil || p.self != p || p.evidence == nil {
		return store.ErrDenied
	}
	s := p.evidence.stage
	if s == nil || s.self != s || s.owner != f || s.claim != c {
		return store.ErrDenied
	}
	// Reconciliation owns its short SQL; it returns before any native close.
	_, e := f.DB.MediaFinalOutcome(ctx, f, c)
	s.mu.Lock()
	p.live = false
	s.pinned = false
	s.mu.Unlock()
	ce := s.Close()
	if e != nil {
		return e
	}
	return ce
}
func (s *RuntimeMediaStage) Close() error {
	if s == nil {
		return nil
	}
	if s.self != s {
		return store.ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeRequested = true
	if s.closed || s.pinned {
		return nil
	}
	s.closed = true
	var e error
	if s.handle != nil {
		s.fileCalls.Add(1)
		e = s.handle.Close()
		s.handle = nil
	}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	// No destructive inference from a caller error. Unknown and unindexed stages
	// remain available to the owning reconciliation/cleanup protocol.
	return e
}

// Every bounded chunk verifies the actual whole small WAV outside SQL. A path
// or descriptor alone is not readable authority; only the original read seal is.
func (f *Store) ReadRuntimeMediaBytes(ctx context.Context, r *store.RuntimeMediaRead, offset, length uint64) (out []byte, err error) {
	p, e := r.FilePlan(f)
	if e != nil {
		return nil, e
	}
	if length == 0 || length > 16384 || offset >= p.Bytes || length > p.Bytes-offset || p.Bytes != 160044 {
		return nil, ErrMetadata
	}
	id, e := hex.DecodeString(p.Storage)
	if e != nil || len(id) != 32 || hex.EncodeToString(id) != p.Storage {
		return nil, ErrMetadata
	}
	dir, e := f.productDir(p.Scope)
	if e != nil {
		return nil, e
	}
	mediaDir := dir + "/runtime-media"
	info, e := f.root.Lstat(mediaDir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, ErrNotFound
	}
	reservation, e := localstate.Open(filepath.Join(f.RootPath, dir, "runtime-media-locks", p.Storage), false)
	if e != nil {
		return nil, e
	}
	unlock, e := reservation.Lock(ctx, "operations.lock")
	if e != nil {
		return nil, e
	}
	defer unlock()
	name := mediaDir + "/" + p.Storage + ".wav"
	before, e := f.root.Lstat(name)
	if e != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != int64(p.Bytes) {
		return nil, ErrNotFound
	}
	h, e := f.root.Open(name)
	if e != nil {
		return nil, e
	}
	defer func() {
		if ce := h.Close(); err == nil && ce != nil {
			out = nil
			err = ce
		}
	}()
	opened, e := h.Stat()
	if e != nil || !os.SameFile(before, opened) {
		return nil, ErrMetadata
	}
	body, e := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: h}, 160045))
	if e != nil || len(body) != 160044 || !bytes.Equal(body, runtimeTone()) {
		return nil, ErrMetadata
	}
	hash := sha256.Sum256(body)
	if hex.EncodeToString(hash[:]) != strings.ToLower(p.SHA256) {
		return nil, ErrMetadata
	}
	after, e := f.root.Lstat(name)
	if e != nil || !os.SameFile(after, opened) || after.Size() != opened.Size() {
		return nil, ErrMetadata
	}
	return append([]byte(nil), body[offset:offset+length]...), nil
}

// Media purge is disjoint from upload cleanup: only the original indexed,
// accepted 64-hex slots in a Store-minted terminal claim can reach native IO.
func (f *Store) CleanupRuntimeMedia(ctx context.Context, c *store.RuntimeMediaPurgeClaim) (bool, error) {
	plan, e := f.DB.RecheckRuntimeMediaPurge(ctx, f, c)
	if e != nil {
		return false, e
	}
	dir, e := f.productDir(plan.Scope)
	if e != nil {
		return false, e
	}
	removed := false
	for _, slot := range plan.Slots {
		id, e := hex.DecodeString(slot)
		if e != nil || len(id) != 32 || hex.EncodeToString(id) != slot {
			return removed, ErrMetadata
		}
		if e = ctx.Err(); e != nil {
			return removed, e
		}
		reservation, e := localstate.Open(filepath.Join(f.RootPath, dir, "runtime-media-locks", slot), true)
		if e != nil {
			return removed, e
		}
		unlock, e := reservation.Lock(ctx, "operations.lock")
		if e != nil {
			return removed, e
		}
		did, err := func() (bool, error) {
			defer unlock()
			// The shared reservation is held, but SQL finishes before Lstat/Open/Remove.
			current, e := f.DB.RecheckRuntimeMediaPurge(ctx, f, c)
			if e != nil {
				return false, e
			}
			if current.Scope != plan.Scope || current.Asset != plan.Asset || len(current.Slots) != len(plan.Slots) {
				return false, store.ErrDenied
			}
			for i := range current.Slots {
				if current.Slots[i] != plan.Slots[i] {
					return false, store.ErrDenied
				}
			}
			mediaDir := dir + "/runtime-media"
			info, e := f.root.Lstat(mediaDir)
			if os.IsNotExist(e) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return false, ErrMetadata
			}
			name := mediaDir + "/" + slot + ".wav"
			before, e := f.root.Lstat(name)
			if os.IsNotExist(e) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
				return false, ErrMetadata
			}
			h, e := f.root.Open(name)
			if e != nil {
				return false, e
			}
			opened, se := h.Stat()
			ce := h.Close()
			if se != nil {
				return false, se
			}
			if ce != nil {
				return false, ce
			}
			after, e := f.root.Lstat(name)
			if e != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(opened, after) || before.Size() != after.Size() {
				return false, ErrMetadata
			}
			if e = ctx.Err(); e != nil {
				return false, e
			}
			if e = f.root.Remove(name); e != nil {
				return false, e
			}
			return true, nil
		}()
		removed = removed || did
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}
