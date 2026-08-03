package agent

import (
	"testing"

	"reasonix/internal/provider"
)

func TestMergeStreamUsageCountsProviderRequests(t *testing.T) {
	first := &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	retry := &provider.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28}
	got := mergeStreamUsage(first, retry)
	if got == nil || got.TotalTokens != 43 || got.RequestCount != 2 {
		t.Fatalf("merged usage = %+v, want total=43 requests=2", got)
	}

	third := &provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}
	got = mergeStreamUsage(got, third)
	if got.RequestCount != 3 {
		t.Fatalf("nested merged request count = %d, want 3", got.RequestCount)
	}

	got = mergeStreamUsage(nil, retry)
	if got == nil || got.TotalTokens != retry.TotalTokens || got.RequestCount != 2 {
		t.Fatalf("missing first usage = %+v, want retry tokens and 2 requests", got)
	}
	got = mergeStreamUsage(first, nil)
	if got == nil || got.TotalTokens != first.TotalTokens || got.RequestCount != 2 {
		t.Fatalf("missing retry usage = %+v, want first tokens and 2 requests", got)
	}
}

func TestTaskUsageModelRefUsesCanonicalRuntimeIdentity(t *testing.T) {
	task := (&TaskTool{baseModel: "deepseek/deepseek-v4-pro"}).WithTranscriptIdentityResolver(
		func(modelRef, effort string) (string, string) {
			if modelRef == "flash" {
				return "deepseek/deepseek-v4-flash", effort
			}
			return "deepseek/deepseek-v4-pro", effort
		},
	)
	if got := task.usageModelRef("flash", "high"); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("alias usage model = %q", got)
	}
	if got := task.usageModelRef("", ""); got != "deepseek/deepseek-v4-pro" {
		t.Fatalf("inherited usage model = %q", got)
	}
}
