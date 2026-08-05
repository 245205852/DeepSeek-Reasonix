package event

// Reserved ToolProgress channel names for local sub-agent progress previews.
//
// These names are an internal wire contract between the agent progress
// tracker and local frontends (desktop and CLI). They must never be presented
// as provider-visible tool names: ACP and bot consumers ignore ToolProgress
// bodies entirely, and the names are filtered out of any transcript or
// provider context by construction (they only ever ride ToolProgress events).
//
// The status channel carries exactly one of the phase values produced by the
// tracker (queued/running/reasoning/responding/tool/retrying/completed/
// failed/cancelled) in Tool.Output; the other channels carry bounded UTF-8
// text deltas. Tool.Truncated marks a merged or truncated preview round,
// Tool.DurationMs rides terminal status events, and progress lookup is keyed
// by Tool.ID (the child task card ID).
const (
	SubagentProgressStatusName    = "reasonix.subagent.status"
	SubagentProgressReasoningName = "reasonix.subagent.reasoning"
	SubagentProgressTextName      = "reasonix.subagent.text"
	SubagentProgressNoticeName    = "reasonix.subagent.notice"
)

// IsSubagentProgressName reports whether the ToolProgress Name carries a
// reserved sub-agent progress channel. Frontends use it to route progress
// previews away from ordinary tool output; ACP/bot rely on it only in tests
// locking that the bodies stay ignored.
func IsSubagentProgressName(name string) bool {
	switch name {
	case SubagentProgressStatusName, SubagentProgressReasoningName,
		SubagentProgressTextName, SubagentProgressNoticeName:
		return true
	}
	return false
}
