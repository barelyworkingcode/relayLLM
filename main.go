package main

import (
	"context"
	"encoding/base64"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	dataDir := flag.String("data-dir", envOrDefault("RELAY_LLM_DATA", ""), "Data directory (default: ~/.config/relayLLM)")
	ollamaURL := flag.String("ollama-url", envOrDefault("OLLAMA_URL", "http://localhost:11434"), "Ollama base URL")
	openaiConfigPath := flag.String("openai-config", envOrDefault("OPENAI_CONFIG", ""), "Path to OpenAI-compatible endpoints config JSON (default: {data-dir}/openai_endpoints.json)")
	schedulerURL := flag.String("scheduler-url", envOrDefault("RELAY_SCHEDULER_URL", "http://localhost:3002"), "relayScheduler base URL")
	socketPath := flag.String("socket", envOrDefault("RELAY_LLM_INTERNAL_SOCKET", ""), "Internal Unix domain socket path. relay binds the front-door socket; this socket only accepts traffic from relay.")
	internalToken := flag.String("token", envOrDefault("RELAY_LLM_INTERNAL_TOKEN", ""), "Internal bearer token. Validated on every request as defense-in-depth on top of the socket's 0600 permissions. Empty = trust filesystem perms only.")
	comfyuiURL := flag.String("comfyui-url", envOrDefault("COMFYUI_URL", ""), "ComfyUI base URL for image generation (empty to disable)")
	llamaServerPath := flag.String("llama-server-path", envOrDefault("LLAMA_SERVER_PATH", ""), "Path to llama-server binary (default: llama-server on PATH)")
	llamaProxyPort := flag.String("llama-proxy-port", envOrDefault("LLAMA_PROXY_PORT", ""), "Port for OpenAI-compatible llama proxy (empty to disable)")
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

	// Fail loudly: a misconfigured launch should not silently come up unreachable.
	if *socketPath == "" {
		slog.Error("RELAY_LLM_INTERNAL_SOCKET is required")
		os.Exit(1)
	}
	if *internalToken == "" {
		slog.Warn("RELAY_LLM_INTERNAL_TOKEN unset — relying on 0600 filesystem perms only")
	}

	slog.Info("starting relayLLM", "socket", *socketPath, "dataDir", *dataDir)

	sessionStore := NewSessionStore(filepath.Join(*dataDir, "sessions"))
	perms := NewPermissionManager()
	sessions := NewSessionManager(sessionStore, perms)

	// Load provider + pty config up front. Terminal subsystem needs the pty
	// map to seed defaults before serving requests.
	openaiCfg, llamaCfg, ptyCfg, err := LoadConfig(*dataDir, *openaiConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Terminal subsystem.
	templateStore := NewTemplateStore(*dataDir)
	if err := templateStore.Load(ptyCfg); err != nil {
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

	// The Claude CLI hook subprocess (runs as the user) dials our Unix
	// socket and authenticates with the internal token.
	sessions.SetHookSocket(*socketPath)
	sessions.SetHookToken(*internalToken)
	sessions.SetOllamaURL(*ollamaURL)

	if len(openaiCfg.Endpoints) > 0 {
		slog.Info("openai endpoints loaded", "count", len(openaiCfg.Endpoints), "names", openaiCfg.Names())
	}
	sessions.SetOpenAIConfig(openaiCfg)
	var llamaManager *LlamaServerManager
	if len(llamaCfg.Models) > 0 {
		llamaManager = NewLlamaServerManager(llamaCfg, *llamaServerPath)
		slog.Info("llama models configured", "count", len(llamaCfg.Models), "binary", llamaManager.binaryPath)
	}
	sessions.SetLlamaManager(llamaManager)

	// Separate TCP surface called by relayLLM's own provider code, not by
	// external clients — keep it.
	var llamaProxyAddr string
	if *llamaProxyPort != "" && llamaManager != nil {
		llamaProxyAddr = ":" + *llamaProxyPort
	}
	llamaProxy := StartLlamaProxy(llamaProxyAddr, llamaManager)

	// Image generation via ComfyUI (optional).
	if *comfyuiURL != "" {
		comfyui := NewComfyUIClient(*comfyuiURL, *dataDir)

		// Discover available models. Retry for up to 30s to handle the case
		// where ComfyUI starts slower than relayLLM (common on cold boot).
		var checkpoints, loras []string
		{
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := comfyui.Ping(ctx); err == nil {
					slog.Info("ComfyUI connected", "url", *comfyuiURL)
					checkpoints, _ = comfyui.ListCheckpoints(ctx)
					loras, _ = comfyui.ListLoRAs(ctx)
					slog.Info("ComfyUI models discovered", "checkpoints", len(checkpoints), "loras", len(loras))
					cancel()
					break
				}
				cancel()
				time.Sleep(2 * time.Second)
			}
			if len(checkpoints) == 0 {
				slog.Warn("ComfyUI not reachable after 30s — image generation tool registered without model discovery", "url", *comfyuiURL)
			}
		}

		builtinTools := NewBuiltinToolRegistry()
		RegisterImageGenTool(builtinTools, comfyui, "/api/generated", checkpoints, loras)
		sessions.SetBuiltinTools(builtinTools)
	}

	schedulerClient := NewSchedulerClient(*schedulerURL)

	mux := http.NewServeMux()
	RegisterSessionRoutes(mux, sessions)
	RegisterTerminalRoutes(mux, templateStore, terminalMgr)
	RegisterPermissionRoutes(mux, perms, sessions)
	RegisterModelRoutes(mux, *ollamaURL, openaiCfg, llamaManager)
	RegisterSchedulerProxyRoutes(mux, schedulerClient)
	RegisterGeneratedImageRoutes(mux, *dataDir)
	mux.HandleFunc("/ws", wsHub.HandleUpgrade)

	// Forward scheduler WebSocket events to all connected clients.
	schedulerWS := NewSchedulerWSForwarder(*schedulerURL, wsHub)
	go schedulerWS.Run()

	// Build the handler chain. recoverMiddleware sits closest to the mux so it
	// catches panics from real handlers; bearerAuth sits in front so unauth
	// requests never reach a real handler (and never allocate a WS session).
	handler := bearerAuth(*internalToken, recoverMiddleware(mux))

	server := &http.Server{Handler: handler}

	if err := os.MkdirAll(filepath.Dir(*socketPath), 0o700); err != nil {
		slog.Error("failed to create socket parent dir", "path", *socketPath, "error", err)
		os.Exit(1)
	}
	// Remove any stale socket file from a previous crashed run.
	_ = os.Remove(*socketPath)
	listener, err := net.Listen("unix", *socketPath)
	if err != nil {
		slog.Error("failed to listen on internal socket", "path", *socketPath, "error", err)
		os.Exit(1)
	}
	if err := os.Chmod(*socketPath, 0o600); err != nil {
		slog.Warn("failed to chmod internal socket", "path", *socketPath, "error", err)
	}

	// Graceful shutdown: drain HTTP requests, then clean up providers and terminals.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		_ = os.Remove(*socketPath)
	}()

	slog.Info("internal socket listening", "path", *socketPath)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Server stopped — clean up background resources.
	schedulerWS.Close()
	sessions.StopAll()
	if llamaProxy != nil {
		llamaProxy.Close()
	}
	if llamaManager != nil {
		llamaManager.StopAll()
	}
	terminalMgr.StopAll()
	slog.Info("shutdown complete")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

