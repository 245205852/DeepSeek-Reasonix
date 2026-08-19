// Package workspacelease serializes Delivery writers that target the same
// workspace. Readers never acquire a lease. A writer keeps a lease only while
// a write-tool hold is active; a retained background job then extends it for a
// bounded grace period so a finished conversation cannot pin the workspace.
//
// Owner is participation accounting only. Cross-process and in-process
// serialization is delegated to internal/filelock.
package workspacelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"reasonix/internal/filelock"
)

// backgroundGrace bounds how long a retained background job may keep the write
// lease after the last agent run ends. run_in_background is used for long-lived
// services (dev servers, watchers) whose job channel may never close, so an
// unbounded retention would hand one session the workspace permanently. The
// window still covers a job's initial burst of workspace writes.
const backgroundGrace = 30 * time.Second

// WaitNotice is called once when an acquisition cannot complete immediately.
// It must return quickly and must not call back into Owner.
type WaitNotice func()

// Owner is one Delivery session's re-entrant workspace lease. One Owner may be
// shared by the root agent and all of its subagents. Different sessions must
// use different Owners, even when they share a workspace.
type Owner struct {
	lockPath   string
	lockDir    string
	canonical  string
	onWait     WaitNotice
	graceAfter time.Duration

	mu            sync.Mutex
	activeRuns    int
	background    int
	acquired      bool
	exclusive     bool
	shared        bool
	acquiring     bool
	waiting       bool
	waitingScope  string
	waitingLabel  string
	toolHolds     int
	acquireDone   chan struct{}
	releaseSystem func()
	fileHeld      map[string]func()
	fileCounts    map[string]int
	fileNames     map[string]string
	// leaseEpoch changes on every acquisition so a pending grace release can
	// prove it still targets the lease it was armed for.
	leaseEpoch uint64
	graceTimer *time.Timer
}

// State is a sanitized process-local snapshot used by Desktop to explain a
// workspace conflict. Scope/Label never include home paths or PIDs — Label is
// a basename or a file count.
type State struct {
	Acquired bool
	Waiting  bool
	Scope    string
	Label    string
}

// State returns the current acquisition state without performing lease I/O.
func (o *Owner) State() State {
	if o == nil {
		return State{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	st := State{Acquired: o.acquired, Waiting: o.waiting}
	if o.waiting {
		st.Scope = o.waitingScope
		st.Label = o.waitingLabel
		if st.Scope == "" {
			st.Scope = "workspace"
		}
		return st
	}
	st.Scope, st.Label = o.holdScopeLocked()
	return st
}

func (o *Owner) holdScopeLocked() (scope, label string) {
	if o.exclusive {
		return "workspace", ""
	}
	switch len(o.fileCounts) {
	case 0:
		return "workspace", ""
	case 1:
		for key := range o.fileCounts {
			return "file", o.fileNames[key]
		}
	}
	return "files", fmt.Sprintf("%d files", len(o.fileCounts))
}

// New returns a Delivery-session lease owner for workspaceRoot. lockDir must be
// shared by Reasonix processes for cross-process protection; it is kept outside
// the workspace so acquiring a lease never dirties user files.
func New(workspaceRoot, lockDir string, onWait WaitNotice) (*Owner, error) {
	canonical, err := CanonicalWorkspace(workspaceRoot)
	if err != nil {
		return nil, err
	}
	lockDir = strings.TrimSpace(lockDir)
	if lockDir == "" {
		return nil, errors.New("workspace lease directory is unavailable")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace lease directory: %w", err)
	}
	sum := sha256.Sum256([]byte(canonical))
	key := hex.EncodeToString(sum[:])

	return &Owner{
		lockPath:   filepath.Join(lockDir, key+".lock"),
		lockDir:    lockDir,
		canonical:  canonical,
		onWait:     onWait,
		graceAfter: backgroundGrace,
	}, nil
}

func lockFileFor(lockDir, canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(lockDir, hex.EncodeToString(sum[:])+".lock")
}

// HeldKeys returns the workspace identity and any nested repo identities this
// session currently holds. Desktop uses the set to match a waiting tab to a
// local holder. Nil/empty means no write lock is held.
func (o *Owner) HeldKeys() []string {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.acquired {
		return nil
	}
	out := []string{o.canonical}
	for key := range o.fileCounts {
		if key != "" && key != o.canonical {
			out = append(out, key)
		}
	}
	return out
}

// CanonicalWorkspace returns the stable identity used to key a workspace. It
// resolves symlinks when possible and folds case on Windows, where paths are
// case-insensitive by default.
func CanonicalWorkspace(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("workspace root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	abs = filepath.Clean(abs)
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = filepath.Clean(resolved)
	} else if !os.IsNotExist(resolveErr) {
		return "", fmt.Errorf("canonicalize workspace root: %w", resolveErr)
	}
	abs = nearestGitWorktreeRoot(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(filepath.ToSlash(abs))
	}
	return abs, nil
}

// nearestGitWorktreeRoot folds a repository root and any selected directory
// beneath it into one writer domain. It intentionally detects the .git marker
// through the filesystem instead of invoking Git, so the no-Git Windows path
// keeps the same safety guarantee. Linked worktrees each have their own .git
// marker and therefore remain independent writer domains.
func nearestGitWorktreeRoot(path string) string {
	start := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		start = filepath.Dir(path)
	}
	for current := start; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
	}
}

// BeginRun registers an agent run that participates in this session. The call
// is intentionally cheap and does not acquire the write lease; read-only turns
// therefore remain fully concurrent.
func (o *Owner) BeginRun() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.activeRuns++
	// A new run may write immediately, so revoke any grace release still pending
	// from the previous run's background jobs.
	o.cancelGraceLocked()
	o.mu.Unlock()
}

// EndRun drops leftover tool holds when the last run finishes, then releases
// the flock if nothing else is retaining it.
func (o *Owner) EndRun() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.activeRuns > 0 {
		o.activeRuns--
	}
	if o.activeRuns == 0 {
		o.toolHolds = 0
	}
	release := o.releaseIfIdleLocked()
	o.mu.Unlock()
	if release != nil {
		release()
	}
}

// AcquireWrite acquires an exclusive workspace write lease. Pair with
// ReleaseWrite (or EndRun) so the flock does not outlive the write.
func (o *Owner) AcquireWrite(ctx context.Context) error {
	_, err := o.HoldWrite(ctx)
	return err
}

// HoldWrite acquires exclusive workspace write and returns a release for this
// hold. The flock drops when no holds remain, even if a Run is still active.
func (o *Owner) HoldWrite(ctx context.Context) (func(), error) {
	if o == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		o.mu.Lock()
		if o.exclusive {
			o.toolHolds++
			o.cancelGraceLocked()
			o.mu.Unlock()
			return o.releaseHold, nil
		}
		if o.acquiring {
			done := o.acquireDone
			o.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return func() {}, ctx.Err()
			}
		}
		o.acquiring = true
		o.acquireDone = make(chan struct{})
		done := o.acquireDone
		o.waitingScope = "workspace"
		o.waitingLabel = ""
		old := o.detachHoldsLocked()
		o.mu.Unlock()
		for _, rel := range old {
			rel()
		}

		release, err := o.acquire(ctx)
		o.mu.Lock()
		o.acquiring = false
		o.waiting = false
		o.waitingScope = ""
		o.waitingLabel = ""
		if err == nil {
			o.acquired = true
			o.exclusive = true
			o.shared = false
			o.releaseSystem = release
			o.toolHolds++
			o.cancelGraceLocked()
			o.leaseEpoch++
		}
		close(done)
		releaseIfIdle := o.releaseIfIdleLocked()
		o.mu.Unlock()
		if releaseIfIdle != nil {
			releaseIfIdle()
		}
		if err != nil {
			return func() {}, err
		}
		return o.releaseHold, nil
	}
}

// ReleaseWrite drops one exclusive/path hold acquired without HoldWrite.
func (o *Owner) ReleaseWrite() {
	if o == nil {
		return
	}
	o.releaseHold()
}

func (o *Owner) releaseHold() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.toolHolds > 0 {
		o.toolHolds--
	}
	release := o.releaseIfIdleLocked()
	o.mu.Unlock()
	if release != nil {
		release()
	}
}

func (o *Owner) detachHoldsLocked() []func() {
	var old []func()
	for _, rel := range o.fileHeld {
		if rel != nil {
			old = append(old, rel)
		}
	}
	if o.releaseSystem != nil {
		old = append(old, o.releaseSystem)
	}
	o.fileHeld = nil
	o.fileCounts = nil
	o.fileNames = nil
	o.releaseSystem = nil
	o.shared = false
	o.exclusive = false
	o.acquired = false
	return old
}

// RetainUntil keeps an already-acquired lease alive for a background job. It
// is a no-op when this session has not acquired the workspace, which preserves
// concurrency for background readers.
func (o *Owner) RetainUntil(done <-chan struct{}) {
	if o == nil || done == nil {
		return
	}
	o.mu.Lock()
	if !o.acquired {
		o.mu.Unlock()
		return
	}
	o.background++
	o.mu.Unlock()
	go func() {
		<-done
		o.mu.Lock()
		if o.background > 0 {
			o.background--
		}
		release := o.releaseIfIdleLocked()
		o.mu.Unlock()
		if release != nil {
			release()
		}
	}()
}

func (o *Owner) releaseIfIdleLocked() func() {
	if !o.acquired || o.acquiring || o.toolHolds > 0 {
		return nil
	}
	if o.background != 0 {
		o.armGraceLocked()
		return nil
	}
	return o.takeLeaseLocked()
}

// takeLeaseLocked detaches the system release from this Owner exactly once, so
// no two paths can release the same acquisition.
func (o *Owner) takeLeaseLocked() func() {
	o.cancelGraceLocked()
	old := o.detachHoldsLocked()
	if len(old) == 0 {
		return nil
	}
	return func() {
		for _, rel := range old {
			rel()
		}
	}
}

// armGraceLocked schedules the bounded release that keeps a resident background
// job from owning the workspace indefinitely. The callback re-checks the epoch
// and participation because Timer.Stop loses the race with an already-running
// callback, and because a run may start or the lease may be reacquired first.
func (o *Owner) armGraceLocked() {
	if o.graceAfter <= 0 || o.graceTimer != nil {
		return
	}
	epoch := o.leaseEpoch
	o.graceTimer = time.AfterFunc(o.graceAfter, func() {
		o.mu.Lock()
		if o.graceTimer == nil || o.leaseEpoch != epoch || !o.acquired || o.acquiring || o.toolHolds > 0 {
			o.mu.Unlock()
			return
		}
		release := o.takeLeaseLocked()
		o.mu.Unlock()
		if release != nil {
			release()
		}
	})
}

func (o *Owner) cancelGraceLocked() {
	if o.graceTimer != nil {
		o.graceTimer.Stop()
		o.graceTimer = nil
	}
}

// acquire tries filelock.TryAcquire, then waits via filelock.Acquire.
// Contention flips waiting=true and fires the wait notice once.
func (o *Owner) acquire(ctx context.Context) (func(), error) {
	release, err := filelock.TryAcquire(o.lockPath)
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, filelock.ErrHeld) {
		return nil, fmt.Errorf("acquire workspace write lease: %w", err)
	}

	o.mu.Lock()
	o.waiting = true
	o.mu.Unlock()
	if o.onWait != nil {
		o.onWait()
	}
	release, err = filelock.Acquire(ctx, o.lockPath)
	if err != nil {
		return nil, fmt.Errorf("acquire workspace write lease: %w", err)
	}
	return release, nil
}
