package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	port := flag.String("port", envOrDefault("RELAY_LLM_PORT", "3001"), "HTTP/WebSocket listen port")
	dataDir := flag.String("data-dir", envOrDefault("RELAY_LLM_DATA", ""), "Data directory (default: ~/.config/relayLLM)")
	ollamaURL := flag.String("ollama-url", envOrDefault("OLLAMA_URL", "http://localhost:11434"), "Ollama base URL")
	openaiConfigPath := flag.String("openai-config", envOrDefault("OPENAI_CONFIG", ""), "Path to OpenAI-compatible endpoints config JSON (default: {data-dir}/openai_endpoints.json)")
	schedulerURL := flag.String("scheduler-url", envOrDefault("RELAY_SCHEDULER_URL", "http://localhost:3002"), "relayScheduler base URL")
	apiToken := flag.String("token", envOrDefault("RELAY_LLM_TOKEN", ""), "Bearer token required on every HTTP request and WS upgrade. Empty = dev mode (NO AUTH).")
	socketPath := flag.String("socket", envOrDefault("RELAY_LLM_SOCKET", ""), "Optional Unix domain socket path. When set, relayLLM serves the same HTTP/WS handler from this socket (mode 0600) in addition to the TCP listener.")
	flag.Parse()

	if *dataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			dir, _ = os.UserHomeDir()
		}
		*dataDir = filepath.Join(dir, "relayLLM")
	}
	if err := os.MkdirAll(*dataDir, 0700); err != nil {
		slog.Error("failed to create data directory", "path", *dataDir, "error", err)
		os.Exit(1)
	}

	// Fail-closed startup validation for the relay channel.
	// See eve/plans/cozy-honking-toast.md Section B for the threat model.
	if *socketPath != "" && *apiToken == "" {
		slog.Error("RELAY_LLM_SOCKET is set but RELAY_LLM_TOKEN is missing — refusing to start")
		os.Exit(1)
	}
	if *apiToken == "" {
		slog.Warn("RELAY_LLM_TOKEN is not set — running with NO AUTH on the relay channel. Safe only for local dev on loopback. Set RELAY_LLM_TOKEN to enable authentication.")
	} else {
		slog.Info("relay channel: bearer token enabled")
	}

	killPortHolder(*port)
	slog.Info("starting relayLLM", "port", *port, "dataDir", *dataDir)

	store := NewProjectStore(filepath.Join(*dataDir, "projects.json"))
	if err := store.Load(); err != nil {
		slog.Error("failed to load projects", "error", err)
	}

	sessionStore := NewSessionStore(filepath.Join(*dataDir, "sessions"))
	perms := NewPermissionManager()
	sessions := NewSessionManager(store, sessionStore, perms)

	// Terminal subsystem.
	templateStore := NewTemplateStore(filepath.Join(*dataDir, "terminals", "templates.json"))
	if err := templateStore.Load(); err != nil {
		slog.Error("failed to load terminal templates", "error", err)
	}
	terminalMgr := NewTerminalManager(templateStore)

	wsHub := NewWSHub(sessions, perms, terminalMgr)
	sessions.SetEventSink(wsHub)
	perms.SetEventSink(wsHub)

	// Wire terminal I/O to WebSocket hub.
	terminalMgr.SetOutputHandler(func(terminalID string, data []byte) {
		wsHub.SendToTerminal(terminalID, map[string]interface{}{
			"type":       "terminal_output",
			"terminalId": terminalID,
			"data":       base64.StdEncoding.EncodeToString(data),
		})
	})
	terminalMgr.SetExitHandler(func(terminalID string, exitCode int) {
		wsHub.SendToTerminal(terminalID, map[string]interface{}{
			"type":       "terminal_exit",
			"terminalId": terminalID,
			"exitCode":   exitCode,
		})
	})

	// Set the hook URL so providers know where to send permission requests.
	// The hook binary runs inside the LLM provider's child process and POSTs
	// to /api/permission. When auth is enabled, the hook needs the same
	// bearer token relayLLM uses to validate inbound calls — we plumb it
	// through SessionManager → ClaudeProvider → child env.
	sessions.SetHookURL(fmt.Sprintf("http://localhost:%s", *port))
	sessions.SetHookToken(*apiToken)
	sessions.SetOllamaURL(*ollamaURL)

	// Load OpenAI-compatible endpoints. Default to {dataDir}/openai_endpoints.json.
	// Falls back to OPENAI_BASE_URL / OPENAI_API_KEY env vars if the file is absent.
	openaiPath := *openaiConfigPath
	if openaiPath == "" {
		openaiPath = filepath.Join(*dataDir, "openai_endpoints.json")
	}
	openaiCfg, err := LoadOpenAIConfig(openaiPath)
	if err != nil {
		slog.Error("failed to load openai config", "path", openaiPath, "error", err)
		os.Exit(1)
	}
	if len(openaiCfg.Endpoints) > 0 {
		names := openaiCfg.Names()
		slog.Info("openai endpoints loaded", "count", len(openaiCfg.Endpoints), "names", names)
	}
	sessions.SetOpenAIConfig(openaiCfg)

	schedulerClient := NewSchedulerClient(*schedulerURL)

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, schedulerClient)
	RegisterSessionRoutes(mux, sessions)
	RegisterTerminalRoutes(mux, templateStore, terminalMgr)
	RegisterPermissionRoutes(mux, perms)
	RegisterModelRoutes(mux, *ollamaURL, openaiCfg)
	RegisterSchedulerProxyRoutes(mux, schedulerClient)
	mux.HandleFunc("/ws", wsHub.HandleUpgrade)

	// Forward scheduler WebSocket events to all connected clients.
	schedulerWS := NewSchedulerWSForwarder(*schedulerURL, wsHub)
	go schedulerWS.Run()

	// Build the handler chain. recoverMiddleware sits closest to the mux so it
	// catches panics from real handlers; bearerAuth sits in front so unauth
	// requests never reach a real handler (and never allocate a WS session).
	handler := bearerAuth(*apiToken, recoverMiddleware(mux))

	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", *port),
		Handler: handler,
	}

	// Optional Unix domain socket listener — preferred transport when running
	// under the relay orchestrator. Same handler, same auth chain. Kernel file
	// permissions (0600) anchor authorization; the bearer token is defense-
	// in-depth.
	var unixListener net.Listener
	if *socketPath != "" {
		// Ensure the parent dir exists with restrictive perms.
		if err := os.MkdirAll(filepath.Dir(*socketPath), 0o700); err != nil {
			slog.Error("failed to create socket parent dir", "path", *socketPath, "error", err)
			os.Exit(1)
		}
		// Remove any stale socket file from a previous crashed run.
		_ = os.Remove(*socketPath)
		ln, err := net.Listen("unix", *socketPath)
		if err != nil {
			slog.Error("failed to listen on unix socket", "path", *socketPath, "error", err)
			os.Exit(1)
		}
		// Tighten perms before serving — most umasks already give 0600 for
		// sockets but be explicit.
		if err := os.Chmod(*socketPath, 0o600); err != nil {
			slog.Warn("failed to chmod unix socket", "path", *socketPath, "error", err)
		}
		unixListener = ln
		slog.Info("relay unix socket listening", "path", *socketPath)

		go func() {
			if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
				slog.Error("unix socket serve error", "error", err)
			}
		}()
	}

	// Graceful shutdown: drain HTTP requests, then clean up providers and terminals.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		if unixListener != nil {
			_ = unixListener.Close()
			_ = os.Remove(*socketPath)
		}
	}()

	slog.Info("listening", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Server stopped — clean up background resources.
	// (Unix socket is already unlinked by the shutdown goroutine above.)
	schedulerWS.Close()
	sessions.StopAll()
	terminalMgr.StopAll()
	slog.Info("shutdown complete")
}

// killPortHolder checks if the port is already in use and kills the holding process.
func killPortHolder(port string) {
	ln, err := net.Listen("tcp", ":"+port)
	if err == nil {
		ln.Close()
		return // port is free
	}

	// Port is occupied. Find and kill the holder via lsof.
	out, err := exec.Command("lsof", "-ti", ":"+port).Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, pidStr := range strings.Fields(strings.TrimSpace(string(out))) {
		var pid int
		if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid == os.Getpid() {
			continue
		}
		slog.Info("killing stale process on port", "port", port, "pid", pid)
		syscall.Kill(pid, syscall.SIGTERM)
	}

	// Brief wait for the port to free up.
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		ln, err := net.Listen("tcp", ":"+port)
		if err == nil {
			ln.Close()
			return
		}
	}
	slog.Warn("port still occupied after killing stale process", "port", port)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
