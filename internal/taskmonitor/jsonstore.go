package taskmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileStore is a Store backed by a JSON file tree under a project-local
// directory.  Tasks are stored as <dir>/<task-id>/snapshot.json and
// <dir>/<task-id>/events.jsonl.  It is read-only in TM-02; write support
// is added in TM-04.
type FileStore struct {
	baseDir string // projectDir → task data root (e.g. ".reasonix/tasks")
}

// NewFileStore returns a FileStore rooted at baseDir.  baseDir is typically
// ".reasonix/tasks" relative to the project root.
func NewFileStore(baseDir string) *FileStore {
	return &FileStore{baseDir: baseDir}
}

// safeID validates a user-supplied identifier for use as a filesystem path
// component. It rejects empty strings, ".", "..", and values containing a
// path separator. Used for both taskID and idempotency keys.
func safeID(name string) (string, error) {
	if name == "" {
		return "", errors.New("identifier must not be empty")
	}
	cleaned := filepath.Base(name)
	if cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("invalid identifier %q", name)
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("identifier %q contains path separator", name)
	}
	return cleaned, nil
}

// taskRoot returns the sanitised directory holding task data for projectDir.
// It cleans projectDir and rejects paths that attempt to escape.
func (s *FileStore) taskRoot(projectDir string) (string, error) {
	if projectDir == "" {
		return s.baseDir, nil
	}
	// Reject any path containing ".." before cleaning — catches both
	// relative (../) and absolute traversal (../../etc).
	if strings.Contains(filepath.ToSlash(projectDir), "..") {
		return "", fmt.Errorf("projectDir %q escapes the intended root", projectDir)
	}
	cleaned := filepath.Clean(projectDir)
	return filepath.Join(cleaned, s.baseDir), nil
}

// ListTasks implements Store.
func (s *FileStore) ListTasks(ctx context.Context, projectDir string) ([]TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []TaskSnapshot{}, nil
		}
		return nil, fmt.Errorf("read task dir %s: %w", root, err)
	}
	result := make([]TaskSnapshot, 0)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		snap, err := s.readSnapshot(filepath.Join(root, e.Name()))
		if err != nil {
			continue // skip corrupt entries
		}
		result = append(result, snap)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

// GetTask implements Store.
func (s *FileStore) GetTask(ctx context.Context, projectDir string, taskID string) (*TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := safeID(taskID)
	if err != nil {
		return nil, err
	}
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return nil, err
	}
	snap, err := s.readSnapshot(filepath.Join(root, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return &snap, nil
}

// ListEvents implements Store.
func (s *FileStore) ListEvents(ctx context.Context, projectDir string, taskID string, afterSequence int) ([]TaskEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := safeID(taskID)
	if err != nil {
		return nil, err
	}
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return nil, err
	}
	events, err := s.readEvents(filepath.Join(root, id))
	if err != nil {
		if os.IsNotExist(err) {
			return []TaskEvent{}, nil
		}
		return nil, err
	}
	result := make([]TaskEvent, 0)
	for _, e := range events {
		if e.Sequence > afterSequence {
			result = append(result, e)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Sequence < result[j].Sequence
	})
	return result, nil
}

func (s *FileStore) readSnapshot(taskDir string) (TaskSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(taskDir, "snapshot.json"))
	if err != nil {
		return TaskSnapshot{}, err
	}
	var snap TaskSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return TaskSnapshot{}, fmt.Errorf("parse snapshot: %w", err)
	}
	return snap, nil
}

func (s *FileStore) readEvents(taskDir string) ([]TaskEvent, error) {
	data, err := os.ReadFile(filepath.Join(taskDir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	// JSONL: one JSON object per line
	var events []TaskEvent
	raw := string(data)
	for raw != "" {
		idx := 0
		// find newline
		for idx < len(raw) && raw[idx] != '\n' {
			idx++
		}
		line := raw[:idx]
		raw = raw[idx:]
		if len(raw) > 0 {
			raw = raw[1:] // skip newline
		}
		if line == "" {
			continue
		}
		var ev TaskEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // skip corrupt lines
		}
		events = append(events, ev)
	}
	return events, nil
}

// SaveTask implements WriteStore. It atomically writes the snapshot,
// failing if a concurrent write has changed the version.
func (s *FileStore) SaveTask(ctx context.Context, projectDir string, snap TaskSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id, err := safeID(snap.TaskID)
	if err != nil {
		return err
	}
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return err
	}
	taskDir := filepath.Join(root, id)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return fmt.Errorf("save task: %w", err)
	}

	target := filepath.Join(taskDir, "snapshot.json")
	// Read current version for CAS check
	current, err := s.readSnapshot(taskDir)
	if err == nil && snap.Version <= current.Version {
		return fmt.Errorf("save task: version conflict: stored=%d, given=%d", current.Version, snap.Version)
	}

	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("save task: marshal: %w", err)
	}

	// Atomic write via temp file + rename
	tmp, err := os.CreateTemp(taskDir, ".snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("save task: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("save task: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("save task: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("save task: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("save task: %w", err)
	}
	return nil
}

// SaveEvent implements WriteStore.
func (s *FileStore) SaveEvent(ctx context.Context, projectDir string, ev TaskEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ev.Validate(); err != nil {
		return fmt.Errorf("save event: %w", err)
	}
	id, err := safeID(ev.TaskID)
	if err != nil {
		return err
	}
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return err
	}
	taskDir := filepath.Join(root, id)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return fmt.Errorf("save event: %w", err)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("save event: marshal: %w", err)
	}
	// Append JSONL line
	f, err := os.OpenFile(filepath.Join(taskDir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("save event: %w", err)
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, string(data))
	return err
}

// NextSequence implements WriteStore.
func (s *FileStore) NextSequence(ctx context.Context, projectDir string, taskID string) (int, error) {
	id, err := safeID(taskID)
	if err != nil {
		return 0, err
	}
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return 0, err
	}
	events, err := s.readEvents(filepath.Join(root, id))
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	max := 0
	for _, e := range events {
		if e.Sequence > max {
			max = e.Sequence
		}
	}
	return max + 1, nil
}

// CheckIdempotency implements WriteStore.
func (s *FileStore) CheckIdempotency(ctx context.Context, projectDir string, key string) (*IdempotencyRecord, error) {
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return nil, err
	}
	id, err := safeID(key)
	if err != nil {
		return nil, err
	}
	idemDir := filepath.Join(root, ".idempotency")
	data, err := os.ReadFile(filepath.Join(idemDir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec IdempotencyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, nil
	}
	return &rec, nil
}

// RecordIdempotency implements WriteStore.
func (s *FileStore) RecordIdempotency(ctx context.Context, projectDir string, r IdempotencyRecord) error {
	root, err := s.taskRoot(projectDir)
	if err != nil {
		return err
	}
	id, err := safeID(r.Key)
	if err != nil {
		return err
	}
	idemDir := filepath.Join(root, ".idempotency")
	if err := os.MkdirAll(idemDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	target := filepath.Join(idemDir, id+".json")
	// Atomic write via temp file + rename
	tmp, err := os.CreateTemp(idemDir, ".idem-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Rename: on most filesystems, this atomically replaces the target.
	// InMemoryStore's RecordIdempotency does its own conflict check under
	// lock. The ControlService.mu serialises calls, so in practice the
	// first writer always wins.
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
