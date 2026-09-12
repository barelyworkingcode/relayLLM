package main

import (
	"encoding/json"

	"relayllm/internal/types"
)

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/types now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file). Type aliases (not new
// types) so identity is preserved across the package boundary — a
// *Session built here is a *types.Session, not a distinct type.
type (
	Session              = types.Session
	Provider             = types.Provider
	FileAttachment       = types.FileAttachment
	Message              = types.Message
	SessionStats         = types.SessionStats
	EventHandler         = types.EventHandler
	EventSink            = types.EventSink
	PermissionPolicy     = types.PermissionPolicy
	HostSpec             = types.HostSpec
	ProviderCapabilities = types.ProviderCapabilities
)

var (
	CapabilitiesForProvider = types.CapabilitiesForProvider
)

func extractTextContent(msg Message) string {
	return types.ExtractTextContent(msg)
}

func flattenTextBlocks(raw json.RawMessage) string {
	return types.FlattenTextBlocks(raw)
}
