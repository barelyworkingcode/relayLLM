package main

import "relayllm/internal/session"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/session now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	SessionManager = session.SessionManager
	SessionStore   = session.SessionStore
)

var (
	NewSessionManager       = session.NewSessionManager
	NewSessionStore         = session.NewSessionStore
	SweepSessions           = session.SweepSessions
	SweepOrphanedPiSessions = session.SweepOrphanedPiSessions
)
