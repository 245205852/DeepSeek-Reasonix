package control

import (
	"context"
	"strings"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
)

func steerFallbackController(t *testing.T) (*Controller, *agent.Agent) {
	t.Helper()
	prov := &scriptedTurns{turns: [][]provider.Chunk{textTurn("ok")}}
	ag := agent.New(prov, tool.NewRegistry(), agent.NewSession(""), agent.Options{}, event.Discard)
	return New(Options{Runner: ag, Executor: ag, Sink: event.Discard}), ag
}

func sessionHasUserText(ag *agent.Agent, text string) bool {
	for _, m := range ag.Session().Snapshot() {
		if strings.Contains(m.Content, text) {
			return true
		}
	}
	return false
}

// TestSteerFallbackParksWhileRunning forces the turn-exit window: the
// controller still reports running (the previous body has not returned), but
// the agent's steer intake is already closed, so exec.Steer rejects the text.
// The fallback must park the steer as guidance and deliver it when the window
// closes — runGuarded's deliberately-silent running drop would lose the
// user's words.
func TestSteerFallbackParksWhileRunning(t *testing.T) {
	c, ag := steerFallbackController(t)

	block := make(chan struct{})
	started := make(chan struct{})
	c.runGuarded(func(context.Context) error {
		close(started)
		<-block
		return nil
	})
	<-started

	c.Steer("queued while exiting")

	c.mu.Lock()
	parked := len(c.parkedTurns)
	c.mu.Unlock()
	if parked != 1 {
		t.Fatalf("steer fallback should park while running, parked=%d", parked)
	}

	close(block)
	waitIdleAdmission(t, c)
	deadline := time.Now().Add(30 * time.Second)
	for !sessionHasUserText(ag, "queued while exiting") {
		if time.Now().After(deadline) {
			t.Fatalf("parked steer was never delivered as a turn")
		}
		time.Sleep(time.Millisecond)
		waitIdleAdmission(t, c)
	}
	for _, m := range ag.Session().Snapshot() {
		if strings.Contains(m.Content, "queued while exiting") {
			if got, ok := agent.SteerText(m.Content); !ok || got != "queued while exiting" {
				t.Fatalf("parked fallback = %q, want persisted steer guidance", m.Content)
			}
			if !m.LocalOnly {
				t.Fatal("parked fallback must not enter a later model request")
			}
		}
	}
}

// TestSteerBetweenTurnsRunsGuidanceTurn pins the idle fallback: with no turn
// running the executor rejects the steer, and the controller must submit it
// as synthetic guidance instead of returning silently (also covers the
// executor-rejection path a stale frontend running flag would hit).
func TestSteerBetweenTurnsRunsGuidanceTurn(t *testing.T) {
	c, ag := steerFallbackController(t)

	c.Steer("late steer")

	deadline := time.Now().Add(30 * time.Second)
	for !sessionHasUserText(ag, "late steer") {
		if time.Now().After(deadline) {
			t.Fatalf("idle steer was never delivered as a turn")
		}
		time.Sleep(time.Millisecond)
		waitIdleAdmission(t, c)
	}
	for _, m := range ag.Session().Snapshot() {
		if strings.Contains(m.Content, "late steer") {
			if got, ok := agent.SteerText(m.Content); !ok || got != "late steer" {
				t.Fatalf("idle fallback = %q, want persisted steer guidance", m.Content)
			}
			if !m.LocalOnly {
				t.Fatal("idle fallback must not enter a later model request")
			}
		}
	}
}

func TestSteerFallbackDoesNotInjectCapabilityRoute(t *testing.T) {
	runner := &capabilityRecordingRunner{}
	ag := agent.New(nil, tool.NewRegistry(), agent.NewSession(""), agent.Options{}, event.Discard)
	reg := tool.NewRegistry()
	reg.Add(capabilityTestTool{name: "run_skill"})
	c := New(Options{
		Runner:   runner,
		Executor: ag,
		Skills: []skill.Skill{{
			Name:        "code-edit",
			Description: "modify source code",
			Scope:       skill.ScopeBuiltin,
		}},
		Registry: reg,
	})

	c.Steer("modify polish_client.py")
	waitIdleAdmission(t, c)

	if runner.input != "" {
		t.Fatalf("steer fallback unexpectedly opened a model turn:\n%s", runner.input)
	}
	if strings.Contains(runner.input, "<capability-route") {
		t.Fatalf("steer fallback received capability routing:\n%s", runner.input)
	}
	msgs := ag.Session().Snapshot()
	if len(msgs) != 1 {
		t.Fatalf("unapplied steer messages = %d, want 1 local record", len(msgs))
	}
	if got, ok := agent.SteerText(msgs[0].Content); !ok || got != "modify polish_client.py" || !msgs[0].LocalOnly {
		t.Fatalf("unapplied steer = %+v, want stable local-only guidance", msgs[0])
	}
}
