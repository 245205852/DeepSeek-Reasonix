package boot

import (
	"context"
	"testing"

	"reasonix/internal/extension"
	"reasonix/internal/extension/protocol"
	"reasonix/internal/extension/sidecar"
)

func TestRebuildPlanNoOpCacheGuard(t *testing.T) {
	isolateConfigHome(t)
	oldRes, err := BuildRuntime(context.Background(), Options{})
	if err != nil {
		t.Fatalf("BuildRuntime: %v", err)
	}
	t.Cleanup(func() {
		if oldRes.Controller != nil {
			oldRes.Controller.Close()
		}
	})
	res, err := RebuildFrom(context.Background(), oldRes, Options{})
	if err != nil {
		t.Fatalf("RebuildFrom: %v", err)
	}
	t.Cleanup(func() {
		if res.Controller != nil {
			res.Controller.Close()
		}
	})
	if res.Plan == nil {
		t.Fatal("expected plan")
	}
	if res.Plan.IsNoOp() && res.Plan.CacheChanged {
		t.Fatal("no-op plan marked CacheChanged")
	}
	if oldRes.Snapshot != nil && res.Snapshot != nil && res.Plan.IsNoOp() {
		if oldRes.Snapshot.CacheHash() != res.Snapshot.CacheHash() {
			t.Fatalf("no-op rebuild changed CacheHash: %s -> %s", oldRes.Snapshot.CacheHash(), res.Snapshot.CacheHash())
		}
	}
}

func TestStartPackagesWithPlanEmptyHome(t *testing.T) {
	plan := &extension.RuntimePlan{
		Unchanged: []extension.ComponentID{"plugin/demo"},
	}
	session := protocol.SessionContext{SessionID: "s", WorkspaceRoot: t.TempDir(), Generation: 1}
	m, warnings, err := sidecar.StartPackagesWithPlan(context.Background(), t.TempDir(), session, nil, nil, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
	if len(m.Clients()) != 0 {
		t.Fatalf("clients = %d, want 0", len(m.Clients()))
	}
	_ = m.Close()
}
