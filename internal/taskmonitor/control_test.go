package taskmonitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestControlService_StopTask(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)
	ctx := context.Background()

	// Setup: task with version 1
	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	res, err := cs.StopTask(ctx, "/p", "t1", 1, "user request", "idem-1")
	if err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if !res.Accepted {
		t.Errorf("expected accepted, got %+v", res)
	}
	if res.State != TaskStateCancelled {
		t.Errorf("expected cancelled, got %q", res.State)
	}
	if res.Version != 2 {
		t.Errorf("expected version 2, got %d", res.Version)
	}
}

func TestControlService_StopTask_VersionConflict(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)
	ctx := context.Background()

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 3,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	res, err := cs.StopTask(ctx, "/p", "t1", 1, "", "")
	if err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if res.Accepted {
		t.Error("expected rejection due to version conflict")
	}
	if res.Error == nil || res.Error.Code != ErrTaskVersionConflict {
		t.Errorf("expected version conflict, got %+v", res.Error)
	}
}

func TestControlService_StopTask_NotFound(t *testing.T) {
	cs := NewControlService(NewInMemoryStore())
	res, err := cs.StopTask(context.Background(), "/p", "ghost", 1, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Error == nil || res.Error.Code != ErrTaskNotFound {
		t.Errorf("expected not_found, got %+v", res.Error)
	}
}

func TestControlService_StopTask_TerminalGuard(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateSucceeded, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	res, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
	if res.Error == nil || res.Error.Code != ErrTaskAlreadyTerminal {
		t.Errorf("expected terminal guard, got %+v", res.Error)
	}
}

func TestControlService_StopTask_InvalidTransition(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateQueued, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	// queued → stop = queued → cancelled = valid
	res, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
	if res.Error != nil {
		t.Errorf("queued→cancelled should be valid, got %+v", res.Error)
	}
}

func TestControlService_Idempotency(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	// First call succeeds
	res1, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "key-1")
	if !res1.Accepted {
		t.Fatal("first call should succeed")
	}

	// Second call with same key — idempotent
	res2, _ := cs.StopTask(context.Background(), "/p", "t1", 2, "", "key-1")
	if !res2.Idempotent {
		t.Errorf("expected idempotent, got %+v", res2)
	}
	if !res2.Accepted {
		t.Error("idempotent response should be accepted")
	}
}

func TestControlService_IdempotencyConflict(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})
	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t2", SessionID: "s2",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	cs.StopTask(context.Background(), "/p", "t1", 1, "", "key-1")
	// Same key on different task
	res, _ := cs.StopTask(context.Background(), "/p", "t2", 1, "", "key-1")
	if !strings.Contains(res.Error.Code, "idempotency") {
		t.Errorf("expected idempotency conflict, got %+v", res.Error)
	}
}

func TestControlService_CancelTask(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateWaiting, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	res, _ := cs.CancelTask(context.Background(), "/p", "t1", 1, "timeout", "")
	if !res.Accepted || res.State != TaskStateCancelled {
		t.Errorf("expected cancelled, got %+v", res)
	}
}

func TestControlService_OpenSession(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "sess-abc",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	res, err := cs.OpenTaskSession(context.Background(), "/p", "t1")
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if res.SessionID != "sess-abc" {
		t.Errorf("expected sess-abc, got %q", res.SessionID)
	}
	if !res.Accepted {
		t.Error("expected accepted")
	}
}

func TestControlService_AuditEvent(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	})

	cs.StopTask(context.Background(), "/p", "t1", 1, "user requested stop", "")

	events, _ := s.ListEvents(context.Background(), "/p", "t1", 0)
	found := false
	for _, ev := range events {
		if ev.EventType == "control_stop" {
			found = true
			if ev.ErrorSummary != "user requested stop" {
				t.Errorf("expected reason in event, got %q", ev.ErrorSummary)
			}
		}
	}
	if !found {
		t.Error("expected audit event for stop")
	}
}

// mustUpsertControl uses UpsertTask but with Version support.
func mustUpsertControl(t *testing.T, s *InMemoryStore, proj string, snap TaskSnapshot) {
	t.Helper()
	if err := s.UpsertTask(proj, snap); err != nil {
		t.Fatal(err)
	}
}
