package config

import (
	"fmt"
	"slices"
)

// Wire APIs an upstream can declare in its `api` list. Absent/empty means
// openai only, which is today's behavior (Anthropic requests are translated).
// "openai" is informational: the OpenAI route never gates on it.
const (
	APIOpenAI    = "openai"
	APIAnthropic = "anthropic"
)

// ValidateAPIs rejects any value outside the known set. It is a load error,
// not a warning: a typo such as "anthropics" would otherwise fall back to
// translation silently and send debugging the wrong way. Matching is exact
// (lowercase); duplicates are accepted.
func ValidateAPIs(where string, apis []string) error {
	for _, a := range apis {
		if a != APIOpenAI && a != APIAnthropic {
			return fmt.Errorf("%s.api: unknown value %q (allowed: %s, %s)", where, a, APIOpenAI, APIAnthropic)
		}
	}
	return nil
}

// SpeaksAnthropic reports whether apis declares the anthropic dialect.
func SpeaksAnthropic(apis []string) bool {
	return slices.Contains(apis, APIAnthropic)
}

// SpeaksAnthropic reports whether this endpoint serves the Anthropic Messages API.
func (ep OpenAIEndpoint) SpeaksAnthropic() bool { return SpeaksAnthropic(ep.API) }

// SpeaksAnthropic reports whether the section declares anthropic. The flag is
// section-level because per-model keys map 1:1 to CLI flags; nil-safe.
func (c *ServerConfig) SpeaksAnthropic() bool {
	return c != nil && SpeaksAnthropic(c.API)
}
