package main

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// checkpoint is one workspace snapshot taken during a run, graded offline by
// the hidden grader after the run ends. Pass answers the question no final
// grade can: from which moment was the workspace already a correct answer.
type checkpoint struct {
	Seq       int   `json:"seq"`
	ElapsedMs int64 `json:"elapsed_ms"`
	Pass      bool  `json:"pass"`

	dir string
}

// snapshotter polls a running task's workdir and copies it on every observed
// content change. Torn copies of files mid-write are acceptable: they grade
// as failures, which is the truthful state of that instant.
type snapshotter struct {
	src, dst string
	start    time.Time
	stop     chan struct{}
	done     chan struct{}
	taken    []checkpoint
	lastSig  uint64
}

const snapshotPollInterval = 300 * time.Millisecond

func startSnapshotter(src, dst string, start time.Time) *snapshotter {
	s := &snapshotter{src: src, dst: dst, start: start, stop: make(chan struct{}), done: make(chan struct{})}
	s.lastSig = dirSignature(src)
	go s.run()
	return s
}

func (s *snapshotter) run() {
	defer close(s.done)
	ticker := time.NewTicker(snapshotPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.snapshotIfChanged()
		}
	}
}

func (s *snapshotter) snapshotIfChanged() {
	sig := dirSignature(s.src)
	if sig == s.lastSig {
		return
	}
	s.lastSig = sig
	elapsed := time.Since(s.start).Milliseconds()
	dir := filepath.Join(s.dst, fmt.Sprintf("%03d-%dms", len(s.taken)+1, elapsed))
	if err := copyDir(s.src, dir); err != nil {
		return
	}
	os.Remove(filepath.Join(dir, ".run-metrics.json"))
	s.taken = append(s.taken, checkpoint{Seq: len(s.taken) + 1, ElapsedMs: elapsed, dir: dir})
}

// halt stops polling, takes one final snapshot if the tail changed after the
// last tick, and returns everything captured.
func (s *snapshotter) halt() []checkpoint {
	close(s.stop)
	<-s.done
	s.snapshotIfChanged()
	return s.taken
}

// dirSignature hashes the workdir's shape (path, size, mtime). The agent's
// metrics sidecar updates every turn without touching the workspace, so it
// is excluded — as are bytecode caches, which no grader reads.
func dirSignature(root string) uint64 {
	h := fnv.New64a()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".run-metrics.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(h, "%s|%d|%d\n", rel, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return h.Sum64()
}

// gradeCheckpoints runs the hidden grader over every snapshot, oldest first,
// after the agent is gone — the agent never sees a verdict.
func gradeCheckpoints(checkpoints []checkpoint, taskDir string) []checkpoint {
	for i := range checkpoints {
		checkpoints[i].Pass = grade(checkpoints[i].dir, taskDir)
	}
	return checkpoints
}

// firstCorrect returns the elapsed ms of the earliest passing snapshot (0 if
// none) and whether a passing state was later broken (a snapshot passed but
// the final workspace failed).
func firstCorrect(checkpoints []checkpoint, finalPassed bool) (firstMs int64, solvedThenBroken bool) {
	anyPassed := false
	for _, cp := range checkpoints {
		if cp.Pass {
			anyPassed = true
			if firstMs == 0 {
				firstMs = cp.ElapsedMs
			}
		}
	}
	return firstMs, anyPassed && !finalPassed
}
