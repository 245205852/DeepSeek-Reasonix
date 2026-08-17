package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/filelock"
	fileencoding "reasonix/internal/fileutil/encoding"
)

func TestHeartbeatConfigPathUsesReasonixUserStateDir(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	want := filepath.Join(config.MemoryUserDir(), "heartbeat-tasks.json")

	if got := engine.configPath(); got != want {
		t.Fatalf("configPath = %q, want %q", got, want)
	}
}

func TestHeartbeatConfigRevisionKeepsLegacyFilesReadable(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	legacy := `{"tasks":[{"id":"legacy","title":"Legacy","interval":"1h","enabled":false}]}`
	if err := os.MkdirAll(filepath.Dir(engine.configPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.configPath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	tasks := engine.ReloadTasks()
	if len(tasks) != 1 || tasks[0].ID != "legacy" {
		t.Fatalf("legacy tasks = %+v, want one readable task", tasks)
	}
	if engine.cfgRevision != 0 {
		t.Fatalf("legacy revision = %d, want zero", engine.cfgRevision)
	}
	if err := engine.ReplaceTasks(tasks); err != nil {
		t.Fatalf("upgrade save: %v", err)
	}
	data, err := os.ReadFile(engine.configPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg heartbeatConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Revision != 1 || len(cfg.Tasks) != 1 {
		t.Fatalf("upgraded config = %+v, want revision 1 with legacy task", cfg)
	}
	var previousReader struct {
		Tasks []HeartbeatTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &previousReader); err != nil || len(previousReader.Tasks) != 1 {
		t.Fatalf("previous reader could not ignore revision: tasks=%+v err=%v", previousReader.Tasks, err)
	}
}

func TestHeartbeatReplaceTasksRejectsStaleRevision(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	initial := []HeartbeatTask{{ID: "same", Title: "initial", Interval: "1h", Enabled: false}}
	if err := engine.saveTasks(initial); err != nil {
		t.Fatal(err)
	}
	engine.ReloadTasks()
	external := []HeartbeatTask{{ID: "same", Title: "edited externally", Interval: "2h", Enabled: false}}
	if err := engine.saveTasks(external); err != nil {
		t.Fatal(err)
	}
	err := engine.ReplaceTasks([]HeartbeatTask{{ID: "same", Title: "stale UI edit", Interval: "3h", Enabled: false}})
	if !errors.Is(err, ErrHeartbeatConfigConflict) {
		t.Fatalf("ReplaceTasks error = %v, want config conflict", err)
	}
	onDisk := engine.loadTasks()
	if len(onDisk) != 1 || onDisk[0].Title != "edited externally" || onDisk[0].Interval != "2h" {
		t.Fatalf("stale replacement changed disk config: %+v", onDisk)
	}
	if got := engine.ListTasks()[0].Title; got != "initial" {
		t.Fatalf("stale replacement changed in-memory tasks: %q", got)
	}
}

func TestHeartbeatReplaceConfigRejectsSameRevisionExternalEditByETag(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	initial := []HeartbeatTask{{ID: "same", Title: "initial", Interval: "1h", Enabled: false}}
	if err := engine.saveTasks(initial); err != nil {
		t.Fatal(err)
	}
	loaded := engine.ReloadConfig()
	external := heartbeatConfig{Revision: loaded.Revision, Tasks: []HeartbeatTask{{ID: "same", Title: "edited externally", Interval: "2h", Enabled: false}}}
	data, err := json.MarshalIndent(external, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.configPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = engine.ReplaceConfig(HeartbeatConfigUpdate{
		Revision: loaded.Revision,
		ETag:     loaded.ETag,
		Tasks:    []HeartbeatTask{{ID: "same", Title: "stale UI edit", Interval: "3h", Enabled: false}},
	})
	if !errors.Is(err, ErrHeartbeatConfigConflict) {
		t.Fatalf("ReplaceConfig error = %v, want config conflict", err)
	}
	onDisk := engine.loadTasks()
	if len(onDisk) != 1 || onDisk[0].Title != "edited externally" {
		t.Fatalf("same-revision external edit was overwritten: %+v", onDisk)
	}
}

func TestHeartbeatLoadTasksDecodesGB18030Config(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	body := `{"tasks":[{"id":"daily","title":"每日检查","prompt":"总结中文状态","interval":"1h","enabled":true}]}`
	if err := os.MkdirAll(filepath.Dir(engine.configPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.configPath(), fileencoding.Encode(body, fileencoding.GB18030), 0o644); err != nil {
		t.Fatal(err)
	}

	tasks := engine.loadTasks()
	if len(tasks) != 1 || tasks[0].Title != "每日检查" || tasks[0].Prompt != "总结中文状态" {
		t.Fatalf("loadTasks = %+v, want decoded Chinese task", tasks)
	}
}

func TestHeartbeatTaskDueAtWaitsForDailySchedule(t *testing.T) {
	loc := time.FixedZone("test", 8*60*60)
	created := time.Date(2026, 6, 18, 8, 30, 0, 0, loc)
	task := HeartbeatTask{
		ID:        "daily",
		Interval:  "24h|daily@09:00",
		Enabled:   true,
		CreatedAt: created.UnixMilli(),
	}

	if heartbeatTaskDueAt(task, time.Date(2026, 6, 18, 8, 59, 0, 0, loc)) {
		t.Fatal("daily task should wait for the configured clock time")
	}
	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 18, 9, 0, 0, 0, loc)) {
		t.Fatal("daily task should be due at the configured clock time")
	}

	task.LastRunAt = time.Date(2026, 6, 18, 9, 0, 0, 0, loc).UnixMilli()
	if heartbeatTaskDueAt(task, time.Date(2026, 6, 18, 10, 0, 0, 0, loc)) {
		t.Fatal("daily task should not run twice for the same scheduled occurrence")
	}
	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 19, 9, 0, 0, 0, loc)) {
		t.Fatal("daily task should be due at the next scheduled occurrence")
	}
}

func TestHeartbeatTaskDueAtCronExpression(t *testing.T) {
	loc := time.FixedZone("test", 8*60*60)
	task := HeartbeatTask{
		ID:        "cron",
		Interval:  "0 9 * * 1-5", // weekdays at 09:00
		Enabled:   true,
		CreatedAt: time.Date(2026, 6, 15, 0, 0, 0, 0, loc).UnixMilli(), // Monday
	}

	// Not due outside the cron window (Monday 08:59).
	if heartbeatTaskDueAt(task, time.Date(2026, 6, 15, 8, 59, 0, 0, loc)) {
		t.Fatal("cron task should wait for the configured time")
	}
	// Due exactly at Monday 09:00.
	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 15, 9, 0, 0, 0, loc)) {
		t.Fatal("cron task should be due at the configured time")
	}
	// Not due again within the same minute after running.
	task.LastRunAt = time.Date(2026, 6, 15, 9, 0, 0, 0, loc).UnixMilli()
	if heartbeatTaskDueAt(task, time.Date(2026, 6, 15, 9, 0, 30, 0, loc)) {
		t.Fatal("cron task should not fire twice for the same occurrence")
	}
	// Due again on the next weekday.
	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 16, 9, 0, 0, 0, loc)) {
		t.Fatal("cron task should be due at the next weekday occurrence")
	}
	// Weekend (Saturday) is not part of 1-5.
	if heartbeatTaskDueAt(task, time.Date(2026, 6, 20, 9, 0, 0, 0, loc)) {
		t.Fatal("cron task should skip weekends")
	}
}

func TestHeartbeatTaskDueAtCronEvery15Minutes(t *testing.T) {
	loc := time.FixedZone("test", 8*60*60)
	task := HeartbeatTask{
		ID:        "cron-15",
		Interval:  "*/15 * * * *",
		Enabled:   true,
		CreatedAt: time.Date(2026, 6, 18, 0, 0, 0, 0, loc).UnixMilli(),
	}

	for _, tt := range []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 6, 18, 10, 7, 0, 0, loc), false},
		{time.Date(2026, 6, 18, 10, 15, 0, 0, loc), true},
		{time.Date(2026, 6, 18, 10, 30, 0, 0, loc), true},
		{time.Date(2026, 6, 18, 10, 31, 0, 0, loc), false},
	} {
		if got := heartbeatTaskDueAt(task, tt.at); got != tt.want {
			t.Fatalf("cron */15 due at %v = %v, want %v", tt.at, got, tt.want)
		}
	}
}

func TestHeartbeatTaskDueAtHonorsWeeklySelection(t *testing.T) {
	loc := time.UTC
	task := HeartbeatTask{
		ID:        "weekly",
		Interval:  "168h|weekly:fri@09:00",
		Enabled:   true,
		CreatedAt: time.Date(2026, 6, 15, 8, 0, 0, 0, loc).UnixMilli(),
	}

	if heartbeatTaskDueAt(task, time.Date(2026, 6, 18, 12, 0, 0, 0, loc)) {
		t.Fatal("weekly task should not run before the selected weekday")
	}
	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 19, 9, 0, 0, 0, loc)) {
		t.Fatal("weekly task should run on the selected weekday and time")
	}
}

type heartbeatStatusStub struct {
	status control.RuntimeStatus
}

func (s heartbeatStatusStub) RuntimeStatus() control.RuntimeStatus {
	return s.status
}

type heartbeatExecuteTaskCtrlStub struct {
	stubSessionAPI
	status       control.RuntimeStatus
	submitted    []string
	approvalMode string
}

func (s *heartbeatExecuteTaskCtrlStub) RuntimeStatus() control.RuntimeStatus {
	return s.status
}

func (s *heartbeatExecuteTaskCtrlStub) SubmitUserTurn(input, display string) {
	s.submitted = append(s.submitted, input)
	s.status.Running = true
}

func (s *heartbeatExecuteTaskCtrlStub) SetToolApprovalMode(mode string) {
	s.approvalMode = mode
}

func (s *heartbeatExecuteTaskCtrlStub) PlanMode() bool {
	return false
}

func (s *heartbeatExecuteTaskCtrlStub) AutoApproveTools() bool {
	return false
}

func (s *heartbeatExecuteTaskCtrlStub) Goal() string {
	return ""
}

func (s *heartbeatExecuteTaskCtrlStub) ToolApprovalMode() string {
	return s.approvalMode
}

func (s *heartbeatExecuteTaskCtrlStub) SetSessionPath(string) {}

func (s *heartbeatExecuteTaskCtrlStub) SessionPath() string {
	return ""
}

func (s *heartbeatExecuteTaskCtrlStub) SessionDir() string {
	return ""
}

func (s *heartbeatExecuteTaskCtrlStub) Close() {}

func TestHeartbeatControllerBusyIncludesPendingPrompt(t *testing.T) {
	if heartbeatControllerBusy(heartbeatStatusStub{status: control.RuntimeStatus{Running: false, PendingPrompt: false}}) {
		t.Fatal("idle controller should be available for heartbeat execution")
	}
	if !heartbeatControllerBusy(heartbeatStatusStub{status: control.RuntimeStatus{Running: true}}) {
		t.Fatal("running controller should be busy")
	}
	if !heartbeatControllerBusy(heartbeatStatusStub{status: control.RuntimeStatus{PendingPrompt: true}}) {
		t.Fatal("pending prompt should keep controller busy")
	}
}

func TestHeartbeatTaskExecutionReservationSerializesTriggers(t *testing.T) {
	engine := &HeartbeatEngine{}
	if !engine.claimTask("same") {
		t.Fatal("first task claim should succeed")
	}
	second := make(chan bool, 1)
	go func() { second <- engine.claimTask("same") }()
	if <-second {
		t.Fatal("overlapping task trigger should be rejected")
	}
	engine.releaseTask("same")
	if !engine.claimTask("same") {
		t.Fatal("task should be claimable after the owner releases it")
	}
	engine.releaseTask("same")
}

func TestHeartbeatExecuteTaskPersistsFreshConversationTopicID(t *testing.T) {
	isolateDesktopUserDirs(t)
	app := NewApp()
	app.ctx = context.Background()
	app.readyHook = func() {}
	app.runtimeEvents.emit = func(context.Context, string, ...any) {}
	engine := &HeartbeatEngine{
		app:           app,
		pendingTopics: map[string]heartbeatPendingTopic{},
	}
	ctrl := &heartbeatExecuteTaskCtrlStub{}
	injected := make(chan struct{})

	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-injected:
				return
			case <-ticker.C:
				var cancel context.CancelFunc
				var tabToInject *WorkspaceTab
				app.mu.Lock()
				for _, tab := range app.tabs {
					if tab == nil {
						continue
					}
					tab.removed = true
					cancel = tab.buildCancel
					tabToInject = tab
					break
				}
				app.mu.Unlock()
				if tabToInject == nil {
					continue
				}
				if cancel != nil {
					cancel()
				}
				app.mu.Lock()
				if tabToInject.Ctrl == nil {
					tabToInject.Ctrl = ctrl
					tabToInject.Ready = true
					tabToInject.StartupErr = ""
					app.advanceSessionRuntimeEpochLocked(tabToInject)
					app.mu.Unlock()
					close(injected)
					return
				}
				app.mu.Unlock()
			}
		}
	}()

	got := engine.executeTask(HeartbeatTask{
		ID:                     "fresh",
		Title:                  "Fresh",
		Prompt:                 "ping",
		NewConversationEachRun: true,
		ApprovalMode:           "auto",
	})

	if got.TopicID == "" {
		t.Fatal("fresh conversation task should return the newly created topic ID")
	}
	if got.LastRunAt == 0 {
		t.Fatal("fresh conversation task should update LastRunAt after submit")
	}
	if len(ctrl.submitted) != 1 || ctrl.submitted[0] != "ping" {
		t.Fatalf("submitted prompts = %v, want [ping]", ctrl.submitted)
	}
	if ctrl.approvalMode != "auto" {
		t.Fatalf("approval mode = %q, want auto", ctrl.approvalMode)
	}
	pending := engine.pendingTopics["fresh"]
	if pending.TopicID != got.TopicID || !pending.Submitted {
		t.Fatalf("pending topic = %+v, want submitted %q", pending, got.TopicID)
	}
}

func TestHeartbeatExecuteTaskSkipsPendingPrompt(t *testing.T) {
	isolateDesktopUserDirs(t)
	app := NewApp()
	app.ctx = context.Background()
	app.readyHook = func() {}
	app.runtimeEvents.emit = func(context.Context, string, ...any) {}
	engine := &HeartbeatEngine{
		app:           app,
		pendingTopics: map[string]heartbeatPendingTopic{},
	}
	ctrl := &heartbeatExecuteTaskCtrlStub{status: control.RuntimeStatus{PendingPrompt: true}}
	injected := make(chan struct{})

	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-injected:
				return
			case <-ticker.C:
				var cancel context.CancelFunc
				var tabToInject *WorkspaceTab
				app.mu.Lock()
				for _, tab := range app.tabs {
					if tab == nil {
						continue
					}
					tab.removed = true
					cancel = tab.buildCancel
					tabToInject = tab
					break
				}
				app.mu.Unlock()
				if tabToInject == nil {
					continue
				}
				if cancel != nil {
					cancel()
				}
				app.mu.Lock()
				if tabToInject.Ctrl == nil {
					tabToInject.Ctrl = ctrl
					tabToInject.Ready = true
					tabToInject.StartupErr = ""
					app.advanceSessionRuntimeEpochLocked(tabToInject)
					app.mu.Unlock()
					close(injected)
					return
				}
				app.mu.Unlock()
			}
		}
	}()

	got := engine.executeTask(HeartbeatTask{
		ID:                     "fresh",
		Title:                  "Fresh",
		Prompt:                 "ping",
		NewConversationEachRun: true,
		ApprovalMode:           "auto",
	})

	if got.LastRunAt != 0 {
		t.Fatalf("pending prompt should not mark heartbeat run complete, LastRunAt=%d", got.LastRunAt)
	}
	if len(ctrl.submitted) != 0 {
		t.Fatalf("submitted prompts = %v, want none while prompt is pending", ctrl.submitted)
	}
	if ctrl.approvalMode != "" {
		t.Fatalf("approval mode = %q, want unchanged while prompt is pending", ctrl.approvalMode)
	}
}

func TestHeartbeatTaskDueAtHonorsIntervalTimeWindow(t *testing.T) {
	loc := time.UTC
	lastRun := time.Date(2026, 6, 18, 16, 0, 0, 0, loc)
	task := HeartbeatTask{
		ID:              "window",
		Interval:        "30m",
		Enabled:         true,
		LastRunAt:       lastRun.UnixMilli(),
		TimeWindowStart: "09:00",
		TimeWindowEnd:   "17:00",
	}

	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 18, 16, 30, 0, 0, loc)) {
		t.Fatal("interval task should run in the configured time window once due")
	}
	if heartbeatTaskDueAt(task, time.Date(2026, 6, 18, 17, 20, 0, 0, loc)) {
		t.Fatal("interval task should wait while outside the configured time window")
	}
	if !heartbeatTaskDueAt(task, time.Date(2026, 6, 19, 9, 0, 0, 0, loc)) {
		t.Fatal("interval task should run when the next time window opens")
	}

	neverRun := HeartbeatTask{
		ID:              "never-run-window",
		Interval:        "30m",
		Enabled:         true,
		TimeWindowStart: "09:00",
		TimeWindowEnd:   "17:00",
	}
	if heartbeatTaskDueAt(neverRun, time.Date(2026, 6, 18, 20, 0, 0, 0, loc)) {
		t.Fatal("never-run interval task should wait while outside the configured time window")
	}
	if !heartbeatTaskDueAt(neverRun, time.Date(2026, 6, 19, 9, 0, 0, 0, loc)) {
		t.Fatal("never-run interval task should run when the configured time window opens")
	}
}

func TestHeartbeatMergeRunUpdatesPreservesConcurrentEditsAndDeletes(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{
		tasks: []HeartbeatTask{
			{ID: "run", Title: "edited", Prompt: "new", Interval: "2h", Enabled: false, CreatedAt: 10},
			{ID: "keep", Title: "keep", Interval: "1h", Enabled: true},
		},
	}

	engine.mergeRunUpdatesLocked(map[string]HeartbeatTask{
		"run": {
			ID:        "run",
			Title:     "old",
			Prompt:    "old",
			Interval:  "1h",
			Enabled:   true,
			TopicID:   "topic-run",
			LastRunAt: 200,
			CreatedAt: 100,
		},
		"deleted": {
			ID:        "deleted",
			TopicID:   "topic-deleted",
			LastRunAt: 200,
		},
	})

	if len(engine.tasks) != 2 {
		t.Fatalf("tasks len = %d, want 2", len(engine.tasks))
	}
	got := engine.tasks[0]
	if got.Title != "edited" || got.Prompt != "new" || got.Interval != "2h" || got.Enabled {
		t.Fatalf("concurrent task edits were overwritten: %+v", got)
	}
	if got.TopicID != "topic-run" || got.LastRunAt != 200 || got.CreatedAt != 10 {
		t.Fatalf("run fields were not patched correctly: %+v", got)
	}
	for _, task := range engine.tasks {
		if task.ID == "deleted" {
			t.Fatalf("deleted task was resurrected: %+v", engine.tasks)
		}
	}
}

func TestHeartbeatMergeRunUpdatesNeverRegressesNewerRunState(t *testing.T) {
	tasks := []HeartbeatTask{{
		ID:        "run",
		TopicID:   "topic-new",
		LastRunAt: 300,
	}}
	mergeHeartbeatRunUpdates(tasks, map[string]HeartbeatTask{
		"run": {ID: "run", TopicID: "topic-old", LastRunAt: 200},
	})
	if tasks[0].TopicID != "topic-new" || tasks[0].LastRunAt != 300 {
		t.Fatalf("stale run state regressed the owner result: %+v", tasks[0])
	}

	mergeHeartbeatRunUpdates(tasks, map[string]HeartbeatTask{
		"run": {ID: "run", TopicID: "topic-latest", LastRunAt: 400},
	})
	if tasks[0].TopicID != "topic-latest" || tasks[0].LastRunAt != 400 {
		t.Fatalf("newer run state was not adopted: %+v", tasks[0])
	}
}

func TestHeartbeatReplaceTasksPrunesFreshConversationPendingTopics(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{
		pendingTopics: map[string]heartbeatPendingTopic{
			"fresh":   {TopicID: "topic-fresh", Submitted: true},
			"legacy":  {TopicID: "topic-legacy", Submitted: true},
			"deleted": {TopicID: "topic-deleted", Submitted: true},
		},
	}

	err := engine.ReplaceTasks([]HeartbeatTask{
		{ID: "fresh", NewConversationEachRun: true},
		{ID: "legacy", NewConversationEachRun: false},
	})
	if err != nil {
		t.Fatalf("ReplaceTasks: %v", err)
	}

	if len(engine.pendingTopics) != 1 {
		t.Fatalf("pendingTopics len = %d, want 1: %+v", len(engine.pendingTopics), engine.pendingTopics)
	}
	if got := engine.pendingTopics["fresh"]; got.TopicID != "topic-fresh" || !got.Submitted {
		t.Fatalf("fresh pending topic = %+v, want submitted topic-fresh", got)
	}
	if _, ok := engine.pendingTopics["legacy"]; ok {
		t.Fatalf("legacy task should not keep a fresh-conversation pending topic")
	}
	if _, ok := engine.pendingTopics["deleted"]; ok {
		t.Fatalf("deleted task should not keep a pending topic")
	}
}

func TestHeartbeatInactiveOpenDoesNotChangeActiveTab(t *testing.T) {
	isolateDesktopUserDirs(t)
	projectRoot := t.TempDir()
	app := &App{
		tabs: map[string]*WorkspaceTab{
			"heartbeat": {
				ID:            "heartbeat",
				Scope:         "project",
				WorkspaceRoot: projectRoot,
				TopicID:       "topic-heartbeat",
				TopicTitle:    "Heartbeat",
				Ready:         true,
				disabledMCP:   map[string]ServerView{},
			},
			"active": {
				ID:            "active",
				Scope:         "project",
				WorkspaceRoot: projectRoot,
				TopicID:       "topic-active",
				TopicTitle:    "Active",
				Ready:         true,
				disabledMCP:   map[string]ServerView{},
			},
		},
		tabOrder:    []string{"heartbeat", "active"},
		activeTabID: "active",
	}

	meta, err := app.openProjectTabInactive(projectRoot, "topic-heartbeat")
	if err != nil {
		t.Fatalf("openProjectTabInactive: %v", err)
	}
	if got := app.activeTabID; got != "active" {
		t.Fatalf("active tab = %q, want active", got)
	}
	if meta.ID != "heartbeat" || meta.Active {
		t.Fatalf("inactive open meta = %+v, want heartbeat and inactive", meta)
	}
}

func TestHeartbeatMergeRunUpdatesAdoptsExternalFileEdits(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{
		tasks: []HeartbeatTask{
			{ID: "a", Title: "stale title", Prompt: "stale", Interval: "1h", Enabled: true},
		},
	}
	// An external editor (the documented human/AI flow) rewrote the file after
	// the engine's in-memory snapshot: task a was edited and task b was added.
	external := []HeartbeatTask{
		{ID: "a", Title: "edited externally", Prompt: "new prompt", Interval: "2h", Enabled: true},
		{ID: "b", Title: "added externally", Prompt: "hello", Interval: "1h", Enabled: false},
	}
	if err := engine.saveTasks(external); err != nil {
		t.Fatalf("seed external file: %v", err)
	}

	engine.mergeRunUpdatesLocked(map[string]HeartbeatTask{
		"a": {ID: "a", TopicID: "topic-a", LastRunAt: 4242},
	})

	if len(engine.tasks) != 2 {
		t.Fatalf("tasks len = %d, want 2 (external addition adopted): %+v", len(engine.tasks), engine.tasks)
	}
	got := engine.tasks[0]
	if got.Title != "edited externally" || got.Prompt != "new prompt" || got.Interval != "2h" {
		t.Fatalf("external edit was rolled back by the run-state save: %+v", got)
	}
	if got.TopicID != "topic-a" || got.LastRunAt != 4242 {
		t.Fatalf("run state was not merged onto the disk copy: %+v", got)
	}
	// The full-list save must have preserved the externally added task on disk.
	onDisk := engine.loadTasks()
	if len(onDisk) != 2 || onDisk[1].ID != "b" || onDisk[1].Title != "added externally" {
		t.Fatalf("externally added task was lost on save: %+v", onDisk)
	}
}

func TestHeartbeatTickAdoptsExternalFileEdits(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := newHeartbeatEngine(nil)
	if err := engine.saveTasks([]HeartbeatTask{{ID: "a", Title: "A", Interval: "1h", Enabled: false}}); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	engine.mu.Lock()
	engine.tasks = engine.loadTasks()
	engine.mu.Unlock()

	// External edit lands after the engine last touched the file. Force the
	// mtime forward so coarse filesystem timestamps cannot make this flaky.
	if err := engine.saveTasks([]HeartbeatTask{
		{ID: "a", Title: "A", Interval: "1h", Enabled: false},
		{ID: "b", Title: "added externally", Interval: "1h", Enabled: false},
	}); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(engine.configPath(), future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	engine.tick() // disabled tasks only: adoption runs, nothing executes

	tasks := engine.ListTasks()
	if len(tasks) != 2 || tasks[1].ID != "b" {
		t.Fatalf("tick did not adopt the external edit: %+v", tasks)
	}
}

func TestHeartbeatExternalDeletionDoesNotResurrectTasks(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := newHeartbeatEngine(nil)
	if err := engine.saveTasks([]HeartbeatTask{{ID: "deleted", Title: "old", Interval: "1h", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.readConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.recordConfigSnapshotLocked(snapshot)
	engine.tasks = append([]HeartbeatTask(nil), snapshot.cfg.Tasks...)
	if err := os.Remove(engine.configPath()); err != nil {
		engine.mu.Unlock()
		t.Fatal(err)
	}
	engine.adoptExternalEditsLocked()
	if len(engine.tasks) != 0 || !engine.cfgDeleted {
		engine.mu.Unlock()
		t.Fatalf("deleted config left stale tasks: tasks=%+v deleted=%v", engine.tasks, engine.cfgDeleted)
	}
	engine.mergeRunUpdatesLocked(map[string]HeartbeatTask{"deleted": {ID: "deleted", LastRunAt: 123}})
	engine.mu.Unlock()
	if _, err := os.Stat(engine.configPath()); !os.IsNotExist(err) {
		t.Fatalf("deleted heartbeat config was recreated, stat err=%v", err)
	}
}

func TestHeartbeatRunCompletionObservesDeletionBeforeNextTick(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := newHeartbeatEngine(nil)
	if err := engine.saveTasks([]HeartbeatTask{{ID: "deleted", Title: "old", Interval: "1h", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := engine.readConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.recordConfigSnapshotLocked(snapshot)
	engine.tasks = append([]HeartbeatTask(nil), snapshot.cfg.Tasks...)
	if err := os.Remove(engine.configPath()); err != nil {
		engine.mu.Unlock()
		t.Fatal(err)
	}
	// Simulate a run that finishes before the scheduler's next external-edit
	// adoption pass. The completion merge itself must observe the deletion.
	engine.mergeRunUpdatesLocked(map[string]HeartbeatTask{"deleted": {ID: "deleted", LastRunAt: 123}})
	deleted := engine.cfgDeleted
	taskCount := len(engine.tasks)
	engine.mu.Unlock()

	if !deleted || taskCount != 0 {
		t.Fatalf("run completion retained deleted config: tasks=%d deleted=%v", taskCount, deleted)
	}
	if _, err := os.Stat(engine.configPath()); !os.IsNotExist(err) {
		t.Fatalf("run completion recreated deleted heartbeat config, stat err=%v", err)
	}
}

func TestHeartbeatTaskLeaseIsCrossEngine(t *testing.T) {
	isolateDesktopUserDirs(t)
	first := newHeartbeatEngine(nil)
	second := newHeartbeatEngine(nil)
	release, err := first.tryAcquireTaskLease("same-task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.tryAcquireTaskLease("same-task"); !errors.Is(err, filelock.ErrHeld) {
		t.Fatalf("second task lease err=%v, want filelock.ErrHeld", err)
	}
	release()
	retry, err := second.tryAcquireTaskLease("same-task")
	if err != nil {
		t.Fatalf("task lease after release: %v", err)
	}
	retry()
}

func TestMergeHeartbeatRunUpdatesKeepsRunHistory(t *testing.T) {
	tasks := []HeartbeatTask{{ID: "t1", Title: "task", RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}}}}
	updates := map[string]HeartbeatTask{
		"t1": {ID: "t1", Title: "task", LastRunAt: 200, RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}, {At: 200, TopicID: "b"}}},
	}
	mergeHeartbeatRunUpdates(tasks, updates)
	got := tasks[0].RunHistory
	if len(got) != 2 {
		t.Fatalf("run history len=%d, want 2 (deduped union)", len(got))
	}
	if got[0].At != 100 || got[1].At != 200 {
		t.Fatalf("run history order=%v, want [100 200]", got)
	}
	if tasks[0].LastRunAt != 200 {
		t.Fatalf("LastRunAt=%d, want 200", tasks[0].LastRunAt)
	}
}

func TestMergeHeartbeatRunUpdatesCapsHistory(t *testing.T) {
	tasks := []HeartbeatTask{{ID: "t1"}}
	updates := map[string]HeartbeatTask{"t1": {ID: "t1"}}
	history := make([]HeartbeatRun, 0, maxRunHistory+5)
	for i := 0; i < maxRunHistory+5; i++ {
		history = append(history, HeartbeatRun{At: int64(i)})
	}
	updates["t1"] = HeartbeatTask{ID: "t1", RunHistory: history}
	mergeHeartbeatRunUpdates(tasks, updates)
	if got := len(tasks[0].RunHistory); got != maxRunHistory {
		t.Fatalf("run history len=%d, want %d", got, maxRunHistory)
	}
	if tasks[0].RunHistory[0].At != int64(5) {
		t.Fatalf("oldest kept run At=%d, want 5", tasks[0].RunHistory[0].At)
	}
}

// TestHeartbeatReplaceTasksPreservesRunHistory: 前端整表保存（ReplaceTasks）时，
// 前端快照可能不含引擎刚写入的 runHistory（竞态/旧 state），不得清空磁盘已有的历史。
// 回归：此前 ReplaceTasks 直接整表覆写，会把引擎已持久化的 runHistory 全部冲掉。
func TestHeartbeatReplaceTasksPreservesRunHistory(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	// 引擎已执行两次：磁盘上有 2 条 runHistory
	if err := engine.ReplaceTasks([]HeartbeatTask{{
		ID:         "t1",
		Title:      "task",
		RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}, {At: 200, TopicID: "b"}},
	}}); err != nil {
		t.Fatalf("seed ReplaceTasks: %v", err)
	}

	// 前端旧快照：只改了 enabled，runHistory 字段缺失（竞态下 load 到旧数据）
	if err := engine.ReplaceTasks([]HeartbeatTask{{
		ID:      "t1",
		Title:   "task",
		Enabled: true,
	}}); err != nil {
		t.Fatalf("stale ReplaceTasks: %v", err)
	}

	got := engine.ListTasks()
	if len(got) != 1 {
		t.Fatalf("tasks len=%d, want 1", len(got))
	}
	if len(got[0].RunHistory) != 2 {
		t.Fatalf("run history len=%d, want 2 (stale frontend save must not clear engine-written history): %+v", len(got[0].RunHistory), got[0].RunHistory)
	}
}

// TestHeartbeatReplaceConfigPreservesRunHistory: ReplaceConfig（revision/ETag 校验的
// 前端保存）同样不得用旧快照清掉引擎已写入的 runHistory。
func TestHeartbeatReplaceConfigPreservesRunHistory(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	seed := []HeartbeatTask{{ID: "t1", Title: "task", RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}}}}
	view, err := engine.ReplaceConfig(HeartbeatConfigUpdate{Revision: 0, Tasks: seed})
	if err != nil {
		t.Fatalf("seed ReplaceConfig: %v", err)
	}
	// 引擎随后写入一条新执行（模拟后台执行落盘）
	if err := engine.ReplaceTasks([]HeartbeatTask{{ID: "t1", Title: "task", RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}, {At: 200, TopicID: "b"}}}}); err != nil {
		t.Fatalf("engine run write: %v", err)
	}
	// 前端旧快照（revision 过期场景改用全新 engine 读取磁盘模拟 stale load）：
	// 直接验证磁盘保护——重新加载磁盘后前端提交不含 runHistory 的旧快照
	reloaded := &HeartbeatEngine{}
	snap, err := reloaded.readConfigSnapshot()
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	_ = view
	stale := []HeartbeatTask{{ID: "t1", Title: "task", Enabled: true}}
	protected := mergeHeartbeatDiskRunHistory(stale, snap.cfg.Tasks)
	if len(protected[0].RunHistory) != 2 {
		t.Fatalf("protected run history len=%d, want 2: %+v", len(protected[0].RunHistory), protected[0].RunHistory)
	}
}

// TestHeartbeatMergeRunUpdatesPersistsRunHistory: 模拟 TriggerNow 的完整写盘链路——
// executeTask 返回含 runHistory 的 t → mergeRunUpdatesLocked → 磁盘。验证 runHistory 真实落盘。
func TestHeartbeatMergeRunUpdatesPersistsRunHistory(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	// 先建立基线任务（磁盘 + 内存一致）
	seed := []HeartbeatTask{{ID: "t1", Title: "task", Enabled: true}}
	if err := engine.ReplaceTasks(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 模拟 executeTask 返回值：lastRunAt 更新 + runHistory 追加 1 条
	updates := map[string]HeartbeatTask{
		"t1": {
			ID:         "t1",
			Title:      "task",
			Enabled:    true,
			LastRunAt:  200,
			TopicID:    "topic-b",
			RunHistory: []HeartbeatRun{{At: 200, TopicID: "topic-b"}},
		},
	}
	engine.mergeRunUpdatesLocked(updates)

	// 从磁盘重新读，确认 runHistory 落盘
	reloaded := &HeartbeatEngine{}
	snap, err := reloaded.readConfigSnapshot()
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if len(snap.cfg.Tasks) != 1 {
		t.Fatalf("tasks len=%d", len(snap.cfg.Tasks))
	}
	got := snap.cfg.Tasks[0]
	if got.LastRunAt != 200 {
		t.Fatalf("LastRunAt=%d, want 200", got.LastRunAt)
	}
	if len(got.RunHistory) != 1 {
		t.Fatalf("run history on disk len=%d, want 1: %+v", len(got.RunHistory), got.RunHistory)
	}
}

// TestCronDueDomDowOrSemantics: 标准 cron 中 day-of-month 与 day-of-week 双受限时
// 为 OR 语义（任一匹配即触发），非 AND。回归：此前实现要求两者同时匹配。
func TestCronDueDomDowOrSemantics(t *testing.T) {
	// "0 9 1 * 1": fires on 1st of month OR Monday
	// 2026-08-03 is a Monday, not the 1st → should fire
	mon := time.Date(2026, 8, 3, 9, 0, 0, 0, time.Local)
	if !cronDue("0 9 1 * 1", mon) {
		t.Fatalf("Monday 09:00 should match (dow OR dom)")
	}
	// 2026-08-01 is a Saturday, not Monday → should fire (dom=1)
	sat := time.Date(2026, 8, 1, 9, 0, 0, 0, time.Local)
	if !cronDue("0 9 1 * 1", sat) {
		t.Fatalf("1st of month should match (dow OR dom)")
	}
	// 2026-08-05 is Wednesday, not 1st/Monday → should NOT fire
	wed := time.Date(2026, 8, 5, 9, 0, 0, 0, time.Local)
	if cronDue("0 9 1 * 1", wed) {
		t.Fatalf("Wednesday should not match")
	}
	// "0 9 * * 1": only dow restricted → Monday only
	tue := time.Date(2026, 8, 4, 9, 0, 0, 0, time.Local)
	if cronDue("0 9 * * 1", tue) {
		t.Fatalf("Tuesday should not match dow=1-only")
	}
	// "0 9 1 * *": only dom restricted → 1st only
	if cronDue("0 9 1 * *", mon) {
		t.Fatalf("Monday (not 1st) should not match dom-only")
	}
}

// TestHeartbeatConfigSchemaVersionWritten: 新版本保存的配置必须带 schemaVersion=2，
// 供未来版本识别格式；旧配置（无字段）读取兼容且升级保存后带版本号。
func TestHeartbeatConfigSchemaVersionWritten(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	if err := engine.saveTasks([]HeartbeatTask{{ID: "t1", Title: "task", Interval: "1h", Enabled: false}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(engine.configPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg heartbeatConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != heartbeatSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", cfg.SchemaVersion, heartbeatSchemaVersion)
	}
	// 旧格式（无 schemaVersion）仍可读：模拟 v1 配置
	legacy := `{"tasks":[{"id":"legacy","title":"L","interval":"1h","enabled":false}]}`
	if err := os.WriteFile(engine.configPath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.ReloadTasks()
	if err := engine.ReplaceTasks(engine.ListTasks()); err != nil {
		t.Fatalf("legacy upgrade save: %v", err)
	}
	data, _ = os.ReadFile(engine.configPath())
	_ = json.Unmarshal(data, &cfg)
	if cfg.SchemaVersion != heartbeatSchemaVersion {
		t.Fatalf("legacy upgrade schemaVersion = %d, want %d", cfg.SchemaVersion, heartbeatSchemaVersion)
	}
}

// TestHeartbeatConfigForwardProtection: 未来版本（更高 schemaVersion）写入的配置，
// 当前二进制整表保存必须拒绝，不能静默降级覆盖 runHistory 等未来字段。
func TestHeartbeatConfigForwardProtection(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	future := `{"schemaVersion":99,"tasks":[{"id":"f","title":"future","interval":"1h","enabled":false,"runHistory":[{"at":100,"topicId":"x"}]}]}`
	if err := os.MkdirAll(filepath.Dir(engine.configPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.configPath(), []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.ReloadTasks() // 读取成功（未知高版本不阻塞读取）
	err := engine.ReplaceTasks([]HeartbeatTask{{ID: "f", Title: "edited", Interval: "2h", Enabled: true}})
	if err == nil {
		t.Fatal("ReplaceTasks on future-schema config must be refused")
	}
	// 磁盘内容未被覆盖
	data, _ := os.ReadFile(engine.configPath())
	if !bytes.Contains(data, []byte(`"schemaVersion":99`)) {
		t.Fatalf("future config was overwritten: %s", data)
	}
}

func TestCronDueDowSevenSundayAlias(t *testing.T) {
	// "0 9 * * 7": 7 is the standard Sunday alias in the dow field — it must
	// fire on Sundays (time.Weekday() == 0), not silently never match.
	sunday := time.Date(2026, 8, 9, 9, 0, 0, 0, time.Local) // 2026-08-09 is a Sunday
	if !cronDue("0 9 * * 7", sunday) {
		t.Fatalf("Sunday 09:00 should match dow=7 (Sunday alias)")
	}
	// "0 9 * * 0,7": both Sunday spellings together
	if !cronDue("0 9 * * 0,7", sunday) {
		t.Fatalf("Sunday 09:00 should match dow=0,7")
	}
	// A non-Sunday must not match dow=7
	monday := time.Date(2026, 8, 10, 9, 0, 0, 0, time.Local)
	if cronDue("0 9 * * 7", monday) {
		t.Fatalf("Monday should not match dow=7")
	}
	// "0 9 * * 6-7": dow range ending in 7 covers Sunday (6=Sat, 7=Sun)
	if !cronDue("0 9 * * 6-7", sunday) {
		t.Fatalf("Sunday should match dow range 6-7")
	}
}

func TestIsCronExprFieldBounds(t *testing.T) {
	// dom/month are 1-based: 0 can never match and must be rejected so the
	// UI refuses the expression instead of silently scheduling a task that
	// never fires (e.g. "0 0 0 * *" typed as "midnight every day").
	rejected := []string{
		"0 0 0 * *",    // dom 0
		"0 0 1 0 *",    // month 0
		"0 0 32 * *",   // dom 32
		"0 0 1 13 *",   // month 13
		"0 0 * 0-13 *", // month range with 0
		"*/0 * * * *",  // zero step never fires (minute % 0)
		"0 0 5-1 * *",  // descending range never matches
		"0 60 * * *",   // hour 60
		"0 0 1 * 8",    // dow 8 out of range
	}
	for _, expr := range rejected {
		if isCronExpr(expr) {
			t.Fatalf("isCronExpr(%q) should be false (out-of-bounds field)", expr)
		}
	}
	accepted := []string{
		"0 9 * * 7",   // dow 7 is a valid Sunday alias
		"0 9 1 1 0-7", // dow range 0-7 valid
		"*/15 * * * *",
		"0 9 1-31 * *",
		"5-10/2 * * * *", // stepping range
	}
	for _, expr := range accepted {
		if !isCronExpr(expr) {
			t.Fatalf("isCronExpr(%q) should be true", expr)
		}
	}
}

// TestHeartbeatConfigForwardProtectionOnRead: 读侧也要拒绝更高 schema 的配置——
// 不能加载并按旧逻辑执行（调度/权限语义可能已变化）。此前只在写入侧拒绝。
func TestHeartbeatConfigForwardProtectionOnRead(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	future := `{"schemaVersion":99,"tasks":[{"id":"f","title":"future","interval":"1h","enabled":false}]}`
	if err := os.MkdirAll(filepath.Dir(engine.configPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine.configPath(), []byte(future), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.readConfigSnapshot(); err == nil {
		t.Fatal("readConfigSnapshot should reject a future schemaVersion")
	}
	if tasks := engine.loadTasks(); tasks != nil {
		t.Fatal("loadTasks should refuse to load a future schemaVersion")
	}
}

// TestHeartbeatRunHistorySidecarSurvivesOlderFullTableSave: runHistory 存放在
// 主配置之外的 sidecar 文件。模拟一个不认识 runHistory 字段的旧版二进制对
// 主配置做整表保存（直接重写 heartbeat-tasks.json，不带 runHistory），
// 新版读取后 runHistory 必须仍然存在——旧 writer 无法触碰 sidecar。
func TestHeartbeatRunHistorySidecarSurvivesOlderFullTableSave(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	// 新版引擎写入：runHistory 落 sidecar
	if err := engine.ReplaceTasks([]HeartbeatTask{{
		ID:         "t1",
		Title:      "task",
		RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}, {At: 200, TopicID: "b"}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 模拟旧版整表保存：重写主配置，任务结构体里没有 runHistory 字段
	legacySave := `{"schemaVersion":1,"tasks":[{"id":"t1","title":"task"}]}`
	if err := os.WriteFile(engine.configPath(), []byte(legacySave), 0o644); err != nil {
		t.Fatal(err)
	}
	// 新版重新读取：sidecar 里的 runHistory 必须仍可恢复
	reloaded := &HeartbeatEngine{}
	snap, err := reloaded.readConfigSnapshot()
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if len(snap.cfg.Tasks) != 1 {
		t.Fatalf("tasks len=%d, want 1", len(snap.cfg.Tasks))
	}
	if got := snap.cfg.Tasks[0].RunHistory; len(got) != 2 {
		t.Fatalf("run history len=%d, want 2 (sidecar survives older full-table save): %+v", len(got), got)
	}
}

// TestHeartbeatRunHistorySidecarTrimmedOnSave: 整表保存时不再携带 runHistory，
// 主配置保持干净，sidecar 单独承载；再次保存后 sidecar 仍完整。
func TestHeartbeatRunHistorySidecarTrimmedOnSave(t *testing.T) {
	isolateDesktopUserDirs(t)
	engine := &HeartbeatEngine{}
	if err := engine.ReplaceTasks([]HeartbeatTask{{
		ID:         "t1",
		Title:      "task",
		RunHistory: []HeartbeatRun{{At: 100, TopicID: "a"}},
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := os.ReadFile(engine.configPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg heartbeatConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tasks) != 1 {
		t.Fatalf("main config tasks len=%d, want 1", len(cfg.Tasks))
	}
	if cfg.Tasks[0].RunHistory != nil {
		t.Fatal("main config must not carry runHistory (lives in sidecar)")
	}
	if runs, _ := os.ReadFile(engine.runHistoryPath()); len(runs) == 0 {
		t.Fatal("runHistory sidecar file must exist after save")
	}
}
