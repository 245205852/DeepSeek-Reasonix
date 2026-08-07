package extension

import (
	"testing"

	"reasonix/internal/extensioncontract"
)

func TestRuntimePlanNoOp(t *testing.T) {
	g, err := BuildDependencyGraph([]ComponentDescriptor{
		{ID: "host", Provides: []extensioncontract.Capability{cap("reasonix", "provider", "p", "1.0.0", "sha256:p")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := DiffRuntimePlan(g, g, 1, 2)
	if !plan.IsNoOp() || plan.CacheChanged {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Unchanged) != 1 || plan.Unchanged[0] != "host" {
		t.Fatalf("unchanged = %v", plan.Unchanged)
	}
}

func TestRuntimePlanProviderOnlyChange(t *testing.T) {
	from, err := BuildDependencyGraph([]ComponentDescriptor{
		{ID: "host", Provides: []extensioncontract.Capability{cap("reasonix", "provider", "p", "1.0.0", "sha256:a")}},
		{ID: "consumer", Requires: []extensioncontract.Requirement{req("reasonix", "provider", "p", ">=1.0.0", false)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	to, err := BuildDependencyGraph([]ComponentDescriptor{
		{ID: "host", Provides: []extensioncontract.Capability{cap("reasonix", "provider", "p", "1.1.0", "sha256:b")}},
		{ID: "consumer", Requires: []extensioncontract.Requirement{req("reasonix", "provider", "p", ">=1.0.0", false)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := DiffRuntimePlan(from, to, 1, 2)
	if plan.IsNoOp() || !plan.CacheChanged {
		t.Fatal("expected reload")
	}
	// Host identity changed; consumer epoch changed → both reloaded.
	reloaded := map[ComponentID]bool{}
	for _, id := range plan.Reloaded {
		reloaded[id] = true
	}
	if !reloaded["host"] || !reloaded["consumer"] {
		t.Fatalf("reloaded = %v", plan.Reloaded)
	}
}

func TestRuntimePlanAddedRemoved(t *testing.T) {
	from, _ := BuildDependencyGraph([]ComponentDescriptor{{ID: "a"}})
	to, _ := BuildDependencyGraph([]ComponentDescriptor{{ID: "b"}})
	plan := DiffRuntimePlan(from, to, 1, 2)
	if len(plan.Added) != 1 || plan.Added[0] != "b" {
		t.Fatalf("added = %v", plan.Added)
	}
	if len(plan.Removed) != 1 || plan.Removed[0] != "a" {
		t.Fatalf("removed = %v", plan.Removed)
	}
}
