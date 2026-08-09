package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
)

func TestSessionSidecarSavesUseDurableAtomicWrite(t *testing.T) {
	tests := []struct {
		name string
		path func(string) string
		save func(string) error
	}{
		{
			name: "titles",
			path: sessionTitlesPath,
			save: func(dir string) error {
				return saveSessionTitles(dir, map[string]string{"session.jsonl": "Durable title"})
			},
		},
		{
			name: "displays",
			path: sessionDisplayPath,
			save: func(dir string) error {
				return saveSessionDisplays(dir, sessionDisplayMap{
					"session.jsonl": {messageDisplayKey("expanded prompt"): "visible prompt"},
				})
			},
		},
		{
			name: "planner displays",
			path: sessionPlannerDisplayPath,
			save: func(dir string) error {
				return saveSessionPlannerDisplays(dir, sessionPlannerDisplayMap{
					"session.jsonl": {{UserHash: "prompt-digest", Messages: []HistoryMessage{}}},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := tt.path(dir)
			previousCrashPoint := fileutil.CrashPoint
			t.Cleanup(func() { fileutil.CrashPoint = previousCrashPoint })
			usedDurableWrite := false
			fileutil.CrashPoint = func(op, gotPath string) {
				if op == "atomic-write" && gotPath == path {
					usedDurableWrite = true
				}
			}

			if err := tt.save(dir); err != nil {
				t.Fatalf("save %s sidecar: %v", tt.name, err)
			}
			if !usedDurableWrite {
				t.Fatalf("%s sidecar bypassed fileutil.AtomicWriteFile", tt.name)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat %s sidecar: %v", tt.name, err)
				}
				if got := info.Mode().Perm(); got != 0o600 {
					t.Fatalf("%s sidecar mode = %o, want 600", tt.name, got)
				}
			}
		})
	}
}

func TestSetSessionTitleHonorsSidecarLock(t *testing.T) {
	dir := t.TempDir()
	release, err := filelock.Acquire(context.Background(), sessionTitlesPath(dir)+".lock")
	if err != nil {
		t.Fatalf("acquire title sidecar lock: %v", err)
	}
	previousTimeout := sessionTitlesLockTimeout
	sessionTitlesLockTimeout = 40 * time.Millisecond
	t.Cleanup(func() { sessionTitlesLockTimeout = previousTimeout })

	err = setSessionTitle(dir, filepath.Join(dir, "locked.jsonl"), "Locked")
	if err == nil {
		release()
		t.Fatal("setSessionTitle succeeded while the title sidecar lock was held")
	}
	release()
	if err := setSessionTitle(dir, filepath.Join(dir, "locked.jsonl"), "Saved"); err != nil {
		t.Fatalf("setSessionTitle after release: %v", err)
	}
}

func TestSetSessionTitleSerializesConcurrentUpdates(t *testing.T) {
	dir := t.TempDir()
	const sessions = 32
	errs := make(chan error, sessions)
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := filepath.Join(dir, fmt.Sprintf("session-%02d.jsonl", i))
			errs <- setSessionTitle(dir, path, fmt.Sprintf("Title %02d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("setSessionTitle: %v", err)
		}
	}

	titles := loadSessionTitles(dir)
	if len(titles) != sessions {
		t.Fatalf("title count = %d, want %d: %#v", len(titles), sessions, titles)
	}
	for i := range sessions {
		key := fmt.Sprintf("session-%02d.jsonl", i)
		if got := titles[key]; got != fmt.Sprintf("Title %02d", i) {
			t.Fatalf("%s title = %q, want retained concurrent value", key, got)
		}
	}
}
