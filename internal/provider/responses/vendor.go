package responses

import (
	"net/url"
	"strings"
)

// vendorCapabilities describes how a Responses-compatible endpoint deviates
// from the base OpenAI Responses wire behavior. Vendors are detected from the
// base URL (DetectVendor); unknown endpoints get the zero value, which is the
// standard OpenAI-compatible behavior (stateful, no session-cache header, no
// summary requirement, no tool-call reasoning retention, temperature honored).
//
// This table is the single source of truth for wire-level vendor differences:
// adding a new Responses-compatible vendor means adding one entry here and a
// base-URL case in DetectVendor — not threading more string comparisons
// through responses.go.
type vendorCapabilities struct {
	// stateless marks endpoints that reject previous_response_id and require
	// the full input history on every turn (DeepSeek, MiMo). stateful is the
	// OpenAI default.
	stateless bool

	// sessionCacheHeader marks DashScope, whose session cache must be opted
	// into with the x-dashscope-session-cache header.
	sessionCacheHeader bool

	// toolCallReasoning marks stateless vendors whose documentation requires
	// retaining historical reasoning content in the input on multi-turn tool
	// calls (DeepSeek, MiMo).
	toolCallReasoning bool

	// ignoresTemperature marks vendors that force temperature/top_p to their
	// defaults in thinking mode, so sending them is a no-op (MiMo forces
	// 1.0 / 0.95). Keeps the wire request lean for such endpoints.
	ignoresTemperature bool
}

var vendorTable = map[string]vendorCapabilities{
	"dashscope": {
		stateless:          false,
		sessionCacheHeader: true,
		toolCallReasoning:  false,
		ignoresTemperature: false,
	},
	"deepseek": {
		stateless:          true,
		sessionCacheHeader: false,
		toolCallReasoning:  true,
		ignoresTemperature: false,
	},
	"mimo": {
		stateless:          true,
		sessionCacheHeader: false,
		toolCallReasoning:  true,
		ignoresTemperature: true,
	},
	// "" (unknown OpenAI-compatible endpoint) → zero value = default behavior.
}

// capabilitiesFor returns the wire capabilities for a detected vendor name.
// Unknown vendors fall back to the zero value (standard OpenAI behavior).
func capabilitiesFor(vendor string) vendorCapabilities {
	return vendorTable[vendor]
}

// DetectVendor identifies endpoint behavior that affects the Responses wire.
// Empty means an unknown OpenAI-compatible endpoint with default behavior.
func DetectVendor(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "dashscope.aliyuncs.com", strings.HasSuffix(host, ".dashscope.aliyuncs.com"), strings.HasSuffix(host, ".maas.aliyuncs.com"):
		return "dashscope"
	case host == "api.deepseek.com", strings.HasSuffix(host, ".deepseek.com"):
		return "deepseek"
	case host == "api.xiaomimimo.com", strings.HasSuffix(host, ".xiaomimimo.com"):
		return "mimo"
	default:
		return ""
	}
}
