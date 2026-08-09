package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func TestRemoveProviderAccessesRemovesGroupedOfficialAliasesAtomically(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	setDesktopTestCredential(t, "MIMO_API_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "deepseek-flash/deepseek-v4-flash"
	cfg.Agent.PlannerModel = "deepseek-pro/deepseek-v4-pro"
	cfg.Agent.SubagentModel = "deepseek-flash/deepseek-v4-flash"
	cfg.Agent.SubagentModels = map[string]string{"review": "deepseek-pro/deepseek-v4-pro"}
	cfg.Desktop.ProviderAccess = []string{"deepseek-flash", "deepseek-pro", "mimo-pro"}
	cfg.Providers = []config.ProviderEntry{
		{
			Name: "deepseek-flash", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic",
			Models: []string{"deepseek-v4-flash"}, Default: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY",
			Headers: map[string]string{"X-Route": "flash"},
		},
		{
			Name: "deepseek-pro", Kind: "openai", BaseURL: "https://api.deepseek.com",
			Models: []string{"deepseek-v4-pro"}, Default: "deepseek-v4-pro", APIKeyEnv: "DEEPSEEK_API_KEY",
			Headers: map[string]string{"X-Route": "pro"},
		},
		{Name: "mimo-pro", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5-pro", APIKeyEnv: "MIMO_API_KEY"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := NewApp()
	flashTab := &WorkspaceTab{ID: "flash", Scope: "global", model: "deepseek-flash/deepseek-v4-flash"}
	proTab := &WorkspaceTab{ID: "pro", Scope: "global", model: "deepseek-pro/deepseek-v4-pro"}
	app.tabs = map[string]*WorkspaceTab{flashTab.ID: flashTab, proTab.ID: proTab}
	app.tabOrder = []string{flashTab.ID, proTab.ID}
	app.activeTabID = flashTab.ID

	if err := app.RemoveProviderAccesses([]string{"deepseek-flash", "deepseek-pro", "deepseek-flash"}); err != nil {
		t.Fatalf("RemoveProviderAccesses: %v", err)
	}

	got := config.LoadForEdit(config.UserConfigPath())
	access := providerAccessSet(got.Desktop.ProviderAccess)
	if access["deepseek"] || access["deepseek-flash"] || access["deepseek-pro"] || !access["mimo-pro"] {
		t.Fatalf("provider_access = %+v, want only mimo-pro", got.Desktop.ProviderAccess)
	}
	fallback := "mimo-pro/mimo-v2.5-pro"
	if got.DefaultModel != fallback || got.Agent.PlannerModel != fallback || got.Agent.SubagentModel != fallback || got.Agent.SubagentModels["review"] != fallback {
		t.Fatalf("grouped provider refs were not retargeted: default=%q planner=%q subagent=%q skills=%+v", got.DefaultModel, got.Agent.PlannerModel, got.Agent.SubagentModel, got.Agent.SubagentModels)
	}
	if flashTab.model != fallback || proTab.model != fallback {
		t.Fatalf("grouped provider tabs = %q, %q; want %q", flashTab.model, proTab.model, fallback)
	}
	flash, flashOK := got.Provider("deepseek-flash")
	pro, proOK := got.Provider("deepseek-pro")
	if !flashOK || !proOK || flash.Headers["X-Route"] != "flash" || pro.Headers["X-Route"] != "pro" {
		t.Fatalf("built-in profiles or custom fields changed: flash=%+v/%v pro=%+v/%v", flash, flashOK, pro, proOK)
	}
}

func TestDeleteProviderKeepsOldControllerWhenFallbackBuildFails(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "REASONIX_TEST_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "prov-a/model-a"
	cfg.Desktop.ProviderAccess = []string{"prov-a", "broken"}
	cfg.Providers = []config.ProviderEntry{
		{Name: "prov-a", Kind: "openai", BaseURL: "https://a.example.invalid/v1", Model: "model-a", APIKeyEnv: "REASONIX_TEST_KEY"},
		{Name: "broken", Kind: "missing-provider-kind", BaseURL: "https://broken.example.invalid", Model: "model-b", APIKeyEnv: "REASONIX_TEST_KEY"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	base := control.New(control.Options{Label: "prov-a/model-a"})
	ctrl := newBlockingSnapshotCtrl(base)
	close(ctrl.releaseSnapshot)
	app := NewApp()
	app.ctx = context.Background()
	tab := &WorkspaceTab{ID: "active", Scope: "global", Ready: true, Ctrl: ctrl, model: cfg.DefaultModel, Label: cfg.DefaultModel}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID
	t.Cleanup(func() {
		if tab.Ctrl != nil {
			tab.Ctrl.Close()
		}
	})

	err := app.DeleteProvider("prov-a")
	if err == nil || !strings.Contains(err.Error(), "missing-provider-kind") {
		t.Fatalf("DeleteProvider error = %v, want replacement build failure", err)
	}
	if tab.Ctrl != ctrl || ctrl.closeCount.Load() != 0 {
		t.Fatalf("failed replacement closed or replaced the old controller: ctrl=%T closes=%d", tab.Ctrl, ctrl.closeCount.Load())
	}
	if tab.model != cfg.DefaultModel || tab.Label != cfg.DefaultModel {
		t.Fatalf("failed replacement changed live tab identity: model=%q label=%q", tab.model, tab.Label)
	}
}

func TestProviderRemovalContinuesRebuildingSiblingTabsAfterOneFails(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "REASONIX_TEST_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "good/model-b"
	cfg.Desktop.ProviderAccess = []string{"good"}
	cfg.Providers = []config.ProviderEntry{
		{Name: "good", Kind: "openai", BaseURL: "https://good.example.invalid/v1", Model: "model-b", APIKeyEnv: "REASONIX_TEST_KEY"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	brokenRoot := t.TempDir()
	brokenProject := `[agent]
system_prompt_file = "/outside-workspace/system.md"
`
	if err := os.WriteFile(filepath.Join(brokenRoot, "reasonix.toml"), []byte(brokenProject), 0o600); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	workingRoot := t.TempDir()

	newOld := func(label string) *blockingSnapshotCtrl {
		wrapped := newBlockingSnapshotCtrl(control.New(control.Options{Label: label, Sink: event.Discard}))
		close(wrapped.releaseSnapshot)
		return wrapped
	}
	oldBroken := newOld("removed/model-a")
	oldWorking := newOld("removed/model-a")
	app := NewApp()
	app.ctx = context.Background()
	app.readyHook = func() {}
	broken := &WorkspaceTab{
		ID: "a-broken", Scope: "project", WorkspaceRoot: brokenRoot, Ready: true,
		Ctrl: oldBroken, model: "removed/model-a", sink: &tabEventSink{tabID: "a-broken", app: app},
		disabledMCP: map[string]ServerView{},
	}
	working := &WorkspaceTab{
		ID: "b-working", Scope: "project", WorkspaceRoot: workingRoot, Ready: true,
		Ctrl: oldWorking, model: "removed/model-a", sink: &tabEventSink{tabID: "b-working", app: app},
		disabledMCP: map[string]ServerView{},
	}
	app.tabs = map[string]*WorkspaceTab{broken.ID: broken, working.ID: working}
	app.tabOrder = []string{broken.ID, working.ID}
	app.activeTabID = broken.ID
	t.Cleanup(func() {
		for _, tab := range []*WorkspaceTab{broken, working} {
			if tab.Ctrl != nil {
				tab.Ctrl.Close()
			}
			tab.releaseSessionLease()
		}
	})

	err := app.applyProviderRemovalRuntime([]providerRemovalTab{
		{id: broken.ID, ctrl: oldBroken},
		{id: working.ID, ctrl: oldWorking},
	}, "good/model-b", "provider")
	if err == nil || !strings.Contains(err.Error(), "relative path within the workspace") {
		t.Fatalf("applyProviderRemovalRuntime error = %v, want first tab build failure", err)
	}
	if broken.Ctrl != oldBroken || oldBroken.closeCount.Load() != 0 || broken.model != "removed/model-a" {
		t.Fatalf("failed tab was not failure-atomic: ctrl=%T closes=%d model=%q", broken.Ctrl, oldBroken.closeCount.Load(), broken.model)
	}
	if working.Ctrl == oldWorking || oldWorking.closeCount.Load() != 1 || working.model != "good/model-b" {
		t.Fatalf("working sibling was not rebuilt: ctrl=%T closes=%d model=%q", working.Ctrl, oldWorking.closeCount.Load(), working.model)
	}
}

func TestDeleteProviderRebuildsLiveTabAndReusesSharedHost(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "REASONIX_TEST_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "prov-a/model-a"
	cfg.Desktop.ProviderAccess = []string{"prov-a", "prov-b"}
	cfg.Providers = []config.ProviderEntry{
		{Name: "prov-a", Kind: "openai", BaseURL: "https://a.example.invalid/v1", Model: "model-a", APIKeyEnv: "REASONIX_TEST_KEY"},
		{Name: "prov-b", Kind: "openai", BaseURL: "https://b.example.invalid/v1", Model: "model-b", APIKeyEnv: "REASONIX_TEST_KEY"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	dir := config.SessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	path := filepath.Join(dir, "provider-removal-success.jsonl")
	session := agent.NewSession("system")
	session.Add(provider.Message{Role: provider.RoleUser, Content: "preserve this history"})
	exec := agent.New(nil, nil, session, agent.Options{}, event.Discard)

	app := NewApp()
	app.ctx = context.Background()
	app.readyHook = func() {}
	hostKey := "provider-removal-success-host"
	host := app.acquireSharedHost(hostKey)
	old := newBlockingSnapshotCtrl(control.New(control.Options{
		Executor: exec, SessionDir: dir, SessionPath: path, Label: cfg.DefaultModel, Host: host, Sink: event.Discard,
	}))
	close(old.releaseSnapshot)
	tab := &WorkspaceTab{
		ID: "active", Scope: "global", Ready: true, Ctrl: old, SessionPath: path,
		model: cfg.DefaultModel, Label: cfg.DefaultModel, SharedHostKey: hostKey, disabledMCP: map[string]ServerView{},
	}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID
	t.Cleanup(func() {
		if tab.Ctrl != nil {
			tab.Ctrl.Close()
		}
		tab.releaseSessionLease()
		app.releaseSharedHost(hostKey)
	})

	if err := app.DeleteProvider("prov-a"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if tab.Ctrl == nil || tab.Ctrl == old || old.closeCount.Load() != 1 {
		t.Fatalf("controller swap = %T, old closes = %d; want one successful replacement", tab.Ctrl, old.closeCount.Load())
	}
	if tab.model != "prov-b/model-b" || tab.Label != "model-b" {
		t.Fatalf("replacement identity = model:%q label:%q, want prov-b/model-b and model-b", tab.model, tab.Label)
	}
	if !sameDesktopPath(tab.Ctrl.SessionPath(), path) || !sameDesktopPath(tab.SessionPath, path) {
		t.Fatalf("replacement session path = ctrl:%q tab:%q, want %q", tab.Ctrl.SessionPath(), tab.SessionPath, path)
	}
	if tab.Ctrl.Host() != host || tab.SharedHostKey != hostKey {
		t.Fatalf("replacement did not reuse shared host: host=%p want=%p key=%q", tab.Ctrl.Host(), host, tab.SharedHostKey)
	}
	history := tab.Ctrl.History()
	preserved := false
	for _, message := range history {
		if message.Content == "preserve this history" {
			preserved = true
			break
		}
	}
	if !preserved {
		t.Fatalf("replacement history = %+v, want preserved user message", history)
	}
}

func TestRemoveProviderAccessRejectsDetachedRuntimeBeforeMutation(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	setDesktopTestCredential(t, "MIMO_API_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "mimo-pro/mimo-v2.5-pro"
	cfg.Desktop.ProviderAccess = []string{"deepseek", "mimo-pro"}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := NewApp()
	detachedCtrl := control.New(control.Options{Label: "deepseek"})
	detached := &WorkspaceTab{
		ID: detachedRuntimeTabID("detached-provider-removal"), Scope: "global", Ready: true,
		Ctrl: detachedCtrl, model: "deepseek/deepseek-v4-flash", Label: "deepseek",
	}
	app.detachedSessions = map[string]*WorkspaceTab{"detached-provider-removal": detached}
	t.Cleanup(detachedCtrl.Close)

	err := app.RemoveProviderAccess("deepseek")
	if err == nil || !strings.Contains(err.Error(), "background session is still using") {
		t.Fatalf("RemoveProviderAccess error = %v, want detached-runtime guard", err)
	}
	got := config.LoadForEdit(config.UserConfigPath())
	if !providerAccessSet(got.Desktop.ProviderAccess)["deepseek"] {
		t.Fatalf("provider access changed after detached-runtime rejection: %+v", got.Desktop.ProviderAccess)
	}
	if detached.Ctrl != detachedCtrl || detached.model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("detached runtime changed after rejection: ctrl=%T model=%q", detached.Ctrl, detached.model)
	}
}

func TestRemoveProviderAccessesRejectsMixedGroupBeforeMutation(t *testing.T) {
	isolateDesktopUserDirs(t)
	cfg := config.Default()
	cfg.Desktop.ProviderAccess = []string{"deepseek", "custom"}
	cfg.Providers = []config.ProviderEntry{
		{Name: "deepseek", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-v4-flash"},
		{Name: "custom", Kind: "openai", BaseURL: "https://proxy.example/v1", Model: "custom-model"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	if err := NewApp().RemoveProviderAccesses([]string{"deepseek", "custom"}); err == nil {
		t.Fatal("RemoveProviderAccesses accepted mixed official and custom providers")
	}
	got := config.LoadForEdit(config.UserConfigPath())
	access := providerAccessSet(got.Desktop.ProviderAccess)
	if !access["deepseek"] || !access["custom"] {
		t.Fatalf("provider access was partially mutated after rejection: %+v", got.Desktop.ProviderAccess)
	}
}

func TestProviderAccessFallbackSkipsUnconfiguredProviders(t *testing.T) {
	cfg := &config.Config{
		Desktop: config.DesktopConfig{ProviderAccess: []string{"deepseek", "unconfigured", "local"}},
		Providers: []config.ProviderEntry{
			{Name: "deepseek", Kind: "anthropic", BaseURL: "https://api.deepseek.com/anthropic", Model: "deepseek-v4-flash"},
			{Name: "unconfigured", Kind: "openai", BaseURL: "https://api.example.invalid/v1", Model: "remote", APIKeyEnv: "MISSING_API_KEY"},
			{Name: "local", Kind: "openai", BaseURL: "http://127.0.0.1:11434/v1", Model: "local-model"},
		},
	}

	if got := providerAccessFallbackRef(cfg, []string{"deepseek"}); got != "local/local-model" {
		t.Fatalf("fallback = %q, want configured local provider", got)
	}
}

func TestProviderAccessFallbackSkipsConfiguredProvidersOutsideAccessList(t *testing.T) {
	cfg := &config.Config{
		Desktop: config.DesktopConfig{ProviderAccess: []string{"removed", "visible"}},
		Providers: []config.ProviderEntry{
			{Name: "removed", Kind: "openai", BaseURL: "https://removed.example/v1", Model: "removed-model"},
			{Name: "hidden", Kind: "openai", BaseURL: "http://127.0.0.1:11434/v1", Model: "hidden-model"},
			{Name: "visible", Kind: "openai", BaseURL: "http://127.0.0.1:11434/v1", Model: "visible-model"},
		},
	}

	if got := providerAccessFallbackRef(cfg, []string{"removed"}); got != "visible/visible-model" {
		t.Fatalf("fallback = %q, want remaining accessed provider", got)
	}
}

func TestDeleteProviderPersistsVisibleFallbackInsteadOfHiddenConfiguredProvider(t *testing.T) {
	isolateDesktopUserDirs(t)
	cfg := config.Default()
	cfg.DefaultModel = "removed/removed-model"
	cfg.Agent.PlannerModel = "removed"
	cfg.Agent.SubagentModel = "removed/removed-model"
	cfg.Agent.SubagentModels = map[string]string{"review": "removed/removed-model"}
	cfg.Desktop.ProviderAccess = []string{"removed", "visible"}
	cfg.Providers = []config.ProviderEntry{
		{Name: "removed", Kind: "openai", BaseURL: "https://removed.example/v1", Model: "removed-model"},
		{Name: "hidden", Kind: "openai", BaseURL: "http://127.0.0.1:11434/v1", Model: "hidden-model"},
		{Name: "visible", Kind: "openai", BaseURL: "http://127.0.0.1:11434/v1", Model: "visible-model"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	if err := NewApp().DeleteProvider("removed"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	got := config.LoadForEdit(config.UserConfigPath())
	want := "visible"
	if got.DefaultModel != want || got.Agent.PlannerModel != want || got.Agent.SubagentModel != want || got.Agent.SubagentModels["review"] != want {
		t.Fatalf("persisted refs used a hidden fallback: default=%q planner=%q subagent=%q skills=%+v", got.DefaultModel, got.Agent.PlannerModel, got.Agent.SubagentModel, got.Agent.SubagentModels)
	}
	if _, ok := got.Provider("hidden"); !ok {
		t.Fatal("hidden provider should remain configured even though it is not a removal fallback")
	}
}

func TestRemoveProviderAccessesRejectsInUseProviderWithoutConfiguredFallback(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "deepseek/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek", "mimo-pro"}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := NewApp()
	tab := &WorkspaceTab{ID: "deepseek", Scope: "global", model: cfg.DefaultModel}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID

	err := app.RemoveProviderAccess("deepseek")
	if err == nil || !strings.Contains(err.Error(), "no other configured provider exists") {
		t.Fatalf("RemoveProviderAccess error = %v, want no configured fallback", err)
	}
	got := config.LoadForEdit(config.UserConfigPath())
	access := providerAccessSet(got.Desktop.ProviderAccess)
	if !access["deepseek"] || !access["mimo-pro"] {
		t.Fatalf("provider access changed after rejected removal: %+v", got.Desktop.ProviderAccess)
	}
	if got.DefaultModel != cfg.DefaultModel || tab.model != cfg.DefaultModel {
		t.Fatalf("model refs changed after rejected removal: config=%q tab=%q", got.DefaultModel, tab.model)
	}
}

func TestRemoveProviderAccessesRejectsOfficialProviderChangedDuringSnapshot(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	setDesktopTestCredential(t, "MIMO_API_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "deepseek/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek", "mimo-pro"}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := NewApp()
	ctrl := newBlockingSnapshotCtrl(control.New(control.Options{Label: "deepseek"}))
	tab := &WorkspaceTab{ID: "deepseek", Scope: "global", model: cfg.DefaultModel, Ctrl: ctrl}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID

	done := make(chan error, 1)
	go func() { done <- app.RemoveProviderAccess("deepseek") }()
	<-ctrl.firstSnapshotStarted

	unlock := config.LockUserConfigEdits()
	changed := config.LoadForEdit(config.UserConfigPath())
	provider, ok := changed.Provider("deepseek")
	if !ok {
		unlock()
		t.Fatal("deepseek provider missing")
	}
	provider.BaseURL = "https://proxy.example/v1"
	if err := changed.SaveTo(config.UserConfigPath()); err != nil {
		unlock()
		t.Fatalf("save overlapping config edit: %v", err)
	}
	unlock()
	close(ctrl.releaseSnapshot)

	if err := <-done; err == nil {
		t.Fatal("RemoveProviderAccess accepted an official provider changed during snapshot")
	}
	got := config.LoadForEdit(config.UserConfigPath())
	access := providerAccessSet(got.Desktop.ProviderAccess)
	if !access["deepseek"] || !access["mimo-pro"] {
		t.Fatalf("provider access changed after rejected overlap: %+v", got.Desktop.ProviderAccess)
	}
	if ctrl.closeCount.Load() != 0 || tab.Ctrl != ctrl || tab.model != cfg.DefaultModel {
		t.Fatalf("runtime mutated after rejected overlap: closes=%d ctrl=%T model=%q", ctrl.closeCount.Load(), tab.Ctrl, tab.model)
	}
}

func TestRemoveProviderAccessesRejectsCredentialChangeDuringSnapshot(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	setDesktopTestCredential(t, "MIMO_API_KEY", "old-key")

	cfg := config.Default()
	cfg.DefaultModel = "deepseek/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek", "mimo-pro"}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := NewApp()
	ctrl := newBlockingSnapshotCtrl(control.New(control.Options{Label: "deepseek"}))
	tab := &WorkspaceTab{ID: "deepseek", Scope: "global", model: cfg.DefaultModel, Ctrl: ctrl}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID

	done := make(chan error, 1)
	go func() { done <- app.RemoveProviderAccess("deepseek") }()
	<-ctrl.firstSnapshotStarted
	setDesktopTestCredential(t, "MIMO_API_KEY", "new-key")
	close(ctrl.releaseSnapshot)

	if err := <-done; err == nil {
		t.Fatal("RemoveProviderAccess accepted credentials changed during snapshot")
	}
	got := config.LoadForEdit(config.UserConfigPath())
	access := providerAccessSet(got.Desktop.ProviderAccess)
	if !access["deepseek"] || !access["mimo-pro"] {
		t.Fatalf("provider access changed after rejected credential overlap: %+v", got.Desktop.ProviderAccess)
	}
	if ctrl.closeCount.Load() != 0 || tab.Ctrl != ctrl || tab.model != cfg.DefaultModel {
		t.Fatalf("runtime mutated after rejected credential overlap: closes=%d ctrl=%T model=%q", ctrl.closeCount.Load(), tab.Ctrl, tab.model)
	}
}

func TestRemoveProviderAccessRejectsCustomProviderThatBecameOfficialDuringSnapshot(t *testing.T) {
	isolateDesktopUserDirs(t)
	setDesktopTestCredential(t, "DEEPSEEK_API_KEY", "sk-test")
	setDesktopTestCredential(t, "MIMO_API_KEY", "sk-test")

	cfg := config.Default()
	cfg.DefaultModel = "deepseek-flash/deepseek-v4-flash"
	cfg.Desktop.ProviderAccess = []string{"deepseek-flash", "mimo-pro"}
	cfg.Providers = []config.ProviderEntry{
		{Name: "deepseek-flash", Kind: "openai", BaseURL: "https://proxy.example/v1", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
		{Name: "mimo-pro", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5-pro", APIKeyEnv: "MIMO_API_KEY"},
	}
	if err := cfg.SaveTo(config.UserConfigPath()); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := NewApp()
	ctrl := newBlockingSnapshotCtrl(control.New(control.Options{Label: "custom-deepseek"}))
	tab := &WorkspaceTab{ID: "custom-deepseek", Scope: "global", model: cfg.DefaultModel, Ctrl: ctrl}
	app.tabs = map[string]*WorkspaceTab{tab.ID: tab}
	app.tabOrder = []string{tab.ID}
	app.activeTabID = tab.ID

	done := make(chan error, 1)
	go func() { done <- app.RemoveProviderAccess("deepseek-flash") }()
	<-ctrl.firstSnapshotStarted

	unlock := config.LockUserConfigEdits()
	changed := config.LoadForEdit(config.UserConfigPath())
	provider, ok := changed.Provider("deepseek-flash")
	if !ok {
		unlock()
		t.Fatal("deepseek-flash provider missing")
	}
	provider.BaseURL = "https://api.deepseek.com/anthropic"
	provider.Kind = "anthropic"
	if err := changed.SaveTo(config.UserConfigPath()); err != nil {
		unlock()
		t.Fatalf("save overlapping config edit: %v", err)
	}
	unlock()
	close(ctrl.releaseSnapshot)

	if err := <-done; err == nil {
		t.Fatal("RemoveProviderAccess deleted a custom provider that became official during snapshot")
	}
	got := config.LoadForEdit(config.UserConfigPath())
	provider, ok = got.Provider("deepseek-flash")
	if !ok || officialProviderKindFromEntry(*provider) != "deepseek" {
		t.Fatalf("updated official provider was not preserved: %+v/%v", provider, ok)
	}
	access := providerAccessSet(got.Desktop.ProviderAccess)
	if !access["deepseek"] && !access["deepseek-flash"] {
		t.Fatalf("provider access changed after rejected overlap: %+v", got.Desktop.ProviderAccess)
	}
	if ctrl.closeCount.Load() != 0 || tab.Ctrl != ctrl || tab.model != cfg.DefaultModel {
		t.Fatalf("runtime mutated after rejected overlap: closes=%d ctrl=%T model=%q", ctrl.closeCount.Load(), tab.Ctrl, tab.model)
	}
}
