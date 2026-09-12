package router

// Hermetic coverage for status_metrics.go: rollingRate's window math, the
// ProxyMetrics registry lifecycle, the recent-request ring, the unified
// state classification (RECONCILED_SCHEMA.md §4), and meteredResponseWriter.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	clk "relayllm/internal/clock"
	"relayllm/internal/testutil"
)

// ---------------------------------------------------------------------------
// rollingRate
// ---------------------------------------------------------------------------

func TestRollingRate_WindowBlend(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	r := newRollingRate(5*time.Second, t0)

	// 100 bytes at t0: fully inside the window, so the blended rate should
	// read close to 100/5 = 20 B/s (frac ~= 1 immediately after the write).
	r.add(t0, 100)
	if got := r.rate(t0); got < 19.9 || got > 20.1 {
		t.Errorf("rate immediately after add = %v, want ~20", got)
	}

	// Halfway through the window: still blending the same bucket (nothing
	// has rolled), so the rate is unchanged — cur/window with no prev
	// contribution yet since nothing rolled.
	half := t0.Add(2500 * time.Millisecond)
	if got := r.rate(half); got < 19.9 || got > 20.1 {
		t.Errorf("rate mid-window = %v, want ~20 (nothing rolled yet)", got)
	}

	// Cross exactly one window: the bucket rolls on the next add. Add 50
	// bytes at t0+5s — this becomes the new `cur`, and the 100 bytes become
	// `prev`. Reading immediately after (frac ~= 1) blends nearly all of
	// prev's 100 plus cur's 50: (50+100)/5 = 30 B/s.
	t1 := t0.Add(5 * time.Second)
	r.add(t1, 50)
	if got := r.rate(t1); got < 29.5 || got > 30.1 {
		t.Errorf("rate just after rolling = %v, want ~30", got)
	}

	// Cross a second full window with no further writes: elapsed >= 2*window
	// from t1 must report exactly 0 — the "long stream that went quiet still
	// looks fast" regression rollingRate exists to prevent.
	t2 := t1.Add(10 * time.Second)
	if got := r.rate(t2); got != 0 {
		t.Errorf("rate after two silent windows = %v, want exactly 0", got)
	}
}

// TestRollingRate_NoWritesReportsZero guards the specific regression a naive
// total-bytes/total-elapsed estimator has: a stream that wrote a burst early
// and then went silent must not keep reporting that burst's rate forever.
func TestRollingRate_NoWritesReportsZero(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	r := newRollingRate(5*time.Second, t0)
	r.add(t0, 5_000_000) // a large early burst

	silent := t0.Add(11 * time.Second) // well past 2*window with nothing since
	if got := r.rate(silent); got != 0 {
		t.Errorf("rate long after a burst = %v, want 0 (a stalled stream must not look fast)", got)
	}
}

// ---------------------------------------------------------------------------
// ProxyMetrics lifecycle
// ---------------------------------------------------------------------------

func newTestProxyMetrics(clock clk.Clock) *ProxyMetrics {
	return NewProxyMetrics(clock)
}

func TestProxyMetrics_BeginEndLifecycle(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	m := newTestProxyMetrics(clock)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	conn, w := m.begin(rec, req, "qwen3-8b", false, 42)
	if conn == nil {
		t.Fatal("begin returned a nil conn")
	}

	active, _, _ := m.Snapshot()
	if len(active) != 1 || active[0].ID != conn.id {
		t.Fatalf("active snapshot = %+v, want exactly the just-begun connection", active)
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("hello"))

	m.end(conn)

	active, _, recent := m.Snapshot()
	if len(active) != 0 {
		t.Errorf("active after end = %+v, want empty", active)
	}
	if len(recent) != 1 {
		t.Fatalf("recent after end = %+v, want exactly 1 entry", recent)
	}
	if recent[0].ID != conn.id || recent[0].Model != "qwen3-8b" || recent[0].Status != http.StatusOK {
		t.Errorf("recent[0] = %+v, want id=%d model=qwen3-8b status=200", recent[0], conn.id)
	}
	if recent[0].BytesOut != 5 {
		t.Errorf("recent[0].BytesOut = %d, want 5", recent[0].BytesOut)
	}
}

func TestProxyMetrics_RecentRingBounded(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	m := newTestProxyMetrics(clock)

	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		conn, w := m.begin(rec, req, "m", false, 0)
		w.WriteHeader(http.StatusOK)
		m.end(conn)
		clock.Advance(time.Millisecond)
	}

	_, _, recent := m.Snapshot()
	if len(recent) != recentRequestCap {
		t.Fatalf("len(recent) = %d, want %d", len(recent), recentRequestCap)
	}
	// Newest-first: the last-registered connection (id 200) must lead.
	if recent[0].ID != 200 {
		t.Errorf("recent[0].ID = %d, want 200 (newest first)", recent[0].ID)
	}
	if recent[len(recent)-1].ID != 200-uint64(recentRequestCap)+1 {
		t.Errorf("recent[last].ID = %d, want %d (oldest surviving entry)", recent[len(recent)-1].ID, 200-uint64(recentRequestCap)+1)
	}
}

// ---------------------------------------------------------------------------
// Unified state classification (RECONCILED_SCHEMA.md §4) — table-driven over
// fake-clock offsets, pinning the exact thresholds.
// ---------------------------------------------------------------------------

func TestProxyConn_StallClassification(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)

	cases := []struct {
		name      string
		streaming bool
		upgraded  bool
		headerAt  time.Duration // offset from start; -1 means never
		lastByte  time.Duration // offset from start; meaningless if headerAt < 0
		now       time.Duration // offset from start
		want      string
	}{
		{"pre-header, streaming, within grace", true, false, -1, 0, 10 * time.Second, ConnStateActive},
		{"pre-header, streaming, past grace -> stalled", true, false, -1, 0, 181 * time.Second, ConnStateStalled},
		{"pre-header, non-streaming, never stalled by header wait", false, false, -1, 0, 10 * time.Minute, ConnStateActive},
		{"headers sent, fresh write -> active", true, false, 0, 0, 2 * time.Second, ConnStateActive},
		{"headers sent, 5s since last byte -> quiet", true, false, 0, 0, 5 * time.Second, ConnStateQuiet},
		{"headers sent, 59s since last byte -> still quiet", true, false, 0, 0, 59 * time.Second, ConnStateQuiet},
		{"headers sent, 60s since last byte -> stalled", true, false, 0, 0, 60 * time.Second, ConnStateStalled},
		{"non-stream, headers sent, quiet applies too", false, false, 0, 0, 6 * time.Second, ConnStateQuiet},
		{"non-stream, headers sent, stalled applies too", false, false, 0, 0, 61 * time.Second, ConnStateStalled},
		{"upgraded, fresh traffic -> active", false, true, 0, 0, 2 * time.Second, ConnStateActive},
		{"upgraded, silent between turns -> idle, not quiet", false, true, 0, 0, 5 * time.Second, ConnStateIdle},
		{"upgraded, silent for minutes -> idle, never stalled", false, true, 0, 0, 10 * time.Minute, ConnStateIdle},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := start.Add(c.now)
			var headerNano int64
			var lastWriteNano int64
			if c.headerAt >= 0 {
				headerNano = start.Add(c.headerAt).UnixNano()
				lastWriteNano = start.Add(c.lastByte).UnixNano()
			}
			got := proxyConnState(now, start, headerNano, lastWriteNano, c.streaming, c.upgraded)
			if got != c.want {
				t.Errorf("proxyConnState(...) = %q, want %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// meteredResponseWriter
// ---------------------------------------------------------------------------

func TestMeteredResponseWriter_CountsActualBytes(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	m := newTestProxyMetrics(clock)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	conn, w := m.begin(rec, req, "m", false, 0)

	// httptest.ResponseRecorder.Write always reports len(b), so this only
	// pins the "count n, not len(b)" contract at the meteredResponseWriter
	// layer itself, driving noteWrite directly with a short count.
	conn.noteWrite(3)
	if got := conn.bytesOut.Load(); got != 3 {
		t.Errorf("bytesOut after noteWrite(3) = %d, want 3", got)
	}

	w.Write([]byte("hello world"))
	if got := conn.bytesOut.Load(); got != 3+11 {
		t.Errorf("bytesOut after Write = %d, want %d", got, 3+11)
	}
}

// mockShortWriter wraps an httptest.ResponseRecorder but reports writing
// fewer bytes than it was given, so the test can tell noteWrite(n) apart
// from noteWrite(len(b)).
type mockShortWriter struct {
	*httptest.ResponseRecorder
	report int
}

func (m *mockShortWriter) Write(b []byte) (int, error) {
	m.ResponseRecorder.Write(b)
	return m.report, nil
}

func TestMeteredResponseWriter_CreditsShortWriteCount(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	m := newTestProxyMetrics(clock)
	rec := &mockShortWriter{ResponseRecorder: httptest.NewRecorder(), report: 4}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	conn, w := m.begin(rec, req, "m", false, 0)

	n, err := w.Write([]byte("this is way more than four bytes"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != 4 {
		t.Fatalf("Write returned %d, want the underlying writer's short count 4", n)
	}
	if got := conn.bytesOut.Load(); got != 4 {
		t.Errorf("bytesOut = %d, want 4 (the actual count, not len(b))", got)
	}
}

// flushRecorder wraps httptest.ResponseRecorder (which has no Flush) so the
// test can observe whether Flush reached the bottom.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

func TestMeteredResponseWriter_FlushPassthrough(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	m := newTestProxyMetrics(clock)
	inner := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, w := m.begin(inner, req, "m", true, 0)

	// Wrap the metered writer in a virtualResponseRecorder, exactly as
	// routeVirtual does — Flush must reach through both layers to the
	// underlying recorder, or SSE would silently buffer.
	rec := &virtualResponseRecorder{ResponseWriter: w}
	rec.Flush()

	if !inner.flushed {
		t.Error("Flush did not reach the underlying recorder through meteredResponseWriter + virtualResponseRecorder")
	}
}
