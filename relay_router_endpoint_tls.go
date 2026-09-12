package main

// No InsecureSkipVerify knob exists anywhere in this file, and none may be
// added: a pinned or CA-anchored hop that can be silently downgraded to
// "trust anything" is a false sense of safety, not a compatible fallback.

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"relayllm/internal/netutil"
)

// normalizePin lowercases a pin and strips optional colon separators (the
// format `openssl x509 -fingerprint -sha256` prints), then checks the result
// is exactly a SHA-256 hex digest.
//
// Fingerprint-of-leaf was chosen over SPKI pinning for two reasons: it needs
// nothing beyond the stdlib (sha256.Sum256(cert.Raw)), and it matches the
// output of `openssl x509 -fingerprint -sha256` directly — the tool an
// operator reaches for already — with no separate SPKI-extraction step to
// get wrong. The motivating deployment pins a VM's relayLLM router to a Mac
// host's relayLLM router over what would otherwise be a plaintext LAN hop;
// sibling relayTTS/relaySTT daemons already pin that same host, so this
// closes the one unpinned link in the chain.
func normalizePin(pin string) (string, error) {
	norm := strings.ToLower(strings.ReplaceAll(pin, ":", ""))
	if len(norm) != 64 {
		return "", fmt.Errorf("pin %q must be 64 hex characters (sha256 of the DER leaf certificate) once colons are removed, got %d", pin, len(norm))
	}
	if _, err := hex.DecodeString(norm); err != nil {
		return "", fmt.Errorf("pin %q is not valid hex: %w", pin, err)
	}
	return norm, nil
}

// loadCAPool reads a PEM bundle from path and returns a pool anchored ONLY
// on those certificates — the caller sets this as tls.Config.RootCAs, which
// replaces (not appends to) the system roots for that connection.
func loadCAPool(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read caFile %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("caFile %s contains no certificates", path)
	}
	return pool, nil
}

// validateEndpointTransport checks one OpenAIEndpoint's TLS-related fields
// against the fail-closed rules for the relayLLM-to-upstream hop, without
// touching the network: BaseURL must be well-formed http(s); http is only
// reachable unconditionally when the host is loopback, otherwise only when
// allowPlaintext opts the deployment into an acknowledged-unencrypted hop
// (logged, never affecting https verification); caFile/pinSHA256 require an
// https URL to have any effect at all, since neither means anything over
// plain http; a caFile must be readable and contain at least one
// certificate; every pin must normalize to a 64-hex-char SHA-256 digest.
func validateEndpointTransport(ep OpenAIEndpoint, allowPlaintext bool) error {
	u, err := url.Parse(ep.BaseURL)
	if err != nil {
		return fmt.Errorf("endpoint %q: invalid baseURL %q: %w", ep.Name, ep.BaseURL, err)
	}

	switch u.Scheme {
	case "https":
		// verified below
	case "http":
		if !netutil.IsLoopbackHost(u.Hostname()) {
			if !allowPlaintext {
				return fmt.Errorf("endpoint %q: baseURL %q is plain http to a non-loopback host; use https with caFile, or set top-level \"allowPlaintextEndpoints\": true to acknowledge this hop is unencrypted", ep.Name, ep.BaseURL)
			}
			slog.Warn("endpoint TLS: plaintext http to a non-loopback host allowed by allowPlaintextEndpoints",
				"endpoint", ep.Name, "baseURL", ep.BaseURL)
		}
	default:
		return fmt.Errorf("endpoint %q: baseURL scheme must be http or https, got %q", ep.Name, u.Scheme)
	}

	hasCA := ep.CAFile != ""
	hasPins := len(ep.PinSHA256) > 0
	if u.Scheme == "http" {
		if hasCA || hasPins {
			return fmt.Errorf("endpoint %q: caFile/pinSHA256 have no effect on an http baseURL and must not be set alongside one", ep.Name)
		}
		return nil
	}

	if hasCA {
		if _, err := loadCAPool(ep.CAFile); err != nil {
			return fmt.Errorf("endpoint %q: %w", ep.Name, err)
		}
	}
	for _, pin := range ep.PinSHA256 {
		if _, err := normalizePin(pin); err != nil {
			return fmt.Errorf("endpoint %q: %w", ep.Name, err)
		}
	}
	return nil
}

// newEndpointTransport clones base (the caller's http.DefaultTransport or
// virtualDialTransport, so dial-timeout behavior on the virtual-model retry
// path survives) and, for an https endpoint, installs the pinned
// TLSClientConfig: RootCAs from caFile when set (nil RootCAs falls back to
// the platform's system pool, same as an unset TLSClientConfig), and a
// VerifyConnection callback when pins are configured. ep is assumed already
// valid (validateEndpointTransport must run first); an http endpoint's clone
// is returned unchanged.
func newEndpointTransport(ep OpenAIEndpoint, base *http.Transport) (*http.Transport, error) {
	u, err := url.Parse(ep.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("endpoint %q: invalid baseURL %q: %w", ep.Name, ep.BaseURL, err)
	}
	t := base.Clone()
	if u.Scheme != "https" {
		return t, nil
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if ep.CAFile != "" {
		pool, err := loadCAPool(ep.CAFile)
		if err != nil {
			return nil, fmt.Errorf("endpoint %q: %w", ep.Name, err)
		}
		tlsCfg.RootCAs = pool
	}

	if len(ep.PinSHA256) > 0 {
		pins := make(map[string]struct{}, len(ep.PinSHA256))
		for _, raw := range ep.PinSHA256 {
			norm, err := normalizePin(raw)
			if err != nil {
				return nil, fmt.Errorf("endpoint %q: %w", ep.Name, err)
			}
			pins[norm] = struct{}{}
		}
		name := ep.Name
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			// VerifyConnection runs AFTER the standard library's own chain
			// verification when InsecureSkipVerify is false (the case here —
			// that field is never set anywhere in this codebase), so by the
			// time this callback runs the leaf has already chained to a
			// trusted root (system pool or caFile above). This only adds the
			// fingerprint check on top — it is not a substitute for chain
			// verification, which is what makes pinning-a-valid-but-wrong-cert
			// (the MITM-with-a-valid-cert case) catchable at all.
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("endpoint %q: no peer certificate presented", name)
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			fp := hex.EncodeToString(sum[:])
			if _, ok := pins[fp]; !ok {
				return fmt.Errorf("endpoint %q: presented certificate fingerprint %s matches none of the configured pinSHA256 entries", name, fp)
			}
			return nil
		}
	}

	t.TLSClientConfig = tlsCfg
	return t, nil
}

// prepareEndpointTransports validates ep's TLS configuration and, on
// success, builds and caches both of its transports (see OpenAIEndpoint's
// transport/virtualTransport fields) so no outbound call site constructs a
// transport per request. Called once per endpoint by normalizeOpenAI, at
// config load — a bad endpoint fails relayLLM startup rather than the first
// chat request against it.
func prepareEndpointTransports(ep *OpenAIEndpoint, allowPlaintext bool) error {
	if err := validateEndpointTransport(*ep, allowPlaintext); err != nil {
		return err
	}
	t, err := newEndpointTransport(*ep, http.DefaultTransport.(*http.Transport))
	if err != nil {
		return err
	}
	vt, err := newEndpointTransport(*ep, virtualDialTransport.(*http.Transport))
	if err != nil {
		return err
	}
	ep.transport = t
	ep.virtualTransport = vt
	return nil
}
