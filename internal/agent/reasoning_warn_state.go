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
	"runtime"
	"sort"
	"strings"
	"sync"
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

// missingReasoningWarnClaimFlights coalesces concurrent observations of the
// same incident inside one process. filelock.Acquire intentionally has a short
// deadline so a diagnostic cannot stall a turn behind another process. On a
// slower filesystem (notably Windows CI), several local callers can otherwise
// exhaust that deadline while queued behind the first atomic write and each
// take the fail-visible path, producing duplicate warnings.
//
// Followers update the latest observation timestamp and let the leader persist
// it before the flight is removed. This preserves resolveAt's stale-health
// ordering guarantee instead of merely suppressing duplicate return values.
type missingReasoningWarnClaimFlight struct {
	latestObservedAt time.Time
}

var missingReasoningWarnClaimFlights = struct {
	sync.Mutex
	flights map[string]*missingReasoningWarnClaimFlight
}{flights: map[string]*missingReasoningWarnClaimFlight{}}

// missingReasoningWarnProcessLocks serializes transactions for one state file
// before they enter filelock.Acquire. The file lock's short deadline is meant
// to bound waits on another process, not make distinct local incidents lose
// persistence while queued behind a slower Windows atomic write. Reasonix uses
// one state path per home, so retaining these few mutexes for the process
// lifetime is bounded in normal operation.
var missingReasoningWarnProcessLocks sync.Map

func newMissingReasoningWarnState(dir string) *missingReasoningWarnState {
	return &missingReasoningWarnState{dir: strings.TrimSpace(dir)}
}

func (s *missingReasoningWarnState) path() string {
	return filepath.Join(s.dir, missingReasoningWarnStateFilename)
}

func (s *missingReasoningWarnState) lockPath() string {
	return filepath.Join(s.dir, missingReasoningWarnStateLockFilename)
}

func (s *missingReasoningWarnState) processLockKey() string {
	path, err := filepath.Abs(s.path())
	if err != nil {
		path = s.path()
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(filepath.ToSlash(path))
	}
	return path
}

func (s *missingReasoningWarnState) processLock() *sync.Mutex {
	lock, _ := missingReasoningWarnProcessLocks.LoadOrStore(s.processLockKey(), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *missingReasoningWarnState) claimFlightKey(fingerprint string) string {
	return s.processLockKey() + "\x00" + fingerprint
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

// persistClaimAt performs one cross-process read-check-write transaction.
// Persistence failure returns true so diagnostics fail visible rather than
// fail silent.
func (s *missingReasoningWarnState) persistClaimAt(fingerprint string, observedAt time.Time) bool {
	processLock := s.processLock()
	processLock.Lock()
	defer processLock.Unlock()

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

// claimAt records the latest missing-reasoning observation and reports whether
// this active incident should emit a warning. Concurrent observations of the
// same incident in this process share one leader; the file lock covers the
// leader's read-check-write transactions against other processes sharing the
// Reasonix home.
func (s *missingReasoningWarnState) claimAt(fingerprint string, observedAt time.Time) bool {
	fingerprint = strings.TrimSpace(fingerprint)
	if s == nil || s.dir == "" || !validMissingReasoningFingerprint(fingerprint) {
		return true
	}
	observedAt = normalizeMissingReasoningObservedAt(observedAt)
	key := s.claimFlightKey(fingerprint)

	missingReasoningWarnClaimFlights.Lock()
	if flight := missingReasoningWarnClaimFlights.flights[key]; flight != nil {
		if observedAt.After(flight.latestObservedAt) {
			flight.latestObservedAt = observedAt
		}
		missingReasoningWarnClaimFlights.Unlock()
		return false
	}
	flight := &missingReasoningWarnClaimFlight{latestObservedAt: observedAt}
	missingReasoningWarnClaimFlights.flights[key] = flight
	missingReasoningWarnClaimFlights.Unlock()

	processedAt := time.Time{}
	shouldWarn := false
	for {
		missingReasoningWarnClaimFlights.Lock()
		nextObservedAt := flight.latestObservedAt
		missingReasoningWarnClaimFlights.Unlock()

		if nextObservedAt.After(processedAt) {
			shouldWarn = s.persistClaimAt(fingerprint, nextObservedAt) || shouldWarn
			processedAt = nextObservedAt
		}

		missingReasoningWarnClaimFlights.Lock()
		if !flight.latestObservedAt.After(processedAt) {
			delete(missingReasoningWarnClaimFlights.flights, key)
			missingReasoningWarnClaimFlights.Unlock()
			return shouldWarn
		}
		missingReasoningWarnClaimFlights.Unlock()
	}
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
	processLock := s.processLock()
	processLock.Lock()
	defer processLock.Unlock()

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
