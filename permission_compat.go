package main

import "relayllm/internal/permission"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/permission now owns these definitions; every remaining
// package-main file keeps referring to the unqualified names until it
// migrates to its own package (each migration step qualifies its own files'
// references and, once this file's aliases are unused, deletes this file).
type (
	PermissionManager  = permission.PermissionManager
	PermissionDecision = permission.PermissionDecision
	PermissionRequest  = permission.PermissionRequest
)

var (
	NewPermissionManager = permission.NewPermissionManager
	MatchToolRule        = permission.MatchToolRule
)
