package taskmonitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"reasonix/internal/jobs"
)

// TaskRecorder bridges jobs.Manager lifecycle events into the Task Store. It
// is the write side of task monitoring: RecordStart persists a running
// snapshot, RecordDone advances it to its terminal state. All failures are
// swallowed — monitoring is best-effort and must never break the job pipeline.
// The store's per-task lock keeps concurrent recorders (CLI + Desktop) safe.
type TaskRecorder struct {
	store       WriteStore
	projectDir  string
	sessionIDFn func() string
	mu          sync.Mutex
	monitorIDs  map[string]string
}

// NewTaskRecorder returns a TaskRecorder writing to store under projectDir.
// sessionIDFn is called per record so the snapshot reflects the session id at
// creation time (controllers resolve their session path lazily); it may return
// "" when no session is bound yet.
func NewTaskRecorder(store WriteStore, projectDir string, sessionIDFn func() string) *TaskRecorder {
	return &TaskRecorder{store: store, projectDir: projectDir, sessionIDFn: sessionIDFn, monitorIDs: make(map[string]string)}
}

// monitorTaskID creates a globally unique monitor identity for a job within
// a session. jobs.Manager IDs are local to a manager and restart from task-1
// for every session, so persisting the raw job ID would cause cross-session
// overwrites in the shared project store.
func monitorTaskID(sessionID, jobID string) string {
	if sessionID == "" {
		return jobID
	}
	id := fmt.Sprintf("%s--%s", sessionID, jobID)
	if len(id) <= maxFieldLen {
		return id
	}
	h := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(h[:8]) + "--" + jobID
}

func (r *TaskRecorder) rememberMonitorID(jobID, monitorID string) {
	r.mu.Lock()
	r.monitorIDs[jobID] = monitorID
	r.mu.Unlock()
}

func (r *TaskRecorder) lookupMonitorID(jobID string) (string, bool) {
	r.mu.Lock()
	monitorID, ok := r.monitorIDs[jobID]
	r.mu.Unlock()
	return monitorID, ok
}

// RecordStart implements jobs.TaskRecorder.
func (r *TaskRecorder) RecordStart(id, kind, label string) {
	ctx := context.Background()
	now := timeNow()
	sessionID := ""
	if r.sessionIDFn != nil {
		sessionID = r.sessionIDFn()
	}
	monitorID := monitorTaskID(sessionID, id)
	r.rememberMonitorID(id, monitorID)
	snap := TaskSnapshot{
		SchemaVersion: 1,
		TaskID:        monitorID,
		SessionID:     sessionID,
		State:         TaskStateRunning,
		RuntimeState:  RuntimeStateAlive,
		Version:       1,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := r.store.SaveTask(ctx, r.projectDir, snap); err != nil {
		return
	}
	_ = r.store.AppendAuditEvent(ctx, r.projectDir, TaskEvent{
		Timestamp: now, EventType: "state_change",
		TaskID: monitorID, SessionID: sessionID, State: TaskStateRunning,
		RuntimeState: RuntimeStateAlive,
	})
}

// RecordDone implements jobs.TaskRecorder.
func (r *TaskRecorder) RecordDone(id string, st jobs.Status, jobErr error) {
	ctx := context.Background()
	target := terminalState(st)
	if target == "" {
		return // non-terminal/unknown status: leave the snapshot untouched
	}
	monitorID, ok := r.lookupMonitorID(id)
	if !ok {
		return // no matching lifecycle was recorded by this recorder
	}
	const maxSaveAttempts = 4
	for attempt := 0; attempt < maxSaveAttempts; attempt++ {
		cur, gerr := r.store.GetTask(ctx, r.projectDir, monitorID)
		if gerr != nil || cur == nil {
			return // never recorded (recorder attached after the job started)
		}
		now := timeNow()
		cur.State = target
		cur.RuntimeState = RuntimeStateExited
		cur.Version++
		cur.UpdatedAt = now
		if jobErr != nil {
			cur.ErrorCode = "job_failed"
			cur.ErrorSummary = truncateSummary(jobErr.Error())
		}
		if serr := r.store.SaveTask(ctx, r.projectDir, *cur); serr != nil {
			if errors.Is(serr, ErrStoreVersionConflict) {
				continue
			}
			return
		}
		_ = r.store.AppendAuditEvent(ctx, r.projectDir, TaskEvent{
			Timestamp: now, EventType: "state_change",
			TaskID: monitorID, SessionID: cur.SessionID, State: target,
			RuntimeState: RuntimeStateExited,
			ErrorCode:    cur.ErrorCode, ErrorSummary: cur.ErrorSummary,
		})
		return
	}
}

// terminalState maps a job status to the task state it reports. Unknown or
// non-terminal statuses map to "" (no update).
func terminalState(st jobs.Status) TaskState {
	switch st {
	case jobs.Done:
		return TaskStateSucceeded
	case jobs.Failed:
		return TaskStateFailed
	case jobs.Killed, jobs.Interrupted:
		return TaskStateCancelled
	default:
		return ""
	}
}

// truncateSummary caps an error message at maxErrorSummaryLen bytes without
// splitting a UTF-8 rune.
func truncateSummary(s string) string {
	if len(s) <= maxErrorSummaryLen {
		return s
	}
	b := []byte(s)[:maxErrorSummaryLen]
	for len(b) > 0 && b[len(b)-1]&0xc0 == 0x80 {
		b = b[:len(b)-1]
	}
	return string(b)
}
