package config

import (
	"net"
	"net/http"
	"time"
)

// VirtualDialTransport is used only by the virtual-model retry path. A
// target host that black-holes packets (rather than actively refusing the
// connection — the real case this fixes, an unreachable LAN host) would
// otherwise eat http.DefaultTransport's ~30s dial timeout per candidate
// before failover even started trying the next one. ResponseHeaderTimeout is
// deliberately left unset: generation can legitimately take a long time, and
// a slow-but-alive backend must not be mistaken for a dead one. Direct
// (non-virtual) routes keep plain http.DefaultTransport behavior.
var VirtualDialTransport http.RoundTripper = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 3 * time.Second}).DialContext
	return t
}()
