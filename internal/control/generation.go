package control

import (
	"reasonix/internal/event"
	"reasonix/internal/extension"
)

// RuntimePhase is the observable publish/drain phase for this controller.
type RuntimePhase string

const (
	RuntimePhaseActive   RuntimePhase = "Active"
	RuntimePhaseDraining RuntimePhase = "Draining"
	RuntimePhaseUnknown  RuntimePhase = "Unknown"
)

// SetRuntimeGeneration binds turn admission to the extension PublishGate.
// Zero clears generation-based admission.
func (c *Controller) SetRuntimeGeneration(gen uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.runtimeGeneration = gen
	c.mu.Unlock()
}

// RuntimeGeneration returns the generation this controller serves.
func (c *Controller) RuntimeGeneration() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runtimeGeneration
}

// RuntimePhase reports Active when this generation is published, Draining
// when superseded, Unknown when generation tracking is disabled.
func (c *Controller) RuntimePhase() RuntimePhase {
	if c == nil {
		return RuntimePhaseUnknown
	}
	gen := c.RuntimeGeneration()
	if gen == 0 {
		return RuntimePhaseUnknown
	}
	gate := extension.DefaultPublishGate()
	if gate.AdmitNewWork(gen) {
		return RuntimePhaseActive
	}
	if gate.IsDraining(gen) || gate.IsStale(gen) {
		return RuntimePhaseDraining
	}
	return RuntimePhaseUnknown
}

func (c *Controller) rejectDrainingGenerationLocked() bool {
	gen := c.runtimeGeneration
	if gen == 0 || extension.DefaultPublishGate().AdmitNewWork(gen) {
		return false
	}
	extension.DefaultLifecycleMetrics.AdmissionRejected.Add(1)
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "input was not accepted: runtime is draining after rebuild — please resend"})
	return true
}

// LastResumeDecision returns the most recent DecideResume result from
// checkpoint open (zero value when never assessed).
func (c *Controller) LastResumeDecision() extension.ResumeDecision {
	if c == nil {
		return extension.ResumeDecision{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastResumeDecision
}

// AssessResume runs DecideResume for the controller's generation. Recovery
// and doctor use this to refuse claiming clean rollback after irreversible
// external work.
func (c *Controller) AssessResume() extension.ResumeDecision {
	if c == nil {
		return extension.ResumeDecision{AllowResume: true, CleanRollback: true}
	}
	gen := c.RuntimeGeneration()
	if gen == 0 {
		gen = extension.DefaultPublishGate().Published()
	}
	d := extension.DecideResumeDefault(gen)
	c.mu.Lock()
	c.lastResumeDecision = d
	c.mu.Unlock()
	return d
}
