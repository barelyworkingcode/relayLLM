package main

import "relayllm/internal/relay"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/relay now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type (
	relayBridgeRequest           = relay.BridgeRequest
	relayBridgeResponse          = relay.BridgeResponse
	RelayPtyEnvRequest           = relay.RelayPtyEnvRequest
	RelayPtyEnvResponse          = relay.RelayPtyEnvResponse
	RelayProjectTemplateRequest  = relay.RelayProjectTemplateRequest
	RelayProjectTemplateResponse = relay.RelayProjectTemplateResponse
)

const (
	envBridgeSocket       = relay.EnvBridgeSocket
	envServiceID          = relay.EnvServiceID
	envFrontendToken      = relay.EnvFrontendToken
	envServiceToken       = relay.EnvServiceToken
	envServiceTokenLegacy = relay.EnvServiceTokenLegacy
	envProjectToken       = relay.EnvProjectToken
	envProjectTokenLegacy = relay.EnvProjectTokenLegacy

	reqResolvePtyEnv          = relay.ReqResolvePtyEnv
	reqResolveProjectTemplate = relay.ReqResolveProjectTemplate
	reqRegisterManifest       = relay.ReqRegisterManifest
	respError                 = relay.RespError
	respPtyEnv                = relay.RespPtyEnv
	respProjectTemplate       = relay.RespProjectTemplate
	respOK                    = relay.RespOK
)

var (
	serviceToken                = relay.ServiceToken
	sendBridgeRequest           = relay.SendBridgeRequest
	resolveRelayPtyEnv          = relay.ResolvePtyEnv
	resolveRelayProjectTemplate = relay.ResolveProjectTemplate
	maybeRegisterManifest       = relay.MaybeRegisterManifest
)
