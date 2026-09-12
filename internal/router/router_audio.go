package router

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

// maxTranscriptionBytes caps a buffered audio upload. The handler below has to
// hold the whole request in memory to rewrite the model field, so this is the
// difference between a bounded cost per request and an OOM lever. 25 MB is
// OpenAI's own audio limit, which is what clients are written against.
// A var, not a const, so tests can exercise the overflow path without
// allocating 25 MB to do it.
var maxTranscriptionBytes int64 = 25 << 20

// handleAudioTranscription proxies OpenAI's speech-to-text endpoint.
//
// It exists because handleProxy cannot: that one decodes the body as JSON to
// find "model", and /v1/audio/transcriptions is multipart/form-data, so every
// request 400s on the envelope parse before routing is even attempted. Audio
// in, JSON transcript out — the shape is different enough to need its own door.
//
// Routing is endpoint-only, deliberately. Managed servers (llama.cpp, mlx) and
// virtual models are text-completion routes; neither serves audio, so a hit
// there would be a misconfiguration rather than a fallback worth honoring.
//
// The whole request is buffered because the model field must be rewritten from
// the router's prefixed id ("omlx/whatever") to the bare id the endpoint knows,
// and a multipart field cannot be edited in flight — its value may arrive after
// the file part. Re-emission reuses the ORIGINAL boundary so the client's
// Content-Type header stays correct and does not need rewriting too.
func (p *RelayRouter) handleAudioTranscription(w http.ResponseWriter, r *http.Request) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		writeRouterError(w, http.StatusBadRequest,
			"/v1/audio/transcriptions expects multipart/form-data")
		return
	}
	boundary := params["boundary"]
	if boundary == "" {
		writeRouterError(w, http.StatusBadRequest, "multipart body has no boundary")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTranscriptionBytes))
	r.Body.Close()
	if err != nil {
		// MaxBytesReader's error is the overflow case and deserves the status
		// that tells a client to send less, not a generic read failure.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeRouterError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("audio upload exceeds %d bytes", maxTranscriptionBytes))
			return
		}
		writeRouterError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	parts, model, err := readMultipartParts(bytes.NewReader(body), boundary)
	if err != nil {
		writeRouterError(w, http.StatusBadRequest, fmt.Sprintf("malformed multipart body: %v", err))
		return
	}
	if model == "" {
		writeRouterError(w, http.StatusBadRequest, "missing or empty model field")
		return
	}

	if p.registry == nil {
		writeRouterError(w, http.StatusServiceUnavailable, "no endpoints configured")
		return
	}
	ep, upstreamID, ok := p.registry.LookupModel(r.Context(), model)
	if !ok {
		slog.Warn("relay router: unknown transcription model", "model", model)
		writeRouterError(w, http.StatusBadRequest, fmt.Sprintf("unknown model %q", model))
		return
	}

	rewritten, err := rewriteMultipartModel(parts, boundary, upstreamID)
	if err != nil {
		slog.Warn("relay router: multipart rewrite failed", "endpoint", ep.Name, "error", err)
		writeRouterError(w, http.StatusInternalServerError, "failed to rewrite model field")
		return
	}

	target, err := url.Parse(ep.BaseURL)
	if err != nil {
		slog.Warn("relay router: bad endpoint baseURL", "endpoint", ep.Name, "baseURL", ep.BaseURL, "error", err)
		writeRouterError(w, http.StatusInternalServerError, "invalid endpoint configuration")
		return
	}
	proxy := newUpstreamProxy(target, rewritten, ep.APIKey, "audio", ep.Name, nil)
	proxy.Transport = ep.Transport()
	proxy.ServeHTTP(w, r)
}

// multipartPart is one buffered form part. Audio uploads are small enough to
// hold whole (see maxTranscriptionBytes) and there is no way to rewrite a field
// that may arrive last without having read past it.
type multipartPart struct {
	name     string
	fileName string
	header   textproto.MIMEHeader
	data     []byte
}

// readMultipartParts buffers every part and returns the value of the "model"
// form field alongside them.
func readMultipartParts(r io.Reader, boundary string) ([]multipartPart, string, error) {
	mr := multipart.NewReader(r, boundary)
	var parts []multipartPart
	var model string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, "", err
		}
		if part.FormName() == "model" && part.FileName() == "" {
			model = string(data)
		}
		parts = append(parts, multipartPart{
			name:     part.FormName(),
			fileName: part.FileName(),
			header:   part.Header,
			data:     data,
		})
	}
	return parts, model, nil
}

// rewriteMultipartModel re-emits the parts with "model" replaced by the id the
// upstream endpoint expects. The original boundary is reused so the request's
// existing Content-Type header remains accurate — the reverse proxy forwards
// the client's headers, and a fresh boundary would silently contradict them.
//
// Every other part is copied through with its headers intact, so a file part
// keeps its filename and content type: mlx-audio backends route on the file
// extension, and dropping it changes how the audio gets decoded.
func rewriteMultipartModel(parts []multipartPart, boundary, upstreamID string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("set boundary: %w", err)
	}
	for _, part := range parts {
		data := part.data
		if part.name == "model" && part.fileName == "" {
			data = []byte(upstreamID)
		}
		w, err := mw.CreatePart(part.header)
		if err != nil {
			return nil, fmt.Errorf("create part %q: %w", part.name, err)
		}
		if _, err := w.Write(data); err != nil {
			return nil, fmt.Errorf("write part %q: %w", part.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}
	return buf.Bytes(), nil
}
