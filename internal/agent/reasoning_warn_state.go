package agent

// reasoning_warn_state.go — once-per-provider persistence for the missing
// tool-call reasoning notice (#7059).
//
// DeepSeek (and some gateways) never attach reasoning_content to tool_calls
// turns. The historical notice scope was once per session, so a new session
// (or `--resume`) re-emitted the warning even though its text promised a
// single showing. This state records which providers have already received
// the notice, in a shared directory supplied by boot, so the warning fires
// exactly once per provider across sessions.
//
// The state is best-effort by design: every read/write failure degrades to
// the historical once-per-session behavior instead of failing the agent loop.
// The file is replaced atomically (temp file + rename) so concurrent agents
// (executor + planner) cannot corrupt it; a torn or unreadable file is
// treated as an empty set and overwritten on the next notice.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// missingReasoningWarnStateFilename is the state file name inside the
// MissingReasoningWarnStateDir directory.
const missingReasoningWarnStateFilename = "tool-call-reasoning-warning.json"

// missingReasoningWarnState persists the set of provider names that have
// already been shown the missing tool-call reasoning notice.
type missingReasoningWarnState struct {
	dir string

	mu     sync.Mutex
	loaded bool
	seen   map[string]bool
}

// newMissingReasoningWarnState returns a state rooted at dir. An empty dir
// makes both warned and markWarned no-ops, preserving the historical
// once-per-session scope.
func newMissingReasoningWarnState(dir string) *missingReasoningWarnState {
	return &missingReasoningWarnState{
		dir:  strings.TrimSpace(dir),
		seen: map[string]bool{},
	}
}

func (s *missingReasoningWarnState) path() string {
	return filepath.Join(s.dir, missingReasoningWarnStateFilename)
}

// loadLocked reads the persisted set once. Missing or corrupt files are
// treated as an empty set: the next markWarned rewrites the file, so a torn
// write self-heals on the following notice.
func (s *missingReasoningWarnState) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.dir == "" {
		return
	}
	b, err := os.ReadFile(s.path())
	if err != nil {
		return
	}
	var doc struct {
		Providers []string `json:"providers"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return
	}
	for _, p := range doc.Providers {
		if p = strings.TrimSpace(p); p != "" {
			s.seen[p] = true
		}
	}
}

// warned reports whether the provider has already been shown the notice.
func (s *missingReasoningWarnState) warned(provider string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return s.seen[provider]
}

// markWarned records the notice as shown for the provider. Best-effort: a
// failed write never surfaces to the caller, and a failed read falls back to
// the in-session dedupe only.
func (s *missingReasoningWarnState) markWarned(provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	s.seen[provider] = true
	if s.dir == "" {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	names := make([]string, 0, len(s.seen))
	for p := range s.seen {
		names = append(names, p)
	}
	sort.Strings(names)
	b, err := json.Marshal(struct {
		Providers []string `json:"providers"`
	}{Providers: names})
	if err != nil {
		return
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, s.path()); err != nil {
		_ = os.Remove(tmp)
	}
}
