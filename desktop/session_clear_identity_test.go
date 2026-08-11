package main

import (
	"path/filepath"
	"testing"

	"reasonix/internal/plugin"
	"reasonix/internal/provider"
)

func TestClearSessionForTabReturnsReplacementIdentity(t *testing.T) {
	app := historySliceTestApp(t)
	dir := t.TempDir()
	sess, oldPath := saveHistorySliceSession(t, dir, "old.jsonl", []provider.Message{
		{Role: provider.RoleUser, Content: "old content that must not reappear"},
		{Role: provider.RoleAssistant, Content: "old assistant reply"},
	})
	tab := newLiveHistoryTab(t, app, dir, oldPath, sess)
	beforeGen := tab.SessionGeneration

	result, err := app.ClearSessionForTab(tab.ID)
	if err != nil {
		t.Fatalf("ClearSessionForTab: %v", err)
	}
	if result.SessionPath == "" {
		t.Fatal("sessionPath empty after clear")
	}
	if sameDesktopPath(result.SessionPath, oldPath) {
		t.Fatalf("sessionPath stayed %q; want a rotated path", result.SessionPath)
	}
	if result.SessionGeneration <= beforeGen {
		t.Fatalf("sessionGeneration = %d, want > %d", result.SessionGeneration, beforeGen)
	}
	if tab.SessionGeneration != result.SessionGeneration {
		t.Fatalf("tab generation = %d, result = %d", tab.SessionGeneration, result.SessionGeneration)
	}
	if !sameDesktopPath(tab.currentSessionPath(), result.SessionPath) {
		t.Fatalf("tab path = %q, result = %q", tab.currentSessionPath(), result.SessionPath)
	}
	meta := app.tabMeta(tab, true)
	if meta.SessionGeneration != result.SessionGeneration || !sameDesktopPath(meta.SessionPath, result.SessionPath) {
		t.Fatalf("tabMeta identity = path:%q gen:%d, want path:%q gen:%d",
			meta.SessionPath, meta.SessionGeneration, result.SessionPath, result.SessionGeneration)
	}
	// Replacement path must not share the old file identity.
	if filepath.Base(result.SessionPath) == filepath.Base(oldPath) && sameDesktopPath(result.SessionPath, oldPath) {
		t.Fatal("replacement path collided with destroyed session")
	}
}

func TestResetSharedHostMCPClearsStillEnabledFailure(t *testing.T) {
	host := plugin.NewHost()
	// Same-name server remains "enabled" after ClearAuth; a stale build must
	// not keep the pre-clear Host failure/client around.
	host.RecordFailure(plugin.Spec{Name: "time"}, assertError("auth cleared"))
	if len(host.Failures()) == 0 {
		t.Fatal("expected recorded failure before reset")
	}
	(&App{}).resetSharedHostMCP(host)
	if got := host.Failures(); len(got) != 0 {
		t.Fatalf("failures after reset = %+v, want empty", got)
	}
	if names := host.ServerNames(); len(names) != 0 {
		t.Fatalf("servers after reset = %v, want empty", names)
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }
