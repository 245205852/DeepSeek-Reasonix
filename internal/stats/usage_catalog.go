package stats

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"reasonix/internal/config"
	"reasonix/internal/usagecatalog"
)

type usageManager struct {
	catalog atomic.Pointer[usagecatalog.Catalog]
}

var usageManagers = struct {
	sync.Mutex
	byDir map[string]*usageManager
}{byDir: map[string]*usageManager{}}

func managerForUsage(dir string) *usageManager {
	dir = strings.TrimSpace(dir)
	if dir == "" || !sameUsageDirectory(dir, config.StatsDir()) {
		return nil
	}
	usageManagers.Lock()
	manager := usageManagers.byDir[dir]
	if manager == nil {
		manager = &usageManager{}
		usageManagers.byDir[dir] = manager
		go func() {
			catalog, err := usagecatalog.Open(context.Background(), "")
			if err != nil {
				return
			}
			manager.catalog.Store(catalog)
			_ = catalog.ReconcileDir(context.Background(), dir)
		}()
	}
	usageManagers.Unlock()
	return manager
}

// The single usage catalog projects the single authoritative Reasonix stats
// directory. Test/custom writers retain the exact JSONL implementation rather
// than accidentally sharing rollups with the production cache database.
func sameUsageDirectory(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(filepath.Clean(left))
	rightAbs, rightErr := filepath.Abs(filepath.Clean(right))
	return leftErr == nil && rightErr == nil && leftAbs == rightAbs
}

// existingUsageManager returns an already-started projection without creating
// background work. Read-only commands, Query and Flush use this path so merely
// inspecting authoritative JSONL cannot make the process outlive the command.
func existingUsageManager(dir string) *usageManager {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	usageManagers.Lock()
	defer usageManagers.Unlock()
	return usageManagers.byDir[dir]
}

func usageEntry(day string, r record) usagecatalog.Entry {
	turns := 0
	if r.Turn {
		turns = 1
	}
	return usagecatalog.Entry{Day: day, Source: r.Source, ModelRef: r.ModelRef, Provider: providerOf(r.ModelRef),
		Prompt: r.Prompt, Completion: r.Completion, Reasoning: r.Reasoning, CacheHit: r.CacheHit,
		CacheMiss: r.CacheMiss, Total: r.Total, Requests: r.Requests, Turns: turns}
}
