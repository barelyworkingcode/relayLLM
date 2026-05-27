package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runImageGenMCPServer runs an MCP stdio server that exposes the
// generate_image tool. Spawned as a child of provider subprocesses
// (Claude CLI's --mcp-config, pi's --extension) that lack relayLLM's
// in-process built-in tool dispatch but do speak MCP.
//
// The server is provider-agnostic: it speaks MCP over stdin/stdout and
// reuses ComfyUIClient + the workflow builders directly. Images are
// written to the same {dataDir}/generated/ that relayLLM serves under
// /api/generated, so the URL returned to the LLM is browser-renderable
// without any cross-process file passing.
//
// Environment contract (read from process env so the parent doesn't have
// to invent a CLI flag convention):
//
//	COMFYUI_URL       — required; same value the parent relayLLM uses
//	RELAY_LLM_DATA    — optional; falls back to UserConfigDir/relayLLM
//	RELAY_IMAGE_BASE  — optional; URL prefix returned to the LLM
//	                    (default "/api/generated")
//
// The subcommand exits non-zero only on hard configuration errors. A
// failure to reach ComfyUI at startup is logged but the server still
// registers the tool — individual tool calls will surface the ComfyUI
// outage as a structured error to the LLM, which the model can describe
// to the user instead of opaquely crashing the chat.
func runImageGenMCPServer() error {
	comfyURL := os.Getenv("COMFYUI_URL")
	if comfyURL == "" {
		return fmt.Errorf("mcp-image-gen: COMFYUI_URL not set")
	}

	dataDir := os.Getenv("RELAY_LLM_DATA")
	if dataDir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			dataDir = filepath.Join(d, "relayLLM")
		}
	}
	if dataDir == "" {
		return fmt.Errorf("mcp-image-gen: cannot resolve data directory")
	}

	imageBaseURL := os.Getenv("RELAY_IMAGE_BASE")
	if imageBaseURL == "" {
		imageBaseURL = "/api/generated"
	}

	if err := os.MkdirAll(filepath.Join(dataDir, "generated"), 0o755); err != nil {
		return fmt.Errorf("mcp-image-gen: ensure generated dir: %w", err)
	}

	comfyui := NewComfyUIClient(comfyURL, dataDir)

	// Best-effort model discovery for a richer schema. ComfyUI being down
	// at startup is non-fatal: we ship a schema without enums and the LLM
	// can still call the tool with sensible defaults once ComfyUI returns.
	var checkpoints, loras []string
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := comfyui.Ping(ctx); err == nil {
			checkpoints, _ = comfyui.ListCheckpoints(ctx)
			loras, _ = comfyui.ListLoRAs(ctx)
		} else {
			slog.Warn("mcp-image-gen: ComfyUI unreachable at startup", "url", comfyURL, "error", err)
		}
		cancel()
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "relayllm-image-gen",
		Version: "1.0.0",
	}, nil)

	schema := buildImageGenSchema(checkpoints, loras)
	var schemaAny any
	if err := json.Unmarshal(schema, &schemaAny); err != nil {
		return fmt.Errorf("mcp-image-gen: parse image schema: %w", err)
	}

	tool := &mcp.Tool{
		Name:        "generate_image",
		Description: "Generate an image from a text description using a local Stable Diffusion model. Returns a JSON object with status, image_url, and metadata.",
		InputSchema: schemaAny,
	}

	server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// MCP has no per-call progress callback that maps onto the parent
		// session's stream, so we discard intra-call progress events. The
		// final tool result still surfaces success/failure to the LLM.
		emit := func(eventType string, data json.RawMessage) {}

		// No image attachments arrive through MCP in this flow — the
		// Claude CLI subprocess can't currently forward user image
		// attachments to an MCP server, so img2img is text-prompt only
		// for the MCP path. The img2img branch in handleGenerateImage
		// will short-circuit with a clear error if use_input_image=true.
		resultStr, err := handleGenerateImage(ctx, req.Params.Arguments, nil, emit, comfyui, imageBaseURL)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
			}, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: resultStr}},
		}, nil
	})

	slog.Info("mcp-image-gen: serving",
		"comfyui", comfyURL,
		"dataDir", dataDir,
		"checkpoints", len(checkpoints),
		"loras", len(loras))

	return server.Run(context.Background(), &mcp.StdioTransport{})
}
