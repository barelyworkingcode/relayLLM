package main

import "testing"

// TestRouterListenAddr pins the relay-router's bind/port composition:
// loopback-only by default (see docs/security note in README.md), explicit
// override reaches every interface, and an empty port (router disabled)
// never picks up a bind address.
func TestRouterListenAddr(t *testing.T) {
	cases := []struct {
		name string
		bind string
		port string
		want string
	}{
		{"default bind", "127.0.0.1", "8180", "127.0.0.1:8180"},
		{"explicit wildcard bind", "0.0.0.0", "8180", "0.0.0.0:8180"},
		{"router disabled ignores bind", "127.0.0.1", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routerListenAddr(tc.bind, tc.port)
			if got != tc.want {
				t.Errorf("routerListenAddr(%q, %q) = %q, want %q", tc.bind, tc.port, got, tc.want)
			}
		})
	}
}
