package workspacelease

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestReversePathBatchesSerializeWithoutDeadlock(t *testing.T) {
	root, locks := t.TempDir(), t.TempDir()
	first, _ := New(root, locks, nil)
	second, _ := New(root, locks, nil)
	a := filepath.Join(root, "a.go")
	b := filepath.Join(root, "b.go")
	start := make(chan struct{})
	type result struct {
		release func()
		err     error
	}
	results := make(chan result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for owner, paths := range map[*Owner][]string{first: {a, b}, second: {b, a}} {
		go func() {
			<-start
			release, err := owner.HoldWriteForPaths(ctx, paths)
			results <- result{release: release, err: err}
		}()
	}
	close(start)

	one := <-results
	if one.err != nil {
		t.Fatalf("first batch acquire: %v", one.err)
	}
	one.release()
	two := <-results
	if two.err != nil {
		t.Fatalf("reverse batch acquire: %v", two.err)
	}
	two.release()
}

func TestExclusiveUpgradeKeepsActiveFileProtected(t *testing.T) {
	root, locks := t.TempDir(), t.TempDir()
	owner, _ := New(root, locks, nil)
	contender, _ := New(root, locks, nil)
	path := filepath.Join(root, "protected.go")
	releasePath, err := owner.HoldWriteForPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	upgrade := make(chan error, 1)
	go func() {
		release, err := owner.HoldWrite(context.Background())
		if err == nil {
			release()
		}
		upgrade <- err
	}()
	waitForOwnerAcquisition(t, owner)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	_, err = contender.HoldWriteForPath(ctx, path)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active file lost protection during upgrade: %v", err)
	}
	releasePath()
	select {
	case err := <-upgrade:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive upgrade did not continue after the file hold released")
	}
}

func TestUncontendedPathWriteDoesNotNotifyWait(t *testing.T) {
	var notices atomic.Int32
	owner, err := New(t.TempDir(), t.TempDir(), func() { notices.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	release, err := owner.HoldWriteForPath(context.Background(), filepath.Join(owner.canonical, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got := notices.Load(); got != 0 {
		t.Fatalf("uncontended path write emitted %d wait notices", got)
	}
}

func TestWorkspaceWriterGetsPriorityOverNewPathReader(t *testing.T) {
	root, locks := t.TempDir(), t.TempDir()
	reader, _ := New(root, locks, nil)
	writerWaiting := make(chan struct{}, 1)
	writer, _ := New(root, locks, func() { writerWaiting <- struct{}{} })
	lateReader, _ := New(root, locks, nil)
	releaseReader, err := reader.HoldWriteForPath(context.Background(), filepath.Join(root, "a.go"))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		release func()
		err     error
	}
	writerResult := make(chan result, 1)
	go func() {
		release, acquireErr := writer.HoldWrite(context.Background())
		writerResult <- result{release: release, err: acquireErr}
	}()
	select {
	case <-writerWaiting:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace writer did not report contention")
	}
	lateResult := make(chan result, 1)
	go func() {
		release, acquireErr := lateReader.HoldWriteForPath(context.Background(), filepath.Join(root, "b.go"))
		lateResult <- result{release: release, err: acquireErr}
	}()
	releaseReader()
	acquiredWriter := <-writerResult
	if acquiredWriter.err != nil {
		t.Fatal(acquiredWriter.err)
	}
	select {
	case late := <-lateResult:
		if late.release != nil {
			late.release()
		}
		t.Fatal("new path reader bypassed the waiting workspace writer")
	default:
	}
	acquiredWriter.release()
	late := <-lateResult
	if late.err != nil {
		t.Fatal(late.err)
	}
	late.release()
}

func TestPathLockFilesUseBoundedStripes(t *testing.T) {
	owner, err := New(t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < pathLockStripes*3; i++ {
		seen[owner.pathLockPath(fmt.Sprintf("file-%d", i))] = true
	}
	if len(seen) > pathLockStripes {
		t.Fatalf("path lock files = %d, want at most %d", len(seen), pathLockStripes)
	}
}

func TestNestedWorkspaceRootsSharePathStripes(t *testing.T) {
	root, locks := t.TempDir(), t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	parentOwner, _ := New(root, locks, nil)
	childOwner, _ := New(child, locks, nil)
	path := filepath.Join(child, "shared.go")
	release, err := parentOwner.HoldWriteForPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := childOwner.HoldWriteForPath(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("nested workspace bypassed the same-file stripe: %v", err)
	}
}

func TestRetainedPathHoldCanBeReused(t *testing.T) {
	owner, err := New(t.TempDir(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(owner.canonical, "retained.go")
	release, err := owner.HoldWriteForPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	owner.RetainUntil(done)
	release()
	reused, err := owner.HoldWriteForPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	reused()
	close(done)
}

func TestDarwinCaseAliasesShareFileKey(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS case-insensitive alias regression")
	}
	root := t.TempDir()
	actual := filepath.Join(root, "CaseAlias.go")
	if err := os.WriteFile(actual, []byte("package alias"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "casealias.go")
	actualKey, _, err := canonicalFileKey(actual)
	if err != nil {
		t.Fatal(err)
	}
	aliasKey, _, err := canonicalFileKey(alias)
	if err != nil {
		t.Fatal(err)
	}
	if actualKey != aliasKey {
		t.Fatalf("case aliases produced different keys: %q != %q", actualKey, aliasKey)
	}
}

func waitForOwnerAcquisition(t *testing.T, owner *Owner) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		owner.mu.Lock()
		acquiring := owner.lease.acquiring
		changed := owner.lease.changed
		owner.mu.Unlock()
		if acquiring {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("owner did not begin acquisition")
		}
	}
}
