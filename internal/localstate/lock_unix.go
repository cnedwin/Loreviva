//go:build linux || darwin

package localstate

import (
	"os"
	"path/filepath"
	"syscall"
)

func tryLock(f *os.File, shared bool) (bool, error) {
	flags := syscall.LOCK_EX | syscall.LOCK_NB
	if shared {
		flags = syscall.LOCK_SH | syscall.LOCK_NB
	}
	e := syscall.Flock(int(f.Fd()), flags)
	if e == syscall.EWOULDBLOCK {
		return false, nil
	}
	return e == nil, e
}
func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func replace(old, new string) error {
	if e := os.Rename(old, new); e != nil {
		return e
	}
	f, e := os.Open(filepath.Dir(new))
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// R0 production support is the verified Windows local fixed-volume path.
func localVolume(string) error { return ErrClosed }
