package main

// Coverage for /v1/audio/transcriptions (relay_router.go).
//
// This route exists because handleProxy decodes the body as JSON to find
// "model", and transcription requests are multipart/form-data. These tests pin
// the parts of that which are easy to break silently:
//
//   - the model field is rewritten to the endpoint's bare upstream id
//   - the audio part survives byte-for-byte, with filename and content type
//   - the client's boundary is reused, so its Content-Type header stays true
//   - other form fields (language, prompt, …) pass through untouched
//   - auth is replaced at the trust boundary
//   - bad input fails with a reason, not a proxy attempt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// audioForm builds a transcription request body the way an OpenAI client would.
func audioForm(t *testing.T, model string, audio []byte, extra map[string]string) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// File first, model after — the awkward ordering on purpose: the handler
	// must not assume it can read the model field before reaching the audio.
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if model != "" {
		if err := mw.WriteField("model", model); err != nil {
			t.Fatalf("write model: %v", err)
		}
	}
	for k, v := range extra {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write %s: %v", k, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

// transcriptionUpstream is a fake OpenAI-compatible STT endpoint that records
// the multipart request it was handed.
type transcriptionUpstream struct {
	*httptest.Server
	model    string
	language string
	fileName string
	fileType string
	audio    []byte
	auth     string
	hits     int
}

func newTranscriptionUpstream(t *testing.T, advertise string) *transcriptionUpstream {
	t.Helper()
	up := &transcriptionUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": advertise}},
			})
		case "/v1/audio/transcriptions":
			up.hits++
			up.auth = r.Header.Get("Authorization")
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, "upstream could not parse multipart: %v", err)
				return
			}
			up.model = r.FormValue("model")
			up.language = r.FormValue("language")
			if up.model != advertise {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"error":{"message":"Model %q not found"}}`, up.model)
				return
			}
			if f, hdr, err := r.FormFile("file"); err == nil {
				up.audio, _ = io.ReadAll(f)
				up.fileName = hdr.Filename
				up.fileType = hdr.Header.Get("Content-Type")
				f.Close()
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"text":"hello there"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func newAudioRouter(t *testing.T, upstreamURL, key string) *httptest.Server {
	t.Helper()
	registry := NewProxyRegistry(&OpenAIConfig{
		Endpoints: []OpenAIEndpoint{{Name: "fakeep", BaseURL: upstreamURL + "/v1", APIKey: key}},
	})
	r := NewRelayRouter(":0", nil, registry, nil)
	srv := httptest.NewServer(r.server.Handler)
	t.Cleanup(srv.Close)
	return srv
}

func postForm(t *testing.T, url string, body []byte, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRouter_Transcription_RewritesModelAndForwardsAudioIntact(t *testing.T) {
	audio := []byte("RIFF....not-really-a-wav-but-bytes-are-bytes")
	up := newTranscriptionUpstream(t, "Qwen3-ASR")
	srv := newAudioRouter(t, up.URL, "upstream-key")

	body, ct := audioForm(t, "fakeep/Qwen3-ASR", audio, map[string]string{"language": "en"})
	resp := postForm(t, srv.URL+"/v1/audio/transcriptions", body, ct)

	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, got)
	}
	if up.hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.hits)
	}
	if up.model != "Qwen3-ASR" {
		t.Errorf("upstream model = %q, want bare %q (prefix not stripped)", up.model, "Qwen3-ASR")
	}
	if !bytes.Equal(up.audio, audio) {
		t.Errorf("audio corrupted in transit: got %d bytes, want %d", len(up.audio), len(audio))
	}
	if up.fileName != "speech.wav" {
		t.Errorf("filename = %q, want %q — backends route on the extension", up.fileName, "speech.wav")
	}
	if up.language != "en" {
		t.Errorf("language = %q, want %q — other form fields must pass through", up.language, "en")
	}
	if up.auth != "Bearer upstream-key" {
		t.Errorf("auth = %q, want the endpoint's own key", up.auth)
	}

	var parsed struct {
		Text string `json:"text"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Text != "hello there" {
		t.Errorf("transcript not returned to the client: %s", raw)
	}
}

func TestRouter_Transcription_ReusesClientBoundary(t *testing.T) {
	// The reverse proxy forwards the client's Content-Type header verbatim. If
	// re-emission invented a new boundary the header would describe a body that
	// no longer exists, and the upstream would fail to parse it.
	audio := []byte("audio-bytes")
	body, ct := audioForm(t, "fakeep/Qwen3-ASR", audio, nil)
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	boundary := params["boundary"]

	parts, model, err := readMultipartParts(bytes.NewReader(body), boundary)
	if err != nil {
		t.Fatalf("read parts: %v", err)
	}
	if model != "fakeep/Qwen3-ASR" {
		t.Fatalf("model = %q, want %q", model, "fakeep/Qwen3-ASR")
	}

	out, err := rewriteMultipartModel(parts, boundary, "Qwen3-ASR")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Contains(out, []byte("--"+boundary)) {
		t.Error("rewritten body does not use the original boundary")
	}
	// And it must still parse as that boundary, with the swap applied.
	parts2, model2, err := readMultipartParts(bytes.NewReader(out), boundary)
	if err != nil {
		t.Fatalf("re-read rewritten body: %v", err)
	}
	if model2 != "Qwen3-ASR" {
		t.Errorf("rewritten model = %q, want %q", model2, "Qwen3-ASR")
	}
	if len(parts2) != len(parts) {
		t.Errorf("part count changed: %d -> %d", len(parts), len(parts2))
	}
}

func TestRouter_Transcription_NonMultipart_Returns400(t *testing.T) {
	up := newTranscriptionUpstream(t, "Qwen3-ASR")
	srv := newAudioRouter(t, up.URL, "k")

	resp := postForm(t, srv.URL+"/v1/audio/transcriptions",
		[]byte(`{"model":"fakeep/Qwen3-ASR"}`), "application/json")

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "multipart") {
		t.Errorf("error should name the expected encoding, got: %s", raw)
	}
	if up.hits != 0 {
		t.Error("router proxied a request it could not route")
	}
}

func TestRouter_Transcription_MissingModel_Returns400(t *testing.T) {
	up := newTranscriptionUpstream(t, "Qwen3-ASR")
	srv := newAudioRouter(t, up.URL, "k")

	body, ct := audioForm(t, "", []byte("audio"), nil)
	resp := postForm(t, srv.URL+"/v1/audio/transcriptions", body, ct)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if up.hits != 0 {
		t.Error("router proxied a request with no model field")
	}
}

func TestRouter_Transcription_UnknownEndpointPrefix_Returns400(t *testing.T) {
	// Routing resolves on the endpoint prefix. No such endpoint, no route, and
	// the router says so itself rather than proxying into the dark.
	up := newTranscriptionUpstream(t, "Qwen3-ASR")
	srv := newAudioRouter(t, up.URL, "k")

	body, ct := audioForm(t, "nosuchendpoint/Qwen3-ASR", []byte("audio"), nil)
	resp := postForm(t, srv.URL+"/v1/audio/transcriptions", body, ct)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "unknown model") {
		t.Errorf("error should say the model is unknown, got: %s", raw)
	}
	if up.hits != 0 {
		t.Error("router proxied to an endpoint that does not exist")
	}
}

func TestRouter_Transcription_KnownEndpointUnknownModel_SurfacesUpstreamError(t *testing.T) {
	// LookupModel deliberately does not verify the id against the endpoint's
	// catalog — the upstream owns that list, and duplicating it here would go
	// stale. So an unknown id under a real endpoint is forwarded, and the
	// upstream's own 404 reaches the client instead of a router-invented one.
	// This mirrors handleProxy; changing it here would make the two disagree.
	up := newTranscriptionUpstream(t, "Qwen3-ASR")
	srv := newAudioRouter(t, up.URL, "k")

	body, ct := audioForm(t, "fakeep/does-not-exist", []byte("audio"), nil)
	resp := postForm(t, srv.URL+"/v1/audio/transcriptions", body, ct)

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want the upstream's 404", resp.StatusCode)
	}
	if up.hits != 1 {
		t.Errorf("upstream hits = %d, want 1 — the request should reach it", up.hits)
	}
	if up.model != "does-not-exist" {
		t.Errorf("upstream saw model %q, want the stripped id", up.model)
	}
}

func TestRouter_Transcription_OversizeUpload_Returns413(t *testing.T) {
	// The handler buffers the whole request to rewrite a field that may arrive
	// last, so the cap is the difference between bounded cost and an OOM lever.
	orig := maxTranscriptionBytes
	maxTranscriptionBytes = 512
	t.Cleanup(func() { maxTranscriptionBytes = orig })

	up := newTranscriptionUpstream(t, "Qwen3-ASR")
	srv := newAudioRouter(t, up.URL, "k")

	body, ct := audioForm(t, "fakeep/Qwen3-ASR", bytes.Repeat([]byte("a"), 4096), nil)
	resp := postForm(t, srv.URL+"/v1/audio/transcriptions", body, ct)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if up.hits != 0 {
		t.Error("router forwarded an oversize upload")
	}
}
