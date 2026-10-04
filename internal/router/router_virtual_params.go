package router

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
)

// paramsUse reports what applyVirtualParams did, for the log line. Paths are
// dotted and sorted; both slices are non-nil when applied.
type paramsUse struct {
	applied  bool
	injected []string
	kept     []string
}

// applyVirtualParams merges a target's declared params into the request body.
// A declared key the client did not send is injected; where both sides hold an
// object the merge recurses; anything else (arrays, scalars, a client null)
// leaves the client's value whole. Params and body stay json.RawMessage so
// untouched values keep their bytes. When nothing is injected the original
// slice is returned, so a fully client-specified request is not re-marshalled.
// A body that is not a JSON object (invalid, multipart) passes through.
func applyVirtualParams(body []byte, params json.RawMessage) ([]byte, paramsUse) {
	if len(bytes.TrimSpace(params)) == 0 {
		return body, paramsUse{}
	}
	var declared map[string]json.RawMessage
	if json.Unmarshal(params, &declared) != nil || len(declared) == 0 {
		return body, paramsUse{} // null, {} or non-object: nothing declared
	}
	var client map[string]json.RawMessage
	if json.Unmarshal(body, &client) != nil || client == nil {
		return body, paramsUse{}
	}
	use := paramsUse{applied: true, injected: []string{}, kept: []string{}}
	changed := mergeParams(client, declared, "", &use)
	sort.Strings(use.injected)
	sort.Strings(use.kept)
	if !changed {
		return body, use
	}
	out, err := json.Marshal(client)
	if err != nil {
		return body, paramsUse{} // unreachable for RawMessage values from valid JSON
	}
	return out, use
}

func mergeParams(client, declared map[string]json.RawMessage, prefix string, use *paramsUse) bool {
	changed := false
	for key, dv := range declared {
		path := prefix + key
		cv, ok := client[key]
		if !ok {
			client[key] = dv
			use.injected = append(use.injected, path)
			changed = true
			continue
		}
		var dObj, cObj map[string]json.RawMessage
		if json.Unmarshal(dv, &dObj) == nil && dObj != nil &&
			json.Unmarshal(cv, &cObj) == nil && cObj != nil {
			if mergeParams(cObj, dObj, path+".", use) {
				merged, err := json.Marshal(cObj)
				if err == nil {
					client[key] = merged
					changed = true
				}
			}
			continue
		}
		use.kept = append(use.kept, path)
	}
	return changed
}

// logVirtualParams writes the one line per attempt that used declared params.
// It names fields only: values, prompts and credentials never reach the log.
func logVirtualParams(ctx context.Context, virtual string, target ResolvedVirtualTarget, use paramsUse, status int) {
	level := slog.LevelInfo
	if status >= http.StatusBadRequest {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "relay router: virtual model params applied",
		"model", virtual, "target", target.TargetHeaderValue(),
		"injected", use.injected, "kept", use.kept, "status", status)
}
