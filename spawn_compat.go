package main

import "relayllm/internal/spawn"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/spawn now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	RelayManagedSpec = spawn.RelayManagedSpec
	SpawnSubs        = spawn.SpawnSubs
)

var (
	applyEnvPassthrough = spawn.ApplyEnvPassthrough
	setProjectTokenEnv  = spawn.SetProjectTokenEnv
	childBaseEnv        = spawn.ChildBaseEnv
	resolveProjectToken = spawn.ResolveProjectToken
	setEnv              = spawn.SetEnv
	ensurePath          = spawn.EnsurePath
	resolveClaudePath   = spawn.ResolveClaudePath
	hasArg              = spawn.HasArg
)
