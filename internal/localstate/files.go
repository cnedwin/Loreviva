// Package localstate supplies bounded local-file durability primitives shared by
// the independent governance journal and the single maintenance coordinator.
package localstate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrClosed = errors.New("independent local state unavailable")

type Root struct{ dir string }

func Open(dir string, create bool) (*Root, error) {
	abs, e := filepath.Abs(dir)
	if e != nil || strings.HasPrefix(abs, `\\`) {
		return nil, ErrClosed
	}
	if create {
		if e = os.MkdirAll(abs, 0700); e != nil {
			return nil, e
		}
	}
	resolved, e := filepath.EvalSymlinks(abs)
	if e != nil || !strings.EqualFold(filepath.Clean(abs), filepath.Clean(resolved)) {
		return nil, ErrClosed
	}
	if e = localVolume(abs); e != nil {
		return nil, e
	}
	return &Root{abs}, nil
}
func (r *Root) path(name string) (string, error) {
	switch name {
	case "journal.jsonl", "anchor.json", "coordinator.json", "state.lock", "operations.lock", "governance.lock":
	default:
		return "", ErrClosed
	}
	p := filepath.Join(r.dir, name)
	if i, e := os.Lstat(p); e == nil && (i.Mode()&os.ModeSymlink != 0 || !i.Mode().IsRegular()) {
		return "", ErrClosed
	}
	return p, nil
}
func (r *Root) Read(name string) ([]byte, error) {
	p, e := r.path(name)
	if e != nil {
		return nil, e
	}
	i, e := os.Stat(p)
	if e != nil {
		return nil, e
	}
	if i.Size() > 32<<20 {
		return nil, ErrClosed
	}
	return os.ReadFile(p)
}
func (r *Root) Create(name string, b []byte) error {
	if len(b) > 32<<20 {
		return ErrClosed
	}
	p, e := r.path(name)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if _, e = f.Write(b); e != nil {
		return e
	}
	return f.Sync()
}
func (r *Root) Append(name string, b []byte) error {
	p, e := r.path(name)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if info.Size() > 32<<20-int64(len(b)) {
		return ErrClosed
	}
	if _, e = f.Write(b); e != nil {
		return e
	}
	return f.Sync()
}
func (r *Root) Replace(name string, b []byte) error {
	if len(b) > 32<<20 {
		return ErrClosed
	}
	p, e := r.path(name)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(r.dir, ".pending-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return replace(tmp, p)
}

// Lock is an OS lock, never a PID or timeout-based ownership assertion.
func (r *Root) Lock(ctx context.Context, name string) (func(), error) {
	return r.lock(ctx, name, false)
}
func (r *Root) Shared(ctx context.Context) (func(), error) {
	return r.lock(ctx, "operations.lock", true)
}
func (r *Root) lock(ctx context.Context, name string, shared bool) (func(), error) {
	if name != "state.lock" && name != "operations.lock" && name != "governance.lock" {
		return nil, ErrClosed
	}
	p, e := r.path(name)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	for {
		ok, e := tryLock(f, shared)
		if e != nil {
			f.Close()
			return nil, e
		}
		if ok {
			return func() { unlock(f); f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
