package api

// WebSocket protocol coverage. Each subtest exercises one inbound message
// type and asserts the resulting server-side state changes + outbound events.
// Drives the full relayLLM stack via TestServer; the only fake is the LLM
// provider (testutil.FakeProvider) so we can script event sequences deterministically.

import (
	"encoding/base64"
	"encoding/json"
	"relayllm/internal/events"
	"relayllm/internal/testutil"
	"relayllm/internal/types"
	"slices"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Session-side protocol
// ---------------------------------------------------------------------------

func TestWS_JoinSession_KnownSession_ReturnsSessionJoined(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(map[string]interface{}{"name": "joined-session"})

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	msg := ReadUntilType(t, conn, "session_joined", 2*time.Second)

	if msg["sessionId"] != sessionID {
		t.Errorf("sessionId: got %v, want %s", msg["sessionId"], sessionID)
	}
	if msg["name"] != "joined-session" {
		t.Errorf("name: got %v, want %q", msg["name"], "joined-session")
	}
	if _, ok := msg["history"]; !ok {
		t.Error("session_joined missing history field")
	}
	if _, ok := msg["stats"]; !ok {
		t.Error("session_joined missing stats field")
	}
	if _, ok := msg["protocolVersion"]; !ok {
		t.Error("session_joined missing protocolVersion field")
	}
}

func TestWS_JoinSession_UnknownSession_ReturnsError(t *testing.T) {
	srv := NewTestServer(t, nil)
	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": "does-not-exist"})

	msg := ReadUntilType(t, conn, "error", 2*time.Second)
	if !strings.Contains(strOf(msg["message"]), "session not found") {
		t.Errorf("error message: got %v", msg["message"])
	}
}

func TestWS_SendMessage_StreamsScriptedEvents(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	fp.ScriptText("hello back")
	fp.ScriptResult("end_turn", types.SessionStats{InputTokens: 4, OutputTokens: 2})

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	WSSend(t, conn, map[string]interface{}{"type": "send_message", "sessionId": sessionID, "text": "hello"})

	// Collect events until message_complete.
	var seenTypes []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg := ReadWSEvent(t, conn)
		seenTypes = append(seenTypes, strOf(msg["type"]))
		if msg["type"] == events.HandlerMessageComplete {
			break
		}
	}
	if !slices.Contains(seenTypes, events.HandlerLLMEvent) || !slices.Contains(seenTypes, events.HandlerMessageComplete) {
		t.Errorf("expected at least one llm_event + message_complete, got %v", seenTypes)
	}
	if sent := fp.Sent(); len(sent) != 1 || sent[0].Text != "hello" {
		t.Errorf("provider Sent: %+v", sent)
	}
}

func TestWS_SendMessage_MissingSessionID_ReturnsError(t *testing.T) {
	srv := NewTestServer(t, nil)
	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "send_message", "text": "hi"})

	msg := ReadUntilType(t, conn, "error", 2*time.Second)
	if !strings.Contains(strOf(msg["message"]), "sessionId") {
		t.Errorf("error message: got %v", msg["message"])
	}
}

func TestWS_StopGeneration_CallsProviderStop(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "stop_generation", "sessionId": sessionID})

	testutil.WaitFor(t, 1*time.Second, func() bool { return fp.Stopped() })
}

func TestWS_RenameSession_UpdatesName(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(map[string]interface{}{"name": "original"})

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{
		"type": "rename_session", "sessionId": sessionID, "name": "renamed",
	})

	testutil.WaitFor(t, 1*time.Second, func() bool {
		sess, ok := srv.Sessions.GetSession(sessionID)
		if !ok {
			return false
		}
		// RenameSession mutates Name under session.mu on the WS handler
		// goroutine; read it under the same lock so -race stays clean.
		sess.Lock()
		defer sess.Unlock()
		return sess.Name == "renamed"
	})
}

func TestWS_EndSession_StopsProvider(t *testing.T) {
	srv := NewTestServer(t, nil)
	fp := srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "end_session", "sessionId": sessionID})

	// EndSession kills the provider — wait for that to land.
	testutil.WaitFor(t, 1*time.Second, func() bool { return fp.Killed() })
}

func TestWS_DeleteSession_RemovesSessionAndNotifies(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	WSSend(t, conn, map[string]interface{}{"type": "delete_session", "sessionId": sessionID})
	msg := ReadUntilType(t, conn, "session_ended", 2*time.Second)
	if msg["sessionId"] != sessionID {
		t.Errorf("session_ended sessionId: got %v", msg["sessionId"])
	}

	if _, ok := srv.Sessions.GetSession(sessionID); ok {
		t.Error("session still present after delete")
	}
}

func TestWS_ClearSession_WipesHistory(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	// Manually inject a message to clear.
	sess, _ := srv.Sessions.GetSession(sessionID)
	sess.Lock()
	sess.Messages = []types.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}
	sess.Unlock()

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "clear_session", "sessionId": sessionID})

	testutil.WaitFor(t, 1*time.Second, func() bool {
		sess.Lock()
		defer sess.Unlock()
		return len(sess.Messages) == 0
	})
}

func TestWS_LeaveSession_StopsReceivingBroadcasts(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	WSSend(t, conn, map[string]interface{}{"type": "leave_session", "sessionId": sessionID})

	// After leaving, a session-targeted broadcast must not reach this conn.
	// Wait briefly to let the leave land, then SendToSession and assert no read.
	time.Sleep(50 * time.Millisecond)
	srv.WSHub.SendToSession(sessionID, map[string]interface{}{"type": "post_leave_marker"})

	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("ReadMessage returned a message after leave_session")
	}
}

func TestWS_PermissionResponse_ResolvesPendingRequest(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()

	// Create a permission request directly via the manager (no provider needed).
	_, ch := srv.Perms.CreateRequest("sess", "Read", `{"path":"/etc/passwd"}`, "tool-use-1")
	pending := srv.pendingPermissionID(t)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{
		"type":         "permission_response",
		"permissionId": pending,
		"approved":     true,
		"reason":       "ok",
	})

	select {
	case d := <-ch:
		if d.Decision != "allow" || d.Reason != "ok" {
			t.Errorf("decision: %+v", d)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("permission decision never delivered")
	}
}

func TestWS_SetPermissionMode_NotSupported_ForFakeProvider(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{
		"type": "set_permission_mode", "sessionId": sessionID, "mode": "ask",
	})

	msg := ReadUntilType(t, conn, "error", 2*time.Second)
	if !strings.Contains(strOf(msg["message"]), "permission mode") {
		t.Errorf("expected permission-mode error, got %v", msg["message"])
	}
}

// ---------------------------------------------------------------------------
// Terminal-side protocol — relies on a real PTY via the built-in shell template
// ---------------------------------------------------------------------------

func TestWS_TerminalList_ReturnsEmpty_WhenNoTerminalsCreated(t *testing.T) {
	srv := NewTestServer(t, nil)
	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "terminal_list"})
	msg := ReadUntilType(t, conn, "terminal_list", 2*time.Second)
	if _, ok := msg["terminals"]; !ok {
		t.Errorf("terminal_list missing terminals field: %v", msg)
	}
}

func TestWS_TerminalTemplates_ReturnsBuiltins(t *testing.T) {
	srv := NewTestServer(t, nil)
	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "terminal_templates"})
	msg := ReadUntilType(t, conn, "terminal_templates", 2*time.Second)
	templates, ok := msg["templates"].([]interface{})
	if !ok || len(templates) == 0 {
		t.Fatalf("expected non-empty templates list: %v", msg)
	}
}

func TestWS_TerminalCreate_AndInput_AndClose(t *testing.T) {
	srv := NewTestServer(t, nil)
	conn := srv.DialWS()

	// Use the built-in shell template (seeded by TemplateStore.Load).
	WSSend(t, conn, map[string]interface{}{
		"type": "terminal_create", "templateId": "shell",
		"name": "test", "directory": srv.DataDir,
		"cols": 80, "rows": 24,
	})
	// Server sends terminal_joined first (with scrollback), then terminal_created.
	// Both carry the terminalId.
	joined := ReadUntilType(t, conn, "terminal_joined", 2*time.Second)
	terminalID := strOf(joined["terminalId"])
	if terminalID == "" {
		t.Fatalf("terminal_joined missing terminalId: %v", joined)
	}
	ReadUntilType(t, conn, "terminal_created", 2*time.Second)

	// Send input — base64-encoded "exit 7\n".
	input := base64.StdEncoding.EncodeToString([]byte("exit 7\n"))
	WSSend(t, conn, map[string]interface{}{
		"type": "terminal_input", "terminalId": terminalID, "data": input,
	})

	// Expect terminal_exit eventually.
	exit := ReadUntilType(t, conn, "terminal_exit", 3*time.Second)
	if exit["terminalId"] != terminalID {
		t.Errorf("exit terminalId: got %v, want %s", exit["terminalId"], terminalID)
	}
}

// ---------------------------------------------------------------------------
// SnapshotConnections (status_metrics.go / GET /api/status/detailed feed)
// ---------------------------------------------------------------------------

func TestWSConn_CountersAndIdle(t *testing.T) {
	clock := testutil.NewFakeClock(time.Unix(1_700_000_000, 0))
	srv := NewTestServer(t, &TestServerOptions{Clock: clock})
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	// join_session's read plus session_joined's write must both be counted —
	// give the server goroutine a moment to process the read before polling.
	testutil.WaitFor(t, time.Second, func() bool {
		snap := srv.WSHub.SnapshotConnections()
		return len(snap) == 1 && snap[0].BytesIn > 0 && snap[0].BytesOut > 0
	})

	snap := srv.WSHub.SnapshotConnections()
	if len(snap) != 1 {
		t.Fatalf("SnapshotConnections = %+v, want exactly 1 row", snap)
	}
	if snap[0].State != "active" {
		t.Errorf("state right after activity = %q, want active", snap[0].State)
	}

	// Advance well past wsIdleAfter (60s) with no further traffic.
	clock.Advance(90 * time.Second)
	snap = srv.WSHub.SnapshotConnections()
	if len(snap) != 1 {
		t.Fatalf("SnapshotConnections after advance = %+v, want exactly 1 row", snap)
	}
	if snap[0].State != "idle" {
		t.Errorf("state after 90s idle = %q, want idle (never stalled — see wsIdleAfter's comment)", snap[0].State)
	}
	if snap[0].IdleSeconds < 90 {
		t.Errorf("IdleSeconds = %d, want >= 90", snap[0].IdleSeconds)
	}
}

func TestWSHub_SnapshotBindings(t *testing.T) {
	srv := NewTestServer(t, nil)
	srv.SetFakeProvider()
	sessionID := srv.CreateSession(nil)

	conn := srv.DialWS()
	WSSend(t, conn, map[string]interface{}{"type": "join_session", "sessionId": sessionID})
	ReadUntilType(t, conn, "session_joined", 2*time.Second)

	WSSend(t, conn, map[string]interface{}{
		"type": "terminal_create", "templateId": "shell",
		"name": "bindings-test", "directory": srv.DataDir,
		"cols": 80, "rows": 24,
	})
	joined := ReadUntilType(t, conn, "terminal_joined", 2*time.Second)
	terminalID := strOf(joined["terminalId"])
	ReadUntilType(t, conn, "terminal_created", 2*time.Second)

	testutil.WaitFor(t, time.Second, func() bool {
		snap := srv.WSHub.SnapshotConnections()
		return len(snap) == 1 && len(snap[0].Sessions) == 1 && len(snap[0].Terminals) == 1
	})

	snap := srv.WSHub.SnapshotConnections()
	if len(snap) != 1 {
		t.Fatalf("SnapshotConnections = %+v, want exactly 1 row", snap)
	}
	if len(snap[0].Sessions) != 1 || snap[0].Sessions[0] != sessionID {
		t.Errorf("Sessions = %v, want [%s]", snap[0].Sessions, sessionID)
	}
	if len(snap[0].Terminals) != 1 || snap[0].Terminals[0] != terminalID {
		t.Errorf("Terminals = %v, want [%s]", snap[0].Terminals, terminalID)
	}

	// Leave the session; the terminal binding must survive independently.
	WSSend(t, conn, map[string]interface{}{"type": "leave_session", "sessionId": sessionID})
	testutil.WaitFor(t, time.Second, func() bool {
		snap := srv.WSHub.SnapshotConnections()
		return len(snap) == 1 && len(snap[0].Sessions) == 0
	})
	snap = srv.WSHub.SnapshotConnections()
	if len(snap[0].Terminals) != 1 || snap[0].Terminals[0] != terminalID {
		t.Errorf("Terminals after leaving the session = %v, want [%s] (unaffected)", snap[0].Terminals, terminalID)
	}

	// Disconnecting entirely must drop the connection (and so its bindings).
	conn.Close()
	testutil.WaitFor(t, time.Second, func() bool {
		return len(srv.WSHub.SnapshotConnections()) == 0
	})
}

// ---------------------------------------------------------------------------
// Helpers specific to ws_test
// ---------------------------------------------------------------------------

// pendingPermissionID returns the single in-flight permission ID, failing the
// test if there isn't exactly one. Used to grab the auto-generated UUID after
// CreateRequest without exposing internal map state to every test.
func (s *TestServer) pendingPermissionID(t *testing.T) string {
	t.Helper()
	ids := s.Perms.PendingIDs()
	if len(ids) != 1 {
		t.Fatalf("expected exactly 1 pending permission, got %d", len(ids))
	}
	return ids[0]
}

func strOf(v interface{}) string {
	s, _ := v.(string)
	return s
}
