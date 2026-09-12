package main

import "relayllm/internal/router"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/router now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	RelayRouter        = router.RelayRouter
	ProxyConnInfo      = router.ProxyConnInfo
	RecentRequestInfo  = router.RecentRequestInfo
	VirtualTargetShape = router.VirtualTargetShape
)

const (
	connStateIdle    = router.ConnStateIdle
	connStateActive  = router.ConnStateActive
	connStateQuiet   = router.ConnStateQuiet
	connStateStalled = router.ConnStateStalled

	virtualTargetInvalid  = router.VirtualTargetInvalid
	virtualTargetEndpoint = router.VirtualTargetEndpoint
	virtualTargetAlias    = router.VirtualTargetAlias
)

var (
	StartRelayRouter      = router.StartRelayRouter
	NewRelayRouter        = router.NewRelayRouter
	candidatesForVirtual  = router.CandidatesForVirtual
	classifyVirtualTarget = router.ClassifyVirtualTarget
)
