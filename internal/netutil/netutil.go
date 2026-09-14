// Package netutil holds the bind-address parsing and multi-listener helpers
// shared by relayLLM's --router-bind/--router-port and --http-bind/--http-port
// listener groups, plus the loopback-host check used by endpoint TLS policy
// and the pi-overlay host resolution.
package netutil

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
)

// ParseBindList splits a --router-bind/--http-bind value into its component
// addresses (comma-separated, brackets stripped, blanks and duplicates
// dropped in order, since "127.0.0.1,127.0.0.1" is a guaranteed
// self-inflicted EADDRINUSE, not two distinct interfaces.
//
// A value that trims to nothing at all still returns one empty-string
// element — that's today's wildcard bind (net.JoinHostPort("", port) binds
// every interface), not "listen nowhere".
func ParseBindList(s string) []string {
	parts := strings.Split(s, ",")
	binds := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimPrefix(p, "[")
		p = strings.TrimSuffix(p, "]")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		binds = append(binds, p)
	}
	if len(binds) == 0 {
		return []string{""}
	}
	return binds
}

// ListenAddrs composes one TCP listen address per bind (already split by
// ParseBindList) for a --router-bind/--router-port or
// --http-bind/--http-port pair. An empty port means that listener is
// disabled, so binds are irrelevant and deliberately not consulted in that
// case; every caller treats a nil result as "don't listen".
func ListenAddrs(binds []string, port string) []string {
	if port == "" {
		return nil
	}
	addrs := make([]string, len(binds))
	for i, b := range binds {
		addrs[i] = net.JoinHostPort(b, port)
	}
	return addrs
}

// ListenAll binds every address in addrs on a best-effort basis: an address
// that fails to bind (already in use, no such interface, …) is logged and
// skipped rather than aborting every other bind. This is deliberate, not an
// oversight — a configured address is not guaranteed locally assignable in
// every deployment (a gateway IP, say, that some environments can bind and
// others can't), and requiring every one of several binds to succeed would
// make that address's mere presence in the list a single point of failure
// for the whole listener. what tags the log line (e.g. "relay router",
// "http front") so an operator can tell which listener a failure belongs to
// when both are configured with overlapping addresses.
//
// Only when NOT ONE address could be bound does this return an error — a
// listener that binds nothing at all is exactly the original single-bind
// failure case and must still surface as a startup failure, the same as
// before this function accepted more than one address.
func ListenAll(addrs []string, what string) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("failed to bind listener; continuing with any other configured addresses",
				"component", what, "addr", addr, "error", err)
			continue
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return nil, fmt.Errorf("%s: no listener could be bound (tried %v)", what, addrs)
	}
	return lns, nil
}

// IsLoopbackHost reports whether host (a URL's Hostname(), so brackets
// already stripped from a literal IPv6 address) refers to this machine.
// "localhost" is treated as loopback by name — it isn't guaranteed to
// resolve to 127.0.0.1/::1 in every resolver configuration, but nothing on
// this hop does a DNS lookup to find out, and the whole point of the
// loopback carve-out is "this can only ever be the same box regardless of
// resolver," which the literal name already guarantees in practice for the
// deployments this code runs on.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
