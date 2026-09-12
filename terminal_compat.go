package main

import "relayllm/internal/terminal"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/terminal now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	TerminalManager = terminal.TerminalManager
	TerminalSession = terminal.TerminalSession
	TemplateStore   = terminal.TemplateStore
	TerminalSummary = terminal.TerminalSummary
)

var (
	NewTerminalManager     = terminal.NewTerminalManager
	NewTemplateStore       = terminal.NewTemplateStore
	ResolveTemplateCommand = terminal.ResolveTemplateCommand
	OpenTerminalLogReaders = terminal.OpenTerminalLogReaders
	ErrTerminalLogNotFound = terminal.ErrTerminalLogNotFound
	SweepTerminalLogs      = terminal.SweepTerminalLogs
)
