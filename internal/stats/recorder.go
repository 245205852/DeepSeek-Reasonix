package stats

import (
	"strings"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
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
	} else if r != nil && r.writer != nil && e.Kind == event.TurnDone {
		_ = r.writer.Append(record{
			Timestamp: time.Now(),
			Source:    r.source,
			Turn:      true,
		})
	}
	if r != nil && r.inner != nil {
		r.inner.Emit(e)
	}
}

// RecordReadinessAudit forwards audit receipts to the wrapped sink.
func (r *Recorder) RecordReadinessAudit(a evidence.ReadinessAudit) {
	event.RecordReadinessAudit(r.inner, a)
}

func (r *Recorder) recordUsage(e event.Event) {
	if e.Usage == nil || e.Usage.TotalTokens <= 0 {
		return
	}
	// Recording is best-effort: a stats file failure (disk full, permissions)
	// must never interrupt the event stream, matching telemetry's append idiom.
	_ = r.writer.Append(record{
		Timestamp:  time.Now(),
		ModelRef:   e.ModelRef,
		Source:     r.source,
		Prompt:     e.Usage.PromptTokens,
		Completion: e.Usage.CompletionTokens,
		Reasoning:  e.Usage.ReasoningTokens,
		CacheHit:   e.Usage.CacheHitTokens,
		CacheMiss:  e.Usage.CacheMissTokens,
		Total:      e.Usage.TotalTokens,
	})
}
