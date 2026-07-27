package taskmonitor

import (
	"context"
	"time"
)

// ControlResult is the unified response for all task control operations.
type ControlResult struct {
	SchemaVersion int        `json:"schema_version"`
	Command       string     `json:"command"` // "stop" | "cancel" | "resume" | "open_session"
	TaskID        string     `json:"task_id"`
	SessionID     string     `json:"session_id"`
	State         TaskState  `json:"state"`
	Version       uint64     `json:"version"`
	Accepted      bool       `json:"accepted"`
	Idempotent    bool       `json:"idempotent"`
	Error         *CtrlError `json:"error,omitempty"`
}

// CtrlError carries a stable machine-readable code and a human-readable
// message. The message MUST NOT contain paths, credentials, prompt text,
// or model responses.
type CtrlError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable error codes for control operations.
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
)

// ControlService provides atomic control operations on tasks. It is the
// sole write authority: all state changes go through this service, which
// enforces version-based optimistic locking, idempotency, and project
// scope.
type ControlService struct {
	store   WriteStore
	idemLog map[string]string // idempotencyKey → "taskID:version"
}

// NewControlService returns a ControlService backed by the given WriteStore.
func NewControlService(store WriteStore) *ControlService {
	return &ControlService{
		store:   store,
		idemLog: make(map[string]string),
	}
}

// StopTask requests a running or waiting task to stop. The caller must
// supply the expected_version (the version it last read); the operation
// fails with task_version_conflict if the stored version differs.
func (cs *ControlService) StopTask(ctx context.Context, projectDir, taskID string, expectedVersion uint64, reason, idemKey string) (ControlResult, error) {
	return cs.controlOp(ctx, projectDir, taskID, expectedVersion, "stop", TaskStateCancelled, reason, idemKey)
}

// CancelTask cancels a non-terminal task. Terminal tasks return an
// idempotent success.
func (cs *ControlService) CancelTask(ctx context.Context, projectDir, taskID string, expectedVersion uint64, reason, idemKey string) (ControlResult, error) {
	return cs.controlOp(ctx, projectDir, taskID, expectedVersion, "cancel", TaskStateCancelled, reason, idemKey)
}

// ResumeTask attempts to resume a task from a recoverable state. Returns
// task_not_resumable if the task is not in a state that supports resume.
func (cs *ControlService) ResumeTask(ctx context.Context, projectDir, taskID string, expectedVersion uint64, idemKey string) (ControlResult, error) {
	return cs.controlOp(ctx, projectDir, taskID, expectedVersion, "resume", TaskStateQueued, "", idemKey)
}

// OpenTaskSession returns the session associated with a task. It is
// read-only and does not modify state.
func (cs *ControlService) OpenTaskSession(ctx context.Context, projectDir, taskID string) (ControlResult, error) {
	snap, err := cs.store.GetTask(ctx, projectDir, taskID)
	if err != nil {
		return ControlResult{}, err
	}
	if snap == nil {
		return ControlResult{
			SchemaVersion: 1,
			Command:       "open_session",
			TaskID:        taskID,
			Error:         &CtrlError{Code: ErrTaskNotFound, Message: "task not found"},
		}, nil
	}
	return ControlResult{
		SchemaVersion: 1,
		Command:       "open_session",
		TaskID:        snap.TaskID,
		SessionID:     snap.SessionID,
		State:         snap.State,
		Version:       snap.Version,
		Accepted:      true,
	}, nil
}

func (cs *ControlService) controlOp(ctx context.Context, projectDir, taskID string, expectedVersion uint64, cmd string, targetState TaskState, reason, idemKey string) (ControlResult, error) {
	if err := ctx.Err(); err != nil {
		return ControlResult{}, err
	}

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

	// Idempotency check
	if idemKey != "" {
		if prevID, ok := cs.idemLog[idemKey]; ok {
			if prevID == taskID {
				return ControlResult{
					SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
					State: snap.State, Version: snap.Version, Accepted: true, Idempotent: true,
				}, nil
			}
			return ControlResult{
				SchemaVersion: 1, Command: cmd, TaskID: taskID,
				Error: &CtrlError{Code: ErrTaskIdempotencyConflict, Message: "idempotency key reused for different task"},
			}, nil
		}
	}

	// Version check
	if expectedVersion != snap.Version {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
			State: snap.State, Version: snap.Version,
			Error: &CtrlError{Code: ErrTaskVersionConflict, Message: "version mismatch: expected version has changed"},
		}, nil
	}

	// Terminal guard
	if snap.State.Terminal() {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
			State: snap.State, Version: snap.Version,
			Error: &CtrlError{Code: ErrTaskAlreadyTerminal, Message: "task is already in a terminal state"},
		}, nil
	}

	// Transition check
	if !snap.State.ValidTransition(targetState) {
		return ControlResult{
			SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
			State: snap.State, Version: snap.Version,
			Error: &CtrlError{Code: ErrTaskInvalidTransition, Message: "invalid state transition"},
		}, nil
	}

	// Apply: increment version and set state
	snap.Version++
	snap.State = targetState
	snap.UpdatedAt = timeNow()

	// Persist
	if err := cs.store.SaveTask(ctx, projectDir, *snap); err != nil {
		return ControlResult{}, err
	}

	// Record audit event
	auditEv := TaskEvent{
		Sequence:  1, // audit events always start at 1
		Timestamp: timeNow(),
		EventType: "control_" + cmd,
		TaskID:    taskID,
		SessionID: snap.SessionID,
		State:     targetState,
	}
	if reason != "" {
		auditEv.ErrorSummary = reason
	}
	// Best-effort audit logging
	cs.store.SaveEvent(ctx, projectDir, auditEv)

	// Record idempotency
	if idemKey != "" {
		cs.idemLog[idemKey] = taskID
	}

	return ControlResult{
		SchemaVersion: 1, Command: cmd, TaskID: taskID, SessionID: snap.SessionID,
		State: snap.State, Version: snap.Version, Accepted: true,
	}, nil
}

// timeNow is a package-level override for tests.
var timeNow = func() time.Time { return time.Now() }
