package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMissingReasoningWarnStatePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s := newMissingReasoningWarnState(dir)
	if !s.claim("deepseek") {
		t.Fatal("fresh provider must claim its first notice")
	}
	if s.claim("deepseek") {
		t.Fatal("same instance must not claim an already warned provider")
	}

	// A fresh instance over the same dir sees the persisted set.
	s2 := newMissingReasoningWarnState(dir)
	if s2.claim("deepseek") {
		t.Fatal("fresh instance must not claim a persisted provider")
	}

	b, err := os.ReadFile(filepath.Join(dir, missingReasoningWarnStateFilename))
	if err != nil {
		t.Fatalf("state file missing after claim: %v", err)
	}
	if got := string(b); got != `{"providers":["deepseek"]}` {
		t.Fatalf("state file = %s, want sorted single-provider JSON", got)
	}
}

func TestMissingReasoningWarnStateSortsProviders(t *testing.T) {
	dir := t.TempDir()
	s := newMissingReasoningWarnState(dir)
	if !s.claim("zebra") || !s.claim("alpha") {
		t.Fatal("fresh providers must claim their notices")
	}
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
	if !s.claim("deepseek") {
		t.Fatal("corrupt file must be treated as an empty set")
	}
	s2 := newMissingReasoningWarnState(dir)
	if s2.claim("deepseek") {
		t.Fatal("claim must rewrite a corrupt state file")
	}
}

func TestMissingReasoningWarnStateMissingFileTreatedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	s := newMissingReasoningWarnState(dir)
	if !s.claim("deepseek") {
		t.Fatal("missing file must be treated as an empty set")
	}
	if _, err := os.Stat(filepath.Join(dir, missingReasoningWarnStateFilename)); err != nil {
		t.Fatalf("claim must create the state file: %v", err)
	}
}

func TestMissingReasoningWarnStateEmptyDirIsNoop(t *testing.T) {
	s := newMissingReasoningWarnState("")
	if !s.claim("deepseek") {
		t.Fatal("empty dir must preserve once-per-session behavior at the caller")
	}
	if !s.claim("deepseek") {
		t.Fatal("empty dir must preserve once-per-session behavior at the caller")
	}
	if s2 := newMissingReasoningWarnState(""); !s2.claim("deepseek") {
		t.Fatal("empty dir must not persist state across instances")
	}
}

func TestMissingReasoningWarnStateClaimsAcrossSharedInstances(t *testing.T) {
	dir := t.TempDir()
	first := newMissingReasoningWarnState(dir)
	second := newMissingReasoningWarnState(dir)

	if !first.claim("deepseek") {
		t.Fatal("first instance must claim the fresh provider")
	}
	if second.claim("deepseek") {
		t.Fatal("second instance must observe the first instance's claim")
	}
}

func TestMissingReasoningWarnStatePreservesClaimsFromSharedInstances(t *testing.T) {
	dir := t.TempDir()
	first := newMissingReasoningWarnState(dir)
	second := newMissingReasoningWarnState(dir)

	if !first.claim("alpha") || !second.claim("zebra") {
		t.Fatal("fresh providers must claim their notices")
	}

	fresh := newMissingReasoningWarnState(dir)
	if fresh.claim("alpha") || fresh.claim("zebra") {
		t.Fatal("claims from separate instances must be retained together")
	}
	b, err := os.ReadFile(filepath.Join(dir, missingReasoningWarnStateFilename))
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if got := string(b); got != `{"providers":["alpha","zebra"]}` {
		t.Fatalf("state file = %s, want merged sorted providers", got)
	}
}

func TestMissingReasoningWarnStateConcurrentClaimsKeepEveryProvider(t *testing.T) {
	dir := t.TempDir()
	providers := []string{"alpha", "bravo", "charlie", "delta"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan string, len(providers))
	for _, provider := range providers {
		provider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if !newMissingReasoningWarnState(dir).claim(provider) {
				errs <- provider
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for provider := range errs {
		t.Errorf("fresh provider %q did not claim its notice", provider)
	}

	fresh := newMissingReasoningWarnState(dir)
	for _, provider := range providers {
		if fresh.claim(provider) {
			t.Errorf("provider %q was lost after concurrent claims", provider)
		}
	}
}
