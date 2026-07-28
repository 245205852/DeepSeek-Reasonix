package taskmonitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlService_StopTask(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)
	ctx := context.Background()

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
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

func TestControlService_VersionConflict(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 3,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	res, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
	if res.Accepted || res.Error == nil || res.Error.Code != ErrTaskVersionConflict {
		t.Errorf("expected version conflict, got %+v", res)
	}
}

func TestControlService_NotFound(t *testing.T) {
	cs := NewControlService(NewInMemoryStore())
	res, _ := cs.StopTask(context.Background(), "/p", "ghost", 1, "", "")
	if res.Error == nil || res.Error.Code != ErrTaskNotFound {
		t.Errorf("expected not_found, got %+v", res.Error)
	}
}

func TestControlService_TerminalGuard(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)
	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateSucceeded, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	res, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
	if res.Error == nil || res.Error.Code != ErrTaskAlreadyTerminal {
		t.Errorf("expected terminal guard, got %+v", res.Error)
	}
}

func TestControlService_Idempotency(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	// First call
	res1, err := cs.StopTask(context.Background(), "/p", "t1", 1, "", "key-1")
	if err != nil || !res1.Accepted {
		t.Fatalf("first call failed: %v, %+v", err, res1)
	}

	// Second call with same key, op, task, version — idempotent
	res2, err := cs.StopTask(context.Background(), "/p", "t1", 1, "", "key-1")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !res2.Idempotent || !res2.Accepted {
		t.Errorf("expected idempotent accepted, got %+v", res2)
	}
}

func TestControlService_IdempotencyConflict_DifferentOp(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	cs.StopTask(context.Background(), "/p", "t1", 1, "", "key-1")
	// Same key but different command
	res, _ := cs.CancelTask(context.Background(), "/p", "t1", 1, "", "key-1")
	if !strings.Contains(res.Error.Code, "idempotency") {
		t.Errorf("expected idempotency conflict, got %+v", res.Error)
	}
}

func TestControlService_IdempotencyConflict_DifferentVersion(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	cs.StopTask(context.Background(), "/p", "t1", 1, "", "key-1")
	res, _ := cs.StopTask(context.Background(), "/p", "t1", 2, "", "key-1")
	if !strings.Contains(res.Error.Code, "idempotency") {
		t.Errorf("expected idempotency conflict for different version, got %+v", res.Error)
	}
}

func TestControlService_AuditEvent(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	cs.StopTask(context.Background(), "/p", "t1", 1, "stop reason", "")

	events, _ := s.ListEvents(context.Background(), "/p", "t1", 0)
	found := false
	for _, ev := range events {
		if ev.EventType == "control_stop" {
			found = true
			if ev.Sequence < 1 {
				t.Errorf("expected positive sequence, got %d", ev.Sequence)
			}
			if ev.ErrorSummary != "stop reason" {
				t.Errorf("expected reason in event, got %q", ev.ErrorSummary)
			}
			if ev.SessionID != "s1" {
				t.Errorf("expected session s1, got %q", ev.SessionID)
			}
			if ev.TaskID != "t1" {
				t.Errorf("expected task t1, got %q", ev.TaskID)
			}
		}
	}
	if !found {
		t.Error("expected audit event for stop")
	}
}

func TestControlService_AuditSequenceMonotonic(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	// Stop creates audit event sequence 1
	cs.StopTask(context.Background(), "/p", "t1", 1, "", "")

	// Reset task to running (simulate resume)
	s.UpsertTask("/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 2,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	// Cancel should get sequence 2 from NextSequence
	res, _ := cs.CancelTask(context.Background(), "/p", "t1", 2, "", "")
	if !res.Accepted {
		t.Fatalf("cancel failed: %+v", res)
	}

	events, _ := s.ListEvents(context.Background(), "/p", "t1", 0)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].Sequence != 2 {
		t.Errorf("expected sequence 2, got %d", events[1].Sequence)
	}
}

func TestControlService_KillJob(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	killed := false
	mk := &mockKiller{fn: func(id string) bool {
		killed = true
		return id == "t1"
	}}
	cs.SetJobKiller(mk)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	res, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
	if !res.Accepted {
		t.Fatalf("stop failed: %+v", res)
	}
	if !killed {
		t.Error("expected Kill to be called for stop")
	}
}

func TestControlService_KillNotCalledForTerminalTask(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	killed := false
	mk := &mockKiller{fn: func(id string) bool { killed = true; return true }}
	cs.SetJobKiller(mk)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateSucceeded, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
	if killed {
		t.Error("Kill should not be called for terminal tasks")
	}
}

func TestControlService_ConcurrentAccess(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateRunning, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	var wg sync.WaitGroup
	success := 0
	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := cs.StopTask(context.Background(), "/p", "t1", 1, "", "")
			if res.Accepted {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Exactly one caller should succeed due to mutex + version CAS
	if success != 1 {
		t.Errorf("expected exactly 1 success, got %d", success)
	}
}

func TestControlService_CancelTask(t *testing.T) {
	s := NewInMemoryStore()
	cs := NewControlService(s)

	mustUpsertControl(t, s, "/p", TaskSnapshot{
		SchemaVersion: 1, TaskID: "t1", SessionID: "s1",
		State: TaskStateWaiting, Version: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
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
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	res, _ := cs.OpenTaskSession(context.Background(), "/p", "t1")
	if res.SessionID != "sess-abc" || !res.Accepted {
		t.Errorf("expected sess-abc, got %+v", res)
	}
}

// mockKiller implements JobKiller for tests.
type mockKiller struct {
	fn func(string) bool
}

func (m *mockKiller) Kill(id string) bool {
	if m.fn != nil {
		return m.fn(id)
	}
	return false
}

func mustUpsertControl(t *testing.T, s *InMemoryStore, proj string, snap TaskSnapshot) {
	t.Helper()
	if err := s.UpsertTask(proj, snap); err != nil {
		t.Fatal(err)
	}
}
