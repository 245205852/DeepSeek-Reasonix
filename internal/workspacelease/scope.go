package workspacelease

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"reasonix/internal/filelock"
)

// AcquireWriteForPath takes a file-scoped lease (shared workspace + exclusive
// file). Different files may proceed in parallel, including inside one git
// repo. Pair with ReleaseWrite or use HoldWriteForPath.
func (o *Owner) AcquireWriteForPath(ctx context.Context, abs string) error {
	_, err := o.HoldWriteForPath(ctx, abs)
	return err
}

// HoldWriteForPath acquires a file-scoped write hold and returns its release.
func (o *Owner) HoldWriteForPath(ctx context.Context, abs string) (func(), error) {
	if o == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	fileKey, display, err := canonicalFileKey(abs)
	if err != nil || fileKey == "" || !canonicalContains(o.canonical, fileKey) {
		return o.HoldWrite(ctx)
	}
	for {
		o.mu.Lock()
		if o.exclusive {
			o.toolHolds++
			o.cancelGraceLocked()
			o.mu.Unlock()
			return o.releaseHold, nil
		}
		if o.fileCounts[fileKey] > 0 {
			o.fileCounts[fileKey]++
			o.toolHolds++
			o.cancelGraceLocked()
			o.mu.Unlock()
			return func() { o.dropFileHold(fileKey) }, nil
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
		needShared := !o.shared
		o.waitingScope = "file"
		o.waitingLabel = display
		o.mu.Unlock()

		var wsRel func()
		var fileRel func()
		var acqErr error
		if needShared {
			wsRel, acqErr = o.acquireMode(ctx, o.lockPath, filelock.ModeShared)
		}
		if acqErr == nil {
			fileRel, acqErr = o.acquireMode(ctx, lockFileFor(o.lockDir, "file:"+fileKey), filelock.ModeExclusive)
			if acqErr != nil && needShared && wsRel != nil {
				wsRel()
				wsRel = nil
			}
		}

		o.mu.Lock()
		o.acquiring = false
		o.waiting = false
		o.waitingScope = ""
		o.waitingLabel = ""
		if acqErr == nil {
			o.acquired = true
			if needShared && wsRel != nil {
				o.shared = true
				o.releaseSystem = wsRel
			}
			if o.fileHeld == nil {
				o.fileHeld = map[string]func(){}
				o.fileCounts = map[string]int{}
				o.fileNames = map[string]string{}
			}
			o.fileHeld[fileKey] = fileRel
			o.fileCounts[fileKey]++
			o.fileNames[fileKey] = display
			o.toolHolds++
			o.cancelGraceLocked()
			o.leaseEpoch++
		}
		close(done)
		idle := o.releaseIfIdleLocked()
		o.mu.Unlock()
		if idle != nil {
			idle()
		}
		if acqErr != nil {
			return func() {}, acqErr
		}
		return func() { o.dropFileHold(fileKey) }, nil
	}
}

func (o *Owner) dropFileHold(fileKey string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.fileCounts[fileKey] > 0 {
		o.fileCounts[fileKey]--
	}
	if o.fileCounts[fileKey] == 0 {
		if rel := o.fileHeld[fileKey]; rel != nil {
			delete(o.fileHeld, fileKey)
			delete(o.fileCounts, fileKey)
			delete(o.fileNames, fileKey)
			o.mu.Unlock()
			rel()
			o.mu.Lock()
		}
	}
	if o.toolHolds > 0 {
		o.toolHolds--
	}
	release := o.releaseIfIdleLocked()
	o.mu.Unlock()
	if release != nil {
		release()
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

func canonicalFileKey(abs string) (key, display string, err error) {
	abs = strings.TrimSpace(abs)
	if abs == "" {
		return "", "", errors.New("path is empty")
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	cur := abs
	tail := ""
	for {
		if resolved, resErr := filepath.EvalSymlinks(cur); resErr == nil {
			abs = filepath.Join(resolved, tail)
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
	display = filepath.Base(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(filepath.ToSlash(abs))
	}
	return abs, display, nil
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
