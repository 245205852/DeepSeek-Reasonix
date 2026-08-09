package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

type sharedWindowTestProvider struct {
	budget int
	shared bool
	last   provider.Request
	calls  int
	finish string
}

func (*sharedWindowTestProvider) Name() string { return "shared-window-test" }

func (p *sharedWindowTestProvider) Stream(_ context.Context, req provider.Request) (<-chan provider.Chunk, error) {
	p.last = req
	p.calls++
	ch := make(chan provider.Chunk, 3)
	ch <- provider.Chunk{Type: provider.ChunkText, Text: "summary"}
	if p.finish != "" {
		ch <- provider.Chunk{Type: provider.ChunkUsage, Usage: &provider.Usage{FinishReason: p.finish}}
	}
	ch <- provider.Chunk{Type: provider.ChunkDone}
	close(ch)
	return ch, nil
}

func TestSharedWindowFoldUsesGuardedInputBudget(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	a := &Agent{
		prov:              prov,
		contextWindow:     100_000,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget},
		sink:              event.Discard,
	}
	fold := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 50_000)}}

	if _, err := a.foldToSummary(context.Background(), fold, ""); err != nil {
		t.Fatalf("foldToSummary: %v", err)
	}
	if prov.calls == 0 || len(prov.last.Messages) < 2 {
		t.Fatalf("guarded fold produced no summarizer request: calls=%d request=%+v", prov.calls, prov.last)
	}
	if got := prov.last.Messages[1].Content; len(got) >= len(renderTranscript(fold)) || !strings.Contains(got, "omitted") {
		t.Fatalf("cold CJK fold was not bounded before summarize: len=%d original=%d", len(got), len(renderTranscript(fold)))
	}
	if got := prov.last.MaxTokens; got < summaryOutputReserve {
		t.Fatalf("summarizer MaxTokens = %d, below summaryOutputReserve %d", got, summaryOutputReserve)
	}
}

func (p *sharedWindowTestProvider) OutputBudget() int         { return p.budget }
func (p *sharedWindowTestProvider) SharesContextWindow() bool { return p.shared }

func TestEffectiveOutputBudgetClipsSharedWindowRequest(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	a := &Agent{prov: prov, contextWindow: 1_048_576,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget}}
	msgs := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 950_000)}}
	// Calibrate this session at one token per rune. The 950K prompt fits, but
	// not beside the provider's full 128K output default.
	a.lastUsage.Store(&provider.Usage{PromptTokens: 950_000})
	a.setPromptTokenCalibration(950_000, requestCalibrationShapeOf(provider.Request{Messages: msgs}))

	got, clipped, err := a.effectiveOutputBudget(provider.Request{Messages: msgs})
	if err != nil {
		t.Fatalf("effectiveOutputBudget: %v", err)
	}
	if !clipped {
		t.Fatal("near-window request kept the provider's full output budget")
	}
	if got <= 0 || got >= prov.budget {
		t.Fatalf("clipped budget = %d, want 0 < budget < %d", got, prov.budget)
	}
	if got+950_000 > a.contextWindow-outputBudgetReserve {
		t.Fatalf("input + output = %d, exceeds reserved shared window %d", got+950_000, a.contextWindow-outputBudgetReserve)
	}
}

func TestCalibratedOutputBudgetIncludesReplayedReasoning(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	a := &Agent{prov: prov, contextWindow: 200_000,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget}}
	previous := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("x", 300_000)}}
	a.setPromptTokenCalibration(75_000, requestCalibrationShapeOf(provider.Request{Messages: previous}))
	current := append(previous, provider.Message{
		Role:             provider.RoleAssistant,
		ReasoningContent: strings.Repeat("r", 400_000),
		ToolCalls:        []provider.ToolCall{{ID: "call_1", Name: "bash", Arguments: `{}`}},
	})

	before := a.estimatedPromptTokens(previous)
	after := a.estimatedPromptTokens(current)
	if after < before+99_000 {
		t.Fatalf("400K replayed reasoning was not calibrated: before=%d after=%d", before, after)
	}
	budget, clipped, err := a.effectiveOutputBudget(provider.Request{Messages: current})
	if err != nil {
		t.Fatalf("effectiveOutputBudget: %v", err)
	}
	if !clipped || budget > 20_000 {
		t.Fatalf("replayed reasoning budget = %d clipped=%v, want a clipped budget <= 20000", budget, clipped)
	}
}

func TestCalibratedOutputBudgetCountsToolSchemasOnce(t *testing.T) {
	a := &Agent{}
	req := provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("x", 100_000)}},
		Tools: []provider.ToolSchema{{
			Name: "lookup", Description: strings.Repeat("y", 100_000), Parameters: []byte(`{"type":"object"}`),
		}},
	}
	a.setPromptTokenCalibration(60_000, requestCalibrationShapeOf(req))

	if got := a.estimatedRequestTokens(req); got != 60_000 {
		t.Fatalf("calibrated request tokens = %d, want tool schema counted once in 60000", got)
	}
}

func TestPrepareSamplingRequestClipsSharedWindowOutput(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	msgs := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 950_000)}}
	sess := NewSession("")
	sess.Replace(msgs)
	a := &Agent{
		prov:              prov,
		tools:             tool.NewRegistry(),
		session:           sess,
		contextWindow:     1_048_576,
		compactRatio:      2,
		compactForceRatio: 2,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget},
	}
	a.lastUsage.Store(&provider.Usage{PromptTokens: 950_000})
	a.setPromptTokenCalibration(950_000, requestCalibrationShapeOf(provider.Request{Messages: msgs}))

	prepared, err := a.prepareSamplingRequest(context.Background())
	if err != nil {
		t.Fatalf("prepareSamplingRequest: %v", err)
	}
	if prepared.req.MaxTokens <= 0 || prepared.req.MaxTokens >= prov.budget {
		t.Fatalf("prepared MaxTokens = %d, want a clipped positive budget below %d", prepared.req.MaxTokens, prov.budget)
	}
}

func TestEffectiveOutputBudgetRejectsExhaustedSharedWindow(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	a := &Agent{prov: prov, contextWindow: 1_048_576,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget}}
	msgs := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 1_045_000)}}
	a.lastUsage.Store(&provider.Usage{PromptTokens: 1_045_000})
	a.setPromptTokenCalibration(1_045_000, requestCalibrationShapeOf(provider.Request{Messages: msgs}))

	_, _, err := a.effectiveOutputBudget(provider.Request{Messages: msgs})
	if !errors.Is(err, ErrCompactionRequired) {
		t.Fatalf("effectiveOutputBudget error = %v, want ErrCompactionRequired", err)
	}
}

func TestEffectiveOutputBudgetLeavesIndependentProviderUnchanged(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: false}
	a := &Agent{prov: prov, contextWindow: 1_048_576,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget}}
	got, clipped, err := a.effectiveOutputBudget(provider.Request{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 950_000)}},
	})
	if err != nil || clipped || got != 0 {
		t.Fatalf("independent provider changed: budget=%d clipped=%v err=%v", got, clipped, err)
	}
}

func TestEffectiveOutputBudgetHonorsExplicitOmit(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	a := &Agent{prov: prov, contextWindow: 1_048_576,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget}}
	got, clipped, err := a.effectiveOutputBudget(provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 950_000)}},
		MaxTokens: -1,
	})
	if err != nil || clipped || got != 0 {
		t.Fatalf("explicit omit changed: budget=%d clipped=%v err=%v", got, clipped, err)
	}
}

func TestSummarizeClipsSharedWindowOutputBudget(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true}
	a := &Agent{
		prov:              prov,
		contextWindow:     100_000,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget},
		sink:              event.Discard,
	}
	region := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("字", 50_000)}}
	a.lastUsage.Store(&provider.Usage{PromptTokens: 50_000})
	a.setPromptTokenCalibration(50_000, requestCalibrationShapeOf(provider.Request{Messages: region}))

	if _, _, err := a.summarize(context.Background(), region, ""); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if prov.last.MaxTokens <= 0 || prov.last.MaxTokens >= prov.budget {
		t.Fatalf("summarizer MaxTokens = %d, want a clipped positive budget below %d", prov.last.MaxTokens, prov.budget)
	}
}

func TestSummarizeRejectsLengthTruncation(t *testing.T) {
	prov := &sharedWindowTestProvider{budget: 128 * 1024, shared: true, finish: "length"}
	a := &Agent{
		prov:              prov,
		contextWindow:     1_048_576,
		outputBudgetState: outputBudgetState{outputBudget: prov.budget},
		sink:              event.Discard,
	}

	_, _, err := a.summarizeWithRetry(context.Background(), []provider.Message{{
		Role: provider.RoleUser, Content: "retain every durable fact",
	}}, "")
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("summarizeWithRetry error = %v, want truncation failure", err)
	}
	if prov.calls != 1 {
		t.Fatalf("length-truncated summary calls = %d, want no identical retry", prov.calls)
	}
}

func TestSetSessionResetsTokenCalibration(t *testing.T) {
	a := &Agent{}
	a.lastUsage.Store(&provider.Usage{PromptTokens: 200_000})
	active := requestCalibrationShape{requestChars: 900_000, compactChars: 850_000}
	a.activeReqShape.Store(&active)
	a.setPromptTokenCalibration(200_000, requestCalibrationShape{requestChars: 1_000_000, compactChars: 950_000})
	a.SetSession(NewSession("new"))

	if got := a.lastUsage.Load(); got != nil {
		t.Fatalf("lastUsage survived session switch: %+v", got)
	}
	if got := a.activeReqShape.Load(); got != nil {
		t.Fatalf("activeReqShape survived session switch: %+v", got)
	}
	if got := a.promptCalibration.Load(); got != nil {
		t.Fatalf("promptCalibration survived session switch: %+v", got)
	}
}

func TestLatestUsagePairsWithActiveRequestSize(t *testing.T) {
	a := &Agent{}
	active := requestCalibrationShape{requestChars: 222, compactChars: 111}
	a.activeReqShape.Store(&active)
	a.storeLatestRequestUsage(&provider.Usage{PromptTokens: 100})

	if got := a.promptCalibration.Load(); got == nil || got.promptTokens != 100 || got.requestChars != 222 || got.compactChars != 111 {
		t.Fatalf("promptCalibration = %+v, want promptTokens=100 requestChars=222 compactChars=111", got)
	}
}
