package main

import "relayllm/internal/tools"

// Bridge aliases for the ongoing package-split migration (see ROADMAP.md).
// internal/tools now owns these definitions; every remaining package-main
// file keeps referring to the unqualified names until it migrates to its own
// package (each migration step qualifies its own files' references and, once
// this file's aliases are unused, deletes this file).
type BuiltinToolDef = tools.BuiltinToolDef
type BuiltinToolRegistry = tools.BuiltinToolRegistry

var NewBuiltinToolRegistry = tools.NewBuiltinToolRegistry
