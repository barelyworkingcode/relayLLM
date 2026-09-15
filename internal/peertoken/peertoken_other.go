//go:build !darwin

package peertoken

// FromFD has no implementation outside darwin: relay and relayLLM's launch
// identity model is macOS-only, and an identity nothing can read must never
// match.
func FromFD(int) (Token, error) { return Token{}, ErrUnsupported }
