package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ComfyUIClient talks to the ComfyUI REST API for image (and future video)
// generation. It is provider-agnostic — the tool handler in builtin_tools.go
// calls its methods and translates results into tool-result strings.
type ComfyUIClient struct {
	baseURL string
	client  *http.Client
	dataDir string
}

// ComfyUIOutput describes a single output file from a completed prompt.
type ComfyUIOutput struct {
	Filename  string
	Subfolder string
	Type      string // "output", "temp", etc.
}

// ImageGenParams are the user-facing knobs exposed through the generate_image
// tool. Zero values are replaced with sensible defaults before use.
type ImageGenParams struct {
	Prompt         string  `json:"prompt"`
	NegativePrompt string  `json:"negative_prompt,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	Steps          int     `json:"steps,omitempty"`
	CfgScale       float64 `json:"cfg_scale,omitempty"`
	Seed           int64   `json:"seed,omitempty"`
	Checkpoint     string  `json:"checkpoint,omitempty"`
	Lora           string  `json:"lora,omitempty"`
	LoraStrength   float64 `json:"lora_strength,omitempty"`
	Sampler        string  `json:"sampler,omitempty"`
	Scheduler      string  `json:"scheduler,omitempty"`
}

func (p *ImageGenParams) applyDefaults() {
	if p.Width <= 0 {
		p.Width = 1024
	}
	if p.Height <= 0 {
		p.Height = 1024
	}
	if p.Steps <= 0 {
		p.Steps = 20
	}
	if p.CfgScale <= 0 {
		p.CfgScale = 7.0
	}
	if p.Seed <= 0 {
		p.Seed = rand.Int64N(1<<53 - 1)
	}
	if p.NegativePrompt == "" {
		p.NegativePrompt = "blurry, low quality, deformed, disfigured"
	}
	if p.Sampler == "" {
		p.Sampler = "euler"
	}
	if p.Scheduler == "" {
		p.Scheduler = "normal"
	}
	if p.LoraStrength == 0 {
		p.LoraStrength = 1.0
	}
}

func NewComfyUIClient(baseURL, dataDir string) *ComfyUIClient {
	return &ComfyUIClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 30 * time.Second},
		dataDir: dataDir,
	}
}

// Ping confirms ComfyUI is reachable.
func (c *ComfyUIClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/system_stats", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("comfyui not reachable at %s: %w", c.baseURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("comfyui returned %d", resp.StatusCode)
	}
	return nil
}

// listModels queries ComfyUI for available models in a folder.
func (c *ComfyUIClient) listModels(ctx context.Context, folder string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models/"+folder, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", folder, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list %s: HTTP %d", folder, resp.StatusCode)
	}
	var models []string
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("parse %s list: %w", folder, err)
	}
	return models, nil
}

// ListCheckpoints returns available checkpoint model filenames.
func (c *ComfyUIClient) ListCheckpoints(ctx context.Context) ([]string, error) {
	return c.listModels(ctx, "checkpoints")
}

// ListLoRAs returns available LoRA filenames.
func (c *ComfyUIClient) ListLoRAs(ctx context.Context) ([]string, error) {
	return c.listModels(ctx, "loras")
}

// QueuePrompt submits a workflow to ComfyUI and returns the prompt_id.
func (c *ComfyUIClient) QueuePrompt(ctx context.Context, workflow map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{"prompt": workflow})
	if err != nil {
		return "", fmt.Errorf("marshal workflow: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/prompt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("queue prompt: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("comfyui queue error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse queue response: %w", err)
	}
	if result.PromptID == "" {
		return "", fmt.Errorf("comfyui returned empty prompt_id")
	}
	return result.PromptID, nil
}

// PollHistory waits for a prompt to complete, calling progressFn with status
// updates. Returns the list of output files.
func (c *ComfyUIClient) PollHistory(ctx context.Context, promptID string, timeout time.Duration, progressFn func(string)) ([]ComfyUIOutput, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	historyURL := fmt.Sprintf("%s/history/%s", c.baseURL, promptID)
	pollCount := 0

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("image generation timed out after %s", timeout)
		}

		pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, historyURL, nil)
		if err != nil {
			pollCancel()
			return nil, err
		}
		resp, err := c.client.Do(req)
		pollCancel()
		if err != nil {
			continue // transient error, retry
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			continue
		}

		// ComfyUI returns {"prompt_id": {...}} when done, {} when pending.
		var history map[string]json.RawMessage
		if err := json.Unmarshal(body, &history); err != nil {
			continue
		}
		promptData, ok := history[promptID]
		if !ok || len(promptData) == 0 {
			pollCount++
			if pollCount%4 == 0 && progressFn != nil {
				progressFn(fmt.Sprintf("Generating... (%ds)", pollCount/2))
			}
			continue
		}

		// Parse outputs from the completed prompt.
		var entry struct {
			Outputs map[string]struct {
				Images []struct {
					Filename  string `json:"filename"`
					Subfolder string `json:"subfolder"`
					Type      string `json:"type"`
				} `json:"images"`
			} `json:"outputs"`
			Status struct {
				Completed bool `json:"completed"`
			} `json:"status"`
		}
		if err := json.Unmarshal(promptData, &entry); err != nil {
			return nil, fmt.Errorf("parse history entry: %w", err)
		}

		var outputs []ComfyUIOutput
		for _, node := range entry.Outputs {
			for _, img := range node.Images {
				outputs = append(outputs, ComfyUIOutput{
					Filename:  img.Filename,
					Subfolder: img.Subfolder,
					Type:      img.Type,
				})
			}
		}

		if len(outputs) == 0 {
			// Check if there's an error status with no outputs
			if entry.Status.Completed {
				return nil, fmt.Errorf("comfyui completed but produced no output images")
			}
			// Not done yet — status_str might be "executing"
			pollCount++
			if pollCount%4 == 0 && progressFn != nil {
				progressFn(fmt.Sprintf("Generating... (%ds)", pollCount/2))
			}
			continue
		}

		return outputs, nil
	}
}

// FetchImage retrieves a generated image from ComfyUI's output directory.
func (c *ComfyUIClient) FetchImage(ctx context.Context, filename, subfolder string) ([]byte, error) {
	q := url.Values{"filename": {filename}, "type": {"output"}}
	if subfolder != "" {
		q.Set("subfolder", subfolder)
	}
	fetchURL := c.baseURL + "/view?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch image: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// SaveOutput writes image/video bytes to the generated output directory and
// returns the filename (UUID-based, no path separators).
func (c *ComfyUIClient) SaveOutput(data []byte, ext string) (string, error) {
	dir := filepath.Join(c.dataDir, "generated")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create generated dir: %w", err)
	}

	filename := fmt.Sprintf("%d-%d%s", time.Now().UnixMilli(), rand.Int64N(99999), ext)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("write output: %w", err)
	}
	return filename, nil
}

// BuildTextToImageWorkflow builds a ComfyUI workflow graph for SDXL
// text-to-image generation. The returned map is ready to pass to QueuePrompt.
// Supports optional LoRA loading and dynamic sampler/scheduler selection.
func (c *ComfyUIClient) BuildTextToImageWorkflow(params ImageGenParams) map[string]any {
	params.applyDefaults()

	ckptName := params.Checkpoint
	if ckptName == "" {
		ckptName = "sd_xl_base_1.0.safetensors"
	}

	// Source nodes for model and CLIP — default to checkpoint outputs.
	// If a LoRA is specified, these get rewired to the LoRA loader outputs.
	modelSource := []any{"1", 0}
	clipSource := []any{"1", 1}

	workflow := map[string]any{
		"1": map[string]any{
			"class_type": "CheckpointLoaderSimple",
			"inputs": map[string]any{
				"ckpt_name": ckptName,
			},
		},
		"4": map[string]any{
			"class_type": "EmptyLatentImage",
			"inputs": map[string]any{
				"width":      params.Width,
				"height":     params.Height,
				"batch_size": 1,
			},
		},
	}

	// Conditionally insert LoRA loader between checkpoint and conditioning.
	if params.Lora != "" {
		workflow["8"] = map[string]any{
			"class_type": "LoraLoader",
			"inputs": map[string]any{
				"model":          []any{"1", 0},
				"clip":           []any{"1", 1},
				"lora_name":      params.Lora,
				"strength_model": params.LoraStrength,
				"strength_clip":  params.LoraStrength,
			},
		}
		modelSource = []any{"8", 0}
		clipSource = []any{"8", 1}
	}

	workflow["2"] = map[string]any{
		"class_type": "CLIPTextEncode",
		"inputs": map[string]any{
			"text": params.Prompt,
			"clip": clipSource,
		},
	}
	workflow["3"] = map[string]any{
		"class_type": "CLIPTextEncode",
		"inputs": map[string]any{
			"text": params.NegativePrompt,
			"clip": clipSource,
		},
	}
	workflow["5"] = map[string]any{
		"class_type": "KSampler",
		"inputs": map[string]any{
			"model":        modelSource,
			"positive":     []any{"2", 0},
			"negative":     []any{"3", 0},
			"latent_image": []any{"4", 0},
			"seed":         params.Seed,
			"steps":        params.Steps,
			"cfg":          params.CfgScale,
			"sampler_name": params.Sampler,
			"scheduler":    params.Scheduler,
			"denoise":      1.0,
		},
	}
	workflow["6"] = map[string]any{
		"class_type": "VAEDecode",
		"inputs": map[string]any{
			"samples": []any{"5", 0},
			"vae":     []any{"1", 2},
		},
	}
	workflow["7"] = map[string]any{
		"class_type": "SaveImage",
		"inputs": map[string]any{
			"images":          []any{"6", 0},
			"filename_prefix": "relay",
		},
	}

	return workflow
}
