package main

import (
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
	lmStudioURL := flag.String("lmstudio-url", envOrDefault("LM_STUDIO_URL", "http://localhost:1234"), "LM Studio base URL")
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

	killPortHolder(*port)
	slog.Info("starting relayLLM", "port", *port, "dataDir", *dataDir)

	store := NewProjectStore(filepath.Join(*dataDir, "projects.json"))
	if err := store.Load(); err != nil {
		slog.Error("failed to load projects", "error", err)
	}

	sessionStore := NewSessionStore(filepath.Join(*dataDir, "sessions"))
	perms := NewPermissionManager()
	sessions := NewSessionManager(store, sessionStore, perms)


	wsHub := NewWSHub(sessions, perms)
	sessions.SetEventSink(wsHub)
	perms.sink = wsHub

	// Set the hook URL so providers know where to send permission requests.
	sessions.SetHookURL(fmt.Sprintf("http://localhost:%s", *port))
	sessions.SetLMStudioURL(*lmStudioURL)

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store)
	RegisterSessionRoutes(mux, sessions)
	RegisterPermissionRoutes(mux, perms)
	RegisterModelRoutes(mux, *lmStudioURL)
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
