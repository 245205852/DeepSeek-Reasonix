// Package autoresearch is a read-only compatibility reader for historical
// `.reasonix/autoresearch/<task-id>/` archives. New Goal runs never create or
// mutate these directories.
package autoresearch

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	fileencoding "reasonix/internal/fileutil/encoding"
)

var safeTaskID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var explicitTaskPath = regexp.MustCompile(`\.reasonix/autoresearch/([A-Za-z0-9][A-Za-z0-9._-]*)/?`)

// Store is a fail-closed reader over a workspace's legacy AutoResearch root.
type Store struct {
	workspaceRoot string
	root          string
}

func NewStore(workspaceRoot string) *Store {
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceRoot = resolved
	}
	return &Store{
		workspaceRoot: workspaceRoot,
		root:          filepath.Join(workspaceRoot, ".reasonix", "autoresearch"),
	}
}

// Root returns the absolute archive root under the workspace.
func (s *Store) Root() string {
	return s.root
}

func (s *Store) ListSummaries() ([]Summary, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Summary{}, nil
		}
		return nil, fmt.Errorf("autoresearch: list tasks: %w", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if validateTaskID(id) != nil {
			continue
		}
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := make([]Summary, 0, len(ids))
	for _, id := range ids {
		summary, err := s.Summary(id)
		if err != nil {
			return nil, err
		}
		out = append(out, *summary)
	}
	return out, nil
}

func (s *Store) LoadTask(taskID string) (*Task, error) {
	storeRoot, taskRel, err := s.openTaskRoot(taskID)
	if err != nil {
		return nil, err
	}
	defer storeRoot.Close()
	info, err := storeRoot.Lstat(taskRel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("autoresearch: task %s not found", taskID)
		}
		return nil, fmt.Errorf("autoresearch: stat task %s: %w", taskID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("autoresearch: task %s is a symlink", taskID)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("autoresearch: task %s is not a directory", taskID)
	}
	var spec TaskSpec
	if err := readJSONFile(storeRoot, filepath.Join(taskRel, "state", "task_spec.json"), &spec); err != nil {
		return nil, err
	}
	return &Task{ID: taskID, Root: s.taskRoot(taskID), Spec: spec}, nil
}

// ResumeFromGoalText loads an archive only when goal text names an explicit
// `.reasonix/autoresearch/<task-id>/` path. ok is true when a path was found;
// err is non-nil when that path is missing, corrupt, a symlink, or invalid.
func (s *Store) ResumeFromGoalText(goal string) (*Task, bool, error) {
	match := explicitTaskPath.FindStringSubmatch(goal)
	if len(match) < 2 {
		return nil, false, nil
	}
	task, err := s.LoadTask(match[1])
	if err != nil {
		return nil, true, err
	}
	if report, err := s.ValidateTask(task.ID); err != nil {
		return nil, true, err
	} else if !report.Valid {
		return nil, true, fmt.Errorf("autoresearch: task %s is invalid: %v", task.ID, report.Errors)
	}
	return task, true, nil
}

// ExplicitTaskID extracts a legacy archive id from free-form goal text without
// loading the archive.
func ExplicitTaskID(goal string) (string, bool) {
	match := explicitTaskPath.FindStringSubmatch(goal)
	if len(match) < 2 {
		return "", false
	}
	return match[1], true
}

func (s *Store) Findings(taskID string, limit int) ([]Finding, error) {
	storeRoot, taskRel, err := s.openTaskRoot(taskID)
	if err != nil {
		return nil, err
	}
	defer storeRoot.Close()
	path := filepath.Join(taskRel, "state", "findings.jsonl")
	// Bounded requests (the newest-N views) read only the file tail; limit 0
	// keeps the full scan because accepted-evidence lookups need every entry.
	lines, err := tailJSONLLines(storeRoot, path, limit)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, line := range lines {
		var f Finding
		if err := json.Unmarshal(fileencoding.DecodeToUTF8(line), &f); err != nil {
			return nil, fmt.Errorf("autoresearch: parse %s: %w", path, err)
		}
		// Kind is fully opaque: unknown historical values pass through.
		findings = append(findings, f)
	}
	for i, j := 0, len(findings)-1; i < j; i, j = i+1, j-1 {
		findings[i], findings[j] = findings[j], findings[i]
	}
	if limit > 0 && len(findings) > limit {
		findings = findings[:limit]
	}
	return findings, nil
}

func (s *Store) Heartbeats(taskID string, limit int) ([]Heartbeat, error) {
	storeRoot, taskRel, err := s.openTaskRoot(taskID)
	if err != nil {
		return nil, err
	}
	defer storeRoot.Close()
	path := filepath.Join(taskRel, "logs", "heartbeat.jsonl")
	lines, err := tailJSONLLines(storeRoot, path, limit)
	if err != nil {
		return nil, err
	}
	var heartbeats []Heartbeat
	for _, line := range lines {
		var h Heartbeat
		if err := json.Unmarshal(fileencoding.DecodeToUTF8(line), &h); err != nil {
			return nil, fmt.Errorf("autoresearch: parse %s: %w", path, err)
		}
		heartbeats = append(heartbeats, h)
	}
	if limit > 0 && len(heartbeats) > limit {
		heartbeats = heartbeats[len(heartbeats)-limit:]
	}
	return heartbeats, nil
}

func (s *Store) LastHeartbeat(taskID string) (Heartbeat, bool, error) {
	heartbeats, err := s.Heartbeats(taskID, 1)
	if err != nil {
		return Heartbeat{}, false, err
	}
	if len(heartbeats) == 0 {
		return Heartbeat{}, false, nil
	}
	return heartbeats[0], true, nil
}

func (s *Store) Progress(taskID string) (*Progress, error) {
	storeRoot, taskRel, err := s.openTaskRoot(taskID)
	if err != nil {
		return nil, err
	}
	defer storeRoot.Close()
	var progress Progress
	if err := readJSONFile(storeRoot, filepath.Join(taskRel, "state", "progress.json"), &progress); err != nil {
		return nil, err
	}
	return &progress, nil
}

func (s *Store) ValidateTask(taskID string) (*ValidationReport, error) {
	storeRoot, taskRel, err := s.openTaskRoot(taskID)
	if err != nil {
		return nil, err
	}
	defer storeRoot.Close()
	report := &ValidationReport{Valid: true}
	info, err := storeRoot.Lstat(taskRel)
	if err != nil {
		report.add("task", "", err.Error())
		report.Valid = false
		return report, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		report.add("task", "", "task directory must not be a symlink")
		report.Valid = false
		return report, nil
	}
	if !info.IsDir() {
		report.add("task", "", "task path is not a directory")
		report.Valid = false
		return report, nil
	}
	var spec TaskSpec
	if err := readJSONFile(storeRoot, filepath.Join(taskRel, "state", "task_spec.json"), &spec); err != nil {
		report.add("task_spec.json", "", err.Error())
	} else {
		validateTaskSpec(report, taskID, spec)
	}
	var progress Progress
	if err := readJSONFile(storeRoot, filepath.Join(taskRel, "state", "progress.json"), &progress); err != nil {
		report.add("progress.json", "", err.Error())
	} else {
		validateProgress(report, progress)
	}
	for _, rel := range []string{
		"state/directions_tried.json",
		"state/findings.jsonl",
		"state/iteration_log.jsonl",
		"logs/heartbeat.jsonl",
	} {
		if _, err := storeRoot.Stat(filepath.Join(taskRel, rel)); err != nil {
			report.add(filepath.Base(rel), "", err.Error())
		}
	}
	report.Valid = len(report.Errors) == 0
	return report, nil
}

func (s *Store) taskRoot(taskID string) string {
	return filepath.Join(s.root, taskID)
}

func (s *Store) taskRel(taskID string, parts ...string) (string, error) {
	if err := validateTaskID(taskID); err != nil {
		return "", err
	}
	all := append([]string{taskID}, parts...)
	rel := filepath.Join(all...)
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("autoresearch: unsafe task-relative path %q", rel)
	}
	return rel, nil
}

func (s *Store) openTaskRoot(taskID string) (*os.Root, string, error) {
	taskRel, err := s.taskRel(taskID)
	if err != nil {
		return nil, "", err
	}
	storeRoot, err := os.OpenRoot(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("autoresearch: task %s not found", taskID)
		}
		return nil, "", fmt.Errorf("autoresearch: open root dir: %w", err)
	}
	return storeRoot, taskRel, nil
}

func validateTaskID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("autoresearch: task id is required")
	}
	if !safeTaskID.MatchString(id) || strings.Contains(id, "..") || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("autoresearch: unsafe task id %q", id)
	}
	return nil
}

// validateFinding checks the base schema fields of a historical finding.
// Kind is intentionally unconstrained so unknown historical values remain
// readable. This helper exists for archive integrity checks and tests only;
// the reader never writes findings.
func validateFinding(f Finding) error {
	if strings.TrimSpace(f.ID) == "" {
		return errors.New("autoresearch: finding id is required")
	}
	if strings.TrimSpace(f.Summary) == "" {
		return errors.New("autoresearch: finding summary is required")
	}
	if f.CreatedAt.IsZero() {
		return errors.New("autoresearch: finding created_at is required")
	}
	return nil
}

func readJSONFile(root *os.Root, path string, out any) error {
	data, err := root.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	data = fileencoding.DecodeToUTF8(data)
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func readJSONL(root *os.Root, path string, each func([]byte) error) error {
	f, err := root.Open(path)
	if err != nil {
		return fmt.Errorf("autoresearch: open %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	// Historical findings can be long; raise the scanner buffer for safety.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := each([]byte(line)); err != nil {
			return fmt.Errorf("autoresearch: parse %s: %w", path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("autoresearch: scan %s: %w", path, err)
	}
	return nil
}

// tailJSONLLines returns the last limit non-empty lines of a JSONL file in
// file order, reading backward in fixed-size chunks so per-turn readers do not
// rescan an append-only log that grows for the life of a task. limit <= 0
// reads the whole file (legacy unbounded behavior).
func tailJSONLLines(root *os.Root, path string, limit int) ([][]byte, error) {
	if limit <= 0 {
		var lines [][]byte
		if err := readJSONL(root, path, func(data []byte) error {
			line := make([]byte, len(data))
			copy(line, data)
			lines = append(lines, line)
			return nil
		}); err != nil {
			return nil, err
		}
		return lines, nil
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("autoresearch: open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("autoresearch: stat %s: %w", path, err)
	}
	const chunkSize = 64 * 1024
	var (
		buf []byte
		off = info.Size()
	)
	for off > 0 {
		readLen := min(off, int64(chunkSize))
		off -= readLen
		chunk := make([]byte, readLen)
		if _, err := f.ReadAt(chunk, off); err != nil {
			return nil, fmt.Errorf("autoresearch: read %s: %w", path, err)
		}
		buf = append(chunk, buf...)
		if countCompleteTailLines(buf, off == 0) > limit {
			break
		}
	}
	segments := strings.Split(string(buf), "\n")
	if off > 0 && len(segments) > 0 {
		segments = segments[1:] // drop the leading partial line
	}
	var lines [][]byte
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		lines = append(lines, []byte(seg))
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

func countCompleteTailLines(buf []byte, atStart bool) int {
	segments := strings.Split(string(buf), "\n")
	if !atStart && len(segments) > 0 {
		segments = segments[1:]
	}
	count := 0
	for _, seg := range segments {
		if strings.TrimSpace(seg) != "" {
			count++
		}
	}
	return count
}
