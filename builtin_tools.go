package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// BuiltinToolHandler executes a built-in tool. files contains the user's
// message attachments (may be nil). The emit callback lets the handler send
// progress events back to the client during long-running operations.
type BuiltinToolHandler func(ctx context.Context, args json.RawMessage,
	files []FileAttachment,
	emit func(eventType string, data json.RawMessage)) (string, error)

// BuiltinToolDef is the static definition of a built-in tool, used both for
// ChatToolDefs() export and for the handler registry.
type BuiltinToolDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
	parsedParams any            // cached parse of Parameters, set at registration
}

// BuiltinToolRegistry holds built-in tools that run alongside MCP tools in
// the BaseChatProvider tool loop. Built-in tools get an emit callback for
// progress events, which MCP tools cannot provide.
type BuiltinToolRegistry struct {
	tools    []BuiltinToolDef
	handlers map[string]BuiltinToolHandler
}

func NewBuiltinToolRegistry() *BuiltinToolRegistry {
	return &BuiltinToolRegistry{
		handlers: make(map[string]BuiltinToolHandler),
	}
}

// Register adds a tool to the registry.
func (r *BuiltinToolRegistry) Register(def BuiltinToolDef, handler BuiltinToolHandler) {
	if len(def.Parameters) > 0 {
		_ = json.Unmarshal(def.Parameters, &def.parsedParams)
	}
	r.tools = append(r.tools, def)
	r.handlers[def.Name] = handler
}

// Has returns true if the named tool is built-in.
func (r *BuiltinToolRegistry) Has(name string) bool {
	_, ok := r.handlers[name]
	return ok
}

// Call executes a built-in tool by name. files are the user's message
// attachments from the current conversation turn.
func (r *BuiltinToolRegistry) Call(ctx context.Context, name string, args json.RawMessage,
	files []FileAttachment, emit func(eventType string, data json.RawMessage)) (string, error) {
	handler, ok := r.handlers[name]
	if !ok {
		return "", fmt.Errorf("unknown built-in tool: %s", name)
	}
	return handler(ctx, args, files, emit)
}

// ChatToolDefs returns tool definitions in the OpenAI/Ollama compatible
// shape: [{type: "function", function: {name, description, parameters}}].
func (r *BuiltinToolRegistry) ChatToolDefs() []map[string]any {
	defs := make([]map[string]any, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.parsedParams,
			},
		})
	}
	return defs
}

// RegisterImageGenTool registers the generate_image tool backed by a
// ComfyUIClient. The imageBaseURL is the HTTP path prefix for serving
// generated images (e.g. "/api/generated"). checkpoints and loras are
// the available model files discovered from ComfyUI at startup.
func RegisterImageGenTool(registry *BuiltinToolRegistry, comfyui *ComfyUIClient, imageBaseURL string, checkpoints, loras []string) {
	schema := buildImageGenSchema(checkpoints, loras)

	registry.Register(
		BuiltinToolDef{
			Name:        "generate_image",
			Description: "Generate an image from a text description using a local Stable Diffusion model. Returns a URL to the generated image.",
			Parameters:  schema,
		},
		func(ctx context.Context, args json.RawMessage, files []FileAttachment, emit func(string, json.RawMessage)) (string, error) {
			return handleGenerateImage(ctx, args, files, emit, comfyui, imageBaseURL)
		},
	)
}

// buildImageGenSchema constructs the tool parameter schema, dynamically
// including checkpoint/lora enums when multiple models are available.
func buildImageGenSchema(checkpoints, loras []string) json.RawMessage {
	props := map[string]any{
		"prompt": map[string]any{
			"type":        "string",
			"description": "Detailed description of the image to generate",
		},
		"negative_prompt": map[string]any{
			"type":        "string",
			"description": "What to avoid in the image",
		},
		"width": map[string]any{
			"type":        "integer",
			"description": "Image width in pixels (default 1024)",
		},
		"height": map[string]any{
			"type":        "integer",
			"description": "Image height in pixels (default 1024)",
		},
		"steps": map[string]any{
			"type":        "integer",
			"description": "Number of diffusion steps (default 20, higher = better quality but slower)",
		},
		"seed": map[string]any{
			"type":        "integer",
			"description": "Random seed for reproducibility (-1 for random)",
		},
		"sampler": map[string]any{
			"type":        "string",
			"description": "Sampling algorithm (default euler). Use dpmpp_2m_sde for higher quality.",
			"enum":        []string{"euler", "euler_ancestral", "dpmpp_2m_sde", "dpmpp_2m", "ddim", "uni_pc"},
		},
		"scheduler": map[string]any{
			"type":        "string",
			"description": "Scheduler algorithm (default normal). karras often produces better results.",
			"enum":        []string{"normal", "karras", "exponential", "simple"},
		},
	}

	// Only expose checkpoint selection if multiple checkpoints are available.
	if len(checkpoints) > 1 {
		props["checkpoint"] = map[string]any{
			"type":        "string",
			"description": "Model checkpoint to use. Different checkpoints produce different styles (e.g. anime, photorealistic).",
			"enum":        checkpoints,
		}
	}

	// Only expose LoRA if any are installed.
	if len(loras) > 0 {
		props["lora"] = map[string]any{
			"type":        "string",
			"description": "Optional LoRA style adapter to apply on top of the checkpoint for style control.",
			"enum":        loras,
		}
		props["lora_strength"] = map[string]any{
			"type":        "number",
			"description": "How strongly to apply the LoRA (0.0-2.0, default 1.0)",
		}
	}

	// Image-to-image support.
	props["use_input_image"] = map[string]any{
		"type":        "boolean",
		"description": "Set to true to use the user's attached image as a starting point. The image will be modified according to the prompt.",
	}
	props["denoise"] = map[string]any{
		"type":        "number",
		"description": "How much to change the input image (0.0 = keep original, 1.0 = fully regenerate). Default 0.7 for img2img, 1.0 for text-to-image.",
	}

	schema, _ := json.Marshal(map[string]any{
		"type":       "object",
		"properties": props,
		"required":   []string{"prompt"},
	})
	return schema
}

func imageGenError(msg string) string {
	result, _ := json.Marshal(map[string]string{"status": "error", "error": msg})
	return string(result)
}

func handleGenerateImage(ctx context.Context, args json.RawMessage, files []FileAttachment,
	emit func(string, json.RawMessage), comfyui *ComfyUIClient, imageBaseURL string) (string, error) {

	var params ImageGenParams
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("parse image gen params: %w", err)
	}
	if params.Prompt == "" {
		return imageGenError("prompt is required"), nil
	}

	// Progress helper.
	emitProgress := func(msg string) {
		data, _ := json.Marshal(map[string]any{
			"type":      "result",
			"subtype":   "tool_progress",
			"tool_name": "generate_image",
			"message":   msg,
		})
		emit("llm_event", data)
	}

	// Determine workflow: img2img if user requested and image is attached.
	var workflow map[string]any
	if params.UseInputImage {
		inputFile := findImageAttachment(files)
		if inputFile == nil {
			return imageGenError("No image attached. Please attach an image to use as a reference."), nil
		}
		emitProgress("Uploading reference image...")
		imageBytes, err := decodeFileAttachment(inputFile)
		if err != nil {
			return imageGenError("Failed to decode attached image: " + err.Error()), nil
		}
		uploadedName, err := comfyui.UploadImage(ctx, inputFile.Name, imageBytes)
		if err != nil {
			slog.Error("comfyui: upload failed", "error", err)
			return imageGenError("Failed to upload image to ComfyUI: " + err.Error()), nil
		}
		if params.Denoise == 0 {
			params.Denoise = 0.7
		}
		workflow = comfyui.BuildImageToImageWorkflow(params, uploadedName)
	} else {
		workflow = comfyui.BuildTextToImageWorkflow(params)
	}

	emitProgress("Queuing image generation...")
	genStart := time.Now()
	promptID, err := comfyui.QueuePrompt(ctx, workflow)
	if err != nil {
		slog.Error("comfyui: queue failed", "error", err)
		return imageGenError("ComfyUI queue failed: " + err.Error()), nil
	}

	emitProgress("Generating image...")

	// Poll for completion.
	outputs, err := comfyui.PollHistory(ctx, promptID, 5*time.Minute, emitProgress)
	if err != nil {
		slog.Error("comfyui: generation failed", "error", err, "promptID", promptID)
		return imageGenError("Image generation failed: " + err.Error()), nil
	}

	if len(outputs) == 0 {
		return imageGenError("ComfyUI produced no output images"), nil
	}

	// Fetch the first output image.
	emitProgress("Downloading generated image...")
	output := outputs[0]
	imageData, err := comfyui.FetchImage(ctx, output.Filename, output.Subfolder)
	if err != nil {
		slog.Error("comfyui: fetch image failed", "error", err, "filename", output.Filename)
		return imageGenError("Failed to fetch image: " + err.Error()), nil
	}

	// Save to disk.
	filename, err := comfyui.SaveOutput(imageData, ".png")
	if err != nil {
		slog.Error("comfyui: save failed", "error", err)
		return imageGenError("Failed to save image: " + err.Error()), nil
	}

	imageURL := fmt.Sprintf("%s/%s", imageBaseURL, filename)
	genDuration := time.Since(genStart).Seconds()
	slog.Info("comfyui: image generated", "filename", filename, "size", len(imageData),
		"duration", fmt.Sprintf("%.1fs", genDuration), "prompt", params.Prompt)

	result, _ := json.Marshal(map[string]any{
		"status":          "success",
		"image_url":       imageURL,
		"filename":        filename,
		"prompt":          params.Prompt,
		"width":           params.Width,
		"height":          params.Height,
		"seed":            params.Seed,
		"generation_time": fmt.Sprintf("%.1fs", genDuration),
	})
	return string(result), nil
}

// findImageAttachment returns the first image file from the user's attachments.
func findImageAttachment(files []FileAttachment) *FileAttachment {
	for i := range files {
		if strings.HasPrefix(files[i].MimeType, "image/") {
			return &files[i]
		}
	}
	return nil
}

// decodeFileAttachment extracts raw bytes from a FileAttachment. The Data
// field may be a bare base64 string or a data URL (data:image/png;base64,...).
func decodeFileAttachment(f *FileAttachment) ([]byte, error) {
	data := f.Data
	if idx := strings.Index(data, ";base64,"); idx >= 0 {
		data = data[idx+8:]
	}
	return base64.StdEncoding.DecodeString(data)
}
