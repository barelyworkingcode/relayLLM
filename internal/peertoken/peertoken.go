// Package peertoken reads the kernel audit token of the process at the far
// end of a Unix-domain socket (getsockopt SOL_LOCAL/LOCAL_PEERTOKEN).
//
// relayLLM uses this twice (C9, plan-broker-and-sessions.md §2): on the
// connecting end of its Hello dial to relay.sock, to capture relay's own
// (pid, pidversion) — spike SP1 confirms the connecting end of a Unix
// stream socket sees the ACCEPTING side's token, not its own, symmetric with
// the well-known accepting-side use for LOCAL_PEERCRED — and on the
// accepting end of router.sock, the ordinary direction, to admit only that
// captured identity.
package peertoken

import (
	"encoding/binary"
	"errors"
	"net"
)

// Size is sizeof(audit_token_t) on darwin.
const Size = 32

// ErrUnsupported is returned for a connection that has no peer audit token:
// anything other than a Unix-domain socket, or any platform but darwin.
var ErrUnsupported = errors.New("peertoken: no peer audit token for this connection")

// Token is a peer's audit token. PID and PIDVersion together name exactly one
// process for the life of the system: a recycled pid gets a new pidversion.
type Token struct {
	raw [Size]byte
}

// FromRaw builds a Token from the bytes of an audit_token_t.
func FromRaw(raw [Size]byte) Token { return Token{raw: raw} }

// Raw returns the audit_token_t bytes.
func (t Token) Raw() [Size]byte { return t.raw }

// PID is audit_token_to_pid: val[5], bytes 20-23.
func (t Token) PID() int32 { return int32(binary.LittleEndian.Uint32(t.raw[20:24])) }

// PIDVersion is audit_token_to_pidversion: val[7], bytes 28-31.
func (t Token) PIDVersion() int32 { return int32(binary.LittleEndian.Uint32(t.raw[28:32])) }

// Valid reports whether the token names a real process. The zero Token is
// what every failed read resolves to, and it must never match anything.
func (t Token) Valid() bool { return t.PID() > 0 }

// Process is the (pid, pidversion) pair that identifies one process.
type Process struct {
	PID        int32
	PIDVersion int32
}

// Process returns the token's (pid, pidversion) pair.
func (t Token) Process() Process { return Process{PID: t.PID(), PIDVersion: t.PIDVersion()} }

// ForProcessForTest builds a token carrying only a pid and pidversion, for
// tests that exercise identity lookups without a real peer.
func ForProcessForTest(pid, pidversion int32) Token {
	var raw [Size]byte
	binary.LittleEndian.PutUint32(raw[20:24], uint32(pid))
	binary.LittleEndian.PutUint32(raw[28:32], uint32(pidversion))
	return Token{raw: raw}
}

// FromConn reads the peer audit token of conn. Any failure, including a conn
// that is not a *net.UnixConn, returns an error and the zero Token.
func FromConn(conn net.Conn) (Token, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return Token{}, ErrUnsupported
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return Token{}, err
	}
	var tok Token
	var readErr error
	if ctrlErr := raw.Control(func(fd uintptr) {
		tok, readErr = FromFD(int(fd))
	}); ctrlErr != nil {
		return Token{}, ctrlErr
	}
	if readErr != nil {
		return Token{}, readErr
	}
	return tok, nil
}
