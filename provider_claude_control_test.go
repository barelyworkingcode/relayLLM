package main

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// Hermetic coverage of the host-session control_request/control_response
// protocol (../relay/docs/ssh-hosts.md decision 5). No real ssh, no real
// claude binary: p.processLine is fed a control_request line exactly as
// readStdout would deliver one from a real subprocess, and p.stdin is a pipe
// this test reads from directly — the same seam a fake claude subprocess
// (a small script that prints a control_request then reads its response off
// stdin) would exercise, without paying for an actual process.

// newControlTestProvider builds a ClaudeProvider with its stdin wired to a
// pipe the test can read control_response bytes off of. perms defaults to a
// fresh PermissionManager if nil.
func newControlTestProvider(t *testing.T, session *Session, perms *PermissionManager) (*ClaudeProvider, *io.PipeReader) {
	t.Helper()
	if perms == nil {
		perms = NewPermissionManager()
	}
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close(); pr.Close() })

	p := &ClaudeProvider{
		session: session,
		model:   session.Model,
		perms:   perms,
		stdin:   pw,
	}
	p.handler = func(string, json.RawMessage) {}
	p.emitter = NewEventEmitter(p.handler)
	return p, pr
}

// readOneLine reads exactly one control_response write off pr. Production
// writes each response as a single Write call (data + trailing '\n'), and
// the buffer here is large enough to drain it in one Read, so this mirrors
// what a real subprocess's stdin reader would see.
func readOneLine(t *testing.T, pr *io.PipeReader) []byte {
	t.Helper()
	buf := make([]byte, 8192)
	done := make(chan struct{})
	var n int
	var err error
	go func() {
		n, err = pr.Read(buf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for control_response")
	}
	if err != nil {
		t.Fatalf("read control_response: %v", err)
	}
	return bytes.TrimRight(buf[:n], "\n")
}

// firstPendingPermissionID returns the id of the (single) pending permission
// request registered in perms, failing the test if there isn't exactly one.
func firstPendingPermissionID(t *testing.T, perms *PermissionManager) string {
	t.Helper()
	perms.mu.Lock()
	defer perms.mu.Unlock()
	if len(perms.pending) != 1 {
		t.Fatalf("pending permission requests = %d, want 1", len(perms.pending))
	}
	for id := range perms.pending {
		return id
	}
	return ""
}

func controlRequestLine(requestID, toolName, input, toolUseID string) []byte {
	raw, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": json.RawMessage(requestID),
		"request": map[string]any{
			"subtype":     "can_use_tool",
			"tool_name":   toolName,
			"input":       json.RawMessage(input),
			"tool_use_id": toolUseID,
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestControlRequest_AllowWritesExactControlResponse(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1"}
	p, pr := newControlTestProvider(t, session, perms)

	p.processLine(controlRequestLine("42", "Write", `{"file_path":"/tmp/x"}`, "tu_1"))

	id := firstPendingPermissionID(t, perms)
	perms.Resolve(id, PermissionDecision{Decision: "allow"})

	got := readOneLine(t, pr)
	want := buildControlResponseAllow(json.RawMessage("42"), json.RawMessage(`{"file_path":"/tmp/x"}`))
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}

func TestControlRequest_DenyWritesExactControlResponse(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1"}
	p, pr := newControlTestProvider(t, session, perms)

	p.processLine(controlRequestLine(`"req-7"`, "Bash", `{"command":"rm -rf /"}`, "tu_2"))

	id := firstPendingPermissionID(t, perms)
	perms.Resolve(id, PermissionDecision{Decision: "deny", Reason: "not on my watch"})

	got := readOneLine(t, pr)
	want := buildControlResponseDeny(json.RawMessage(`"req-7"`), "not on my watch")
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}

// A deny with no reason defaults to "Denied by user" (../relay/docs/ssh-hosts.md).
func TestControlRequest_DenyWithoutReasonDefaultsMessage(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1"}
	p, pr := newControlTestProvider(t, session, perms)

	p.processLine(controlRequestLine("1", "Read", `{}`, "tu_3"))
	id := firstPendingPermissionID(t, perms)
	perms.Resolve(id, PermissionDecision{Decision: "deny"})

	got := readOneLine(t, pr)
	want := buildControlResponseDeny(json.RawMessage("1"), "Denied by user")
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}

// A session policy deny/allow rule short-circuits before ever registering a
// pending request or bothering a viewer — exactly like /api/permission.
func TestControlRequest_PolicyDenyShortCircuits(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1", Policy: &PermissionPolicy{DeniedTools: []string{"Bash"}}}
	p, pr := newControlTestProvider(t, session, perms)

	go p.processLine(controlRequestLine("9", "Bash", `{"command":"ls"}`, "tu_4"))

	got := readOneLine(t, pr)
	want := buildControlResponseDeny(json.RawMessage("9"), "denied by project policy")
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}

	perms.mu.Lock()
	n := len(perms.pending)
	perms.mu.Unlock()
	if n != 0 {
		t.Errorf("policy-denied request must not register a pending entry, got %d", n)
	}
}

func TestControlRequest_PolicyAllowShortCircuits(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1", Policy: &PermissionPolicy{AllowedTools: []string{"Read"}}}
	p, pr := newControlTestProvider(t, session, perms)

	go p.processLine(controlRequestLine("9", "Read", `{"path":"/tmp/x"}`, "tu_5"))

	got := readOneLine(t, pr)
	want := buildControlResponseAllow(json.RawMessage("9"), json.RawMessage(`{"path":"/tmp/x"}`))
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}

// Any subtype other than can_use_tool gets the "unsupported" error response
// so the CLI's stdio permission channel never blocks on a request type this
// provider doesn't implement.
func TestControlRequest_UnsupportedSubtype(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1"}
	p, pr := newControlTestProvider(t, session, perms)

	raw, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": 5,
		"request":    map[string]any{"subtype": "interrupt"},
	})
	go p.processLine(raw)

	got := readOneLine(t, pr)
	want := buildControlResponseUnsupported(json.RawMessage("5"))
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}

// Kill denies every pending control_request for the session instead of
// leaving it to burn its 60s timeout.
func TestClaudeProvider_Kill_DeniesAllPendingControlRequests(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1"}
	p, pr := newControlTestProvider(t, session, perms)

	p.processLine(controlRequestLine("1", "Write", `{}`, "tu_1"))
	firstPendingPermissionID(t, perms) // sanity: exactly one pending

	p.Kill() // cmd is nil, so Kill returns right after DenyAllForSession

	got := readOneLine(t, pr)
	want := buildControlResponseDeny(json.RawMessage("1"), "session stopped")
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}

// permission_response (the WS message Eve sends for both the hook and the
// control_request path) resolves a control_request-registered entry through
// the exact same PermissionManager.Resolve call the hook path uses — no
// separate resolution mechanism needed.
func TestControlRequest_ResolvesThroughSharedPermissionManager(t *testing.T) {
	perms := NewPermissionManager()
	session := &Session{ID: "sess-1"}
	p, pr := newControlTestProvider(t, session, perms)

	p.processLine(controlRequestLine("1", "Edit", `{"path":"/x"}`, "tu_1"))
	id := firstPendingPermissionID(t, perms)

	if ok := perms.Resolve(id, PermissionDecision{Decision: "allow"}); !ok {
		t.Fatal("Resolve reported the id as not found")
	}
	got := readOneLine(t, pr)
	want := buildControlResponseAllow(json.RawMessage("1"), json.RawMessage(`{"path":"/x"}`))
	if string(got) != string(want) {
		t.Errorf("control_response =\n%s\nwant\n%s", got, want)
	}
}
