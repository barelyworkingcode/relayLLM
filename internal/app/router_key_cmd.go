package app

// `relayllm router-key <add|list|revoke>` — C10's offline CLI for
// {dataDir}/router_keys.json. Dispatched from cmd/relayllm/main.go before
// Main's own flag parsing ever runs; the actual file format, atomic writes,
// and request-time reload live in internal/router (keys.go, auth.go).

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"relayllm/internal/relay"
	"relayllm/internal/router"
)

// maybeEnableStandaloneRouterKeys wires C10's request-time gate onto
// relayRouter, but only for a process relay did not launch — a launched
// relayLLM's sole auth story is router.sock's kernel-peer-token admission
// (C9), wired on a completely separate *http.Server this must never touch.
// Factored out of app.Main into a plain function, mirroring model_host.go's
// own rationale for doing the same to its wiring decisions, so a *testing.T
// can call this directly and prove the real guard is exercised rather than
// asserting against a second copy of the condition.
func maybeEnableStandaloneRouterKeys(relayRouter *router.RelayRouter, dataDir string) {
	if relay.Launched() {
		return
	}
	relayRouter.EnableStandaloneRouterKeys(router.RouterKeysPath(dataDir))
}

// RouterKeyMain implements the router-key subcommand. args is os.Args with
// the program name and the "router-key" argument already stripped off.
func RouterKeyMain(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: relayllm router-key <add|list|revoke> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "add":
		routerKeyAddMain(args[1:])
	case "list":
		routerKeyListMain(args[1:])
	case "revoke":
		routerKeyRevokeMain(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "relayllm router-key: unknown subcommand %q (want add, list, or revoke)\n", args[0])
		os.Exit(2)
	}
}

// routerKeyDataDirDefault mirrors Main's own --data-dir default (envOrDefault
// + os.UserConfigDir fallback + the "relayLLM" directory name) exactly, so a
// bare `relayllm router-key add --label x` manages the same file the server
// itself will read with no flags of its own.
func routerKeyDataDirDefault() string {
	if v := os.Getenv("RELAY_LLM_DATA"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	return filepath.Join(dir, "relayLLM")
}

func routerKeyAddMain(args []string) {
	fs := flag.NewFlagSet("relayllm router-key add", flag.ExitOnError)
	label := fs.String("label", "", "label for the new router key (required)")
	dataDir := fs.String("data-dir", "", "data directory (default: RELAY_LLM_DATA, else ~/.config/relayLLM)")
	fs.Parse(args)
	if *label == "" {
		fmt.Fprintln(os.Stderr, "relayllm router-key add: --label is required")
		os.Exit(2)
	}
	dir := *dataDir
	if dir == "" {
		dir = routerKeyDataDirDefault()
	}

	plaintext, err := router.AddRouterKey(dir, *label)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayllm router-key add: %v\n", err)
		os.Exit(1)
	}
	// This is the ONLY place the plaintext key is ever available — it is
	// never recoverable after this process exits, and never written to any
	// file or log. stdout only, exactly once, and nothing else on this line.
	fmt.Fprintln(os.Stderr, "save this key now; it cannot be shown again (only its hash is stored):")
	fmt.Println(plaintext)
}

func routerKeyListMain(args []string) {
	fs := flag.NewFlagSet("relayllm router-key list", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "data directory (default: RELAY_LLM_DATA, else ~/.config/relayLLM)")
	fs.Parse(args)
	dir := *dataDir
	if dir == "" {
		dir = routerKeyDataDirDefault()
	}

	keys, err := router.ListRouterKeys(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relayllm router-key list: %v\n", err)
		os.Exit(1)
	}
	if len(keys) == 0 {
		fmt.Println("no router keys configured")
		return
	}
	for _, k := range keys {
		fmt.Printf("%s\t%s\t%s\n", k.Label, k.Created, k.HashPrefix)
	}
}

func routerKeyRevokeMain(args []string) {
	fs := flag.NewFlagSet("relayllm router-key revoke", flag.ExitOnError)
	label := fs.String("label", "", "label to revoke (required)")
	dataDir := fs.String("data-dir", "", "data directory (default: RELAY_LLM_DATA, else ~/.config/relayLLM)")
	fs.Parse(args)
	if *label == "" {
		fmt.Fprintln(os.Stderr, "relayllm router-key revoke: --label is required")
		os.Exit(2)
	}
	dir := *dataDir
	if dir == "" {
		dir = routerKeyDataDirDefault()
	}

	if err := router.RevokeRouterKey(dir, *label); err != nil {
		fmt.Fprintf(os.Stderr, "relayllm router-key revoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("revoked %q\n", *label)
}
