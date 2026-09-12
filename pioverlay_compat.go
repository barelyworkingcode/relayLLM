package main

import "relayllm/internal/pioverlay"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/pioverlay now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	PiOverlayInputs = pioverlay.PiOverlayInputs
	PiRouterModel   = pioverlay.PiRouterModel
)

const piRelayRouterProvider = pioverlay.RelayRouterProvider

var (
	MaterializePiOverlay = pioverlay.MaterializePiOverlay
	applyPiOverlayEnv    = pioverlay.ApplyPiOverlayEnv
)
