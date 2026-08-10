package taskcatalog

import (
	"context"
	"path/filepath"
	"sync"

	"reasonix/internal/taskmonitor"
)

type sharedManager struct {
	once    sync.Once
	mu      sync.RWMutex
	catalog *Catalog
	pending map[string]string
}

var shared sharedManager

func ensureShared() {
	shared.once.Do(func() {
		go func() {
			catalog, err := Open(context.Background(), "")
			if err != nil {
				return
			}
			shared.mu.Lock()
			shared.catalog = catalog
			pending := shared.pending
			shared.pending = nil
			shared.mu.Unlock()
			for root, label := range pending {
				_, _ = catalog.RegisterProject(context.Background(), root, label)
			}
		}()
	})
}

func Shared() *Catalog {
	ensureShared()
	shared.mu.RLock()
	defer shared.mu.RUnlock()
	return shared.catalog
}

func RegisterSharedProject(root, label string) string {
	ensureShared()
	key := ProjectKey(root)
	shared.mu.Lock()
	if shared.catalog == nil {
		if shared.pending == nil {
			shared.pending = map[string]string{}
		}
		shared.pending[root] = label
		shared.mu.Unlock()
		return key
	}
	catalog := shared.catalog
	shared.mu.Unlock()
	_, _ = catalog.RegisterProject(context.Background(), root, label)
	return key
}

type sharedSink struct{}

func (sharedSink) SnapshotChanged(projectRoot, taskID string) {
	catalog := sharedCatalogForNotification(projectRoot)
	if catalog != nil {
		catalog.SnapshotChanged(projectRoot, taskID)
	}
}

func (sharedSink) EventsChanged(projectRoot, taskID string) {
	catalog := sharedCatalogForNotification(projectRoot)
	if catalog != nil {
		catalog.EventsChanged(projectRoot, taskID)
	}
}

// sharedCatalogForNotification is deliberately SQLite-free. ProjectionSink is
// called after the authoritative task file lock is released, but task saves
// still must never wait for catalog I/O. A notification received while the
// catalog is opening is recovered by the pending project's initial reconcile.
func sharedCatalogForNotification(projectRoot string) *Catalog {
	ensureShared()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.catalog == nil {
		if shared.pending == nil {
			shared.pending = map[string]string{}
		}
		shared.pending[projectRoot] = filepath.Base(projectRoot)
		return nil
	}
	return shared.catalog
}

// ObservedStore remains an authoritative FileStore; only its post-commit sink
// is shared with the disposable catalog.
func ObservedStore() taskmonitor.WriteStore {
	ensureShared()
	return taskmonitor.NewObservedFileStore(filepath.Join(".reasonix", "tasks"), sharedSink{})
}
