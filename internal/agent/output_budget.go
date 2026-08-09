package agent

import (
	"fmt"
	"math"
	"sync/atomic"

	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
)

const outputBudgetReserve = 8 * 1024

type outputBudgetState struct {
	outputBudget      int
	activeReqShape    atomic.Pointer[requestCalibrationShape]
	promptCalibration atomic.Pointer[promptTokenCalibration]
}

type promptTokenCalibration struct {
	promptTokens int
	requestChars int64
	compactChars int64
}

// requestCalibrationShape pairs the complete provider-visible text shape used
// for overflow protection with the legacy content-only shape used by fold
// economics. Keeping both in one immutable pointer ensures readers never pair
// requestChars from one prepared request with compactChars from another.
type requestCalibrationShape struct {
	requestChars int64
	compactChars int64
}

func (a *Agent) resetOutputBudgetState() {
	a.lastUsage.Store(nil)
	a.activeReqShape.Store(nil)
	a.promptCalibration.Store(nil)
}

func (a *Agent) setPromptTokenCalibration(promptTokens int, shape requestCalibrationShape) {
	if a == nil || promptTokens <= 0 || shape.requestChars <= 0 {
		return
	}
	a.promptCalibration.Store(&promptTokenCalibration{
		promptTokens: promptTokens,
		requestChars: shape.requestChars,
		compactChars: shape.compactChars,
	})
}

func (a *Agent) setPromptTokenCalibrationFromActive(promptTokens int) {
	if a == nil {
		return
	}
	if shape := a.activeReqShape.Load(); shape != nil {
		a.setPromptTokenCalibration(promptTokens, *shape)
	}
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

func requestCalibrationShapeOf(req provider.Request) requestCalibrationShape {
	return requestCalibrationShape{
		requestChars: requestCalibrationChars(req),
		compactChars: int64(charsOfMessages(req.Messages)),
	}
}

// requestCalibrationChars counts every textual field that a shared-window
// DeepSeek adapter can replay. In particular, reasoning and Responses items
// must grow the next request estimate even though ordinary OpenAI modes may
// strip them at serialization time; the hard guard is only enabled for the
// shared-window modes that retain those fields.
func requestCalibrationChars(req provider.Request) int64 {
	var total int64
	for _, msg := range req.Messages {
		if msg.LocalOnly {
			continue
		}
		total += int64(4 + len(msg.Role) + len(msg.Content) + len(msg.ReasoningContent))
		total += int64(len(msg.ReasoningID) + len(msg.ReasoningStatus) + len(msg.ReasoningSignature))
		total += int64(len(msg.Name) + len(msg.ToolCallID))
		for _, image := range msg.Images {
			total += int64(len(image))
		}
		for _, call := range msg.ToolCalls {
			total += int64(8 + len(call.ID) + len(call.Name) + len(call.Arguments) + len(call.ThoughtSignature))
		}
		for _, item := range msg.ResponsesItems {
			total += int64(len(item))
		}
	}
	for _, schema := range req.Tools {
		total += int64(8 + len(schema.Name) + len(schema.Description) + len(schema.Parameters))
	}
	return total
}

func (a *Agent) calibratedPromptTokens(chars int64) (int, bool) {
	if chars <= 0 {
		return 0, false
	}
	if cal := a.promptCalibration.Load(); cal != nil && cal.requestChars > 0 {
		ratio := float64(cal.promptTokens) / float64(cal.requestChars)
		if ratio > 0.05 && ratio < 2 {
			return int(math.Ceil(float64(chars) * ratio)), true
		}
	}
	return 0, false
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
	if calibrated, ok := a.calibratedPromptTokens(requestCalibrationChars(provider.Request{Messages: msgs})); ok {
		return calibrated
	}
	return est + cjkRunesInMessages(msgs)
}

func (a *Agent) estimatedRequestTokens(req provider.Request) int {
	if calibrated, ok := a.calibratedPromptTokens(requestCalibrationChars(req)); ok {
		return calibrated
	}
	return a.estimatedPromptTokens(req.Messages) + estimateToolSchemaTokens(req.Tools)
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
	est := a.estimatedRequestTokens(req)
	available := a.contextWindow - est - outputBudgetReserve
	if available <= 0 {
		return 0, false, fmt.Errorf("%w: estimated prompt %d leaves no shared-window output budget", ErrCompactionRequired, est)
	}
	if budget <= available {
		return 0, false, nil
	}
	return available, true, nil
}
