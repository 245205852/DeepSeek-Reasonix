package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"reasonix/internal/checkpoint"
	"reasonix/internal/diff"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// conversationApplier bridges checkpoint transactions to controller session state.
type conversationApplier struct {
	c *Controller
}

func (a conversationApplier) ApplyConversationTruncate(boundary int, forward []byte) error {
	c := a.c
	if c.executor == nil {
		return fmt.Errorf("executor unavailable")
	}
	s := c.executor.Session()
	msgs := s.Snapshot()
	if boundary > len(msgs) {
		return fmt.Errorf("conversation rewind unavailable: the conversation was compacted past this point")
	}
	if len(forward) == 0 {
		var err error
		forward, err = json.Marshal(msgs)
		if err != nil {
			return err
		}
	}
	s.Rewrite(msgs[:boundary])
	if err := c.SnapshotRewrite(); err != nil {
		_ = a.RestoreConversation(forward)
		return fmt.Errorf("persist conversation after rewind: %w", err)
	}
	return nil
}

func (a conversationApplier) RestoreConversation(forward []byte) error {
	c := a.c
	if c.executor == nil {
		return fmt.Errorf("executor unavailable")
	}
	var msgs []provider.Message
	if err := json.Unmarshal(forward, &msgs); err != nil {
		return err
	}
	c.executor.Session().Rewrite(msgs)
	if err := c.SnapshotRewrite(); err != nil {
		return fmt.Errorf("restore conversation: %w", err)
	}
	return nil
}

func (a conversationApplier) TruncateCheckpoints(fromTurn int) error {
	a.c.checkpoints.truncateFrom(fromTurn)
	return nil
}

func (a conversationApplier) RestoreCheckpoints(backup []byte) error {
	store := a.c.checkpoints.storeRef()
	if store == nil {
		return fmt.Errorf("checkpoints unavailable")
	}
	if err := store.RestoreCheckpointBackupPublic(backup); err != nil {
		return err
	}
	bounds := store.Bounds()
	a.c.checkpoints.mu.Lock()
	a.c.checkpoints.bound = bounds
	a.c.checkpoints.turn = store.NextTurn()
	a.c.checkpoints.mu.Unlock()
	return nil
}

// PrepareRewind validates that a rewind can proceed without mutating state.
func (c *Controller) PrepareRewind(turn int, scope RewindScope) (checkpoint.RewindPlan, error) {
	if !c.checkpoints.enabled() || c.executor == nil {
		return checkpoint.RewindPlan{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return checkpoint.RewindPlan{}, c.rewindFail(fmt.Errorf("cannot rewind while a turn is running"))
		}
		return checkpoint.RewindPlan{}, c.rewindFail(err)
	}
	// Release rotation before file precheck I/O.
	c.endRotation()

	boundary, hasBound := c.checkpoints.boundary(turn)
	store := c.checkpoints.storeRef()
	if store == nil {
		return checkpoint.RewindPlan{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if obs := c.mutationObserver; obs != nil {
		store.SetActiveWriters(obs.ActiveWriters())
	}
	rev := atomic.LoadInt64(&c.sessionRevision)
	plan, err := store.PrepareRewind(turn, checkpoint.RewindScope(scope), rev, boundary, hasBound)
	if err != nil {
		return plan, c.rewindFail(err)
	}
	if scope == RewindBoth && !plan.CanConversation {
		plan.CanFiles = false
		if plan.DisabledReason == "" {
			plan.DisabledReason = "conversation boundary unavailable"
		}
	}
	return plan, nil
}

// CommitRewind executes a prepared plan under rotation gate + mutation barrier.
func (c *Controller) CommitRewind(planID string) (checkpoint.RewindResult, error) {
	if !c.checkpoints.enabled() || c.executor == nil {
		return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("cannot rewind while a turn is running"))
		}
		return checkpoint.RewindResult{}, c.rewindFail(err)
	}
	defer c.endRotation()

	store := c.checkpoints.storeRef()
	if store == nil {
		return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := store.ValidatePlanSessionRevision(planID, atomic.LoadInt64(&c.sessionRevision)); err != nil {
		conflict := checkpoint.RewindConflict{Reason: checkpoint.ConflictStalePlan}
		return checkpoint.RewindResult{OK: false, Error: err.Error(), Conflicts: []checkpoint.RewindConflict{conflict}}, c.rewindFail(err)
	}

	forward, err := json.Marshal(c.executor.Session().Snapshot())
	if err != nil {
		return checkpoint.RewindResult{}, c.rewindFail(err)
	}

	result, err := store.CommitRewindWithForward(planID, forward, conversationApplier{c: c}, nil)
	if err != nil {
		return result, c.rewindFail(err)
	}
	if result.OK {
		if len(result.Written) > 0 || len(result.Deleted) > 0 {
			c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
				Text: fmt.Sprintf("rewound code — %d file(s) restored, %d removed", len(result.Written), len(result.Deleted))})
		}
		if result.ConversationOK {
			c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
				Text: "rewound conversation"})
		}
		atomic.AddInt64(&c.sessionRevision, 1)
	}
	return result, nil
}

// UndoRewind reverses the last committed rewind transaction when still available.
func (c *Controller) UndoRewind(transactionID string) (checkpoint.RewindResult, error) {
	if !c.checkpoints.enabled() || c.executor == nil {
		return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("cannot undo rewind while a turn is running"))
		}
		return checkpoint.RewindResult{}, c.rewindFail(err)
	}
	defer c.endRotation()

	store := c.checkpoints.storeRef()
	if store == nil {
		return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	result, err := store.UndoRewind(transactionID, conversationApplier{c: c})
	if err != nil {
		return result, c.rewindFail(err)
	}
	if result.OK {
		atomic.AddInt64(&c.sessionRevision, 1)
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: "undid last rewind"})
	}
	return result, nil
}

// PrepareFileRevert prepares a single-file restore to the session's first-touch preimage.
func (c *Controller) PrepareFileRevert(path string) (checkpoint.RewindPlan, error) {
	if !c.checkpoints.enabled() || c.executor == nil {
		return checkpoint.RewindPlan{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	store := c.checkpoints.storeRef()
	if store == nil {
		return checkpoint.RewindPlan{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	state, ok := store.FileState(path)
	if !ok {
		return checkpoint.RewindPlan{
			Path: path, CanFiles: false, DisabledReason: "file is not session-owned",
		}, nil
	}
	_ = state
	return store.PrepareFileRevert(path, atomic.LoadInt64(&c.sessionRevision))
}

// CommitFileRevert commits a single-file restore with optional conflict resolution.
func (c *Controller) CommitFileRevert(planID string, resolution checkpoint.ConflictResolution) (checkpoint.RewindResult, error) {
	if !c.checkpoints.enabled() || c.executor == nil {
		return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("cannot revert file while a turn is running"))
		}
		return checkpoint.RewindResult{}, c.rewindFail(err)
	}
	defer c.endRotation()

	store := c.checkpoints.storeRef()
	if store == nil {
		return checkpoint.RewindResult{}, c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := store.ValidatePlanSessionRevision(planID, atomic.LoadInt64(&c.sessionRevision)); err != nil {
		conflict := checkpoint.RewindConflict{Reason: checkpoint.ConflictStalePlan}
		return checkpoint.RewindResult{OK: false, Error: err.Error(), Conflicts: []checkpoint.RewindConflict{conflict}}, c.rewindFail(err)
	}
	result, err := store.CommitFileRevert(planID, resolution)
	if err != nil {
		return result, c.rewindFail(err)
	}
	if result.OK {
		atomic.AddInt64(&c.sessionRevision, 1)
	}
	return result, nil
}

// Rewind is the compatibility wrapper used by CLI and existing desktop paths.
// Conversation failures never leave files half-applied for both-scope: files are
// captured first, restored second, and conversation is persisted last with full
// compensation on failure.
func (c *Controller) Rewind(turn int, scope RewindScope) error {
	if !c.checkpoints.enabled() || c.executor == nil {
		return c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return c.rewindFail(fmt.Errorf("cannot rewind while a turn is running"))
		}
		return c.rewindFail(err)
	}
	defer c.endRotation()

	boundary, hasBound := c.checkpoints.boundary(turn)
	return c.rewindLocked(turn, scope, boundary, hasBound)
}

func (c *Controller) rewindLocked(turn int, scope RewindScope, boundary int, hasBound bool) error {
	// Conversation precheck first for both/conversation.
	var msgs []provider.Message
	var forward []byte
	if scope == RewindConversation || scope == RewindBoth {
		if !hasBound {
			return c.rewindFail(fmt.Errorf("conversation rewind unavailable for turn %d (resumed session)", turn))
		}
		msgs = c.executor.Session().Snapshot()
		if boundary > len(msgs) {
			return c.rewindFail(fmt.Errorf("conversation rewind unavailable for turn %d: the conversation was compacted past this point", turn))
		}
		var err error
		forward, err = json.Marshal(msgs)
		if err != nil {
			return c.rewindFail(err)
		}
	}

	type fileFwd struct {
		abs     string
		existed bool
		data    []byte
		mode    os.FileMode
	}
	var fwds []fileFwd

	if scope == RewindCode || scope == RewindBoth {
		// Snapshot current file contents for paths that will be restored.
		store := c.checkpoints.storeRef()
		if store != nil {
			plan, _ := store.PrepareRewind(turn, checkpoint.RewindCode, atomic.LoadInt64(&c.sessionRevision), boundary, hasBound)
			for _, p := range plan.Files {
				abs, err := safeWorkspacePath(c.workspaceRoot, p)
				if err != nil {
					continue
				}
				f := fileFwd{abs: abs}
				if st, err := os.Lstat(abs); err == nil {
					f.existed = true
					f.mode = st.Mode().Perm()
					if data, rerr := os.ReadFile(abs); rerr == nil {
						f.data = data
					}
				}
				fwds = append(fwds, f)
			}
			// Drop the plan so it doesn't linger.
			_ = plan
		}
		written, deleted, err := c.checkpoints.restoreCode(turn)
		if err != nil {
			return c.rewindFail(fmt.Errorf("rewind code: %w", err))
		}
		if len(written) > 0 || len(deleted) > 0 {
			c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
				Text: fmt.Sprintf("rewound code to turn %d — %d file(s) restored, %d removed", turn, len(written), len(deleted))})
		}
	}

	if scope == RewindConversation || scope == RewindBoth {
		s := c.executor.Session()
		s.Rewrite(msgs[:boundary])
		c.checkpoints.truncateFrom(turn)
		if err := c.SnapshotRewrite(); err != nil {
			// Compensate conversation + files.
			s.Rewrite(msgs)
			_ = c.SnapshotRewrite()
			for i := len(fwds) - 1; i >= 0; i-- {
				f := fwds[i]
				if f.existed {
					_ = os.WriteFile(f.abs, f.data, f.mode)
				} else {
					_ = os.Remove(f.abs)
				}
			}
			return c.rewindFail(fmt.Errorf("persist conversation after rewind: %w", err))
		}
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
			Text: fmt.Sprintf("rewound conversation to turn %d", turn)})
		_ = forward
	}
	return nil
}

func safeWorkspacePath(root, p string) (string, error) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	if root != "" {
		r := filepath.Clean(root)
		rel, err := filepath.Rel(r, abs)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("path escapes workspace")
		}
	}
	return abs, nil
}

// wireMutationObserver installs the v2 observer on the executor.
func (c *Controller) wireMutationObserver() {
	store := c.checkpoints.storeRef()
	if store == nil || c.executor == nil {
		return
	}
	obs := checkpoint.NewMutationObserver(checkpoint.ObserverOptions{
		Store:    store,
		WriterID: "root",
	})
	c.mutationObserver = obs
	c.executor.SetMutationObserver(obs)
	// Keep legacy pre-edit hook as a secondary path when observer is absent on
	// a cloned agent; with observer set, BeforeMutation is preferred.
	c.executor.SetPreEditHook(func(ch diff.Change) {
		if c.mutationObserver != nil {
			c.mutationObserver.BeforeMutationFromChange(ch, "legacy_hook")
			return
		}
		c.checkpoints.snapshot(ch)
	})
}
