package agent

// reasoning_warn_state.go rate-limits the missing tool-call thinking notice
// across sessions and processes (#7059).
//
// DeepSeek's thinking-mode contract requires provider-issued thinking content
// to accompany tool calls and be replayed on later requests. Missing content is
// therefore a compatibility incident, not a permanent provider characteristic.
// State is keyed by an opaque configuration fingerprint, expires after a bounded
// cooldown, and is cleared after a healthy tool-call turn so a future regression
// becomes visible again.

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

const missingReasoningWarnStateFilename = "tool-call-reasoning-warning.json"

const (
	missingReasoningWarnStateLockFilename = "tool-call-reasoning-warning.lock"
	missingReasoningWarnStateVersion      = 2
	missingReasoningWarnStateCooldown     = 24 * time.Hour
	missingReasoningWarnStateMaxIncidents = 256
	// A diagnostic must never make a turn wait indefinitely behind another
	// process. On contention or I/O failure, callers emit the warning rather
	// than silently losing it.
	missingReasoningWarnStateLockTimeout = 200 * time.Millisecond
)

type missingReasoningIncident struct {
	Fingerprint       string `json:"fingerprint"`
	WarnedAtUnixMs    int64  `json:"warnedAtUnixMs"`
	LastMissingUnixMs int64  `json:"lastMissingAtUnixMs"`
}

type missingReasoningWarnDocument struct {
	Version   int                        `json:"version"`
	Incidents []missingReasoningIncident `json:"incidents,omitempty"`
	// Providers reads the unreleased v1 preview schema. Provider names cannot
	// safely identify endpoint/model/protocol changes and have no timestamp, so
	// they deliberately re-arm once and are omitted on the next v2 write.
	Providers []string `json:"providers,omitempty"`
}

type missingReasoningWarnState struct {
	dir string
}

func newMissingReasoningWarnState(dir string) *missingReasoningWarnState {
	return &missingReasoningWarnState{dir: strings.TrimSpace(dir)}
}

func (s *missingReasoningWarnState) path() string {
	return filepath.Join(s.dir, missingReasoningWarnStateFilename)
}

func (s *missingReasoningWarnState) lockPath() string {
	return filepath.Join(s.dir, missingReasoningWarnStateLockFilename)
}

func validMissingReasoningFingerprint(fingerprint string) bool {
	if len(fingerprint) != 64 {
		return false
	}
	for _, r := range fingerprint {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// load returns only current v2 incidents. Missing, corrupt, legacy, expired, or
// future-dated entries are treated as absent and self-heal on the next write.
func (s *missingReasoningWarnState) load(now time.Time) map[string]missingReasoningIncident {
	incidents := map[string]missingReasoningIncident{}
	b, err := os.ReadFile(s.path())
	if err != nil {
		return incidents
	}
	var doc missingReasoningWarnDocument
	if json.Unmarshal(b, &doc) != nil || doc.Version != missingReasoningWarnStateVersion {
		return incidents
	}
	for _, incident := range doc.Incidents {
		incident.Fingerprint = strings.TrimSpace(incident.Fingerprint)
		warnedAt := time.UnixMilli(incident.WarnedAtUnixMs)
		age := now.Sub(warnedAt)
		if !validMissingReasoningFingerprint(incident.Fingerprint) || age < 0 || age >= missingReasoningWarnStateCooldown {
			continue
		}
		if incident.LastMissingUnixMs < incident.WarnedAtUnixMs {
			incident.LastMissingUnixMs = incident.WarnedAtUnixMs
		}
		if previous, ok := incidents[incident.Fingerprint]; !ok || previous.LastMissingUnixMs < incident.LastMissingUnixMs {
			incidents[incident.Fingerprint] = incident
		}
	}
	return incidents
}

func (s *missingReasoningWarnState) save(incidents map[string]missingReasoningIncident) error {
	ordered := make([]missingReasoningIncident, 0, len(incidents))
	for _, incident := range incidents {
		ordered = append(ordered, incident)
	}
	if len(ordered) > missingReasoningWarnStateMaxIncidents {
		sort.Slice(ordered, func(i, j int) bool {
			return ordered[i].LastMissingUnixMs > ordered[j].LastMissingUnixMs
		})
		ordered = ordered[:missingReasoningWarnStateMaxIncidents]
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Fingerprint < ordered[j].Fingerprint
	})
	b, err := json.Marshal(missingReasoningWarnDocument{
		Version:   missingReasoningWarnStateVersion,
		Incidents: ordered,
	})
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(s.path(), b, 0o600)
}

func (s *missingReasoningWarnState) acquire() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), missingReasoningWarnStateLockTimeout)
	release, err := filelock.Acquire(ctx, s.lockPath())
	if err != nil {
		cancel()
		return nil, err
	}
	return func() {
		release()
		cancel()
	}, nil
}

func normalizeMissingReasoningObservedAt(observedAt time.Time) time.Time {
	if observedAt.IsZero() {
		return time.Now()
	}
	return observedAt
}

// claimAt records the latest missing-reasoning observation and reports whether
// this active incident should emit a warning. The lock covers read-check-write
// across every process sharing the Reasonix home. Persistence failure returns
// true so diagnostics fail visible rather than fail silent.
func (s *missingReasoningWarnState) claimAt(fingerprint string, observedAt time.Time) bool {
	fingerprint = strings.TrimSpace(fingerprint)
	if s == nil || s.dir == "" || !validMissingReasoningFingerprint(fingerprint) {
		return true
	}
	observedAt = normalizeMissingReasoningObservedAt(observedAt)
	release, err := s.acquire()
	if err != nil {
		return true
	}
	defer release()

	incidents := s.load(observedAt)
	incident, exists := incidents[fingerprint]
	shouldWarn := !exists
	if shouldWarn {
		incident = missingReasoningIncident{Fingerprint: fingerprint, WarnedAtUnixMs: observedAt.UnixMilli()}
	}
	if observed := observedAt.UnixMilli(); incident.LastMissingUnixMs < observed {
		incident.LastMissingUnixMs = observed
	}
	incidents[fingerprint] = incident
	if err := s.save(incidents); err != nil {
		return true
	}
	return shouldWarn
}

func (s *missingReasoningWarnState) claim(fingerprint string) bool {
	return s.claimAt(fingerprint, time.Now())
}

// resolveAt clears an incident after a healthy tool-call turn. A health result
// observed before a newer missing result cannot delete that newer incident,
// even if lock acquisition completes in the opposite order.
func (s *missingReasoningWarnState) resolveAt(fingerprint string, observedAt time.Time) {
	fingerprint = strings.TrimSpace(fingerprint)
	if s == nil || s.dir == "" || !validMissingReasoningFingerprint(fingerprint) {
		return
	}
	observedAt = normalizeMissingReasoningObservedAt(observedAt)
	release, err := s.acquire()
	if err != nil {
		return
	}
	defer release()

	incidents := s.load(observedAt)
	incident, exists := incidents[fingerprint]
	if !exists || incident.LastMissingUnixMs > observedAt.UnixMilli() {
		return
	}
	delete(incidents, fingerprint)
	_ = s.save(incidents)
}
