package agent

import (
	"context"
	"fmt"
	"strings"

	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/planmode"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func (a *Agent) withAgentContext(ctx context.Context) context.Context {
	if a == nil {
		return ctx
	}
	if a.jobs != nil {
		ctx = jobs.WithManager(ctx, a.jobs)
	} else {
		ctx = jobs.WithoutManager(ctx)
	}
	return planmode.WithActive(ctx, a.planMode.Load())
}

func subagentProviderContext(ctx context.Context) context.Context {
	ctx = tool.WithoutGoalTurnRecorder(ctx)
	ctx = jobs.WithoutManager(ctx)
	return memory.WithoutQueue(ctx)
}

func (a *Agent) unavailableContextualToolCalls(ctx context.Context, calls []provider.ToolCall) ([]string, bool) {
	if len(calls) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		t, ok := a.tools.Get(call.Name)
		if !ok {
			continue
		}
		contextual, ok := t.(tool.ContextualTool)
		if ok && !contextual.ProviderVisible(ctx) {
			names = append(names, call.Name)
		}
	}
	return names, len(names) == len(calls)
}

func (a *Agent) rejectRepeatedContextToolCalls(state *runLoopState, calls []provider.ToolCall, unavailable []string) error {
	if len(unavailable) == 0 || state.contextToolRepairs == 0 {
		return nil
	}
	msg := fmt.Sprintf("blocked: context-unavailable tools were called again after the repair instruction: %s", strings.Join(unavailable, ", "))
	for _, call := range calls {
		a.session.Add(provider.Message{Role: provider.RoleTool, Content: msg, ToolCallID: call.ID, Name: call.Name})
	}
	return fmt.Errorf("model repeatedly called context-unavailable tools without a visible answer: %s", strings.Join(unavailable, ", "))
}

func (a *Agent) repairContextToolCalls(ctx context.Context, state *runLoopState, text, reasoning string, usage *provider.Usage, unavailable []string, contextualOnly bool) (bool, bool, error) {
	if len(unavailable) == 0 {
		return false, false, nil
	}
	if contextualOnly && hasVisibleFinalAnswer(text) {
		cont, err := a.handleFinalResponse(ctx, state, text, reasoning, usage)
		return true, cont, err
	}
	state.contextToolRepairs++
	nudge := fmt.Sprintf("The following tools are unavailable in the current workflow phase: %s. Do not call them again. Respond to the user's request with visible answer text now; call a different tool only if it is still needed to complete the request.", strings.Join(unavailable, ", "))
	a.session.Add(provider.Message{Role: provider.RoleUser, Content: a.withTurnPreferences(nudge)})
	return false, false, nil
}
