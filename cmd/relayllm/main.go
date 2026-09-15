// Command relayllm is the standalone LLM engine service entry point.
// All flag parsing, wiring, and server lifecycle live in internal/app.
package main

import "relayllm/internal/app"

func main() {
	app.Main()
}
