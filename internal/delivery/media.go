package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Each producer is the actual once-bound delivery owner, under its real shared
// admission. Claims never resurrect a TURN issuer or mint a CLIENT body right.
func (s *Service) ProduceOneSyntheticMedia(ctx context.Context) (bool, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return false, e
	}
	defer done()
	claim, e := s.store.ClaimSyntheticMedia(ctx, s)
	if errors.Is(e, store.ErrNoTask) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	defer claim.Close()
	stage, e := s.files.StageRuntimeTone(ctx, claim)
	if e != nil {
		return true, e
	}
	defer stage.Close()
	evidence, e := s.files.SealRuntimeMediaStage(ctx, claim, stage)
	if e != nil {
		return true, e
	}
	pin, e := s.files.AcquireMediaFinalEvidence(claim, evidence)
	if e != nil {
		return true, e
	}
	finished := s.store.FinishSyntheticMedia(ctx, s, claim, pin)
	returned := s.files.CompleteMediaFinalEvidence(context.WithoutCancel(ctx), claim, pin)
	if finished != nil {
		return true, finished
	}
	return true, returned
}

// These local owning operations retain the original COMMIT identity; they do
// not install a caller, issue a send lease, or grant private body access.
func (s *Service) ReconcileSyntheticMedia(ctx context.Context, asset string) (bool, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return false, e
	}
	defer done()
	return s.store.ReconcileSyntheticMedia(ctx, s, asset)
}
func (s *Service) CleanupSyntheticMedia(ctx context.Context, asset string) (bool, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return false, e
	}
	defer done()
	claim, e := s.store.ClaimRuntimeMediaPurge(ctx, s, s.files, asset)
	if e != nil {
		return false, e
	}
	defer claim.Close()
	return s.files.CleanupRuntimeMedia(ctx, claim)
}

type MediaRange struct{ Spec string }

var ErrMediaRange = errors.New("runtime media range invalid")

type AuthorizedMedia struct {
	self                 *AuthorizedMedia
	service              *Service
	asset                string
	initial              *store.RuntimeMediaRead
	plan                 store.RuntimeMediaReadPlan
	start, end, position uint64
	partial              bool
	closed               atomic.Bool
	mu                   sync.Mutex
}

func mediaRange(spec string, size uint64) (uint64, uint64, bool, error) {
	if spec == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(spec, "bytes=") || strings.ContainsAny(spec, " ,\t\r\n") {
		return 0, 0, false, ErrMediaRange
	}
	p := strings.Split(strings.TrimPrefix(spec, "bytes="), "-")
	if len(p) != 2 || (p[0] == "" && p[1] == "") {
		return 0, 0, false, ErrMediaRange
	}
	parse := func(s string) (uint64, error) {
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, ErrMediaRange
			}
		}
		return strconv.ParseUint(s, 10, 64)
	}
	if p[0] == "" {
		n, e := parse(p[1])
		if e != nil || n == 0 {
			return 0, 0, false, ErrMediaRange
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}
	start, e := parse(p[0])
	if e != nil || start >= size {
		return 0, 0, false, ErrMediaRange
	}
	end := size - 1
	if p[1] != "" {
		end, e = parse(p[1])
		if e != nil || end < start {
			return 0, 0, false, ErrMediaRange
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}

func (s *Service) ReadRuntimeMedia(ctx context.Context, asset string, requested MediaRange) (*AuthorizedMedia, error) {
	ctx, done, e := s.enter(ctx)
	if e != nil {
		return nil, e
	}
	defer done()
	r, e := s.store.OpenRuntimeMediaRead(ctx, s, asset)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	p, e := r.FilePlan(s.files)
	if e != nil {
		return nil, e
	}
	start, end, partial, e := mediaRange(requested.Spec, p.Bytes)
	if e != nil {
		return nil, e
	}
	if e = r.Finish(ctx); e != nil {
		return nil, e
	}
	m := &AuthorizedMedia{service: s, asset: asset, initial: r, plan: p, start: start, end: end, position: start, partial: partial}
	m.self = m
	return m, nil
}

func (m *AuthorizedMedia) ResponseRange() (int, string, uint64, error) {
	if m == nil || m.self != m || m.closed.Load() {
		return 0, "", 0, store.ErrDenied
	}
	if m.partial {
		return 206, fmt.Sprintf("bytes %d-%d/%d", m.start, m.end, m.plan.Bytes), m.end - m.start + 1, nil
	}
	return 200, "", m.plan.Bytes, nil
}

func (m *AuthorizedMedia) writeChunk(ctx context.Context, maxBytes uint32, write func([]byte) error) error {
	if m == nil || m.self != m {
		return store.ErrDenied
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() || maxBytes == 0 || maxBytes > 16384 {
		return store.ErrDenied
	}
	if m.position > m.end {
		return io.EOF
	}
	ctx, done, e := m.service.enter(ctx)
	if e != nil {
		return e
	}
	defer done()
	r, e := m.service.store.OpenRuntimeMediaRead(ctx, m.service, m.asset)
	if e != nil {
		return e
	}
	defer r.Close()
	if e = r.MatchFinished(m.initial); e != nil {
		return e
	}
	n := uint64(maxBytes)
	if remaining := m.end - m.position + 1; n > remaining {
		n = remaining
	}
	body, e := m.service.files.ReadRuntimeMediaBytes(ctx, r, m.position, n)
	if e != nil {
		return e
	}
	if e = r.Finish(ctx); e != nil {
		return e
	}
	// All SQL and file handles have returned; the real shared parent alone spans
	// this bounded transport write. The next chunk obtains a new current stage.
	if m.closed.Load() {
		return store.ErrDenied
	}
	if e = boundedWrite(ctx, write, body); e != nil {
		return e
	}
	m.position += n
	return nil
}

func (m *AuthorizedMedia) ReadChunk(ctx context.Context, maxBytes uint32) ([]byte, error) {
	var out []byte
	e := m.writeChunk(ctx, maxBytes, func(b []byte) error { out = append([]byte(nil), b...); return nil })
	return out, e
}
func (m *AuthorizedMedia) WriteChunks(ctx context.Context, write func([]byte) error) error {
	for {
		e := m.writeChunk(ctx, 16384, write)
		if errors.Is(e, io.EOF) {
			return nil
		}
		if e != nil {
			return e
		}
	}
}
func (m *AuthorizedMedia) Close() error {
	if m == nil {
		return nil
	}
	if m.self != m {
		return store.ErrDenied
	}
	// Close revokes future chunks without waiting on a reentrant transport
	// callback. The active bounded stage still returns its own actual handles.
	m.closed.Store(true)
	return nil
}
