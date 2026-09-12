package netutil

import (
	"reflect"
	"testing"
)

// TestParseBindList pins the --router-bind/--http-bind comma-splitting
// contract: whitespace and IPv6 brackets are stripped, duplicates collapse,
// and an entirely empty value still yields the single-element wildcard list
// rather than "no binds at all".
func TestParseBindList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"single loopback", "127.0.0.1", []string{"127.0.0.1"}},
		{"two interfaces", "127.0.0.1,192.168.64.1", []string{"127.0.0.1", "192.168.64.1"}},
		{"whitespace around entries", "127.0.0.1, 192.168.64.1 ", []string{"127.0.0.1", "192.168.64.1"}},
		{"ipv6 brackets stripped", "[::1]", []string{"::1"}},
		{"mixed v4 and bracketed v6", "127.0.0.1,[::1]", []string{"127.0.0.1", "::1"}},
		{"exact duplicates collapse", "127.0.0.1,127.0.0.1", []string{"127.0.0.1"}},
		{"empty string is the wildcard bind", "", []string{""}},
		{"blank entries dropped", "127.0.0.1,,192.168.64.1", []string{"127.0.0.1", "192.168.64.1"}},
		{"entirely whitespace is the wildcard bind", "  ", []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseBindList(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseBindList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestListenAddrs pins the bind/port composition shared by the relay-router
// (--router-bind/--router-port) and the main mux's TCP front
// (--http-bind/--http-port): loopback-only by default (see docs/security
// note in README.md), one address per configured bind, and an empty port
// (listener disabled) never picks up any bind address at all.
func TestListenAddrs(t *testing.T) {
	cases := []struct {
		name  string
		binds []string
		port  string
		want  []string
	}{
		{"default bind", []string{"127.0.0.1"}, "8180", []string{"127.0.0.1:8180"}},
		{"explicit wildcard bind", []string{"0.0.0.0"}, "8180", []string{"0.0.0.0:8180"}},
		{"disabled ignores binds", []string{"127.0.0.1"}, "", nil},
		{"http front", []string{"127.0.0.1"}, "8181", []string{"127.0.0.1:8181"}},
		{"multiple interfaces", []string{"127.0.0.1", "192.168.64.1"}, "8180",
			[]string{"127.0.0.1:8180", "192.168.64.1:8180"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ListenAddrs(tc.binds, tc.port)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ListenAddrs(%v, %q) = %v, want %v", tc.binds, tc.port, got, tc.want)
			}
		})
	}
}

// TestFirstNonLoopbackBind pins the fail-closed loopback scan: any single
// non-loopback bind in the list is the offender that gets named, and
// wildcards count as non-loopback since they accept off-box connections
// too.
func TestFirstNonLoopbackBind(t *testing.T) {
	cases := []struct {
		name   string
		binds  []string
		want   string
		wantOK bool
	}{
		{"all loopback", []string{"127.0.0.1", "::1", "localhost"}, "", false},
		{"one lan address", []string{"127.0.0.1", "192.168.64.1"}, "192.168.64.1", true},
		{"wildcard counts as non-loopback", []string{"0.0.0.0"}, "0.0.0.0", true},
		{"empty list", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotHost, gotOK := FirstNonLoopbackBind(tc.binds)
			if gotHost != tc.want || gotOK != tc.wantOK {
				t.Errorf("FirstNonLoopbackBind(%v) = (%q, %t), want (%q, %t)", tc.binds, gotHost, gotOK, tc.want, tc.wantOK)
			}
		})
	}
}

// TestListenAll_PartialFailureSkipsBadAddressAndKeepsTheRest covers the
// best-effort bind contract: a later address failing (already in use) must
// not take down an earlier, already-bound listener, and must not itself be
// an error — only a total failure (every address rejected) is.
func TestListenAll_PartialFailureSkipsBadAddressAndKeepsTheRest(t *testing.T) {
	seed, err := ListenAll([]string{"127.0.0.1:0"}, "test")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	busyAddr := seed[0].Addr().String()
	defer seed[0].Close()

	lns, err := ListenAll([]string{"127.0.0.1:0", busyAddr}, "test")
	if err != nil {
		t.Fatalf("listenAll with one bad address: %v", err)
	}
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	if len(lns) != 1 {
		t.Fatalf("got %d listeners, want 1 (the good address only)", len(lns))
	}
	if lns[0].Addr().String() == busyAddr {
		t.Fatalf("expected the surviving listener to be the good address, not the busy one")
	}
}

// TestListenAll_EveryAddressFailsIsAnError covers the one case a partial
// failure is not enough for: nothing at all got bound, so the caller must
// see an error rather than a healthy-looking empty listener set.
func TestListenAll_EveryAddressFailsIsAnError(t *testing.T) {
	seed, err := ListenAll([]string{"127.0.0.1:0"}, "test")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	busyAddr := seed[0].Addr().String()
	defer seed[0].Close()

	_, err = ListenAll([]string{busyAddr, busyAddr}, "test")
	if err == nil {
		t.Fatal("expected an error when every requested address fails to bind")
	}
}

func TestListenAll_AllSucceed(t *testing.T) {
	lns, err := ListenAll([]string{"127.0.0.1:0", "127.0.0.1:0"}, "test")
	if err != nil {
		t.Fatalf("listenAll: %v", err)
	}
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	if len(lns) != 2 {
		t.Fatalf("got %d listeners, want 2", len(lns))
	}
	if lns[0].Addr().String() == lns[1].Addr().String() {
		t.Fatalf("expected two distinct ephemeral ports, got the same address twice: %s", lns[0].Addr())
	}
}
