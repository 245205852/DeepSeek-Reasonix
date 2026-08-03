package stats

import (
	"strings"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
)

// Recorder is a passthrough event.Sink that snapshots token usage (event.Usage)
// and completed turns (event.TurnDone) into the daily stats files. It observes
// only; it never alters the event stream.
//
// Wire it around the frontend sink at the boot layer so every entry point
// (desktop, CLI, serve) records consistently; Source distinguishes them.
type Recorder struct {
	inner  event.Sink
	writer *Writer
	source string
}

// NewRecorder wraps inner with usage recording. source labels every record
// (desktop/cli/serve/...); an empty source keeps records unlabelled.
func NewRecorder(inner event.Sink, dir, source string) *Recorder {
	return &Recorder{inner: inner, writer: NewWriter(dir), source: strings.TrimSpace(source)}
}

// Emit records the event (if it carries billable usage or ends a turn) then
// forwards it unchanged.
func (r *Recorder) Emit(e event.Event) {
	if r != nil && r.writer != nil && e.Kind == event.Usage {
		r.recordUsage(e)
	} else if r != nil && r.writer != nil && e.Kind == event.GuardianAssessment && e.Guardian.Usage != nil {
		r.recordProviderUsage(e.ModelRef, e.Guardian.Usage)
	} else if r != nil && r.writer != nil && e.Kind == event.TurnDone {
		r.RecordTurnCompletion()
	}
	if r != nil && r.inner != nil {
		r.inner.Emit(e)
	}
}

// RecordTurnCompletion records synchronous controller runs that deliberately do
// not emit TurnDone into the UI event stream.
func (r *Recorder) RecordTurnCompletion() {
	if r == nil || r.writer == nil {
		return
	}
	_ = r.writer.Append(record{Timestamp: time.Now(), Source: r.source, Turn: true})
}

// RecordReadinessAudit forwards audit receipts to the wrapped sink.
func (r *Recorder) RecordReadinessAudit(a evidence.ReadinessAudit) {
	event.RecordReadinessAudit(r.inner, a)
}

// RecordProtocolRecovery preserves the wrapped sink's audit capability.
func (r *Recorder) RecordProtocolRecovery(a event.ProtocolRecoveryAudit) {
	event.RecordProtocolRecovery(r.inner, a)
}

func (r *Recorder) recordUsage(e event.Event) {
	r.recordProviderUsage(e.ModelRef, e.Usage)
}

func (r *Recorder) recordProviderUsage(modelRef string, usage *provider.Usage) {
	if usage == nil || usage.TotalTokens <= 0 {
		return
	}
	// Recording is best-effort: a stats file failure (disk full, permissions)
	// must never interrupt the event stream, matching telemetry's append idiom.
	_ = r.writer.Append(record{
		Timestamp:  time.Now(),
		ModelRef:   modelRef,
		Source:     r.source,
		Prompt:     usage.PromptTokens,
		Completion: usage.CompletionTokens,
		Reasoning:  usage.ReasoningTokens,
		CacheHit:   usage.CacheHitTokens,
		CacheMiss:  usage.CacheMissTokens,
		Total:      usage.TotalTokens,
		Requests:   usageRequestCount(usage),
	})
}

func usageRequestCount(usage *provider.Usage) int {
	if usage != nil && usage.RequestCount > 0 {
		return usage.RequestCount
	}
	return 1
}
