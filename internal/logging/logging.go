// Package logging implements the relay logging standard (one JSON object per
// line on stderr, nine fixed keys, a trace ID) for relayLLM. It is a leaf
// package: stdlib only. The standard is a contract, not shared code.
package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	EnvLogLevel    = "RELAY_LOG_LEVEL"
	EnvServiceID   = "RELAY_SERVICE_ID"
	EnvTraceID     = "RELAY_TRACE_ID"
	TraceHeader    = "X-Trace-Id"
	DefaultService = "relayllm"
	DebugWindow    = 30 * time.Minute
	// MaxTextChars bounds msg, error and Truncate, counted in runes.
	MaxTextChars = 500
)

var (
	traceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	opRe      = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
)

// Options configures a Handler.
type Options struct {
	DefaultService string
	// Getenv is read once, in NewHandler. nil means os.Getenv.
	Getenv func(string) string
	// Now drives the debug window only. nil means time.Now.
	Now func() time.Time
}

// state is shared by a Handler and every handler derived from it by
// WithAttrs/WithGroup, so the writer mutex and the debug window are global.
type state struct {
	mu       sync.Mutex
	w        io.Writer
	service  string
	now      func() time.Time
	level    atomic.Int64 // slog.Level
	deadline time.Time    // zero when not at debug
	expired  atomic.Bool
	invalid  bool // RELAY_LOG_LEVEL was set but unusable
}

// Handler is a slog.Handler that writes the relay line format.
type Handler struct {
	st     *state
	attrs  []slog.Attr // already group-prefixed
	groups []string
}

// NewHandler builds a handler writing to w. RELAY_LOG_LEVEL is read here, once.
func NewHandler(w io.Writer, opts Options) *Handler {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = osGetenv
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	st := &state{w: w, now: now}
	st.service = strings.TrimSpace(getenv(EnvServiceID))
	if st.service == "" {
		st.service = opts.DefaultService
	}
	if st.service == "" {
		st.service = DefaultService
	}
	st.level.Store(int64(slog.LevelInfo))
	if raw := getenv(EnvLogLevel); strings.TrimSpace(raw) != "" {
		if lvl, ok := ParseLevel(raw); ok {
			st.level.Store(int64(lvl))
			if lvl == slog.LevelDebug {
				st.deadline = now().Add(DebugWindow)
			}
		} else {
			st.invalid = true
		}
	}
	return &Handler{st: st}
}

// Install builds a handler, makes it the slog default and reports an invalid
// RELAY_LOG_LEVEL once, without echoing the value.
func Install(w io.Writer, opts Options) *Handler {
	h := NewHandler(w, opts)
	slog.SetDefault(slog.New(h))
	if h.st.invalid {
		slog.Warn("invalid log level, using info", "op", "log.level", "status", "error", "error", "invalid_level")
	}
	return h
}

// ParseLevel accepts error, warn, info, debug, ignoring case and space.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return slog.LevelError, true
	case "warn":
		return slog.LevelWarn, true
	case "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	}
	return slog.LevelInfo, false
}

// Truncate returns the first MaxTextChars runes of s.
func Truncate(s string) string {
	if len(s) <= MaxTextChars {
		return s
	}
	n := 0
	for i := range s {
		if n == MaxTextChars {
			return s[:i]
		}
		n++
	}
	return s
}

// NewTraceID returns 32 lowercase hex characters from crypto/rand.
func NewTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("logging: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ValidTraceID reports whether id may be accepted from outside.
func ValidTraceID(id string) bool { return traceIDRe.MatchString(id) }

// TraceIDOrNew keeps a valid id and otherwise makes a new one. The rejected
// value is never logged.
func TraceIDOrNew(id string) string {
	if ValidTraceID(id) {
		return id
	}
	return NewTraceID()
}

type traceKey struct{}

// ContextWithTrace stores id in ctx. An invalid id leaves ctx unchanged.
func ContextWithTrace(ctx context.Context, id string) context.Context {
	if !ValidTraceID(id) {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, id)
}

// TraceFromContext returns the trace ID, or "" when absent or ctx is nil.
func TraceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(traceKey{}).(string)
	return id
}

// OnBoxHost reports whether host is this machine: localhost, 127.0.0.0/8, ::1.
func OnBoxHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// OnBoxURL reports whether u names a destination on this machine.
func OnBoxURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	if strings.EqualFold(u.Scheme, "unix") {
		return true
	}
	return OnBoxHost(u.Hostname())
}

// SafeURL renders scheme://host[:port]/path with no userinfo, query or fragment.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<invalid url>"
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func (h *Handler) checkWindow() {
	st := h.st
	if st.deadline.IsZero() || st.expired.Load() || st.now().Before(st.deadline) {
		return
	}
	if st.expired.CompareAndSwap(false, true) {
		st.level.Store(int64(slog.LevelInfo))
		st.write(st.now(), slog.LevelWarn, "debug logging expired, back to info", "log.level", "error", 0, "debug_window_expired", "", nil)
	}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(_ context.Context, l slog.Level) bool {
	h.checkWindow()
	return l >= slog.Level(h.st.level.Load())
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	nh := &Handler{st: h.st, groups: h.groups}
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), prefixAll(h.groups, as)...)
	return nh
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{st: h.st, attrs: h.attrs, groups: append(append([]string{}, h.groups...), name)}
}

func prefixAll(groups []string, as []slog.Attr) []slog.Attr {
	if len(groups) == 0 {
		return as
	}
	p := strings.Join(groups, ".") + "."
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = slog.Attr{Key: p + a.Key, Value: a.Value}
	}
	return out
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	h.checkWindow()
	var all []slog.Attr
	all = append(all, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		all = append(all, prefixAll(h.groups, []slog.Attr{a})[0])
		return true
	})
	ts := r.Time
	if ts.IsZero() {
		ts = h.st.now()
	}
	h.st.writeAttrs(ts, r.Level, r.Message, TraceFromContext(ctx), all)
	return nil
}

type extra struct {
	key string
	raw []byte
}

func (st *state) writeAttrs(ts time.Time, lvl slog.Level, msg, trace string, attrs []slog.Attr) {
	op := "log"
	status := ""
	var durMS int64
	errText := ""
	var extras []extra
	add := func(k string, v any) {
		extras = append(extras, extra{k, marshalValue(v)})
	}
	for _, a := range flatten(attrs) {
		v := a.Value.Resolve()
		switch a.Key {
		case "ts", "level", "msg", "service", "trace_id":
			add("attr_"+a.Key, v.Any())
		case "op":
			if s, ok := stringOf(v); ok && opRe.MatchString(s) {
				op = s
			} else {
				add("attr_op", v.Any())
			}
		case "status":
			if v.Kind() != slog.KindString {
				add("http_status", v.Any())
			} else if s := v.String(); s == "ok" || s == "error" || s == "denied" {
				status = s
			} else {
				add("attr_status", s)
			}
		case "duration_ms":
			if d, ok := durationMS(v); ok {
				durMS = d
			} else {
				add("attr_duration_ms", v.Any())
			}
		case "error":
			errText = Truncate(textOf(v))
		default:
			add(a.Key, v.Any())
		}
	}
	if status == "" {
		if lvl >= slog.LevelWarn {
			status = "error"
		} else {
			status = "ok"
		}
	}
	st.write(ts, lvl, msg, op, status, durMS, errText, trace, extras)
}

func (st *state) write(ts time.Time, lvl slog.Level, msg, op, status string, durMS int64, errText, trace string, extras []extra) {
	var b []byte
	b = append(b, `{"ts":`...)
	b = append(b, marshalValue(ts.UTC().Format("2006-01-02T15:04:05.000Z"))...)
	b = append(b, `,"level":`...)
	b = append(b, marshalValue(levelName(lvl))...)
	b = append(b, `,"msg":`...)
	b = append(b, marshalValue(Truncate(msg))...)
	b = append(b, `,"service":`...)
	b = append(b, marshalValue(st.service)...)
	b = append(b, `,"op":`...)
	b = append(b, marshalValue(op)...)
	b = append(b, `,"status":`...)
	b = append(b, marshalValue(status)...)
	b = append(b, fmt.Sprintf(`,"duration_ms":%d`, durMS)...)
	b = append(b, `,"error":`...)
	b = append(b, marshalValue(errText)...)
	b = append(b, `,"trace_id":`...)
	b = append(b, marshalValue(trace)...)
	for _, e := range extras {
		b = append(b, ',')
		b = append(b, marshalValue(e.key)...)
		b = append(b, ':')
		b = append(b, e.raw...)
	}
	b = append(b, '}', '\n')
	st.mu.Lock()
	_, _ = st.w.Write(b)
	st.mu.Unlock()
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}

// flatten expands slog groups into dotted keys.
func flatten(attrs []slog.Attr) []slog.Attr {
	var out []slog.Attr
	for _, a := range attrs {
		v := a.Value.Resolve()
		if v.Kind() == slog.KindGroup {
			sub := flatten(v.Group())
			for _, s := range sub {
				if a.Key != "" {
					s.Key = a.Key + "." + s.Key
				}
				out = append(out, s)
			}
			continue
		}
		if a.Key == "" {
			continue
		}
		out = append(out, slog.Attr{Key: a.Key, Value: v})
	}
	return out
}

func stringOf(v slog.Value) (string, bool) {
	if v.Kind() == slog.KindString {
		return v.String(), true
	}
	return "", false
}

func textOf(v slog.Value) string {
	switch x := v.Any().(type) {
	case nil:
		return ""
	case error:
		return x.Error()
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

func durationMS(v slog.Value) (int64, bool) {
	switch v.Kind() {
	case slog.KindInt64:
		if n := v.Int64(); n >= 0 {
			return n, true
		}
	case slog.KindUint64:
		if n := v.Uint64(); n <= 1<<62 {
			return int64(n), true
		}
	case slog.KindDuration:
		if d := v.Duration(); d >= 0 {
			return d.Milliseconds(), true
		}
	}
	return 0, false
}

// marshalValue JSON-encodes v on one line; unencodable values fall back to
// their printed form so a bad attribute never drops a line.
func marshalValue(v any) []byte {
	switch x := v.(type) {
	case error:
		v = Truncate(x.Error())
	case time.Duration:
		v = x.Milliseconds()
	}
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(fmt.Sprint(v))
	}
	return b
}
