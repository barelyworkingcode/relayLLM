package main

import "relayllm/internal/provider"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/provider now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	ClaudeProvider   = provider.ClaudeProvider
	PiProvider       = provider.PiProvider
	BaseChatProvider = provider.BaseChatProvider
)

var (
	NewClaudeProvider       = provider.NewClaudeProvider
	NewPiProvider           = provider.NewPiProvider
	NewBaseChatProvider     = provider.NewBaseChatProvider
	NewOllamaChatTransport  = provider.NewOllamaChatTransport
	NewOpenAIChatTransport  = provider.NewOpenAIChatTransport
	NewManagedChatTransport = provider.NewManagedChatTransport
	ReadClaudeHistory       = provider.ReadClaudeHistory
	FetchOllamaModels       = provider.FetchOllamaModels
	FetchPiModels           = provider.FetchPiModels
	ProviderSettings        = provider.ProviderSettings
)
