package main

import "relayllm/internal/servermanager"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/servermanager now owns these definitions; every remaining
// package-main file keeps referring to the unqualified names until it
// migrates to its own package (each migration step qualifies its own files'
// references and, once this file's aliases are unused, deletes this file).
type (
	ServerManager      = servermanager.ServerManager
	ServerInstanceInfo = servermanager.ServerInstanceInfo
	BudgetInfo         = servermanager.BudgetInfo
	ManagedModelInfo   = servermanager.ManagedModelInfo
)

const (
	ModelStatusLoaded   = servermanager.ModelStatusLoaded
	ModelStatusLoading  = servermanager.ModelStatusLoading
	ModelStatusUnloaded = servermanager.ModelStatusUnloaded
)

var (
	NewServerManager = servermanager.NewServerManager
	llamaProfile     = servermanager.LlamaProfile
	mlxProfile       = servermanager.MlxProfile
)
