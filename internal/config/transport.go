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

// ProbeDialTransport: the model-list probe's transport. 1s dial + 1s TLS
// handshake, no ResponseHeaderTimeout. A chained relayLLM router answers
// /models only after its own probe; it stays bounded by the caller's 10s
// modelsFetchTimeout instead.
//
// Keep-alives are deliberately off. The dial and TLS bounds apply only when a
// probe dials; a pooled connection to a host that vanished without FIN/RST
// would instead wait out the full 10s. A probe runs at most once per endpoint
// per 15s, so the fresh dial is cheap.
var ProbeDialTransport http.RoundTripper = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 1 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 1 * time.Second
	t.DisableKeepAlives = true
	return t
}()
