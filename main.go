package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	port := flag.String("port", envOrDefault("RELAY_LLM_PORT", "3001"), "HTTP/WebSocket listen port")
	dataDir := flag.String("data-dir", envOrDefault("RELAY_LLM_DATA", ""), "Data directory (default: ~/.config/relayLLM)")
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

	slog.Info("starting relayLLM", "port", *port, "dataDir", *dataDir)

	store := NewProjectStore(filepath.Join(*dataDir, "projects.json"))
	if err := store.Load(); err != nil {
		slog.Error("failed to load projects", "error", err)
	}

	sessionStore := NewSessionStore(filepath.Join(*dataDir, "sessions"))
	perms := NewPermissionManager()
	sessions := NewSessionManager(store, sessionStore, perms)

	// Restore persisted sessions.
	sessions.RestoreAll()

	wsHub := NewWSHub(sessions, perms)
	sessions.SetEventSink(wsHub)
	perms.sink = wsHub

	// Set the hook URL so providers know where to send permission requests.
	sessions.SetHookURL(fmt.Sprintf("http://localhost:%s", *port))

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store)
	RegisterSessionRoutes(mux, sessions)
	RegisterPermissionRoutes(mux, perms)
	mux.HandleFunc("/ws", wsHub.HandleUpgrade)

	// Graceful shutdown.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		sessions.StopAll()
		os.Exit(0)
	}()

	addr := fmt.Sprintf(":%s", *port)
	slog.Info("listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
