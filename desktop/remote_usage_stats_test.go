package main

import (
	"context"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/eventwire"
	"reasonix/internal/provider"
	"reasonix/internal/remote/protocol"
	"reasonix/internal/stats"
)

func TestRemoteProviderStreamPersistsUsageInDesktopState(t *testing.T) {
	t.Setenv("REASONIX_STATE_HOME", t.TempDir())
	in := make(chan provider.Chunk, 2)
	in <- provider.Chunk{Type: provider.ChunkUsage, Usage: &provider.Usage{
		PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14, RequestCount: 2,
	}}
	in <- provider.Chunk{Type: provider.ChunkDone}
	close(in)

	for range recordRemoteProviderStream(context.Background(), "anthropic/claude", in) {
	}
	now := time.Now()
	result, err := stats.NewWriter(config.StatsDir()).Query(stats.SourceFilter{
		From: now.AddDate(0, 0, -1), To: now.AddDate(0, 0, 1), Source: "remote",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Tokens != 14 || result.Requests != 2 || result.Turns != 0 {
		t.Fatalf("remote totals = tokens %d requests %d turns %d, want 14/2/0", result.Tokens, result.Requests, result.Turns)
	}
	if len(result.Models) != 1 || result.Models[0].Model != "anthropic/claude" {
		t.Fatalf("remote models = %+v, want anthropic/claude", result.Models)
	}
	if len(result.Providers) != 1 || result.Providers[0].Provider != "anthropic" {
		t.Fatalf("remote providers = %+v, want anthropic", result.Providers)
	}
}

func TestWorkbenchTurnDonePersistsRemoteCompletionLocally(t *testing.T) {
	t.Setenv("REASONIX_STATE_HOME", t.TempDir())
	app := &App{}
	k := app.workbench()
	_, generation, err := k.targets.BeginRemoteConnect("remote-host", "/srv/work")
	if err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	k.remoteGen = generation
	k.mu.Unlock()

	app.workbenchClientCallbacks(generation, "").OnSessionEvent(protocol.SessionEvent{
		Seq: 1, Event: eventwire.Event{Kind: "turn_done"},
	})
	now := time.Now()
	result, err := stats.NewWriter(config.StatsDir()).Query(stats.SourceFilter{
		From: now.AddDate(0, 0, -1), To: now.AddDate(0, 0, 1), Source: "remote",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Turns != 1 {
		t.Fatalf("remote turns = %d, want 1", result.Turns)
	}
}
