package main

import "relayllm/internal/mcp"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/mcp now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	MCPServerConfig = mcp.MCPServerConfig
	MCPClient       = mcp.MCPClient
)

var NewMCPManager = mcp.NewMCPManager
