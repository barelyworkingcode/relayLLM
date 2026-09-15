// Command relayllm is the standalone LLM engine service entry point.
// All flag parsing, wiring, and server lifecycle live in internal/app.
package main

import (
	"os"

	"relayllm/internal/app"
)

func main() {
	// The one non-flag subcommand this binary has (C10): `relayllm router-key
	// <add|list|revoke>` manages {dataDir}/router_keys.json offline, never as
	// part of the normal server-start flow below. Any other invocation
	// (including no arguments, and any argument starting with "-") falls
	// through to app.Main's ordinary flag parsing, unchanged.
	if len(os.Args) > 1 && os.Args[1] == "router-key" {
		app.RouterKeyMain(os.Args[2:])
		return
	}
	app.Main()
}
