package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// piImageGenSkillBody is the SKILL.md handed to pi via the project
// overlay. It teaches pi to call the synchronous /api/generate-image
// HTTP endpoint over the relayLLM Unix socket and pass the JSON output
// through unmodified so Eve's renderer can detect the image_url and
// inline the image.
//
// The skill body is static (no per-session substitution) so the file
// can be materialized once at startup. Per-session secrets travel via
// env vars that the pi provider sets before spawn: RELAY_LLM_SOCKET +
// RELAY_LLM_TOKEN. Putting them in the skill body would leak the
// bearer to anything that can read the project's .pi/ overlay.
const piImageGenSkillBody = `---
name: comfyui-image-gen
description: Generate an image from a text description using the local ComfyUI service. Use this whenever the user asks for an image, picture, logo, illustration, or visual mockup.
---

# Image generation

When the user asks for an image, illustration, picture, logo, drawing, or
similar visual asset, use this skill. Do not call any other "imagine" or
"draw" tool — they are not available in this environment.

## How to invoke

Call the relayLLM image generation endpoint via the ` + "`bash`" + ` tool. The
endpoint runs synchronously and returns within ~10–30 seconds for a
1024x1024 image at the default 20 sampling steps.

` + "```" + `bash
curl -sS --unix-socket "$RELAY_LLM_SOCKET" \
  -H "Authorization: Bearer $RELAY_LLM_TOKEN" \
  -H "Content-Type: application/json" \
  -X POST http://localhost/api/generate-image \
  -d '{
        "prompt": "A photorealistic golden retriever puppy in a sunlit meadow"
      }'
` + "```" + `

Both ` + "`$RELAY_LLM_SOCKET`" + ` and ` + "`$RELAY_LLM_TOKEN`" + ` are pre-set in your
shell environment. Do not hardcode their values.

## Request body

| Field            | Type    | Default | Notes                                                        |
|------------------|---------|---------|--------------------------------------------------------------|
| ` + "`prompt`" + `         | string  | —       | Required. Be specific and descriptive. No filler words.      |
| ` + "`negative_prompt`" + `| string  | (sane)  | What to avoid; safe to omit.                                 |
| ` + "`width`" + `          | integer | 1024    | Common values: 768, 1024, 1152, 1344.                        |
| ` + "`height`" + `         | integer | 1024    |                                                              |
| ` + "`steps`" + `          | integer | 20      | Higher = better quality, slower. 30 is a sweet spot.         |
| ` + "`seed`" + `           | integer | random  | Pass -1 for random.                                          |
| ` + "`sampler`" + `        | string  | euler   | Try ` + "`dpmpp_2m_sde`" + ` for higher quality.                     |
| ` + "`scheduler`" + `      | string  | normal  | ` + "`karras`" + ` often improves results.                            |
| ` + "`checkpoint`" + `     | string  | server  | Optional; the server picks a sane default checkpoint.        |
| ` + "`lora`" + `           | string  | none    | Optional style adapter.                                      |

Only ` + "`prompt`" + ` is required. Skip every other field unless you have a
specific reason to override the defaults.

## Reading the response

Success looks like:

` + "```" + `json
{
  "status": "success",
  "image_url": "/api/generated/abc123.png",
  "filename": "abc123.png",
  "prompt": "...",
  "width": 1024,
  "height": 1024,
  "seed": 1234567,
  "generation_time": "12.4s"
}
` + "```" + `

Failure looks like:

` + "```" + `json
{ "status": "error", "error": "ComfyUI queue failed: ..." }
` + "```" + `

## What to do with the result

1. If ` + "`status`" + ` is ` + "`error`" + `, explain the failure to the user. Do not retry
   silently.
2. If ` + "`status`" + ` is ` + "`success`" + `, include the ` + "`image_url`" + ` verbatim in your
   reply (the frontend will render it inline). A natural-sounding mention
   plus the URL is enough — no markdown image syntax required:

   > Here's what I came up with: /api/generated/abc123.png

3. Do not download or re-upload the image; the URL is already
   browser-renderable.
`

// MaterializePiImageGenSkill writes the skill file under
// {dataDir}/pi-skills/comfyui-image-gen/ so the pi overlay can include
// the directory in its skills array. Returns the parent directory path
// (suitable for pi's skills setting), or "" if dataDir is empty.
func MaterializePiImageGenSkill(dataDir string) (string, error) {
	if dataDir == "" {
		return "", nil
	}
	parent := filepath.Join(dataDir, "pi-skills")
	skillDir := filepath.Join(parent, "comfyui-image-gen")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", fmt.Errorf("pi skill: mkdir %s: %w", skillDir, err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	// Always overwrite — the skill body is owned by relayLLM and
	// upgrading relayLLM should refresh prompt copy without manual
	// migration steps.
	if err := os.WriteFile(path, []byte(piImageGenSkillBody), 0o644); err != nil {
		return "", fmt.Errorf("pi skill: write %s: %w", path, err)
	}
	return parent, nil
}
