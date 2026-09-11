package main

import "testing"

// TestListenAddr pins the bind/port composition shared by the relay-router
// (--router-bind/--router-port) and the main mux's TCP front
// (--http-bind/--http-port): loopback-only by default (see docs/security
// note in README.md), explicit override reaches every interface, and an
// empty port (listener disabled) never picks up a bind address.
func TestListenAddr(t *testing.T) {
	cases := []struct {
		name string
		bind string
		port string
		want string
	}{
		{"default bind", "127.0.0.1", "8180", "127.0.0.1:8180"},
		{"explicit wildcard bind", "0.0.0.0", "8180", "0.0.0.0:8180"},
		{"disabled ignores bind", "127.0.0.1", "", ""},
		{"http front", "127.0.0.1", "8181", "127.0.0.1:8181"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listenAddr(tc.bind, tc.port)
			if got != tc.want {
				t.Errorf("listenAddr(%q, %q) = %q, want %q", tc.bind, tc.port, got, tc.want)
			}
		})
	}
}
