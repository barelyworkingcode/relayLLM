//go:build darwin

package peertoken

import (
	"fmt"
	"syscall"
	"unsafe"
)

// From <sys/un.h>; not exported by the syscall package.
const (
	solLocal       = 0     // SOL_LOCAL
	localPeerToken = 0x006 // LOCAL_PEERTOKEN
)

// FromFD reads the peer audit token of the connected Unix socket fd.
func FromFD(fd int) (Token, error) {
	var raw [Size]byte
	size := uint32(Size)
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT,
		uintptr(fd), solLocal, localPeerToken,
		uintptr(unsafe.Pointer(&raw[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 {
		return Token{}, fmt.Errorf("peertoken: getsockopt LOCAL_PEERTOKEN: %w", errno)
	}
	// This is deliberate: a short read would leave the pid/pidversion bytes
	// zero-filled and silently name no process, or worse, a partial one.
	if size != Size {
		return Token{}, fmt.Errorf("peertoken: LOCAL_PEERTOKEN returned %d bytes, want %d", size, Size)
	}
	return Token{raw: raw}, nil
}
