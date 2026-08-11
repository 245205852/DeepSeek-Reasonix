package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/store"
)

// topicMigrationMarker records a completed legacy→topic migration for the
// directory's current session signature (name+size hash). New CLI sessions
// change the signature and force re-migration without mtime ordering.
// v2 also re-evaluates recovery-named sessions that v1 skipped by filename.
const topicMigrationMarker = ".topics-migrated-v2"
const topicIndexRepairMarker = ".topic-indexes-repaired-v2"

func invalidateTopicDirMarkers(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	var errs []error
	for _, marker := range []string{topicMigrationMarker, topicIndexRepairMarker} {
		if err := os.Remove(filepath.Join(dir, marker)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func topicDirMarkerDone(dir, marker string) bool {
	dir = strings.TrimSpace(dir)
	marker = strings.TrimSpace(marker)
	if dir == "" || marker == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, marker))
	if err != nil {
		return false
	}
	sig, err := sessionDirMigrationSignature(dir)
	if err != nil {
		// Transient directory read failure: treat as not done so the next
		// reconcile retries rather than permanently skipping migration.
		return false
	}
	// Accept both signature content and legacy empty markers that still match
	// only when the directory has no session files (empty sig of empty dir).
	got := strings.TrimSpace(string(data))
	if got == "" {
		// Legacy empty marker: valid only when the dir currently has no
		// migratable session/meta files. Any new transcript must re-run.
		return sig == emptySessionDirSignature
	}
	return got == sig
}

const emptySessionDirSignature = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sessionDirMigrationSignature(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !store.IsSessionTranscriptName(name) && !strings.HasSuffix(name, ".jsonl.meta") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		// Name + size form a durable identity. Omit mtime so coarse FS clocks
		// cannot produce equal signatures for distinct files, and so two
		// writes within the same timestamp tick still differ by size/name.
		lines = append(lines, fmt.Sprintf("%s\t%d", name, info.Size()))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func topicMigrationDone(dir string) bool {
	return topicDirMarkerDone(dir, topicMigrationMarker)
}

func topicIndexRepairDone(dir string) bool {
	return topicDirMarkerDone(dir, topicIndexRepairMarker)
}

func markTopicDirMarkerDone(dir, marker string) {
	dir = strings.TrimSpace(dir)
	marker = strings.TrimSpace(marker)
	if dir == "" || marker == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	sig, err := sessionDirMigrationSignature(dir)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, marker), []byte(sig+"\n"), 0o644)
}

func markTopicMigrationDone(dir string) {
	markTopicDirMarkerDone(dir, topicMigrationMarker)
}

func markTopicIndexRepairDone(dir string) {
	markTopicDirMarkerDone(dir, topicIndexRepairMarker)
}
