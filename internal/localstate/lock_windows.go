//go:build windows

package localstate

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

var kernel = syscall.NewLazyDLL("kernel32.dll")
var lockFile = kernel.NewProc("LockFileEx")
var unlockFile = kernel.NewProc("UnlockFileEx")

func tryLock(f *os.File, shared bool) (bool, error) {
	var o syscall.Overlapped
	flags := uintptr(3)
	if shared {
		flags = 1
	}
	r, _, e := lockFile.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&o)))
	if r != 0 {
		return true, nil
	}
	if e == syscall.Errno(33) {
		return false, nil
	}
	return false, e
}
func unlock(f *os.File) {
	var o syscall.Overlapped
	unlockFile.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&o)))
}
func replace(old, new string) error {
	a, e := syscall.UTF16PtrFromString(old)
	if e != nil {
		return e
	}
	b, e := syscall.UTF16PtrFromString(new)
	if e != nil {
		return e
	}
	r, _, e := kernel.NewProc("MoveFileExW").Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), 0x1|0x8)
	if r == 0 {
		return e
	}
	return nil
}
func localVolume(dir string) error {
	p, e := syscall.UTF16PtrFromString(filepath.VolumeName(dir) + `\`)
	if e != nil {
		return e
	}
	r, _, _ := kernel.NewProc("GetDriveTypeW").Call(uintptr(unsafe.Pointer(p)))
	if r != 3 {
		return ErrClosed
	}
	return nil
}
