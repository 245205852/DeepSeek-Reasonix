package workspacelease

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"reasonix/internal/filelock"
)

// AcquireWriteForPath takes a repo-scoped lease when abs sits in a nested git
// repository strictly below this session's workspace. Same-repo or unknown
// targets keep the exclusive workspace lock.
func (o *Owner) AcquireWriteForPath(ctx context.Context, abs string) error {
	if o == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	repo, err := canonicalRepoForPath(abs)
	if err != nil || repo == "" || repo == o.canonical || !canonicalContains(o.canonical, repo) {
		return o.AcquireWrite(ctx)
	}
	for {
		o.mu.Lock()
		if o.exclusive {
			o.mu.Unlock()
			return nil
		}
		if o.repoHeld[repo] != nil {
			o.mu.Unlock()
			return nil
		}
		if o.acquiring {
			done := o.acquireDone
			o.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		o.acquiring = true
		o.acquireDone = make(chan struct{})
		done := o.acquireDone
		needShared := !o.shared
		o.mu.Unlock()

		var wsRel func()
		var repoRel func()
		var acqErr error
		if needShared {
			wsRel, acqErr = o.acquireMode(ctx, o.lockPath, filelock.ModeShared)
		}
		if acqErr == nil {
			repoRel, acqErr = o.acquireMode(ctx, lockFileFor(o.lockDir, repo), filelock.ModeExclusive)
			if acqErr != nil && needShared && wsRel != nil {
				wsRel()
				wsRel = nil
			}
		}

		o.mu.Lock()
		o.acquiring = false
		o.waiting = false
		if acqErr == nil {
			o.acquired = true
			if needShared && wsRel != nil {
				o.shared = true
				o.releaseSystem = wsRel
			}
			if o.repoHeld == nil {
				o.repoHeld = map[string]func(){}
			}
			o.repoHeld[repo] = repoRel
			o.leaseEpoch++
		}
		close(done)
		idle := o.releaseIfIdleLocked()
		o.mu.Unlock()
		if idle != nil {
			idle()
		}
		return acqErr
	}
}

func (o *Owner) acquireMode(ctx context.Context, path string, mode filelock.Mode) (func(), error) {
	if mode == filelock.ModeExclusive {
		release, err := filelock.TryAcquire(path)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, filelock.ErrHeld) {
			return nil, fmt.Errorf("acquire workspace write lease: %w", err)
		}
	}
	o.mu.Lock()
	o.waiting = true
	o.mu.Unlock()
	if o.onWait != nil {
		o.onWait()
	}
	release, err := filelock.AcquireMode(ctx, path, mode)
	if err != nil {
		return nil, fmt.Errorf("acquire workspace write lease: %w", err)
	}
	return release, nil
}

func canonicalRepoForPath(abs string) (string, error) {
	abs = strings.TrimSpace(abs)
	if abs == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	cur := abs
	tail := ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return CanonicalWorkspace(filepath.Join(resolved, tail))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return CanonicalWorkspace(abs)
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

func canonicalContains(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	if root == path {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
