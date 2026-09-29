package router

import (
	"encoding/json"
	"fmt"
)

// rewriteProxyBody replaces the top-level "model" field of a proxied body
// with model (endpoint routes rewrite "endpoint.Name/id" down to the bare id
// the endpoint itself expects). An empty model means there is nothing to
// swap: the body is returned completely untouched rather than round-tripped
// through encoding/json, so it stays byte-identical. No other field is
// interpreted or changed; reasoning fields such as reasoning_effort and
// chat_template_kwargs are forwarded exactly as the client sent them.
//
// RawMessage avoids re-marshalling nested payloads (large image_url parts,
// ordered tool definitions, etc.) when the swap does happen. Top-level key
// order is not preserved in that case: json.Marshal of a map sorts keys.
func rewriteProxyBody(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal envelope: %w", err)
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, fmt.Errorf("encode upstream id: %w", err)
	}
	raw["model"] = encoded
	return json.Marshal(raw)
}
