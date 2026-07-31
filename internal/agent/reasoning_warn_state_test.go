package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingReasoningWarnStatePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s := newMissingReasoningWarnState(dir)
	if s.warned("deepseek") {
		t.Fatal("fresh state must not report a provider as warned")
	}
	s.markWarned("deepseek")
	if !s.warned("deepseek") {
		t.Fatal("provider must be warned after markWarned")
	}
	if s.warned("other-provider") {
		t.Fatal("unrelated provider must not be affected")
	}

	// A fresh instance over the same dir sees the persisted set.
	s2 := newMissingReasoningWarnState(dir)
	if !s2.warned("deepseek") {
		t.Fatal("persisted provider must be warned in a fresh instance")
	}

	b, err := os.ReadFile(filepath.Join(dir, missingReasoningWarnStateFilename))
	if err != nil {
		t.Fatalf("state file missing after markWarned: %v", err)
	}
	if got := string(b); got != `{"providers":["deepseek"]}` {
		t.Fatalf("state file = %s, want sorted single-provider JSON", got)
	}
}

func TestMissingReasoningWarnStateSortsProviders(t *testing.T) {
	dir := t.TempDir()
	s := newMissingReasoningWarnState(dir)
	s.markWarned("zebra")
	s.markWarned("alpha")
	b, err := os.ReadFile(filepath.Join(dir, missingReasoningWarnStateFilename))
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if got := string(b); got != `{"providers":["alpha","zebra"]}` {
		t.Fatalf("state file = %s, want providers sorted for stable diffs", got)
	}
}

func TestMissingReasoningWarnStateCorruptFileTreatedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, missingReasoningWarnStateFilename)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	s := newMissingReasoningWarnState(dir)
	if s.warned("deepseek") {
		t.Fatal("corrupt file must be treated as an empty set")
	}
	s.markWarned("deepseek") // must self-heal the corrupt file
	s2 := newMissingReasoningWarnState(dir)
	if !s2.warned("deepseek") {
		t.Fatal("markWarned must rewrite a corrupt state file")
	}
}

func TestMissingReasoningWarnStateMissingFileTreatedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	s := newMissingReasoningWarnState(dir)
	if s.warned("deepseek") {
		t.Fatal("missing file must be treated as an empty set")
	}
	s.markWarned("deepseek")
	if _, err := os.Stat(filepath.Join(dir, missingReasoningWarnStateFilename)); err != nil {
		t.Fatalf("markWarned must create the state file: %v", err)
	}
}

func TestMissingReasoningWarnStateEmptyDirIsNoop(t *testing.T) {
	s := newMissingReasoningWarnState("")
	s.markWarned("deepseek")
	if !s.warned("deepseek") {
		t.Fatal("in-memory dedupe must still work without a dir")
	}
	if s2 := newMissingReasoningWarnState(""); s2.warned("deepseek") {
		t.Fatal("empty dir must not persist state across instances")
	}
}
