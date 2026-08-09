package anthropic

import "reasonix/internal/provider"

const (
	anthropicContextManagementBeta  = "context-management-2025-06-27"
	anthropicToolClearPolicyVersion = "clear_tool_uses_20250919"
)

func (c *client) ContextEditingCapabilities() provider.ContextEditingCapabilities {
	if c == nil || !c.nativeAnthropic {
		return provider.ContextEditingCapabilities{}
	}
	return provider.ContextEditingCapabilities{
		NativeToolUseClear: true,
		PolicyVersion:      anthropicToolClearPolicyVersion,
	}
}

type contextManagement struct {
	Edits []contextEdit `json:"edits,omitempty"`
}

type contextEdit struct {
	Type            string              `json:"type"`
	Trigger         *contextEditTrigger `json:"trigger,omitempty"`
	Keep            *contextEditKeep    `json:"keep,omitempty"`
	ClearAtLeast    *contextEditMinimum `json:"clear_at_least,omitempty"`
	ClearToolInputs bool                `json:"clear_tool_inputs,omitempty"`
}

type contextEditTrigger struct {
	Type  string `json:"type"`
	Value int    `json:"value"`
}

type contextEditKeep struct {
	Type  string `json:"type"`
	Value int    `json:"value"`
}

type contextEditMinimum struct {
	Type  string `json:"type"`
	Value int    `json:"value"`
}

func applyNativeContextEditing(r *anthRequest, req provider.Request, nativeAnthropic bool) {
	if !nativeAnthropic || req.ContextEditing == nil || req.ContextEditing.Mode != "native" {
		return
	}
	policy := req.ContextEditing
	r.ContextManagement = &contextManagement{Edits: []contextEdit{{
		Type:            anthropicToolClearPolicyVersion,
		Trigger:         &contextEditTrigger{Type: "input_tokens", Value: policy.TriggerInputTokens},
		Keep:            &contextEditKeep{Type: "tool_uses", Value: policy.KeepToolUses},
		ClearAtLeast:    &contextEditMinimum{Type: "input_tokens", Value: policy.ClearAtLeastInputTokens},
		ClearToolInputs: policy.ClearToolInputs,
	}}}
}

type responseContextManagement struct {
	AppliedEdits []appliedContextEdit `json:"applied_edits"`
}

type appliedContextEdit struct {
	Type               string `json:"type"`
	ClearedToolUses    int    `json:"cleared_tool_uses"`
	ClearedInputTokens int    `json:"cleared_input_tokens"`
}

type contextEditUsage struct {
	typeName           string
	clearedToolUses    int
	clearedInputTokens int
}

func (u *contextEditUsage) observe(management *responseContextManagement) {
	if management == nil {
		return
	}
	for _, edit := range management.AppliedEdits {
		if edit.Type == anthropicToolClearPolicyVersion {
			u.typeName = edit.Type
			u.clearedToolUses += edit.ClearedToolUses
			u.clearedInputTokens += edit.ClearedInputTokens
		}
	}
}

func (u contextEditUsage) apply(usage *provider.Usage) {
	usage.ContextEditingType = u.typeName
	usage.ContextEditingClearedToolUses = u.clearedToolUses
	usage.ContextEditingClearedTokens = u.clearedInputTokens
}
