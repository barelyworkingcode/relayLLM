package router

// Proxy-path instrumentation for GET /api/status/detailed (api_status_detailed.go).
// This is the one genuinely new mechanism the diagnostic dashboard needs —
// everything else in that handler is aggregation of state that already
// exists (servermanager.ServerManager, registry.ProxyRegistry, TerminalManager, SessionManager).
//
// handleProxy (relay_router.go) registers one ProxyConn per inbound proxied
// request via ProxyMetrics.begin, wraps the client's http.ResponseWriter in
// meteredResponseWriter (outermost — routeVirtual's virtualResponseRecorder
// nests INSIDE it, so a failed-over request's bytes are counted once at the
// client boundary no matter how many candidates were attempted), and
// deregisters with `defer p.metrics.end(conn)`.

import (
	"bufio"
	"net"
	"net/http"
	clk "relayllm/internal/clock"
	"sync"
	"sync/atomic"
	"time"
)

// proxyViaAnthropicKey tags a request context so ProxyMetrics.begin can mark
// the resulting ProxyConn's viaAnthropic field. Set by
// handleAnthropicRedirect (relay_router_anthropic.go) on the synthesized
// OpenAI-shaped request it re-enters handleProxy with.
type proxyViaAnthropicKey struct{}

// throughputWindow is the trailing window rollingRate averages over.
const throughputWindow = 5 * time.Second

// recentRequestCap bounds ProxyMetrics.recent. A 2s dashboard poll cannot see
// a 300ms request in `active` at all, so without a completed-request tail the
// page shows an idle system while hundreds of requests fly through. Fixed
// size, overwritten in place — no allocation after warmup, no goroutine,
// matching registry.ProxyRegistry's and virtualAffinityStore's "bounded, no
// background sweep" style.
const recentRequestCap = 64

// Thresholds driving proxyConnState (below). Package-level vars, not consts,
// so a test can lower them.
var (
	// proxyStallAfter: a backend that has sent headers and then produced
	// nothing for this long is hung. Sized against what legitimately happens
	// between SSE chunks: token generation is sub-second, and the slowest
	// legitimate gap is prompt evaluation on a very long prompt, tens of
	// seconds on Metal. 60s sits above that and below the Anthropic-compat
	// ping interval's own watchdog budget (relay_router_anthropic.go pings
	// every 15s against Claude Code's 300s idle-byte watchdog), so a stall
	// shows on the dashboard long before any client gives up.
	proxyStallAfter = 60 * time.Second

	// proxyHeaderStallAfter applies only to streaming requests that have not
	// received a single byte. Sized at the managed-server admission timeout
	// (defaultAdmissionTimeout, 120s) plus launch headroom: below that, a
	// cold 40GB model queued behind a busy instance would report as stalled
	// while working exactly as designed.
	proxyHeaderStallAfter = 180 * time.Second

	// proxyQuietAfter is the RECONCILED_SCHEMA.md §4 "quiet" threshold: a
	// connection that has sent headers and produced nothing for this long,
	// but not yet long enough to call it stalled, reads as "quiet" rather
	// than "active" — a lighter-weight signal than a full stall alert.
	proxyQuietAfter = 5 * time.Second
)

// Unified connection-state vocabulary, per RECONCILED_SCHEMA.md §4. Folds
// backend.md's three proxy-only states (awaiting-upstream/streaming/stalled)
// into this shared vocabulary so the dashboard's connections[] array can
// discriminate on one `state` enum across proxy, WebSocket, and chat rows
// alike (ws/chat only ever report active/idle — see ws.go's wsIdleAfter and
// Session.IsProcessing's doc comments for why neither gets quiet/stalled).
const (
	ConnStateActive  = "active"
	ConnStateIdle    = "idle"
	ConnStateQuiet   = "quiet"
	ConnStateStalled = "stalled"
)

// proxyConnState implements RECONCILED_SCHEMA.md §4's state machine for a
// proxy connection (kind "sse" or "http"). now/startedAt/headerNano/
// lastWriteNano are all read outside any lock by the caller (snapshot()) —
// nanosecond timestamps read via atomic loads, which is why this takes them
// as plain values rather than the ProxyConn itself.
func proxyConnState(now, startedAt time.Time, headerNano, lastWriteNano int64, streaming, upgraded bool) string {
	sinceHeader := headerNano != 0
	var sinceLastByte time.Duration
	if sinceHeader {
		sinceLastByte = now.Sub(time.Unix(0, lastWriteNano))
	}
	if upgraded {
		// An upgraded socket (a passthrough WebSocket) stays open between
		// turns by design. OMP's codex transport reuses one for a whole
		// session. Silence there is idle, never quiet or stalled.
		if sinceLastByte < proxyQuietAfter {
			return ConnStateActive
		}
		return ConnStateIdle
	}
	switch {
	case sinceHeader && sinceLastByte >= proxyStallAfter:
		return ConnStateStalled
	case streaming && !sinceHeader && now.Sub(startedAt) >= proxyHeaderStallAfter:
		// Pre-header stall, streaming requests only: a non-streaming
		// completion legitimately produces its first byte only when
		// generation is finished, which can be minutes — flagging that as
		// stalled would make the dashboard cry wolf on every long
		// non-streaming call.
		return ConnStateStalled
	case sinceHeader && sinceLastByte >= proxyQuietAfter:
		return ConnStateQuiet
	default:
		return ConnStateActive
	}
}

// ---------------------------------------------------------------------------
// rollingRate — the throughput estimator
// ---------------------------------------------------------------------------

// rollingRate is a two-bucket sliding-window byte-rate estimator. Reading it
// never mutates it, so a dashboard poll cannot perturb the value another
// poller sees.
//
// The bucket roll is a CAS on startNano rather than a mutex: the
// per-connection instances have exactly one writer (the reverse proxy's copy
// goroutine) and the process-wide instance has one per in-flight request.
// Under a concurrent roll, the CAS loser's bytes land in the bucket that just
// rotated — off by at most one window's worth of one writer's bytes on a
// gauge that exists to answer "is this stream moving", which does not
// justify a lock on the hot path.
type rollingRate struct {
	window    time.Duration
	startNano atomic.Int64
	cur       atomic.Int64
	prev      atomic.Int64
}

func newRollingRate(window time.Duration, now time.Time) *rollingRate {
	r := &rollingRate{window: window}
	r.startNano.Store(now.UnixNano())
	return r
}

func (r *rollingRate) add(now time.Time, n int64) {
	start := r.startNano.Load()
	if now.UnixNano()-start >= int64(r.window) {
		if r.startNano.CompareAndSwap(start, now.UnixNano()) {
			r.prev.Store(r.cur.Swap(0))
		}
	}
	r.cur.Add(n)
}

// rate returns bytes/sec over the trailing window, 0 when nothing has been
// written for two full windows.
func (r *rollingRate) rate(now time.Time) float64 {
	start := r.startNano.Load()
	elapsed := now.UnixNano() - start
	switch {
	case elapsed >= 2*int64(r.window):
		// Total/elapsed would misreport a long SSE stream that produced
		// bytes early and has since frozen as still "healthy" — this is
		// exactly the failure this dashboard exists to surface, so a stream
		// with nothing written for two full windows reads as 0, not stale.
		return 0
	case elapsed >= int64(r.window):
		// Current window has expired but nothing has rolled it (no writes).
		// Only the bytes already in cur count, spread over the full window.
		return float64(r.cur.Load()) / r.window.Seconds()
	default:
		frac := 1 - float64(elapsed)/float64(r.window)
		return (float64(r.cur.Load()) + float64(r.prev.Load())*frac) / r.window.Seconds()
	}
}

// ---------------------------------------------------------------------------
// ProxyConn — one in-flight proxied request
// ---------------------------------------------------------------------------

// ProxyConn is one in-flight request through the relay-router's proxy path.
// Every mutable field is an atomic because the writer is the proxy's copy
// goroutine and the reader is a status poll on a different goroutine; the
// immutable fields are set before the value is published into ProxyMetrics.
//
// Every method is nil-receiver safe — the same defensive shape
// virtualAffinityStore.lookup/record already use — so a RelayRouter built
// without metrics (or a unit test calling a route function directly) needs
// no special casing.
type ProxyConn struct {
	id           uint64
	startedAt    time.Time
	method       string
	path         string
	remoteAddr   string
	model        string // what the client asked for, after an anthropic modelMap rewrite
	stream       bool   // request body's "stream": true
	viaAnthropic bool   // arrived through /v1/messages (handleAnthropicRedirect)
	bytesIn      int64  // request body length; known up front

	upgraded        atomic.Bool  // hijacked for a 101 Switching Protocols (WebSocket)
	upgradedBytesIn atomic.Int64 // client-to-upstream bytes after the upgrade

	kind          atomic.Pointer[string] // "managed" | "virtual" | "endpoint" | "unknown"
	target        atomic.Pointer[string] // resolved label: alias, or "endpoint/model"
	attempts      atomic.Int32           // virtual-model candidates tried so far
	bytesOut      atomic.Int64
	statusCode    atomic.Int32
	headerNano    atomic.Int64 // first WriteHeader/Write; 0 until then
	lastWriteNano atomic.Int64
	ended         atomic.Bool // guards ProxyMetrics.end against a double call

	rate    *rollingRate
	metrics *ProxyMetrics // for the process-wide rate/counters
}

func (c *ProxyConn) setTarget(kind, target string) {
	if c == nil {
		return
	}
	c.kind.Store(&kind)
	c.target.Store(&target)
}

// noteAttempt records one virtual-model candidate attempt. Called once per
// candidate by routeVirtual (relay_router_virtual.go), including the first —
// a direct managed/endpoint route never calls this, so its zero value means
// "single direct attempt" (snapshot()/end() both report that as 1, not 0).
func (c *ProxyConn) noteAttempt() {
	if c == nil {
		return
	}
	c.attempts.Add(1)
}

// noteHeader records the moment a response's first header/byte reached the
// metered writer. Idempotent: meteredResponseWriter.Write calls this
// unconditionally before every Write (matching WriteHeader's implicit-200
// contract), and only the first call is allowed to take effect.
func (c *ProxyConn) noteHeader(status int) {
	if c == nil {
		return
	}
	now := c.metrics.clock.Now()
	if c.headerNano.CompareAndSwap(0, now.UnixNano()) {
		c.statusCode.Store(int32(status))
		// Header arrival is itself progress. Seeding lastWriteNano here too
		// means a request that has sent headers but no body bytes yet is
		// timed from the moment something actually happened, not from the
		// zero value — which would otherwise read as instantly "stalled".
		c.lastWriteNano.Store(now.UnixNano())
	}
}

// noteWrite is the hot path: ~50ns against a chunk that costs a syscall to
// flush (two atomic.Int64.Add, one Store, one clock.Now(), two rollingRate.add
// calls). Cost is one clock read plus atomics — acceptable against an SSE
// chunk that already costs a syscall to flush.
func (c *ProxyConn) noteWrite(n int) {
	if c == nil || n <= 0 {
		return
	}
	now := c.metrics.clock.Now()
	c.bytesOut.Add(int64(n))
	c.lastWriteNano.Store(now.UnixNano())
	c.rate.add(now, int64(n))
	c.metrics.outRate.add(now, int64(n))
	c.metrics.totalBytesOut.Add(int64(n))
}

// noteUpgrade marks the connection as hijacked for a 101. ReverseProxy
// writes the 101 straight to the hijacked conn, so it never reaches
// WriteHeader. It is recorded here instead.
func (c *ProxyConn) noteUpgrade() {
	if c == nil {
		return
	}
	c.upgraded.Store(true)
	c.noteHeader(http.StatusSwitchingProtocols)
}

// noteRead counts client-to-upstream bytes on an upgraded connection. Traffic
// in either direction counts as activity for the active/idle state.
func (c *ProxyConn) noteRead(n int) {
	if c == nil || n <= 0 {
		return
	}
	now := c.metrics.clock.Now()
	c.upgradedBytesIn.Add(int64(n))
	c.lastWriteNano.Store(now.UnixNano())
	c.metrics.inRate.add(now, int64(n))
	c.metrics.totalBytesIn.Add(int64(n))
}

// snapshot builds this connection's ProxyConnInfo row as of now.
func (c *ProxyConn) snapshot(now time.Time) ProxyConnInfo {
	header := c.headerNano.Load()
	lastWrite := c.lastWriteNano.Load()
	attempts := c.attempts.Load()
	if attempts == 0 {
		attempts = 1 // never went through routeVirtual: one direct attempt.
	}
	info := ProxyConnInfo{
		ID:             c.id,
		Method:         c.method,
		Path:           c.path,
		RemoteAddr:     c.remoteAddr,
		Model:          c.model,
		TargetKind:     derefOr(c.kind.Load(), "unknown"),
		Target:         derefOr(c.target.Load(), ""),
		Attempts:       attempts,
		Stream:         c.stream,
		ViaAnthropic:   c.viaAnthropic,
		Status:         c.statusCode.Load(),
		StartedAt:      c.startedAt,
		BytesIn:        c.bytesIn + c.upgradedBytesIn.Load(),
		BytesOut:       c.bytesOut.Load(),
		BytesOutPerSec: c.rate.rate(now),
		AgeSeconds:     int(now.Sub(c.startedAt).Seconds()),
	}
	if header != 0 {
		info.LastByteAt = time.Unix(0, lastWrite)
		info.SinceLastByteSeconds = int(now.Sub(info.LastByteAt).Seconds())
	}
	info.State = proxyConnState(now, c.startedAt, header, lastWrite, c.stream, c.upgraded.Load())
	return info
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// ---------------------------------------------------------------------------
// ProxyMetrics — the registry
// ---------------------------------------------------------------------------

// ProxyMetrics is the process-wide registry of in-flight and recently
// completed proxied requests, feeding GET /api/status/detailed. mu is held
// only for map insert/delete and ring append — never across a Write, never
// across clock.Now() in a loop.
type ProxyMetrics struct {
	clock clk.Clock

	nextID atomic.Uint64

	mu         sync.Mutex
	active     map[uint64]*ProxyConn
	recent     []RecentRequestInfo // fixed-size ring, appended up to recentRequestCap then overwritten in place
	recentNext int                 // next write index, mod recentRequestCap

	totalRequests atomic.Uint64
	totalBytesIn  atomic.Int64
	totalBytesOut atomic.Int64
	inRate        *rollingRate
	outRate       *rollingRate
}

// NewProxyMetrics returns a registry. clock nil -> DefaultClock.
func NewProxyMetrics(clock clk.Clock) *ProxyMetrics {
	if clock == nil {
		clock = clk.DefaultClock
	}
	now := clock.Now()
	return &ProxyMetrics{
		clock:   clock,
		active:  make(map[uint64]*ProxyConn),
		inRate:  newRollingRate(throughputWindow, now),
		outRate: newRollingRate(throughputWindow, now),
	}
}

// begin registers an in-flight request and returns the ResponseWriter the
// rest of the dispatch must use. The returned writer is the OUTERMOST
// wrapper: the virtual-model failover path nests its own
// virtualResponseRecorder inside this one, so bytes are counted once at the
// client boundary no matter how many candidates were attempted, and a
// candidate that failed before writing anything contributes nothing
// (newUpstreamProxy's onError hook suppresses its 502, so nothing reaches
// this writer either). Nil-safe: m == nil (a hand-built RelayRouter in a
// test) returns (nil, w) unchanged, and every ProxyConn method tolerates a
// nil receiver.
func (m *ProxyMetrics) begin(w http.ResponseWriter, r *http.Request, model string, stream bool, bodyLen int64) (*ProxyConn, http.ResponseWriter) {
	if m == nil {
		return nil, w
	}
	// The unread passthrough paths pass r.ContentLength, which is -1 for a
	// chunked body. Left as-is it would subtract from the process-wide total.
	if bodyLen < 0 {
		bodyLen = 0
	}
	now := m.clock.Now()
	viaAnthropic, _ := r.Context().Value(proxyViaAnthropicKey{}).(bool)
	c := &ProxyConn{
		id:           m.nextID.Add(1),
		startedAt:    now,
		method:       r.Method,
		path:         r.URL.Path,
		remoteAddr:   r.RemoteAddr,
		model:        model,
		stream:       stream,
		viaAnthropic: viaAnthropic,
		bytesIn:      bodyLen,
		rate:         newRollingRate(throughputWindow, now),
		metrics:      m,
	}

	m.totalRequests.Add(1)
	m.totalBytesIn.Add(bodyLen)
	m.inRate.add(now, bodyLen)

	m.mu.Lock()
	m.active[c.id] = c
	m.mu.Unlock()

	return c, &meteredResponseWriter{ResponseWriter: w, conn: c}
}

// end deregisters conn and appends a RecentRequestInfo. Safe to call twice
// (only the first call has an effect) — handleProxy's `defer p.metrics.end(conn)`
// is the only production call site, but idempotency keeps a future second
// call site from double-recording.
func (m *ProxyMetrics) end(c *ProxyConn) {
	if m == nil || c == nil {
		return
	}
	if !c.ended.CompareAndSwap(false, true) {
		return
	}

	now := m.clock.Now()
	attempts := c.attempts.Load()
	if attempts == 0 {
		attempts = 1
	}
	info := RecentRequestInfo{
		ID:         c.id,
		Model:      c.model,
		TargetKind: derefOr(c.kind.Load(), "unknown"),
		Target:     derefOr(c.target.Load(), ""),
		Attempts:   attempts,
		Status:     c.statusCode.Load(),
		Stream:     c.stream,
		DurationMs: now.Sub(c.startedAt).Milliseconds(),
		TTFBMs:     -1,
		BytesIn:    c.bytesIn + c.upgradedBytesIn.Load(),
		BytesOut:   c.bytesOut.Load(),
		FinishedAt: now,
	}
	if h := c.headerNano.Load(); h != 0 {
		info.TTFBMs = time.Unix(0, h).Sub(c.startedAt).Milliseconds()
	}

	m.mu.Lock()
	delete(m.active, c.id)
	if len(m.recent) < recentRequestCap {
		m.recent = append(m.recent, info)
	} else {
		m.recent[m.recentNext] = info
	}
	m.recentNext = (m.recentNext + 1) % recentRequestCap
	m.mu.Unlock()
}

// ProxyConnInfo is one in-flight proxy connection's snapshot, feeding
// GET /api/status/detailed's connections[] rows (kind "sse"/"http").
type ProxyConnInfo struct {
	ID                   uint64
	Method               string
	Path                 string
	RemoteAddr           string
	Model                string
	TargetKind           string
	Target               string
	Attempts             int32
	Stream               bool
	ViaAnthropic         bool
	Status               int32
	StartedAt            time.Time
	LastByteAt           time.Time // zero when no response byte has arrived yet
	AgeSeconds           int
	SinceLastByteSeconds int // 0 / meaningless when LastByteAt is zero
	BytesIn              int64
	BytesOut             int64
	BytesOutPerSec       float64
	State                string // ConnStateActive | ConnStateQuiet | ConnStateStalled
}

// ProxyAggregate is the process-wide proxy summary feeding
// GET /api/status/detailed's overview.throughput and connection counts.
type ProxyAggregate struct {
	ActiveCount    int
	StalledCount   int
	BytesInPerSec  float64
	BytesOutPerSec float64
	TotalBytesIn   int64
	TotalBytesOut  int64
	TotalRequests  uint64
	WindowSeconds  int
}

// RecentRequestInfo is one completed proxy request, feeding
// GET /api/status/detailed's recentRequests[] ring.
type RecentRequestInfo struct {
	ID         uint64
	Model      string
	TargetKind string
	Target     string
	Attempts   int32
	Status     int32
	Stream     bool
	DurationMs int64
	TTFBMs     int64 // startedAt -> first response byte; -1 if none ever arrived
	BytesIn    int64
	BytesOut   int64
	FinishedAt time.Time
}

// Snapshot returns every in-flight connection, the process-wide aggregate,
// and the recent-request ring (newest first). Nil-safe.
func (m *ProxyMetrics) Snapshot() ([]ProxyConnInfo, ProxyAggregate, []RecentRequestInfo) {
	if m == nil {
		return nil, ProxyAggregate{}, nil
	}
	now := m.clock.Now()

	m.mu.Lock()
	activeConns := make([]*ProxyConn, 0, len(m.active))
	for _, c := range m.active {
		activeConns = append(activeConns, c)
	}
	n := len(m.recent)
	recentOut := make([]RecentRequestInfo, n)
	for i := 0; i < n; i++ {
		// recentNext always advances mod recentRequestCap (even before the
		// ring is full, since len(m.recent) <= recentRequestCap at every
		// point), so this single formula correctly walks newest-first in
		// both the partially-filled and wrapped-around cases.
		idx := (m.recentNext - 1 - i + recentRequestCap) % recentRequestCap
		recentOut[i] = m.recent[idx]
	}
	m.mu.Unlock()

	infos := make([]ProxyConnInfo, 0, len(activeConns))
	stalled := 0
	for _, c := range activeConns {
		info := c.snapshot(now)
		if info.State == ConnStateStalled {
			stalled++
		}
		infos = append(infos, info)
	}
	sortProxyConnInfosByID(infos)

	agg := ProxyAggregate{
		ActiveCount:    len(infos),
		StalledCount:   stalled,
		BytesInPerSec:  m.inRate.rate(now),
		BytesOutPerSec: m.outRate.rate(now),
		TotalBytesIn:   m.totalBytesIn.Load(),
		TotalBytesOut:  m.totalBytesOut.Load(),
		TotalRequests:  m.totalRequests.Load(),
		WindowSeconds:  int(throughputWindow.Seconds()),
	}
	return infos, agg, recentOut
}

// sortProxyConnInfosByID gives Snapshot's active-connection list a
// deterministic order (ascending id, i.e. oldest-registered first) — a plain
// map range would otherwise flap the order on every poll for no reason.
func sortProxyConnInfosByID(infos []ProxyConnInfo) {
	for i := 1; i < len(infos); i++ {
		for j := i; j > 0 && infos[j-1].ID > infos[j].ID; j-- {
			infos[j-1], infos[j] = infos[j], infos[j-1]
		}
	}
}

// ---------------------------------------------------------------------------
// meteredResponseWriter
// ---------------------------------------------------------------------------

// meteredResponseWriter is the outermost writer handleProxy dispatches
// through — sibling to virtualResponseRecorder (relay_router_virtual.go),
// same shape, but always present (conn is nil-safe) and always the layer
// closest to the real client socket.
type meteredResponseWriter struct {
	http.ResponseWriter
	conn *ProxyConn
}

func (m *meteredResponseWriter) WriteHeader(status int) {
	m.conn.noteHeader(status)
	m.ResponseWriter.WriteHeader(status)
}

func (m *meteredResponseWriter) Write(b []byte) (int, error) {
	m.conn.noteHeader(http.StatusOK) // no-op if a header already landed
	n, err := m.ResponseWriter.Write(b)
	m.conn.noteWrite(n) // count what actually reached the socket, not len(b)
	return n, err
}

// Flush is required: newUpstreamProxy sets FlushInterval: -1, which flushes
// through whatever ResponseWriter it was handed.
func (m *meteredResponseWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer through both
// this and the virtualResponseRecorder it may nest.
func (m *meteredResponseWriter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

// Hijack meters a connection that ReverseProxy upgrades (a passthrough
// WebSocket). For a 101, ReverseProxy hijacks the client conn and copies
// frames on it directly. Without this wrapper those bytes never pass through
// Write, and a socket carrying a whole session would show 0 bytes.
// http.ResponseController prefers Hijack over Unwrap, so ReverseProxy finds
// this method first.
func (m *meteredResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(m.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	m.conn.noteUpgrade()
	return &meteredConn{Conn: conn, pc: m.conn}, brw, nil
}

// meteredConn counts both directions of a hijacked connection. Embedding the
// net.Conn interface (not *net.TCPConn) hides ReadFrom/WriteTo, so io.Copy
// always goes through Read/Write here.
type meteredConn struct {
	net.Conn
	pc *ProxyConn
}

func (c *meteredConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.pc.noteRead(n)
	return n, err
}

func (c *meteredConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.pc.noteWrite(n)
	return n, err
}
