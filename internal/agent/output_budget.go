package agent

import (
	"fmt"
	"sync/atomic"

	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

const outputBudgetReserve = 8 * 1024

type outputBudgetState struct {
	outputBudget      int
	activeReqChars    atomic.Int64
	promptCalibration atomic.Pointer[promptTokenCalibration]
}

type promptTokenCalibration struct {
	promptTokens int
	chars        int64
}

func (a *Agent) resetOutputBudgetState() {
	a.lastUsage.Store(nil)
	a.activeReqChars.Store(0)
	a.promptCalibration.Store(nil)
}

func (a *Agent) setPromptTokenCalibration(promptTokens int, chars int64) {
	if a == nil || promptTokens <= 0 || chars <= 0 {
		return
	}
	a.promptCalibration.Store(&promptTokenCalibration{promptTokens: promptTokens, chars: chars})
}

func outputBudgetOf(p provider.Provider) int {
	if nilutil.IsNil(p) {
		return 0
	}
	if budget, ok := p.(provider.OutputBudgetProvider); ok {
		return budget.OutputBudget()
	}
	return 0
}

func sharesContextWindow(p provider.Provider) bool {
	if nilutil.IsNil(p) {
		return false
	}
	shared, ok := p.(provider.SharedWindowOutputProvider)
	return ok && shared.SharesContextWindow()
}

func (a *Agent) configuredOutputBudget(explicit int) int {
	if explicit != 0 {
		return explicit
	}
	return a.outputBudget
}

// estimatedPromptTokens sizes the final provider-visible messages for overflow
// protection. Real same-session usage calibrates the estimate; before that,
// CJK text gets a conservative second token per rune to cover the measured
// tokenizer gap that can otherwise postpone compaction past the hard limit.
func (a *Agent) estimatedPromptTokens(msgs []provider.Message) int {
	est := estimateMessagesTokens(provider.ModelMessages(msgs))
	if est <= 0 {
		return 0
	}
	if cal := a.promptCalibration.Load(); cal != nil {
		ratio := float64(cal.promptTokens) / float64(cal.chars)
		if ratio > 0.05 && ratio < 2 {
			return int(float64(charsOfMessages(msgs)) * ratio)
		}
	}
	return est + cjkRunesInMessages(msgs)
}

func cjkRunesInMessages(msgs []provider.Message) int {
	total := 0
	for _, msg := range msgs {
		if msg.LocalOnly {
			continue
		}
		total += cjkRunesIn(msg.Content) + cjkRunesIn(msg.ReasoningContent)
		total += cjkRunesIn(msg.Name) + cjkRunesIn(msg.ToolCallID)
		for _, call := range msg.ToolCalls {
			total += cjkRunesIn(call.ID) + cjkRunesIn(call.Name) + cjkRunesIn(call.Arguments)
		}
		for _, item := range msg.ResponsesItems {
			total += cjkRunesIn(string(item))
		}
	}
	return total
}

func cjkRunesIn(s string) int {
	total := 0
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) ||
			(r >= 0x3400 && r <= 0x4DBF) ||
			(r >= 0x3040 && r <= 0x30FF) ||
			(r >= 0xAC00 && r <= 0xD7AF) {
			total++
		}
	}
	return total
}

func estimateToolSchemaTokens(schemas []provider.ToolSchema) int {
	total := 0
	for _, schema := range schemas {
		total += 8
		total += estimateTextTokens(schema.Name)
		total += estimateTextTokens(schema.Description)
		total += estimateTextTokens(string(schema.Parameters))
	}
	return total
}

// effectiveOutputBudget returns an explicit smaller output budget only when a
// shared-window request would otherwise exceed the configured context window.
// The final extension-adjusted request is measured, so later payload rewrites
// cannot invalidate the decision. An exhausted window fails locally instead of
// sending a request the provider will reject with HTTP 400.
func (a *Agent) effectiveOutputBudget(req provider.Request) (int, bool, error) {
	if a == nil || a.contextWindow <= 0 || !sharesContextWindow(a.prov) {
		return 0, false, nil
	}
	budget := a.configuredOutputBudget(req.MaxTokens)
	if budget <= 0 {
		return 0, false, nil
	}
	est := a.estimatedPromptTokens(req.Messages) + estimateToolSchemaTokens(req.Tools)
	available := a.contextWindow - est - outputBudgetReserve
	if available <= 0 {
		return 0, false, fmt.Errorf("%w: estimated prompt %d leaves no shared-window output budget", ErrCompactionRequired, est)
	}
	if budget <= available {
		return 0, false, nil
	}
	return available, true, nil
}
