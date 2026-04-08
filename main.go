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
	schedulerURL := flag.String("scheduler-url", envOrDefault("RELAY_SCHEDULER_URL", "http://localhost:3002"), "relayScheduler base URL")
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
	sessions.SetHookURL(fmt.Sprintf("http://localhost:%s", *port))
	sessions.SetOllamaURL(*ollamaURL)

	schedulerClient := NewSchedulerClient(*schedulerURL)

	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, schedulerClient)
	RegisterSessionRoutes(mux, sessions)
	RegisterTerminalRoutes(mux, templateStore, terminalMgr)
	RegisterPermissionRoutes(mux, perms)
	RegisterModelRoutes(mux, *ollamaURL)
	RegisterSchedulerProxyRoutes(mux, schedulerClient)
	mux.HandleFunc("/ws", wsHub.HandleUpgrade)

	// Forward scheduler WebSocket events to all connected clients.
	schedulerWS := NewSchedulerWSForwarder(*schedulerURL, wsHub)
	go schedulerWS.Run()

	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", *port),
		Handler: recoverMiddleware(mux),
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
	}()

	slog.Info("listening", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Server stopped — clean up background resources.
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
