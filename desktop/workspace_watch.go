package main

// Workspace change invalidation lives at the desktop boundary. Agent events
// are authoritative for writes performed by Reasonix, while fsnotify covers
// edits made by an IDE or an external terminal. The hub deliberately publishes
// only bounded, debounced metadata; panels decide which resources to reload.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"reasonix/internal/event"
	"reasonix/internal/fileref"
)

const (
	workspaceWatchQuiet    = 250 * time.Millisecond
	workspaceWatchMaxDirs  = 4096
	workspaceWatchMaxPaths = 512
)

type WorkspaceRevisionView struct {
	Revisions  event.WorkspaceRevision
	WatchState event.WorkspaceWatchState
}

type workspaceWatchRoot struct {
	key            string
	root           string
	gitDirs        []string
	watcher        *fsnotify.Watcher
	dirs           int
	state          event.WorkspaceWatchState
	revisions      event.WorkspaceRevision
	pending        map[string]event.WorkspacePathChange
	recentAgent    map[string]time.Time
	recentAgentAll time.Time
	allPaths       bool
	source         string
	timer          *time.Timer
	closed         bool
}

type workspaceChangeHub struct {
	app     *App
	mu      sync.Mutex
	roots   map[string]*workspaceWatchRoot
	session map[string]uint64
	closed  bool
}

func newWorkspaceChangeHub(app *App) *workspaceChangeHub {
	return &workspaceChangeHub{app: app, roots: make(map[string]*workspaceWatchRoot), session: make(map[string]uint64)}
}

func canonicalWorkspaceRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	return abs
}

func (h *workspaceChangeHub) ensureRoot(root string) string {
	key := canonicalWorkspaceRoot(root)
	if key == "" {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return key
	}
	if _, ok := h.roots[key]; ok {
		return key
	}
	r := &workspaceWatchRoot{key: key, root: key, state: event.WorkspaceWatchActive, pending: make(map[string]event.WorkspacePathChange), recentAgent: make(map[string]time.Time)}
	h.roots[key] = r
	h.startRootLocked(r)
	return key
}

func (h *workspaceChangeHub) startRootLocked(r *workspaceWatchRoot) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.state = event.WorkspaceWatchUnavailable
		return
	}
	r.watcher = watcher
	info, err := os.Stat(r.root)
	if err != nil || !info.IsDir() {
		r.state = event.WorkspaceWatchUnavailable
		_ = watcher.Close()
		r.watcher = nil
		return
	}
	h.addTreeLocked(r, r.root)
	h.addGitMetadataLocked(r)
	if r.dirs == 0 {
		r.state = event.WorkspaceWatchUnavailable
		_ = watcher.Close()
		r.watcher = nil
		return
	}
	go h.watchLoop(r)
}

func (h *workspaceChangeHub) addTreeLocked(r *workspaceWatchRoot, root string) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			r.state = event.WorkspaceWatchDegraded
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(r.root, path)
		if relErr != nil {
			return nil
		}
		if path != r.root && fileref.SkipEntry(filepath.ToSlash(rel), d.Name(), d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if r.dirs >= workspaceWatchMaxDirs {
			r.state = event.WorkspaceWatchDegraded
			return filepath.SkipDir
		}
		if err := r.watcher.Add(path); err != nil {
			r.state = event.WorkspaceWatchDegraded
			return nil
		}
		r.dirs++
		return nil
	})
}

func (h *workspaceChangeHub) addGitMetadataLocked(r *workspaceWatchRoot) {
	if len(r.gitDirs) == 0 {
		r.gitDirs = gitMetadataDirsForWorkspace(r.root)
	}
	if len(r.gitDirs) == 0 || r.watcher == nil || r.dirs >= workspaceWatchMaxDirs {
		return
	}
	// Watching the git directory itself catches HEAD/index replacement. The
	// selected metadata subtrees cover refs, reflogs, and linked worktrees while
	// avoiding the potentially enormous objects and LFS stores.
	for _, gitDir := range r.gitDirs {
		for _, rel := range []string{"", "refs", "logs", "worktrees"} {
			path := gitDir
			if rel != "" {
				path = filepath.Join(gitDir, rel)
			}
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				continue
			}
			if err := r.watcher.Add(path); err != nil {
				r.state = event.WorkspaceWatchDegraded
				continue
			}
			r.dirs++
		}
	}
}

func gitMetadataDirsForWorkspace(root string) []string {
	seen := make(map[string]struct{}, 2)
	var dirs []string
	for _, flag := range []string{"--git-dir", "--git-common-dir"} {
		cmd := exec.Command("git", "-C", root, "rev-parse", flag)
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		gitDir := strings.TrimSpace(string(out))
		if gitDir == "" {
			continue
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		gitDir = canonicalWorkspaceRoot(gitDir)
		if gitDir == "" {
			continue
		}
		if _, ok := seen[gitDir]; ok {
			continue
		}
		seen[gitDir] = struct{}{}
		dirs = append(dirs, gitDir)
	}
	return dirs
}

func (h *workspaceChangeHub) watchLoop(r *workspaceWatchRoot) {
	for {
		select {
		case ev, ok := <-r.watcher.Events:
			if !ok {
				return
			}
			h.observeFilesystem(r.key, ev)
		case _, ok := <-r.watcher.Errors:
			if !ok {
				return
			}
			h.mu.Lock()
			if !r.closed {
				r.state = event.WorkspaceWatchDegraded
				r.allPaths = true
				r.source = mergeWorkspaceSource(r.source, "filesystem")
				h.schedulePublishLocked(r)
			}
			h.mu.Unlock()
		}
	}
}

func (h *workspaceChangeHub) observeFilesystem(key string, ev fsnotify.Event) {
	path := filepath.Clean(ev.Name)
	h.mu.Lock()
	r := h.roots[key]
	if r == nil || r.closed {
		h.mu.Unlock()
		return
	}
	isGit := false
	for _, gitDir := range r.gitDirs {
		if path == gitDir || strings.HasPrefix(path, gitDir+string(filepath.Separator)) {
			isGit = true
			break
		}
	}
	if isGit {
		if ev.Op&fsnotify.Create != 0 {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() && r.dirs < workspaceWatchMaxDirs {
				if addErr := r.watcher.Add(path); addErr == nil {
					r.dirs++
				} else {
					r.state = event.WorkspaceWatchDegraded
				}
			}
		}
		r.revisions.GitMeta++
		r.revisions.WorkingTree++
		r.source = mergeWorkspaceSource(r.source, "git")
		r.allPaths = true
	} else {
		op := workspaceOp(ev.Op)
		if !r.recentAgentAll.IsZero() && time.Since(r.recentAgentAll) <= 350*time.Millisecond {
			r.recentAgentAll = time.Time{}
			h.schedulePublishLocked(r)
			h.mu.Unlock()
			return
		}
		rel, relErr := filepath.Rel(r.root, path)
		if relErr == nil {
			rel = filepath.ToSlash(rel)
			if at, ok := r.recentAgent[rel]; ok {
				if time.Since(at) <= 350*time.Millisecond {
					delete(r.recentAgent, rel)
					h.schedulePublishLocked(r)
					h.mu.Unlock()
					return
				}
				delete(r.recentAgent, rel)
			}
		}
		r.revisions.Content++
		r.revisions.WorkingTree++
		if op == "create" || op == "remove" || op == "rename" || op == "unknown" {
			r.revisions.Tree++
		}
		rel, err := filepath.Rel(r.root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			r.allPaths = true
		} else if len(r.pending) < workspaceWatchMaxPaths {
			rel = filepath.ToSlash(rel)
			r.pending[rel] = mergePathChange(r.pending[rel], event.WorkspacePathChange{Path: rel, Op: op})
		} else {
			r.allPaths = true
		}
		r.source = mergeWorkspaceSource(r.source, "filesystem")
		if ev.Op&fsnotify.Create != 0 {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				h.addTreeLocked(r, path)
			}
		}
		if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && r.watcher != nil {
			_ = r.watcher.Remove(path)
		}
	}
	h.schedulePublishLocked(r)
	h.mu.Unlock()
}

func workspaceOp(op fsnotify.Op) string {
	switch {
	case op&fsnotify.Remove != 0:
		return "remove"
	case op&fsnotify.Rename != 0:
		return "rename"
	case op&fsnotify.Create != 0:
		return "create"
	case op&fsnotify.Write != 0:
		return "write"
	default:
		return "unknown"
	}
}

func mergePathChange(old, next event.WorkspacePathChange) event.WorkspacePathChange {
	if old.Path == "" {
		return next
	}
	// A create/write/remove burst is represented by the final operation while
	// retaining rename semantics when the backend reports it.
	if next.Op == "remove" || next.Op == "rename" {
		old.Op = next.Op
	} else if old.Op != "rename" {
		old.Op = next.Op
	}
	return old
}

func mergeWorkspaceSource(old, next string) string {
	if old == "" || old == next {
		return next
	}
	return "mixed"
}

func (h *workspaceChangeHub) schedulePublishLocked(r *workspaceWatchRoot) {
	if r.timer != nil {
		return
	}
	r.timer = time.AfterFunc(workspaceWatchQuiet, func() { h.publish(r.key) })
}

func (h *workspaceChangeHub) observeAgentMutation(tabID string, paths []string, allPaths bool) {
	root := h.app.workspaceRootForTab(tabID)
	key := h.ensureRoot(root)
	if key == "" {
		return
	}
	h.mu.Lock()
	r := h.roots[key]
	if r == nil || h.closed {
		h.mu.Unlock()
		return
	}
	r.revisions.Content++
	// A writer path does not carry an atomic create-vs-overwrite result. Treat
	// the tree as possibly changed so newly-created files appear immediately;
	// the frontend still reloads only affected open parents when paths are known.
	r.revisions.Tree++
	r.revisions.WorkingTree++
	r.source = mergeWorkspaceSource(r.source, "agent")
	if allPaths || len(paths) == 0 {
		r.allPaths = true
		r.recentAgentAll = time.Now()
	} else {
		for _, raw := range paths {
			path := filepath.Clean(raw)
			if filepath.IsAbs(path) {
				if rel, err := filepath.Rel(r.root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					path = rel
				} else {
					r.allPaths = true
					continue
				}
			}
			path = filepath.ToSlash(path)
			if path == "" || path == "." {
				r.allPaths = true
				continue
			}
			if len(r.pending) >= workspaceWatchMaxPaths {
				r.allPaths = true
				break
			}
			r.pending[path] = mergePathChange(r.pending[path], event.WorkspacePathChange{Path: path, Op: "write"})
			r.recentAgent[path] = time.Now()
		}
	}
	h.session[tabID]++
	h.schedulePublishLocked(r)
	h.mu.Unlock()
}

func (h *workspaceChangeHub) publish(key string) {
	h.mu.Lock()
	r := h.roots[key]
	if r == nil || r.closed || h.closed {
		h.mu.Unlock()
		return
	}
	changes := make([]event.WorkspacePathChange, 0, len(r.pending))
	for _, c := range r.pending {
		changes = append(changes, c)
	}
	allPaths, source, revisions, state := r.allPaths, r.source, r.revisions, r.state
	r.pending = make(map[string]event.WorkspacePathChange)
	for path, at := range r.recentAgent {
		if time.Since(at) > time.Second {
			delete(r.recentAgent, path)
		}
	}
	if !r.recentAgentAll.IsZero() && time.Since(r.recentAgentAll) > time.Second {
		r.recentAgentAll = time.Time{}
	}
	r.allPaths = false
	r.source = ""
	r.timer = nil
	h.mu.Unlock()

	for _, target := range h.tabsForRoot(key) {
		targetID, sink := target.id, target.sink
		if sink == nil {
			continue
		}
		h.mu.Lock()
		revisions.Session = h.session[targetID]
		h.mu.Unlock()
		sink.Emit(event.Event{Kind: event.WorkspaceChanged, Workspace: &event.WorkspaceChangedPayload{
			Revisions: revisions, Changes: append([]event.WorkspacePathChange(nil), changes...),
			AllPaths: allPaths, Source: source, WatchState: state,
		}})
	}
}

type workspaceSinkTarget struct {
	id   string
	sink *tabEventSink
}

func (h *workspaceChangeHub) tabsForRoot(key string) []workspaceSinkTarget {
	if h.app == nil {
		return nil
	}
	globalKey := canonicalWorkspaceRoot(globalWorkspaceRoot())
	h.app.mu.RLock()
	tabs := make([]workspaceSinkTarget, 0, len(h.app.tabs))
	for id, tab := range h.app.tabs {
		if tab == nil || tab.sink == nil {
			continue
		}
		tabRoot := tab.WorkspaceRoot
		if tabRoot == "" {
			if globalKey != key {
				continue
			}
		} else if canonicalWorkspaceRoot(tabRoot) != key {
			continue
		}
		tabs = append(tabs, workspaceSinkTarget{id: id, sink: tab.sink})
	}
	h.app.mu.RUnlock()
	return tabs
}

func (h *workspaceChangeHub) revisionForTab(tabID, root string) WorkspaceRevisionView {
	key := h.ensureRoot(root)
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.roots[key]
	if r == nil {
		return WorkspaceRevisionView{WatchState: event.WorkspaceWatchUnavailable}
	}
	revisions := r.revisions
	revisions.Session = h.session[tabID]
	return WorkspaceRevisionView{Revisions: revisions, WatchState: r.state}
}

func (h *workspaceChangeHub) reconcile(tabID string) {
	root := h.app.workspaceRootForTab(tabID)
	key := h.ensureRoot(root)
	if key == "" {
		return
	}
	h.mu.Lock()
	r := h.roots[key]
	if r != nil && r.state != event.WorkspaceWatchActive {
		r.revisions.Content++
		r.revisions.Tree++
		r.revisions.WorkingTree++
		r.revisions.GitMeta++
		r.source = "reconcile"
		r.allPaths = true
		h.schedulePublishLocked(r)
	}
	h.mu.Unlock()
}

func (h *workspaceChangeHub) reconcileRoots() {
	if h == nil || h.app == nil {
		return
	}
	h.mu.Lock()
	keys := make([]string, 0, len(h.roots))
	for key := range h.roots {
		keys = append(keys, key)
	}
	h.mu.Unlock()
	for _, key := range keys {
		if len(h.tabsForRoot(key)) == 0 {
			h.mu.Lock()
			if r := h.roots[key]; r != nil {
				r.closed = true
				if r.timer != nil {
					r.timer.Stop()
				}
				if r.watcher != nil {
					_ = r.watcher.Close()
				}
				delete(h.roots, key)
			}
			h.mu.Unlock()
		}
	}
}

func (h *workspaceChangeHub) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for key, r := range h.roots {
		r.closed = true
		if r.timer != nil {
			r.timer.Stop()
		}
		if r.watcher != nil {
			_ = r.watcher.Close()
		}
		delete(h.roots, key)
	}
	h.mu.Unlock()
}

func (a *App) workspaceRootForTab(tabID string) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	tab := a.tabs[tabID]
	if tab == nil {
		for _, detached := range a.detachedSessions {
			if detached != nil && detached.ID == tabID {
				tab = detached
				break
			}
		}
	}
	if tab == nil && tabID == "" {
		tab = a.tabs[a.activeTabID]
	}
	root := ""
	if tab != nil {
		root = tab.WorkspaceRoot
	}
	a.mu.RUnlock()
	if root == "" {
		root = globalWorkspaceRoot()
	}
	return root
}

// WorkspaceRevisionForTab is a read-only reconciliation seam for panels that
// were mounted after an event, restored from a runtime, or resumed from focus.
func (a *App) WorkspaceRevisionForTab(tabID string) WorkspaceRevisionView {
	if a == nil || a.workspaceHub == nil {
		return WorkspaceRevisionView{WatchState: event.WorkspaceWatchUnavailable}
	}
	return a.workspaceHub.revisionForTab(tabID, a.workspaceRootForTab(tabID))
}

func (a *App) workspaceChangedFromTool(tabID string, tr event.Tool) {
	if a == nil || a.workspaceHub == nil || !tr.WorkspaceMutation {
		return
	}
	a.workspaceHub.observeAgentMutation(tabID, tr.WorkspacePaths, tr.WorkspaceAllPaths)
}

func (a *App) reconcileWorkspaceForTab(tabID string) {
	if a != nil && a.workspaceHub != nil {
		a.workspaceHub.reconcile(tabID)
	}
}
