package main

import "relayllm/internal/events"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/events now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type EventEmitter = events.EventEmitter

var NewEventEmitter = events.NewEventEmitter
var SynthesizeToolUseID = events.SynthesizeToolUseID

const (
	ProtocolVersion    = events.ProtocolVersion
	ProtocolVersionNum = events.ProtocolVersionNum

	BlockText     = events.BlockText
	BlockThinking = events.BlockThinking
	BlockToolUse  = events.BlockToolUse

	DeltaText      = events.DeltaText
	DeltaThinking  = events.DeltaThinking
	DeltaInputJSON = events.DeltaInputJSON

	EvtSystem    = events.EvtSystem
	EvtAssistant = events.EvtAssistant
	EvtResult    = events.EvtResult

	HandlerLLMEvent        = events.HandlerLLMEvent
	HandlerStatsUpdate     = events.HandlerStatsUpdate
	HandlerMessageComplete = events.HandlerMessageComplete

	SystemInitSubtype              = events.SystemInitSubtype
	SystemPermissionRequestSubtype = events.SystemPermissionRequestSubtype
	SystemQuestionSubtype          = events.SystemQuestionSubtype
	SystemStatusSubtype            = events.SystemStatusSubtype
	SystemAPIErrorSubtype          = events.SystemAPIErrorSubtype
	SystemBridgeStatusSubtype      = events.SystemBridgeStatusSubtype
	SystemStopHookSummarySubtype   = events.SystemStopHookSummarySubtype

	WSMsgJoinSession        = events.WSMsgJoinSession
	WSMsgSendMessage        = events.WSMsgSendMessage
	WSMsgEndSession         = events.WSMsgEndSession
	WSMsgRenameSession      = events.WSMsgRenameSession
	WSMsgSetSessionFolder   = events.WSMsgSetSessionFolder
	WSMsgDeleteSession      = events.WSMsgDeleteSession
	WSMsgLeaveSession       = events.WSMsgLeaveSession
	WSMsgStopGeneration     = events.WSMsgStopGeneration
	WSMsgClearSession       = events.WSMsgClearSession
	WSMsgPermissionResponse = events.WSMsgPermissionResponse
	WSMsgSetPermissionMode  = events.WSMsgSetPermissionMode
	WSMsgTerminalCreate     = events.WSMsgTerminalCreate
	WSMsgJoinTerminal       = events.WSMsgJoinTerminal
	WSMsgLeaveTerminal      = events.WSMsgLeaveTerminal
	WSMsgTerminalInput      = events.WSMsgTerminalInput
	WSMsgTerminalResize     = events.WSMsgTerminalResize
	WSMsgTerminalClose      = events.WSMsgTerminalClose
	WSMsgTerminalReconnect  = events.WSMsgTerminalReconnect

	WSMsgSessionJoined        = events.WSMsgSessionJoined
	WSMsgSessionEnded         = events.WSMsgSessionEnded
	WSMsgSessionRenamed       = events.WSMsgSessionRenamed
	WSMsgSessionFolderChanged = events.WSMsgSessionFolderChanged
	WSMsgUserMessage          = events.WSMsgUserMessage
	WSMsgSystemMessage        = events.WSMsgSystemMessage
	WSMsgClearMessages        = events.WSMsgClearMessages
	WSMsgProcessExited        = events.WSMsgProcessExited
	WSMsgRawOutput            = events.WSMsgRawOutput
	WSMsgModeChanged          = events.WSMsgModeChanged
	WSMsgModelChanged         = events.WSMsgModelChanged
	WSMsgThinkingLevelChanged = events.WSMsgThinkingLevelChanged
	WSMsgPermissionRequest    = events.WSMsgPermissionRequest
	WSMsgError                = events.WSMsgError

	WSMsgTerminalCreated = events.WSMsgTerminalCreated
	WSMsgTerminalClosed  = events.WSMsgTerminalClosed
	WSMsgTerminalJoined  = events.WSMsgTerminalJoined
	WSMsgTerminalExit    = events.WSMsgTerminalExit
	WSMsgTerminalOutput  = events.WSMsgTerminalOutput

	WSMsgTerminalList      = events.WSMsgTerminalList
	WSMsgTerminalTemplates = events.WSMsgTerminalTemplates
)
