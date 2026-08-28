package tool

import (
	"context"
	"encoding/json"
	"strings"
)

// MCPAppResult is the host-local MCP Apps payload one tools/call can produce:
// the standard CallToolResult plus the resource identity a Desktop App needs
// to restore its UI. It travels on the call context (never the tool) and is
// converted to the provider-excluded provider.MCPAppPresentation by the agent.
type MCPAppResult struct {
	Server      string
	Tool        string
	Generation  uint64
	ResourceURI string
	CSP         map[string][]string
	RawResult   json.RawMessage
	Structured  json.RawMessage
}

// maxMCPAppBytes bounds the persisted presentation copy. Oversized results
// keep the text form only.
const maxMCPAppBytes = 512 << 10

// droppableMCPAppContent reports items that never enter the persisted copy:
// audio/video and oversized base64 data blow the budget without adding App
// value; the model-facing text/image forms live on the message itself.
func droppableMCPAppContent(item map[string]any) bool {
	switch item["type"] {
	case "audio", "video":
		return true
	case "image", "resource":
		return false
	}
	if data, ok := item["data"].(string); ok && len(data) > 4096 {
		return true
	}
	mime, _ := item["mimeType"].(string)
	return strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/")
}

// Sanitized returns the bounded presentation copy: inline audio/video and
// oversized base64 content items are dropped, then the whole payload is
// capped at maxMCPAppBytes (text fallback remains in Content).
func (r *MCPAppResult) Sanitized() *MCPAppResult {
	if r == nil {
		return nil
	}
	out := &MCPAppResult{
		Server:      r.Server,
		Tool:        r.Tool,
		Generation:  r.Generation,
		ResourceURI: r.ResourceURI,
		CSP:         r.CSP,
	}
	if len(r.RawResult) > 0 {
		var parsed struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal(r.RawResult, &parsed); err == nil {
			kept := parsed.Content[:0]
			for _, item := range parsed.Content {
				if !droppableMCPAppContent(item) {
					kept = append(kept, item)
				}
			}
			parsed.Content = kept
			if b, err := json.Marshal(parsed); err == nil {
				out.RawResult = b
			}
		} else {
			out.RawResult = r.RawResult
		}
	}
	if len(out.RawResult) > maxMCPAppBytes {
		out.RawResult = nil
	}
	if len(r.Structured) <= maxMCPAppBytes {
		out.Structured = r.Structured
	}
	return out
}

type mcpAppKey struct{}

// WithMCPAppCollector attaches a presentation collector the executing tool
// fills in. Contexts are immutable, so the callee cannot hand a value back by
// deriving a new ctx; the collector is a shared cell the caller reads after
// the call. Returns the collector alongside the derived context.
func WithMCPAppCollector(ctx context.Context) (context.Context, *MCPAppResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	sink := &MCPAppResult{}
	return context.WithValue(ctx, mcpAppKey{}, sink), sink
}

// CollectMCPAppResult fills the call's collector; a no-op without one.
func CollectMCPAppResult(ctx context.Context, r *MCPAppResult) {
	if ctx == nil || r == nil {
		return
	}
	if sink, ok := ctx.Value(mcpAppKey{}).(*MCPAppResult); ok && sink != nil {
		*sink = *r
	}
}
