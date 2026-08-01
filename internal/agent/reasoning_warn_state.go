package agent

// reasoning_warn_state.go — once-per-provider persistence for the missing
// tool-call reasoning notice (#7059).
//
// Some provider endpoints can omit reasoning_content on tool_calls turns. The
// historical notice scope was once per session, so a new session (or
// `--resume`) re-emitted the warning even though its text promised a single
// showing. This state records which providers have already received the notice,
// in a shared directory supplied by boot, so the warning fires exactly once per
// provider across sessions.
//
// The state is best-effort by design: every read/write failure degrades to
// the historical once-per-session behavior instead of failing the agent loop.
// Claims are serialized with a bounded cross-process file lock and the state
// file is atomically replaced, so concurrent agents cannot lose one another's
// updates. A torn or unreadable file is treated as an empty set and overwritten
// on the next notice.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
)

// missingReasoningWarnStateFilename is the state file name inside the
// MissingReasoningWarnStateDir directory.
const missingReasoningWarnStateFilename = "tool-call-reasoning-warning.json"

const (
	missingReasoningWarnStateLockFilename = "tool-call-reasoning-warning.lock"
	// A warning must never make a turn wait indefinitely behind another process.
	// On contention or IO failure, claim falls back to the historical
	// once-per-session behavior.
	missingReasoningWarnStateLockTimeout = 200 * time.Millisecond
)

// missingReasoningWarnState persists the set of provider names that have
// already been shown the missing tool-call reasoning notice.
type missingReasoningWarnState struct {
	dir string
}

// newMissingReasoningWarnState returns a state rooted at dir. An empty dir
// makes claim a no-op, preserving the historical once-per-session scope.
func newMissingReasoningWarnState(dir string) *missingReasoningWarnState {
	return &missingReasoningWarnState{
		dir: strings.TrimSpace(dir),
	}
}

func (s *missingReasoningWarnState) path() string {
	return filepath.Join(s.dir, missingReasoningWarnStateFilename)
}

func (s *missingReasoningWarnState) lockPath() string {
	return filepath.Join(s.dir, missingReasoningWarnStateLockFilename)
}

// load reads the complete persisted set. Missing or corrupt files are treated
// as empty, so the next successful claim self-heals them.
func (s *missingReasoningWarnState) load() map[string]bool {
	seen := map[string]bool{}
	b, err := os.ReadFile(s.path())
	if err != nil {
		return seen
	}
	var doc struct {
		Providers []string `json:"providers"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return seen
	}
	for _, p := range doc.Providers {
		if p = strings.TrimSpace(p); p != "" {
			seen[p] = true
		}
	}
	return seen
}

func (s *missingReasoningWarnState) save(seen map[string]bool) error {
	names := make([]string, 0, len(seen))
	for p := range seen {
		names = append(names, p)
	}
	sort.Strings(names)
	b, err := json.Marshal(struct {
		Providers []string `json:"providers"`
	}{Providers: names})
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(s.path(), b, 0o600)
}

// claim atomically records provider as warned and reports whether this caller
// should emit the notice. The read-check-write transaction is serialized across
// every agent and Reasonix process sharing the state directory. If persistence
// is unavailable, it deliberately returns true so callers keep the historical
// once-per-session warning behavior instead of losing the notice.
func (s *missingReasoningWarnState) claim(provider string) bool {
	provider = strings.TrimSpace(provider)
	if s == nil || s.dir == "" || provider == "" {
		return true
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), missingReasoningWarnStateLockTimeout)
	defer cancel()
	release, err := filelock.Acquire(ctx, s.lockPath())
	if err != nil {
		return true
	}
	defer release()

	seen := s.load()
	if seen[provider] {
		return false
	}
	seen[provider] = true
	if err := s.save(seen); err != nil {
		return true
	}
	return true
}
