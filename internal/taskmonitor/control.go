package taskmonitor

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// JobKiller is an optional interface for stopping running jobs. ControlService
// calls Kill to cancel a job's context when stop/cancel is requested. When nil,
// only the persistent state is updated.
type JobKiller interface {
	Kill(jobID string) bool
}

// ControlResult is the unified response for all task control operations.
type ControlResult struct {
	SchemaVersion int        `json:"schema_version"`
	Command       string     `json:"command"`
	TaskID        string     `json:"task_id"`
	SessionID     string     `json:"session_id"`
	State         TaskState  `json:"state"`
	Version       uint64     `json:"version"`
	Accepted      bool       `json:"accepted"`
	Idempotent    bool       `json:"idempotent"`
	Error         *CtrlError `json:"error,omitempty"`
}

// CtrlError carries a stable machine-readable code and message.
type CtrlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	ErrTaskNotFound            = "task_not_found"
	ErrTaskScopeMismatch       = "task_scope_mismatch"
	ErrTaskVersionConflict     = "task_version_conflict"
	ErrTaskInvalidTransition   = "task_invalid_transition"
	ErrTaskNotResumable        = "task_not_resumable"
	ErrTaskAlreadyTerminal     = "task_already_terminal"
	ErrTaskInProgress          = "task_operation_in_progress"
	ErrTaskPermissionDenied    = "task_permission_denied"
	ErrTaskIdempotencyConflict = "task_idempotency_conflict"
	ErrTaskAuditFailed         = "task_audit_failed"
)

// ControlService provides atomic control operations on tasks.
type ControlService struct {
	mu     sync.Mutex
	store  WriteStore
	killer JobKiller
}

// NewControlService returns a ControlService backed by store.
func NewControlService(store WriteStore) *ControlService {
	return &ControlService{store: store}
}

// SetJobKiller sets an optional JobKiller for stopping running jobs.
func (cs *ControlService) SetJobKiller(k JobKiller) { cs.killer = k }

func (cs *ControlService) StopTask(ctx context.Context, projectDir, taskID string, expectedVersion uint64, reason, idemKey string) (ControlResult, error) {
	return cs.controlOp(ctx, projectDir, taskID, expectedVersion, "stop", TaskStateCancelled, reason, idemKey)
}

func (cs *ControlService) CancelTask(ctx context.Context, projectDir, taskID string, expectedVersion uint64, reason, idemKey string) (ControlResult, error) {
	return cs.controlOp(ctx, projectDir, taskID, expectedVersion, "cancel", TaskStateCancelled, reason, idemKey)
}

func (cs *ControlService) ResumeTask(ctx context.Context, projectDir, taskID string, expectedVersion uint64, idemKey string) (ControlResult, error) {
	return cs.controlOp(ctx, projectDir, taskID, expectedVersion, "resume", TaskStateQueued, "", idemKey)
}

func (cs *ControlService) OpenTaskSession(ctx context.Context, projectDir, taskID string) (ControlResult, error) {
	snap, err := cs.store.GetTask(ctx, projectDir, taskID)
	if err != nil {
		return ControlResult{}, err
	}
	if snap == nil {
		return ControlResult{
			SchemaVersion: 1, Command: "open_session", TaskID: taskID,
			Error: &CtrlError{Code: ErrTaskNotFound, Message: "task not found"},
		}, nil
	}
	return ControlResult{
		SchemaVersion: 1, Command: "open_session",
		TaskID: snap.TaskID, SessionID: snap.SessionID,
		State: snap.State, Version: snap.Version, Accepted: true,
	}, nil
}

func (cs *ControlService) controlOp(ctx context.Context, projectDir, taskID string, expectedVersion uint64, cmd string, targetState TaskState, reason, idemKey string) (ControlResult, error) {
	if err := ctx.Err(); err != nil {
		return ControlResult{}, err
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// ── idempotency check (persisted) ──
	if idemKey != "" {
		rec, err := cs.store.CheckIdempotency(ctx, projectDir, idemKey)
		if err != nil {
			return ControlResult{}, fmt.Errorf("check idempotency: %w", err)
		}
		if rec != nil {
			// Must match exactly
			if rec.Op != cmd || rec.TaskID != taskID || rec.Version != expectedVersion {
				return ControlResult{
					SchemaVersion: 1, Command: cmd, TaskID: taskID,
					Error: &CtrlError{Code: ErrTaskIdempotencyConflict, Message: "idempotency key reused with different parameters"},
				}, nil
			}
			// Replay: fetch current state
			snap, err := cs.store.GetTask(ctx, projectDir, taskID)
			if err != nil {
				return ControlResult{}, err
			}
			if snap == nil {
				return ControlResult{
					SchemaVersion: 1, Command: cmd, TaskID: taskID,
					Error: &CtrlError{Code: ErrTaskNotFound, Message: "task not found"},
				}, nil
			}
			return ControlResult{
				SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
				State: snap.State, Version: snap.Version, Accepted: true, Idempotent: true,
			}, nil
		}
	}

	// ── fetch + validate ──
	snap, err := cs.store.GetTask(ctx, projectDir, taskID)
	if err != nil {
		return ControlResult{}, err
	}
	if snap == nil {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID,
			Error: &CtrlError{Code: ErrTaskNotFound, Message: "task not found"},
		}, nil
	}
	if expectedVersion != snap.Version {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
			State: snap.State, Version: snap.Version,
			Error: &CtrlError{Code: ErrTaskVersionConflict, Message: "version mismatch"},
		}, nil
	}
	resumable := cmd == "resume" && (snap.State == TaskStateFailed || snap.State == TaskStateStale)
	if snap.State.Terminal() && !resumable {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
			State: snap.State, Version: snap.Version,
			Error: &CtrlError{Code: ErrTaskAlreadyTerminal, Message: "task is terminal"},
		}, nil
	}
	if !resumable && !snap.State.ValidTransition(targetState) {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
			State: snap.State, Version: snap.Version,
			Error: &CtrlError{Code: ErrTaskInvalidTransition, Message: "invalid transition"},
		}, nil
	}

	// ── 1. SaveTask (state mutation) ──
	snap.Version++
	snap.State = targetState
	snap.UpdatedAt = timeNow()

	if err := cs.store.SaveTask(ctx, projectDir, *snap); err != nil {
		return ControlResult{}, fmt.Errorf("save task: %w", err)
	}

	// ── 2. AppendAuditEvent (atomic sequence + write) ──
	auditEv := TaskEvent{
		Sequence:  0, // assigned atomically by store
		Timestamp: timeNow(),
		EventType: "control_" + cmd,
		TaskID:    taskID,
		SessionID: snap.SessionID,
		State:     targetState,
	}
	if reason != "" {
		auditEv.ErrorSummary = reason
	}
	if err := cs.store.AppendAuditEvent(ctx, projectDir, auditEv); err != nil {
		// State is committed but audit is missing. This is a degraded
		// but not silent state — the caller receives an error.
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID,
			Error: &CtrlError{Code: ErrTaskAuditFailed, Message: "state saved but audit event failed"},
		}, fmt.Errorf("append audit event: %w", err)
	}

	// ── 3. RecordIdempotency (claim key after successful mutation) ──
	if idemKey != "" {
		rec := IdempotencyRecord{Key: idemKey, Op: cmd, TaskID: taskID, Version: expectedVersion}
		if err := cs.store.RecordIdempotency(ctx, projectDir, rec); err != nil {
			return ControlResult{}, fmt.Errorf("record idempotency: %w", err)
		}
	}

	// ── kill running job ──
	if cs.killer != nil && (cmd == "stop" || cmd == "cancel") {
		cs.killer.Kill(taskID)
	}

	return ControlResult{
		SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
		State: snap.State, Version: snap.Version, Accepted: true,
	}, nil
}

var timeNow = func() time.Time { return time.Now() }
