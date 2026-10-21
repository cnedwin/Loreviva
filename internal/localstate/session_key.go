package localstate

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// SessionHMACKey owns one installation key version. The caller must hold its
// real CONFIG permit outside SQL. Missing referenced material is never repaired.
// This exact filename does not broaden the general local-state file API.
func (r *Root) SessionHMACKey(installation string, create bool) ([32]byte, error) {
	var zero [32]byte
	if r == nil || installation == "" {
		return zero, ErrClosed
	}
	p := filepath.Join(r.dir, "session-hmac-v1.key")
	decode := func() ([32]byte, error) {
		i, e := os.Lstat(p)
		if e != nil {
			return zero, e
		}
		if !i.Mode().IsRegular() || i.Mode()&os.ModeSymlink != 0 || i.Size() > 1024 {
			return zero, ErrClosed
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return zero, e
		}
		var v struct {
			Installation string
			Version      int
			Key          []byte
		}
		if json.Unmarshal(b, &v) != nil || v.Installation != installation || v.Version != 1 || len(v.Key) != 32 {
			return zero, ErrClosed
		}
		var key [32]byte
		copy(key[:], v.Key)
		return key, nil
	}
	k, e := decode()
	if e == nil {
		return k, nil
	}
	if !errors.Is(e, os.ErrNotExist) || !create {
		return zero, ErrClosed
	}
	if _, e = rand.Read(k[:]); e != nil {
		return zero, e
	}
	b, e := json.Marshal(struct {
		Installation string
		Version      int
		Key          []byte
	}{installation, 1, k[:]})
	if e != nil {
		return zero, e
	}
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(e, os.ErrExist) {
		return decode()
	}
	if e != nil {
		return zero, e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return zero, e
	}
	if ce != nil {
		return zero, ce
	}
	return decode()
}
