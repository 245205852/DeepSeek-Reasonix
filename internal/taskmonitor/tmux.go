package taskmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TmuxRunner is the narrow command surface used by Adapter. Implementations
// must pass arguments as an array; callers never construct a shell command.
type TmuxRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type execTmuxRunner struct{ binary string }

func (r execTmuxRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	return cmd.Output()
}

// Mapping records only resources created by this adapter.
type TmuxMapping struct {
	SchemaVersion int       `json:"schema_version"`
	TaskID        string    `json:"task_id"`
	ProjectDir    string    `json:"project_dir"`
	Session       string    `json:"session"`
	Window        string    `json:"window"`
	Pane          string    `json:"pane"`
	CreatedAt     time.Time `json:"created_at"`
	Stale         bool      `json:"stale"`
}

type TmuxResult struct {
	SchemaVersion int          `json:"schema_version"`
	TaskID        string       `json:"task_id"`
	Available     bool         `json:"available"`
	Idempotent    bool         `json:"idempotent"`
	Mapping       *TmuxMapping `json:"mapping,omitempty"`
	Error         *CtrlError   `json:"error,omitempty"`
}

// TmuxAdapter maps tasks to user-visible tmux windows. It never changes task
// state; the Task Store remains the sole source of truth.
type TmuxAdapter struct {
	store  Store
	runner TmuxRunner
	base   string
}

func NewTmuxAdapter(store Store, baseDir string) *TmuxAdapter {
	return &TmuxAdapter{store: store, runner: newDefaultTmuxRunner(), base: baseDir}
}

func NewTmuxAdapterWithRunner(store Store, baseDir string, runner TmuxRunner) *TmuxAdapter {
	return &TmuxAdapter{store: store, runner: runner, base: baseDir}
}

func newDefaultTmuxRunner() TmuxRunner {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil
	}
	return execTmuxRunner{binary: path}
}

func (a *TmuxAdapter) Attach(ctx context.Context, projectDir, taskID, requestedSession string) TmuxResult {
	if err := validateProjectDir(projectDir); err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "invalid project directory")
	}
	if err := validateTmuxName(requestedSession); err != nil {
		return tmuxError(taskID, ErrTmuxInvalidName, err.Error())
	}
	snap, err := a.store.GetTask(ctx, projectDir, taskID)
	if err != nil {
		return tmuxError(taskID, ErrTmuxTaskError, "task lookup failed")
	}
	if snap == nil {
		return tmuxError(taskID, ErrTaskNotFound, "task not found")
	}
	if a.runner == nil {
		return tmuxUnavailable(taskID)
	}
	if old, err := a.load(projectDir, taskID); err == nil && old != nil && !old.Stale {
		if _, err := a.runner.Run(ctx, "has-session", "-t", old.Session); err == nil {
			return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: true, Idempotent: true, Mapping: old}
		}
		old.Stale = true
		_ = a.save(projectDir, *old)
	}
	session := requestedSession
	if session == "" {
		session = "reasonix-" + taskID
	}
	if err := validateTmuxName(session); err != nil {
		return tmuxError(taskID, ErrTmuxInvalidName, err.Error())
	}
	window := "task"
	if _, err := a.runner.Run(ctx, "new-session", "-d", "-s", session, "-n", window); err != nil {
		return tmuxError(taskID, ErrTmuxCommandFailed, "tmux session creation failed")
	}
	m := &TmuxMapping{SchemaVersion: 1, TaskID: taskID, ProjectDir: projectDir, Session: session, Window: window, Pane: session + ":" + window + ".0", CreatedAt: time.Now().UTC()}
	if err := a.save(projectDir, *m); err != nil {
		_, _ = a.runner.Run(ctx, "kill-session", "-t", session)
		return tmuxError(taskID, ErrTmuxMappingFailed, "mapping write failed")
	}
	return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: true, Mapping: m}
}

func (a *TmuxAdapter) Status(ctx context.Context, projectDir, taskID string) TmuxResult {
	if err := validateProjectDir(projectDir); err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "invalid project directory")
	}
	m, err := a.load(projectDir, taskID)
	if err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "mapping read failed")
	}
	if m == nil {
		return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: a.runner != nil}
	}
	if a.runner == nil {
		m.Stale = true
		return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: false, Mapping: m}
	}
	if _, err := a.runner.Run(ctx, "has-session", "-t", m.Session); err != nil {
		m.Stale = true
		_ = a.save(projectDir, *m)
	}
	return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: true, Mapping: m}
}

func (a *TmuxAdapter) Open(ctx context.Context, projectDir, taskID string) TmuxResult {
	if err := validateProjectDir(projectDir); err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "invalid project directory")
	}
	r := a.Status(ctx, projectDir, taskID)
	if r.Mapping == nil || r.Mapping.Stale {
		return r
	}
	if a.runner == nil {
		return tmuxUnavailable(taskID)
	}
	if _, err := a.runner.Run(ctx, "switch-client", "-t", r.Mapping.Pane); err != nil {
		r.Error = &CtrlError{Code: ErrTmuxCommandFailed, Message: "tmux open failed"}
	}
	return r
}

func (a *TmuxAdapter) Detach(ctx context.Context, projectDir, taskID string) TmuxResult {
	if err := validateProjectDir(projectDir); err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "invalid project directory")
	}
	m, err := a.load(projectDir, taskID)
	if err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "mapping read failed")
	}
	if m == nil {
		return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: a.runner != nil, Idempotent: true}
	}
	if a.runner != nil && !m.Stale {
		if _, err := a.runner.Run(ctx, "kill-session", "-t", m.Session); err != nil {
			return tmuxError(taskID, ErrTmuxCommandFailed, "tmux detach failed")
		}
	}
	id, err := safeID(taskID)
	if err != nil {
		return tmuxError(taskID, ErrTmuxMappingFailed, "invalid task id")
	}
	_ = os.Remove(a.mappingPath(projectDir, id))
	return TmuxResult{SchemaVersion: 1, TaskID: taskID, Available: a.runner != nil, Idempotent: false, Mapping: m}
}

const (
	ErrTmuxUnavailable   = "tmux_unavailable"
	ErrTmuxInvalidName   = "tmux_invalid_name"
	ErrTmuxCommandFailed = "tmux_command_failed"
	ErrTmuxMappingFailed = "tmux_mapping_failed"
	ErrTmuxTaskError     = "tmux_task_error"
)

func tmuxUnavailable(taskID string) TmuxResult {
	return tmuxError(taskID, ErrTmuxUnavailable, "tmux is not available")
}

func tmuxError(taskID, code, message string) TmuxResult {
	return TmuxResult{SchemaVersion: 1, TaskID: taskID, Error: &CtrlError{Code: code, Message: message}}
}

func validateTmuxName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 64 || strings.ContainsAny(name, "/\\\n\r\t") {
		return errors.New("tmux name contains invalid characters")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("tmux name contains control characters")
		}
	}
	return nil
}

func validateProjectDir(projectDir string) error {
	if strings.Contains(filepath.ToSlash(projectDir), "..") {
		return errors.New("project directory escapes scope")
	}
	return nil
}

func (a *TmuxAdapter) mappingPath(projectDir, taskID string) string {
	root := projectDir
	if root == "" {
		root = "."
	}
	return filepath.Join(root, a.base, ".tmux", taskID+".json")
}

func (a *TmuxAdapter) load(projectDir, taskID string) (*TmuxMapping, error) {
	id, err := safeID(taskID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(a.mappingPath(projectDir, id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m TmuxMapping
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (a *TmuxAdapter) save(projectDir string, m TmuxMapping) error {
	if _, err := safeID(m.TaskID); err != nil {
		return err
	}
	path := a.mappingPath(projectDir, m.TaskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmux-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
