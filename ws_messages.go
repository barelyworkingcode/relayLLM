package main

// WSMsg* are the type-tags on the WebSocket wire envelope (the JSON `"type"`
// field). Inbound values drive the dispatch switch in WSHub.HandleUpgrade;
// outbound values are stamped on every server-to-client message.
//
// Distinct from the HandlerLLMEvent/HandlerStatsUpdate/HandlerMessageComplete
// constants in events.go, which are in-process handler-channel keys (they
// happen to share their wire values with the WS envelope when forwarded
// verbatim by the session layer). Distinct also from the canonical event
// stream's inner Type/Subtype (EvtSystem etc.) — those live inside the
// payload that travels under WSMsgLLMEvent / HandlerLLMEvent.
//
// Tests intentionally reference these as string literals: if a constant's
// wire value is changed by mistake, the literal in the test breaks and pins
// the contract.
const (
	// --- Client → Server (inbound) ---
	WSMsgJoinSession        = "join_session"
	WSMsgSendMessage        = "send_message"
	WSMsgEndSession         = "end_session"
	WSMsgRenameSession      = "rename_session"
	WSMsgDeleteSession      = "delete_session"
	WSMsgLeaveSession       = "leave_session"
	WSMsgStopGeneration     = "stop_generation"
	WSMsgClearSession       = "clear_session"
	WSMsgPermissionResponse = "permission_response"
	WSMsgSetPermissionMode  = "set_permission_mode"
	WSMsgTerminalCreate     = "terminal_create"
	WSMsgJoinTerminal       = "join_terminal"
	WSMsgLeaveTerminal      = "leave_terminal"
	WSMsgTerminalInput      = "terminal_input"
	WSMsgTerminalResize     = "terminal_resize"
	WSMsgTerminalClose      = "terminal_close"
	WSMsgTerminalReconnect  = "terminal_reconnect"

	// --- Server → Client (outbound, session) ---
	WSMsgSessionJoined        = "session_joined"
	WSMsgSessionEnded         = "session_ended"
	WSMsgSessionRenamed       = "session_renamed"
	WSMsgUserMessage          = "user_message"
	WSMsgSystemMessage        = "system_message"
	WSMsgClearMessages        = "clear_messages"
	WSMsgProcessExited        = "process_exited"
	WSMsgRawOutput            = "raw_output"
	WSMsgModeChanged          = "mode_changed"
	WSMsgModelChanged         = "model_changed"
	WSMsgThinkingLevelChanged = "thinking_level_changed"
	WSMsgPermissionRequest    = "permission_request"
	WSMsgError                = "error"

	// --- Server → Client (outbound, terminal) ---
	WSMsgTerminalCreated = "terminal_created"
	WSMsgTerminalClosed  = "terminal_closed"
	WSMsgTerminalJoined  = "terminal_joined"
	WSMsgTerminalExit    = "terminal_exit"
	WSMsgTerminalOutput  = "terminal_output"

	// --- Bidirectional (request and response share the wire value) ---
	WSMsgTerminalList      = "terminal_list"
	WSMsgTerminalTemplates = "terminal_templates"
)
