package main

import "relayllm/internal/registry"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/registry now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	ProxyRegistry  = registry.ProxyRegistry
	EndpointStatus = registry.EndpointStatus
	UpstreamModel  = registry.UpstreamModel
)

var (
	NewProxyRegistry  = registry.NewProxyRegistry
	FetchOpenAIModels = registry.FetchOpenAIModels
)
