package agent

import (
	"reasonix/internal/event"
	"reasonix/internal/evidence"
)

// nestedSink is the event view a fleet or parallel_tasks item runs under: tool
// events are re-parented, usage is attributed to the sub-agent, everything else
// is dropped. It is a type rather than a func sink because optional sink
// capabilities are forwarded by method, and a bare func sink swallowed every
// delegation audit a fleet child produced.
type nestedSink struct {
	parentID string
	parent   event.Sink
}

func (s nestedSink) Emit(e event.Event) {
	switch e.Kind {
	case event.ToolDispatch, event.ToolResult, event.ToolProgress:
		e.Tool.ParentID = s.parentID
		e.Tool.ID = s.parentID + "/" + e.Tool.ID
		s.parent.Emit(e)
	case event.Usage:
		if e.UsageSource == "" {
			e.UsageSource = event.UsageSourceSubagent
		}
		s.parent.Emit(e)
	}
}

// Audits are accounting, not presentation: they pass straight through so a
// child's receipts reach the session's metrics rather than dying at the nesting
// boundary.
func (s nestedSink) RecordDelegationAudit(a evidence.DelegationAudit) {
	event.RecordDelegationAudit(s.parent, a)
}

func (s nestedSink) RecordReadinessAudit(a evidence.ReadinessAudit) {
	event.RecordReadinessAudit(s.parent, a)
}

func (s nestedSink) RecordCompletionReport(a event.CompletionReportAudit) {
	event.RecordCompletionReport(s.parent, a)
}
