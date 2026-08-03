package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	fileenc "reasonix/internal/fileutil/encoding"
)

// InjectFail is a test seam. When set, CommitRewind fails at the named phase
// after optionally publishing the first N files. Empty = disabled.
//
// Known phases: "publish_file", "delete_file", "conversation", "truncate", "finalize".
type InjectFail struct {
	Phase      string
	AfterFiles int // fail after successfully handling this many file targets
}

// ConversationApplier applies conversation truncation during commit and restores
// forward conversation on compensate. Implemented by the control layer.
type ConversationApplier interface {
	// ApplyConversationTruncate replaces the live message log with msgs[:boundary].
	// forward is the full pre-truncate snapshot for later restore.
	ApplyConversationTruncate(boundary int, forward []byte) error
	// RestoreConversation reinstalls the forward snapshot.
	RestoreConversation(forward []byte) error
	// TruncateCheckpoints drops checkpoints at or after turn.
	TruncateCheckpoints(fromTurn int) error
	// RestoreCheckpoints reinstalls backed-up future checkpoints.
	RestoreCheckpoints(backup []byte) error
}

// PrepareRewind builds a plan and optionally a prepared transaction without
// mutating workspace or conversation. Conflict detection uses last-owned after
// fingerprints when available.
func (s *Store) PrepareRewind(turn int, scope RewindScope, sessionRev int64, boundary int, hasBound bool) (RewindPlan, error) {
	if s == nil {
		return RewindPlan{}, fmt.Errorf("checkpoints unavailable")
	}
	plan := RewindPlan{
		PlanID:          newID("plan"),
		Turn:            turn,
		Scope:           scope,
		SessionRevision: sessionRev,
		BoundaryIndex:   boundary,
		HasBoundary:     hasBound,
		CreatedAt:       time.Now(),
		WorkspaceToken:  fmt.Sprintf("%d", s.barrier.Generation()),
	}

	s.mu.Lock()
	writers := append([]ActiveWriter(nil), s.activeWriters...)
	plan.ActiveWriters = writers
	cov, gaps, legacy, expired := s.coverageFromTurnLocked(turn)
	plan.Coverage = cov
	plan.CoverageGaps = gaps
	plan.Legacy = legacy
	plan.ExpiredFilePayload = expired
	files := s.filesFromTurnLocked(turn)
	plan.Files = files
	plan.FileCount = len(files)
	s.mu.Unlock()

	wantFiles := scope == RewindCode || scope == RewindBoth
	wantConv := scope == RewindConversation || scope == RewindBoth

	if len(writers) > 0 {
		plan.CanFiles = false
		plan.CanConversation = false
		plan.DisabledReason = "active background writer"
		for _, w := range writers {
			plan.Conflicts = append(plan.Conflicts, RewindConflict{
				Path:   "",
				Reason: ConflictBusyWriter,
			})
			_ = w
		}
		return plan, nil
	}

	if wantConv {
		if !hasBound {
			plan.CanConversation = false
			if scope == RewindConversation || scope == RewindBoth {
				plan.DisabledReason = "conversation boundary unavailable"
			}
		} else {
			plan.CanConversation = true
		}
	}

	if wantFiles {
		if cov == CoverageNone {
			plan.CanFiles = false
			plan.DisabledReason = "no file captures"
		} else if expired {
			plan.CanFiles = false
			plan.DisabledReason = "file recovery payload expired"
			plan.Conflicts = append(plan.Conflicts, RewindConflict{Reason: ConflictExpired})
		} else if legacy {
			// Legacy: files can be restored only with explicit warning; batch
			// overwrite without prompt is forbidden. Prepare still reports files
			// but CanFiles stays false for the unprompted path.
			plan.CanFiles = false
			plan.DisabledReason = "legacy checkpoint cannot verify later manual edits"
			plan.Conflicts = append(plan.Conflicts, RewindConflict{Reason: ConflictCoverageLegacy})
		} else {
			conflicts := s.precheckFiles(turn)
			plan.Conflicts = append(plan.Conflicts, conflicts...)
			plan.CanFiles = len(conflicts) == 0 && len(files) > 0
			if len(conflicts) > 0 {
				plan.DisabledReason = "file conflicts detected"
			}
		}
	}

	// both requires both sides to pass precheck.
	if scope == RewindBoth {
		if !plan.CanFiles || !plan.CanConversation {
			if plan.DisabledReason == "" {
				plan.DisabledReason = "both scope requires file and conversation precheck"
			}
		}
	}

	// Persist plan token so Commit can verify freshness.
	s.mu.Lock()
	if s.plans == nil {
		s.plans = map[string]preparedPlan{}
	}
	s.plans[plan.PlanID] = preparedPlan{plan: plan, created: time.Now()}
	// Drop stale plans older than 10 minutes.
	for id, p := range s.plans {
		if time.Since(p.created) > 10*time.Minute {
			delete(s.plans, id)
		}
	}
	s.mu.Unlock()
	return plan, nil
}

type preparedPlan struct {
	plan               RewindPlan
	created            time.Time
	previewFingerprint *Fingerprint
}

// ValidatePlanSessionRevision binds a preview to the controller's exact
// conversation revision. The controller holds its rotation gate while calling
// this and committing, so no turn can slip between validation and mutation.
func (s *Store) ValidatePlanSessionRevision(planID string, current int64) error {
	if s == nil {
		return fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prepared, ok := s.plans[planID]
	if !ok {
		return fmt.Errorf("unknown or expired plan %q", planID)
	}
	if prepared.plan.SessionRevision != current {
		return fmt.Errorf("conversation changed since preview")
	}
	return nil
}

// CommitRewind executes a previously prepared plan under exclusive barrier.
// conversation/checkpoints are applied via applier when non-nil.
func (s *Store) CommitRewind(planID string, applier ConversationApplier, inject *InjectFail) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	pp, ok := s.plans[planID]
	if ok {
		delete(s.plans, planID)
	}
	s.mu.Unlock()
	if !ok {
		return RewindResult{OK: false, Error: "unknown or expired plan"}, fmt.Errorf("unknown or expired plan %q", planID)
	}
	plan := pp.plan

	// Re-validate gate conditions before any mutation.
	if plan.Scope == RewindBoth && (!plan.CanFiles || !plan.CanConversation) {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts, Coverage: plan.Coverage}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if (plan.Scope == RewindCode || plan.Scope == RewindBoth) && !plan.CanFiles && plan.Scope != RewindConversation {
		if plan.Scope == RewindCode || plan.Scope == RewindBoth {
			return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts, Coverage: plan.Coverage}, fmt.Errorf("%s", plan.DisabledReason)
		}
	}
	if (plan.Scope == RewindConversation || plan.Scope == RewindBoth) && !plan.CanConversation {
		return RewindResult{OK: false, Error: plan.DisabledReason}, fmt.Errorf("%s", plan.DisabledReason)
	}

	// Workspace exclusive barrier.
	if err := s.barrier.EnterExclusive(); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	defer s.barrier.ExitExclusive()

	if plan.Scope == RewindCode || plan.Scope == RewindBoth {
		if plan.WorkspaceToken != fmt.Sprintf("%d", s.barrier.Generation()) {
			conflict := RewindConflict{Reason: ConflictStalePlan}
			return RewindResult{OK: false, Error: "workspace changed since preview", Conflicts: []RewindConflict{conflict}, Coverage: plan.Coverage}, fmt.Errorf("workspace changed since preview")
		}
		conflicts := s.precheckFiles(plan.Turn)
		if len(conflicts) > 0 {
			return RewindResult{OK: false, Error: "file conflicts detected", Conflicts: conflicts, Coverage: plan.Coverage}, fmt.Errorf("file conflicts detected")
		}
	}

	tx, err := s.prepareTransaction(plan, applier)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	result, err := s.commitTransaction(tx, applier, inject)
	return result, err
}

// UndoRewind reverses a committed transaction when still available.
func (s *Store) UndoRewind(transactionID string, applier ConversationApplier) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	last := s.lastUndo
	s.mu.Unlock()
	if last == nil || last.ID != transactionID || last.State != TxCommitted {
		return RewindResult{OK: false, Error: "undo not available"}, fmt.Errorf("undo not available for %q", transactionID)
	}

	if err := s.barrier.EnterExclusive(); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	defer s.barrier.ExitExclusive()

	// Precheck that current disk still matches what we published (targets' restore state).
	for _, t := range last.Targets {
		fp, err := FingerprintPath(s.root, t.AbsPath)
		if err != nil && !os.IsNotExist(err) {
			// path issues
		}
		// After commit, disk should match restore image. If it doesn't, refuse.
		if t.Action == "delete" {
			if fp.Existed {
				return RewindResult{OK: false, Error: "file changed since rewind", Conflicts: []RewindConflict{{
					Path: t.Path, Reason: ConflictManualEdit, CurrentSHA: fp.SHA256,
				}}}, fmt.Errorf("file changed since rewind: %s", t.Path)
			}
		} else {
			if !fp.Existed || (t.RestoreSHA != "" && fp.SHA256 != t.RestoreSHA) {
				return RewindResult{OK: false, Error: "file changed since rewind", Conflicts: []RewindConflict{{
					Path: t.Path, Reason: ConflictManualEdit, CurrentSHA: fp.SHA256, LastOwnedSHA: t.RestoreSHA,
				}}}, fmt.Errorf("file changed since rewind: %s", t.Path)
			}
		}
	}

	// Build inverse transaction: restore forward images.
	undo := &TransactionManifest{
		SchemaVersion:       SchemaV2,
		ID:                  newID("tx"),
		SessionID:           last.SessionID,
		WorkspaceRoot:       last.WorkspaceRoot,
		State:               TxPrepared,
		Kind:                "undo",
		Turn:                last.Turn,
		Scope:               last.Scope,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
		SessionRevision:     last.SessionRevision,
		ParentTransaction:   last.ID,
		HasBoundary:         last.HasBoundary,
		BoundaryIndex:       last.BoundaryIndex,
		ConversationForward: last.ConversationForward,
		CheckpointBackup:    last.CheckpointBackup,
		TruncateFrom:        last.TruncateFrom,
	}
	for _, t := range last.Targets {
		inv := TransactionTarget{
			Path:           t.Path,
			AbsPath:        t.AbsPath,
			RestoreExisted: t.ForwardExisted,
			RestoreMode:    t.ForwardMode,
			RestoreSHA:     t.ForwardSHA,
			RestoreBlob:    t.ForwardBlob,
			RestoreInline:  clonePayload(t.ForwardInline),
			ForwardExisted: t.RestoreExisted,
			ForwardMode:    t.RestoreMode,
			ForwardSHA:     t.RestoreSHA,
			ForwardBlob:    t.RestoreBlob,
			ForwardInline:  clonePayload(t.RestoreInline),
		}
		if t.ForwardExisted {
			inv.Action = "write"
		} else {
			inv.Action = "delete"
		}
		undo.Targets = append(undo.Targets, inv)
	}
	if err := s.persistTransaction(undo); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	// Stage publish temps for write targets.
	for i := range undo.Targets {
		t := &undo.Targets[i]
		t.PublishTmp, t.BackupPath = transactionSiblingPaths(t.AbsPath, undo.ID, i)
		if t.Action != "write" {
			t.PublishTmp = ""
			continue
		}
		data, err := s.loadBlobOrInline(t.RestoreBlob, t.RestoreInline)
		if err != nil {
			_ = s.abortTransaction(undo, err)
			return RewindResult{OK: false, Error: err.Error()}, err
		}
		mode := os.FileMode(0o644)
		if t.RestoreMode != 0 {
			mode = os.FileMode(t.RestoreMode)
		}
		if err := writePublishTemp(t.PublishTmp, data, mode); err != nil {
			_ = s.abortTransaction(undo, err)
			return RewindResult{OK: false, Error: err.Error()}, err
		}
	}
	undo.State = TxPrepared
	if err := s.persistTransaction(undo); err != nil {
		_ = s.abortTransaction(undo, err)
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	// For undo of conversation: restore forward conversation and checkpoints.
	// Commit path for undo: publish files, then restore conversation/checkpoints.
	result, err := s.commitUndoTransaction(undo, last, applier)
	return result, err
}

func (s *Store) commitUndoTransaction(undo, original *TransactionManifest, applier ConversationApplier) (RewindResult, error) {
	undo.State = TxCommitting
	undo.UpdatedAt = time.Now()
	if err := s.persistTransaction(undo); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	result := RewindResult{TransactionID: undo.ID, Coverage: CoverageComplete}
	var stages []FileStage

	// Publish files (inverse).
	for i := range undo.Targets {
		t := &undo.Targets[i]
		st := FileStage{Path: t.Path, Phase: "commit", Action: t.Action}
		t.Published = true
		undo.UpdatedAt = time.Now()
		if err := s.persistTransaction(undo); err != nil {
			t.Published = false
			st.Error = err.Error()
			stages = append(stages, st)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if err := s.publishTarget(t); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(undo, undo.Targets[:i+1], stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		undo.UpdatedAt = time.Now()
		if err := s.persistTransaction(undo); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(undo, undo.Targets[:i+1], stages, err)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		st.Phase = "done"
		stages = append(stages, st)
		if t.Action == "write" {
			result.Written = append(result.Written, t.Path)
		} else {
			result.Deleted = append(result.Deleted, t.Path)
		}
	}

	// Restore conversation and checkpoints to pre-rewind state.
	if applier != nil && len(original.ConversationForward) > 0 {
		if err := applier.RestoreConversation(original.ConversationForward); err != nil {
			err = s.failTransaction(undo, undo.Targets, stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		result.ConversationOK = true
	}
	if applier != nil && len(original.CheckpointBackup) > 0 {
		if err := applier.RestoreCheckpoints(original.CheckpointBackup); err != nil {
			// Best-effort: conversation already restored; still try file compensate
			if cerr := applier.RestoreConversation(original.ConversationForward); cerr != nil {
				slog.Warn("checkpoint: undo conversation re-restore", "err", cerr)
			}
			err = s.failTransaction(undo, undo.Targets, stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
	}

	// Also reload store from disk backup.
	if len(original.CheckpointBackup) > 0 {
		if err := s.restoreCheckpointBackup(original.CheckpointBackup); err != nil {
			slog.Warn("checkpoint: restore backup into store", "err", err)
		}
	}

	undo.State = TxCommitted
	undo.UpdatedAt = time.Now()
	if err := s.persistTransaction(undo); err != nil {
		err = errors.Join(err, s.restoreOriginalRewind(original, applier))
		err = s.failTransaction(undo, undo.Targets, stages, err)
		result.OK = false
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}

	// Mark original as undone; clear lastUndo.
	original.State = TxUndone
	original.UpdatedAt = time.Now()
	if err := s.persistTransaction(original); err != nil {
		// The committed undo manifest durably names its parent, so startup will
		// suppress the stale parent even if this secondary write failed.
		slog.Warn("checkpoint: persist original transaction as undone", "err", err)
	}
	s.mu.Lock()
	s.lastUndo = nil
	s.mu.Unlock()

	result.OK = true
	result.UndoAvailable = false
	result.Files = stages
	return result, nil
}

func (s *Store) restoreOriginalRewind(original *TransactionManifest, applier ConversationApplier) error {
	if original == nil || applier == nil {
		return nil
	}
	var err error
	if original.HasBoundary {
		err = errors.Join(err, applier.ApplyConversationTruncate(original.BoundaryIndex, original.ConversationForward))
	}
	err = errors.Join(err, applier.TruncateCheckpoints(original.TruncateFrom))
	return err
}

func (s *Store) prepareTransaction(plan RewindPlan, applier ConversationApplier) (*TransactionManifest, error) {
	tx := &TransactionManifest{
		SchemaVersion:   SchemaV2,
		ID:              newID("tx"),
		WorkspaceRoot:   s.root,
		State:           TxPrepared,
		Kind:            "rewind",
		Turn:            plan.Turn,
		Scope:           plan.Scope,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
		SessionRevision: plan.SessionRevision,
		WorkspaceToken:  plan.WorkspaceToken,
		Coverage:        plan.Coverage,
		CoverageGaps:    append([]CoverageGap(nil), plan.CoverageGaps...),
		BoundaryIndex:   plan.BoundaryIndex,
		HasBoundary:     plan.HasBoundary,
		TruncateFrom:    plan.Turn,
	}

	if plan.Scope == RewindCode || plan.Scope == RewindBoth {
		earliest := s.earliestRevisions(plan.Turn)
		// Stable order for deterministic inject tests.
		paths := make([]string, 0, len(earliest))
		for p := range earliest {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for targetIndex, p := range paths {
			rev := earliest[p]
			abs, err := safePath(s.root, p)
			if err != nil {
				return nil, err
			}
			// Capture forward image.
			fwd, gap, err := CapturePath(abs, CaptureOptions{WorkspaceRoot: s.root, ReadContent: true})
			if err != nil && gap != nil {
				return nil, fmt.Errorf("capture forward %s: %w", p, err)
			}
			t := TransactionTarget{
				Path:            p,
				AbsPath:         abs,
				RestoreExisted:  rev.Existed,
				RestoreMode:     rev.Mode,
				RestoreSHA:      rev.SHA256,
				RestoreBlob:     rev.BlobRef,
				RestoreEncoding: rev.Encoding,
				ForwardExisted:  fwd.Existed,
				ForwardMode:     fwd.Mode,
				ForwardSHA:      fwd.SHA256,
			}
			if rev.Existed {
				t.Action = "write"
				if t.RestoreBlob == "" && rev.Content == nil {
					return nil, fmt.Errorf("missing restore payload for %s", p)
				}
				// Stage publish temp. Blobs hold raw on-disk bytes; inline
				// Content is decoded text and must be re-encoded. Legacy v1
				// snapshots often omit Encoding — fall back to the current
				// file's encoding (same as the pre-v2 RestoreCode path).
				var data []byte
				if rev.BlobRef != "" {
					var lerr error
					data, lerr = s.loadRevisionBytes(rev)
					if lerr != nil {
						return nil, lerr
					}
				} else if rev.Content != nil {
					enc := fileenc.UTF8
					if rev.Encoding != nil {
						enc = *rev.Encoding
					} else if current := detectCurrentEncoding(abs); current != nil {
						enc = *current
					}
					data = fileenc.Encode(*rev.Content, enc)
				} else {
					return nil, fmt.Errorf("missing restore payload for %s", p)
				}
				mode := os.FileMode(0o644)
				if rev.Mode != 0 {
					mode = os.FileMode(rev.Mode)
				}
				if t.RestoreBlob == "" && s.blobs != nil {
					ref, err := s.blobs.Put(data)
					if err != nil {
						return nil, err
					}
					t.RestoreBlob = ref
				} else if t.RestoreBlob == "" {
					t.RestoreInline = clonePayload(data)
				}
				t.PublishTmp, t.BackupPath = transactionSiblingPaths(abs, tx.ID, targetIndex)
				if err := writePublishTemp(t.PublishTmp, data, mode); err != nil {
					return nil, err
				}
			} else {
				t.Action = "delete"
				_, t.BackupPath = transactionSiblingPaths(abs, tx.ID, targetIndex)
			}
			if fwd.Existed && s.blobs != nil {
				ref, err := s.blobs.Put(fwd.Content)
				if err != nil {
					return nil, err
				}
				t.ForwardBlob = ref
			} else if fwd.Existed {
				t.ForwardInline = clonePayload(fwd.Content)
			}
			// Backup existing file for delete path (move later at commit).
			tx.Targets = append(tx.Targets, t)
		}
	}

	if (plan.Scope == RewindConversation || plan.Scope == RewindBoth) && applier != nil {
		// Backup future checkpoints for undo.
		backup, err := s.backupCheckpointsFrom(plan.Turn)
		if err != nil {
			return nil, err
		}
		tx.CheckpointBackup = backup
	}

	if err := s.persistTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func transactionSiblingPaths(absPath, transactionID string, index int) (publish, backup string) {
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	prefix := fmt.Sprintf(".%s.reasonix-%s-%d", base, transactionID, index)
	return filepath.Join(dir, prefix+".tmp"), filepath.Join(dir, prefix+".bak")
}

func writePublishTemp(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create publish temp: %w", err)
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func (s *Store) commitTransaction(tx *TransactionManifest, applier ConversationApplier, inject *InjectFail) (RewindResult, error) {
	tx.State = TxCommitting
	tx.UpdatedAt = time.Now()
	if err := s.persistTransaction(tx); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	result := RewindResult{TransactionID: tx.ID, Coverage: tx.Coverage, CoverageGaps: append([]CoverageGap(nil), tx.CoverageGaps...)}
	stages := make([]FileStage, 0, len(tx.Targets))
	filesDone := 0

	for i := range tx.Targets {
		t := &tx.Targets[i]
		st := FileStage{Path: t.Path, Phase: "commit", Action: t.Action}
		phase := "publish_file"
		if t.Action == "delete" {
			phase = "delete_file"
		}
		if inject != nil && inject.Phase == phase && filesDone >= inject.AfterFiles {
			err := fmt.Errorf("injected failure at %s after %d files", inject.Phase, inject.AfterFiles)
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets[:i], stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		// Persist a conservative "may have published" intent before the first
		// filesystem rename. Recovery can safely compensate even if the crash
		// happened just before publish.
		t.Published = true
		tx.UpdatedAt = time.Now()
		if err := s.persistTransaction(tx); err != nil {
			t.Published = false
			st.Error = err.Error()
			stages = append(stages, st)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if err := s.publishTarget(t); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets[:i+1], stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if inject != nil && inject.Phase == "after_publish_before_progress" && filesDone >= inject.AfterFiles {
			// Deliberately leave the durable state as committing to simulate a
			// process crash at the narrowest progress-persistence window.
			err := fmt.Errorf("injected crash after publish before progress")
			result.Error = err.Error()
			result.Files = append(stages, st)
			return result, err
		}
		tx.UpdatedAt = time.Now()
		if err := s.persistTransaction(tx); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets[:i+1], stages, err)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		st.Phase = "done"
		stages = append(stages, st)
		filesDone++
		if t.Action == "write" {
			result.Written = append(result.Written, t.Path)
		} else {
			result.Deleted = append(result.Deleted, t.Path)
		}
	}
	if err := s.persistTransaction(tx); err != nil {
		err = s.failTransaction(tx, tx.Targets, stages, err)
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}

	// Conversation after files.
	if tx.Scope == RewindConversation || tx.Scope == RewindBoth {
		if inject != nil && inject.Phase == "conversation" {
			err := fmt.Errorf("injected failure at conversation")
			err = s.failTransaction(tx, tx.Targets, stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if applier != nil && tx.HasBoundary {
			// Controller supplies forward via ApplyConversationTruncate.
			if err := applier.ApplyConversationTruncate(tx.BoundaryIndex, tx.ConversationForward); err != nil {
				err = errors.Join(err, s.restoreTransactionConversation(tx, applier))
				err = s.failTransaction(tx, tx.Targets, stages, err)
				result.OK = false
				result.Error = err.Error()
				result.Files = stages
				return result, err
			}
			result.ConversationOK = true
		}
		if inject != nil && inject.Phase == "truncate" {
			err := fmt.Errorf("injected failure at truncate")
			err = errors.Join(err, s.restoreTransactionConversation(tx, applier))
			err = s.failTransaction(tx, tx.Targets, stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if applier != nil {
			if err := applier.TruncateCheckpoints(tx.TruncateFrom); err != nil {
				err = errors.Join(err, s.restoreTransactionConversation(tx, applier))
				err = s.failTransaction(tx, tx.Targets, stages, err)
				result.OK = false
				result.Error = err.Error()
				result.Files = stages
				return result, err
			}
		} else {
			s.TruncateFrom(tx.TruncateFrom)
		}
	}

	if inject != nil && inject.Phase == "finalize" {
		err := fmt.Errorf("injected failure at finalize")
		err = errors.Join(err, s.restoreTransactionConversation(tx, applier))
		err = s.failTransaction(tx, tx.Targets, stages, err)
		result.OK = false
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}

	tx.State = TxCommitted
	tx.UpdatedAt = time.Now()
	if err := s.persistTransaction(tx); err != nil {
		err = errors.Join(err, s.restoreTransactionConversation(tx, applier))
		err = s.failTransaction(tx, tx.Targets, stages, err)
		result.OK = false
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}
	s.mu.Lock()
	s.lastUndo = tx
	s.mu.Unlock()

	result.OK = true
	result.UndoAvailable = true
	result.Files = stages
	return result, nil
}

func (s *Store) restoreTransactionConversation(tx *TransactionManifest, applier ConversationApplier) error {
	if tx == nil {
		return nil
	}
	var restoreErr error
	if applier != nil {
		if len(tx.ConversationForward) > 0 {
			restoreErr = errors.Join(restoreErr, applier.RestoreConversation(tx.ConversationForward))
		}
		if len(tx.CheckpointBackup) > 0 {
			restoreErr = errors.Join(restoreErr, applier.RestoreCheckpoints(tx.CheckpointBackup))
		}
	} else if len(tx.CheckpointBackup) > 0 {
		restoreErr = errors.Join(restoreErr, s.restoreCheckpointBackup(tx.CheckpointBackup))
	}
	return restoreErr
}

// SetConversationForward attaches the pre-truncate conversation snapshot to a
// prepared transaction before commit. The controller calls this after Prepare.
func (s *Store) SetConversationForward(txID string, forward []byte) error {
	path := s.txManifestPath(txID)
	var tx TransactionManifest
	if err := readJSONFile(path, &tx); err != nil {
		// Also check in-memory last prepare path: store plans don't hold tx yet.
		// Commit builds tx fresh; controller should pass forward via Commit options.
		return err
	}
	tx.ConversationForward = forward
	tx.UpdatedAt = time.Now()
	return s.persistTransaction(&tx)
}

// CommitRewindWithForward is CommitRewind plus conversation forward payload.
func (s *Store) CommitRewindWithForward(planID string, forward []byte, applier ConversationApplier, inject *InjectFail) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	pp, ok := s.plans[planID]
	if ok {
		delete(s.plans, planID)
	}
	s.mu.Unlock()
	if !ok {
		return RewindResult{OK: false, Error: "unknown or expired plan"}, fmt.Errorf("unknown or expired plan %q", planID)
	}
	plan := pp.plan

	if plan.Scope == RewindBoth && (!plan.CanFiles || !plan.CanConversation) {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if plan.Scope == RewindCode && !plan.CanFiles {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if (plan.Scope == RewindConversation || plan.Scope == RewindBoth) && !plan.CanConversation {
		return RewindResult{OK: false, Error: plan.DisabledReason}, fmt.Errorf("%s", plan.DisabledReason)
	}

	if err := s.barrier.EnterExclusive(); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	defer s.barrier.ExitExclusive()

	if plan.Scope == RewindCode || plan.Scope == RewindBoth {
		if plan.WorkspaceToken != fmt.Sprintf("%d", s.barrier.Generation()) {
			conflict := RewindConflict{Reason: ConflictStalePlan}
			return RewindResult{OK: false, Error: "workspace changed since preview", Conflicts: []RewindConflict{conflict}, Coverage: plan.Coverage}, fmt.Errorf("workspace changed since preview")
		}
		if conflicts := s.precheckFiles(plan.Turn); len(conflicts) > 0 {
			return RewindResult{OK: false, Error: "file conflicts detected", Conflicts: conflicts}, fmt.Errorf("file conflicts detected")
		}
	}

	tx, err := s.prepareTransaction(plan, applier)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	tx.ConversationForward = forward
	if err := s.persistTransaction(tx); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	return s.commitTransaction(tx, applier, inject)
}

func (s *Store) publishTarget(t *TransactionTarget) error {
	if t.BackupPath == "" {
		return fmt.Errorf("missing transaction backup path for %s", t.Path)
	}
	if err := os.MkdirAll(filepath.Dir(t.AbsPath), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(t.BackupPath); err == nil {
		return fmt.Errorf("transaction backup already exists for %s", t.Path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(t.AbsPath); err == nil {
		if err := os.Rename(t.AbsPath, t.BackupPath); err != nil {
			return fmt.Errorf("backup %s: %w", t.Path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if t.Action == "delete" {
		return nil
	}
	if t.PublishTmp == "" {
		return fmt.Errorf("missing publish tmp for %s", t.Path)
	}
	if err := os.Rename(t.PublishTmp, t.AbsPath); err != nil {
		restoreErr := error(nil)
		if _, statErr := os.Lstat(t.BackupPath); statErr == nil {
			restoreErr = os.Rename(t.BackupPath, t.AbsPath)
		}
		return errors.Join(fmt.Errorf("publish %s: %w", t.Path, err), restoreErr)
	}
	if t.RestoreMode != 0 {
		if err := os.Chmod(t.AbsPath, os.FileMode(t.RestoreMode)); err != nil {
			return fmt.Errorf("chmod restored %s: %w", t.Path, err)
		}
	}
	return nil
}

func (s *Store) compensatePublished(targets []TransactionTarget, stages []FileStage) error {
	var first error
	for i := len(targets) - 1; i >= 0; i-- {
		t := targets[i]
		if !t.Published {
			continue
		}
		// Published is a durable intent. If the target still exactly matches its
		// forward image, the crash happened before publish and compensation is a
		// no-op. Any other unrelated state is preserved with a recovery copy.
		var err error
		cur, fpErr := FingerprintPath(s.root, t.AbsPath)
		if fpErr != nil {
			markCompensationStage(stages, t.Path, fpErr)
			if first == nil {
				first = fpErr
			}
			continue
		} else if fingerprintMatches(cur, t.ForwardExisted, t.ForwardSHA, t.ForwardMode) {
			if t.PublishTmp != "" {
				_ = os.Remove(t.PublishTmp)
			}
			markCompensationStage(stages, t.Path, nil)
			continue
		}
		if t.ForwardExisted {
			data, lerr := s.loadBlobOrInline(t.ForwardBlob, t.ForwardInline)
			if lerr != nil && t.BackupPath != "" {
				data, lerr = os.ReadFile(t.BackupPath)
			}
			if !fingerprintMatches(cur, t.RestoreExisted, t.RestoreSHA, t.RestoreMode) {
				if lerr == nil {
					suffix := t.RestoreSHA
					if len(suffix) > 8 {
						suffix = suffix[:8]
					}
					if suffix == "" {
						suffix = "unknown"
					}
					recov := t.AbsPath + ".reasonix-recovery-" + suffix
					_ = os.WriteFile(recov, data, os.FileMode(t.ForwardMode))
					err = fmt.Errorf("external modification after publish; recovery copy at %s", recov)
				} else {
					err = lerr
				}
			} else if _, backupErr := os.Lstat(t.BackupPath); backupErr == nil {
				if cur.Existed {
					err = os.Remove(t.AbsPath)
				}
				if err == nil {
					err = os.Rename(t.BackupPath, t.AbsPath)
				}
			} else if lerr != nil {
				err = lerr
			} else {
				mode := os.FileMode(0o644)
				if t.ForwardMode != 0 {
					mode = os.FileMode(t.ForwardMode)
				}
				if werr := os.MkdirAll(filepath.Dir(t.AbsPath), 0o755); werr != nil {
					err = werr
				} else {
					if cur.Existed {
						if werr := os.Remove(t.AbsPath); werr != nil {
							err = werr
						}
					}
					tmp, _ := transactionSiblingPaths(t.AbsPath, newID("compensate"), 0)
					if werr := writePublishTemp(tmp, data, mode); werr != nil {
						err = werr
					} else if err == nil {
						if werr := os.Rename(tmp, t.AbsPath); werr != nil {
							err = werr
						}
					}
				}
			}
		} else {
			// Forward did not exist — remove what we published.
			if !fingerprintMatches(cur, t.RestoreExisted, t.RestoreSHA, t.RestoreMode) {
				// External rewrite of a file we restored then someone changed —
				// for compensate of delete action inverse: leave it.
				err = fmt.Errorf("external modification; not removing %s", t.AbsPath)
			} else {
				err = os.Remove(t.AbsPath)
				if os.IsNotExist(err) {
					err = nil
				}
			}
		}
		markCompensationStage(stages, t.Path, err)
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

func fingerprintMatches(fp Fingerprint, existed bool, sha string, mode uint32) bool {
	if fp.Existed != existed {
		return false
	}
	if !existed {
		return true
	}
	if sha != "" && fp.SHA256 != sha {
		return false
	}
	return mode == 0 || fp.Mode == 0 || fp.Mode == mode
}

func markCompensationStage(stages []FileStage, path string, err error) {
	for i := range stages {
		if stages[i].Path != path {
			continue
		}
		stages[i].Compensated = err == nil
		if err != nil {
			stages[i].CompError = err.Error()
		}
	}
}

func (s *Store) failTransaction(tx *TransactionManifest, targets []TransactionTarget, stages []FileStage, cause error) error {
	compensationErr := s.compensatePublished(targets, stages)
	combined := cause
	if compensationErr != nil {
		combined = errors.Join(combined, fmt.Errorf("compensation failed: %w", compensationErr))
	}
	if abortErr := s.abortTransaction(tx, combined); abortErr != nil {
		combined = errors.Join(combined, fmt.Errorf("persist aborted transaction: %w", abortErr))
	}
	return combined
}

func (s *Store) abortTransaction(tx *TransactionManifest, cause error) error {
	tx.State = TxAborted
	tx.Error = cause.Error()
	tx.UpdatedAt = time.Now()
	return s.persistTransaction(tx)
}

func (s *Store) persistTransaction(tx *TransactionManifest) error {
	if s.dir == "" {
		return nil
	}
	return writeJSONAtomic(s.txManifestPath(tx.ID), tx)
}

func (s *Store) txDir() string {
	if s.dir == "" {
		return filepath.Join(os.TempDir(), "reasonix-ckpt-tx")
	}
	return filepath.Join(s.dir, "transactions")
}

func (s *Store) txManifestPath(id string) string {
	return filepath.Join(s.txDir(), id+".json")
}

// RecoverTransactions scans for incomplete transactions and compensates them.
// Call on store load / app startup.
func (s *Store) RecoverTransactions() []string {
	if s == nil || s.dir == "" {
		return nil
	}
	dir := s.txDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	undoneParents := map[string]bool{}
	for _, entry := range ents {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var tx TransactionManifest
		if readJSONFile(filepath.Join(dir, entry.Name()), &tx) == nil && tx.State == TxCommitted && tx.Kind == "undo" && tx.ParentTransaction != "" {
			undoneParents[tx.ParentTransaction] = true
		}
	}
	var notes []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var tx TransactionManifest
		if err := readJSONFile(filepath.Join(dir, e.Name()), &tx); err != nil {
			continue
		}
		switch tx.State {
		case TxPrepared:
			// Never published — safe to discard.
			for _, target := range tx.Targets {
				if target.PublishTmp != "" {
					_ = os.Remove(target.PublishTmp)
				}
			}
			tx.State = TxAborted
			tx.Error = "abandoned prepared transaction on recovery"
			tx.UpdatedAt = time.Now()
			_ = s.persistTransaction(&tx)
			notes = append(notes, fmt.Sprintf("aborted prepared %s", tx.ID))
		case TxCommitting:
			// Compensate published files back to forward images.
			stages := make([]FileStage, len(tx.Targets))
			for i, t := range tx.Targets {
				stages[i] = FileStage{Path: t.Path, Phase: "compensate"}
			}
			if err := s.compensatePublished(tx.Targets, stages); err != nil {
				notes = append(notes, fmt.Sprintf("compensate %s: %v", tx.ID, err))
				tx.Error = fmt.Sprintf("crash recovery compensation pending: %v", err)
				tx.UpdatedAt = time.Now()
				_ = s.persistTransaction(&tx)
			} else {
				notes = append(notes, fmt.Sprintf("compensated committing %s", tx.ID))
				tx.State = TxAborted
				tx.Error = "compensated after crash during commit"
				tx.UpdatedAt = time.Now()
				_ = s.persistTransaction(&tx)
			}
		case TxCommitted:
			if tx.Kind == "undo" || undoneParents[tx.ID] {
				continue
			}
			// Keep as last undo if newer.
			s.mu.Lock()
			if s.lastUndo == nil || s.lastUndo.UpdatedAt.Before(tx.UpdatedAt) {
				cp := tx
				s.lastUndo = &cp
			}
			s.mu.Unlock()
		}
	}
	return notes
}

func (s *Store) precheckFiles(fromTurn int) []RewindConflict {
	earliest := s.earliestRevisions(fromTurn)
	var conflicts []RewindConflict
	for p, rev := range earliest {
		abs, err := safePath(s.root, p)
		if err != nil {
			conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictPathUnsafe})
			continue
		}
		if rev.BlobRef == "" && rev.Content == nil && rev.Existed {
			if rev.SHA256 != "" && s.blobs != nil && !s.blobs.Has(rev.SHA256) && (rev.BlobRef == "" || !s.blobs.Has(rev.BlobRef)) {
				conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictMissingPayload, CheckpointSHA: rev.SHA256})
				continue
			}
			if rev.Content == nil && rev.BlobRef == "" {
				conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictMissingPayload})
				continue
			}
		}
		fp, err := FingerprintPath(s.root, abs)
		if err != nil {
			// unreadable etc.
			conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictExternalChange, CheckpointSHA: rev.SHA256})
			continue
		}
		// Prefer after fingerprint for conflict detection.
		reason := CompareIdentity(fp, rev.AfterSHA256, rev.AfterExisted, rev.AfterMode)
		if reason == ConflictCoverageLegacy {
			// Legacy handled at plan level; skip per-file for batch.
			continue
		}
		if reason != "" {
			conflicts = append(conflicts, RewindConflict{
				Path:            p,
				Reason:          reason,
				CheckpointSHA:   rev.SHA256,
				LastOwnedSHA:    rev.AfterSHA256,
				CurrentSHA:      fp.SHA256,
				CheckpointMode:  rev.Mode,
				CurrentMode:     fp.Mode,
				CurrentExisted:  fp.Existed,
				CheckpointExist: rev.Existed,
			})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return conflicts
}

func (s *Store) earliestRevisions(fromTurn int) map[string]FileRevision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.earliestRevisionsLocked(fromTurn)
}

func (s *Store) earliestRevisionsLocked(fromTurn int) map[string]FileRevision {
	earliest := map[string]FileRevision{}
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		for _, rev := range c.revisions() {
			if _, ok := earliest[rev.Path]; ok {
				continue
			}
			earliest[rev.Path] = rev
		}
	}
	return earliest
}

func (s *Store) filesFromTurnLocked(fromTurn int) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		for _, rev := range c.revisions() {
			if seen[rev.Path] {
				continue
			}
			seen[rev.Path] = true
			out = append(out, rev.Path)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) coverageFromTurnLocked(fromTurn int) (Coverage, []CoverageGap, bool, bool) {
	var gaps []CoverageGap
	legacy := false
	expired := false
	hasFiles := false
	partial := false
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		if c.SchemaVersion < SchemaV2 && c.SchemaVersion != 0 {
			legacy = true
		}
		if c.SchemaVersion == 0 {
			// v1 had no schemaVersion field
			legacy = true
		}
		if c.Coverage == CoverageLegacy || c.Legacy {
			legacy = true
		}
		if c.ExpiredFilePayload {
			expired = true
		}
		if c.Coverage == CoveragePartial {
			partial = true
		}
		gaps = append(gaps, c.CoverageGaps...)
		if len(c.revisions()) > 0 {
			hasFiles = true
		}
	}
	if legacy {
		return CoverageLegacy, append(gaps, CoverageGap{Reason: GapLegacyUnverified}), true, expired
	}
	if expired {
		return CoveragePartial, append(gaps, CoverageGap{Reason: GapExpiredPayload}), false, true
	}
	if !hasFiles {
		if len(gaps) > 0 {
			return CoverageNone, gaps, false, false
		}
		return CoverageNone, nil, false, false
	}
	if partial || len(gaps) > 0 {
		return CoveragePartial, gaps, false, false
	}
	return CoverageComplete, nil, false, false
}

func (s *Store) loadRevisionBytes(rev FileRevision) ([]byte, error) {
	if rev.BlobRef != "" && s.blobs != nil {
		return s.blobs.Get(rev.BlobRef)
	}
	if rev.Content != nil {
		return []byte(*rev.Content), nil
	}
	if rev.SHA256 != "" && s.blobs != nil && s.blobs.Has(rev.SHA256) {
		return s.blobs.Get(rev.SHA256)
	}
	return nil, fmt.Errorf("missing payload for %s", rev.Path)
}

func (s *Store) loadBlobOrInline(ref string, inline []byte) ([]byte, error) {
	if ref != "" && s.blobs != nil {
		return s.blobs.Get(ref)
	}
	if inline != nil {
		return inline, nil
	}
	return nil, fmt.Errorf("missing blob %q", ref)
}

func (s *Store) backupCheckpointsFrom(fromTurn int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var future []*Checkpoint
	for _, c := range s.all() {
		if c.Turn >= fromTurn {
			cp := *c
			future = append(future, &cp)
		}
	}
	return json.Marshal(future)
}

func (s *Store) restoreCheckpointBackup(backup []byte) error {
	var future []*Checkpoint
	if err := json.Unmarshal(backup, &future); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Merge future checkpoints back (by turn).
	byTurn := map[int]*Checkpoint{}
	for _, c := range s.done {
		byTurn[c.Turn] = c
	}
	if s.cur != nil {
		byTurn[s.cur.Turn] = s.cur
	}
	for _, c := range future {
		byTurn[c.Turn] = c
		s.persist(c)
	}
	// Rebuild done/cur: highest turn as cur if it was cur; else all in done.
	turns := make([]int, 0, len(byTurn))
	for t := range byTurn {
		turns = append(turns, t)
	}
	sort.Ints(turns)
	s.done = nil
	s.cur = nil
	for _, t := range turns {
		s.done = append(s.done, byTurn[t])
	}
	return nil
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), Digest([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))[:8])
}

// RestoreCheckpointBackupPublic reloads backed-up checkpoints after an undo.
func (s *Store) RestoreCheckpointBackupPublic(backup []byte) error {
	return s.restoreCheckpointBackup(backup)
}

// PrepareFileRevert prepares a single-file restore to the earliest session preimage.
func (s *Store) PrepareFileRevert(path string, sessionRev int64) (RewindPlan, error) {
	if s == nil {
		return RewindPlan{}, fmt.Errorf("checkpoints unavailable")
	}
	plan := RewindPlan{
		PlanID:          newID("plan"),
		Scope:           RewindCode,
		Path:            path,
		SessionRevision: sessionRev,
		CreatedAt:       time.Now(),
		WorkspaceToken:  fmt.Sprintf("%d", s.barrier.Generation()),
		Files:           []string{path},
		FileCount:       1,
	}
	state, ok := s.FileState(path)
	if !ok {
		plan.CanFiles = false
		plan.DisabledReason = "file is not session-owned"
		return plan, nil
	}
	_ = state
	abs, err := safePath(s.root, path)
	if err != nil {
		plan.CanFiles = false
		plan.DisabledReason = "path unsafe"
		plan.Conflicts = []RewindConflict{{Path: path, Reason: ConflictPathUnsafe}}
		return plan, nil
	}
	revs := s.earliestRevisions(0)
	rev, has := revs[path]
	if !has {
		for p, r := range revs {
			if ap, e := safePath(s.root, p); e == nil && ap == abs {
				rev, has = r, true
				plan.Path = p
				break
			}
		}
	}
	if !has {
		plan.CanFiles = false
		plan.DisabledReason = "file is not session-owned"
		return plan, nil
	}
	fp, fperr := FingerprintPath(s.root, abs)
	if fperr == nil {
		reason := CompareIdentity(fp, rev.AfterSHA256, rev.AfterExisted, rev.AfterMode)
		if reason != "" && reason != ConflictCoverageLegacy {
			plan.Conflicts = []RewindConflict{{
				Path: path, Reason: reason,
				CheckpointSHA: rev.SHA256, LastOwnedSHA: rev.AfterSHA256, CurrentSHA: fp.SHA256,
				CurrentExisted: fp.Existed, CheckpointExist: rev.Existed,
			}}
			plan.CanFiles = true
			plan.DisabledReason = "conflict requires explicit resolution"
		} else {
			plan.CanFiles = true
		}
	} else {
		plan.CanFiles = false
		plan.DisabledReason = "current file identity unavailable"
		plan.Conflicts = []RewindConflict{{Path: path, Reason: ConflictExternalChange}}
	}
	if rev.BlobRef == "" && rev.Content == nil && rev.Existed {
		plan.CanFiles = false
		plan.DisabledReason = "missing file payload"
		plan.Conflicts = append(plan.Conflicts, RewindConflict{Path: path, Reason: ConflictMissingPayload})
	}
	s.mu.Lock()
	if s.plans == nil {
		s.plans = map[string]preparedPlan{}
	}
	s.plans[plan.PlanID] = preparedPlan{plan: plan, created: time.Now(), previewFingerprint: &fp}
	s.mu.Unlock()
	return plan, nil
}

// CommitFileRevert commits a single-file restore.
func (s *Store) CommitFileRevert(planID string, resolution ConflictResolution) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	pp, ok := s.plans[planID]
	if ok {
		delete(s.plans, planID)
	}
	s.mu.Unlock()
	if !ok {
		return RewindResult{OK: false, Error: "unknown or expired plan"}, fmt.Errorf("unknown or expired plan")
	}
	plan := pp.plan
	if plan.Path == "" {
		return RewindResult{OK: false, Error: "not a file plan"}, fmt.Errorf("not a file plan")
	}
	if len(plan.Conflicts) > 0 && resolution != ResolveOverwriteCheckpoint {
		if resolution == ResolveKeepCurrent {
			return RewindResult{OK: true, UndoAvailable: false}, nil
		}
		return RewindResult{OK: false, Error: "conflict requires explicit resolution", Conflicts: plan.Conflicts}, fmt.Errorf("conflict requires explicit resolution")
	}
	if err := s.barrier.EnterExclusive(); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	defer s.barrier.ExitExclusive()
	if plan.WorkspaceToken != fmt.Sprintf("%d", s.barrier.Generation()) {
		conflict := RewindConflict{Path: plan.Path, Reason: ConflictStalePlan}
		return RewindResult{OK: false, Error: "workspace changed since preview", Conflicts: []RewindConflict{conflict}}, fmt.Errorf("workspace changed since preview")
	}
	absPreview, err := safePath(s.root, plan.Path)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	current, err := FingerprintPath(s.root, absPreview)
	if err != nil || pp.previewFingerprint == nil || !sameFingerprint(current, *pp.previewFingerprint) {
		conflict := RewindConflict{Path: plan.Path, Reason: ConflictStalePlan, CurrentSHA: current.SHA256, CurrentExisted: current.Existed}
		return RewindResult{OK: false, Error: "file changed since preview; preview again", Conflicts: []RewindConflict{conflict}}, fmt.Errorf("file changed since preview; preview again")
	}

	// Restore via restoreCodeLegacy for the single earliest path using turn 0.
	// Build synthetic order of one path.
	revs := s.earliestRevisions(0)
	rev, has := revs[plan.Path]
	if !has {
		abs, _ := safePath(s.root, plan.Path)
		for p, r := range revs {
			if ap, e := safePath(s.root, p); e == nil && ap == abs {
				rev, has = r, true
				plan.Path = p
				break
			}
		}
	}
	if !has {
		return RewindResult{OK: false, Error: "file is not session-owned"}, fmt.Errorf("file is not session-owned")
	}

	// Find which turn first touched this path for RestoreCode semantics:
	// restoring one file = write earliest preimage (not all files from a turn).
	abs, err := safePath(s.root, rev.Path)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	fwd, _, _ := CapturePath(abs, CaptureOptions{WorkspaceRoot: s.root, ReadContent: true})
	tx := &TransactionManifest{
		SchemaVersion: SchemaV2,
		ID:            newID("tx"),
		WorkspaceRoot: s.root,
		State:         TxPrepared,
		Kind:          "file_revert",
		Scope:         RewindCode,
		Path:          rev.Path,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	t := TransactionTarget{
		Path: rev.Path, AbsPath: abs,
		RestoreExisted: rev.Existed, RestoreMode: rev.Mode, RestoreSHA: rev.SHA256, RestoreBlob: rev.BlobRef, RestoreEncoding: rev.Encoding,
		ForwardExisted: fwd.Existed, ForwardMode: fwd.Mode, ForwardSHA: fwd.SHA256,
	}
	t.PublishTmp, t.BackupPath = transactionSiblingPaths(abs, tx.ID, 0)
	if rev.Existed {
		t.Action = "write"
		data, lerr := s.loadRevisionBytes(rev)
		if lerr != nil && rev.Content != nil {
			enc := fileenc.UTF8
			if rev.Encoding != nil {
				enc = *rev.Encoding
			} else if current := detectCurrentEncoding(abs); current != nil {
				enc = *current
			}
			data = fileenc.Encode(*rev.Content, enc)
			lerr = nil
		}
		if lerr != nil {
			return RewindResult{OK: false, Error: lerr.Error()}, lerr
		}
		mode := os.FileMode(0o644)
		if rev.Mode != 0 {
			mode = os.FileMode(rev.Mode)
		}
		if t.RestoreBlob == "" && s.blobs != nil {
			ref, perr := s.blobs.Put(data)
			if perr != nil {
				return RewindResult{OK: false, Error: perr.Error()}, perr
			}
			t.RestoreBlob = ref
		} else if t.RestoreBlob == "" {
			t.RestoreInline = clonePayload(data)
		}
		if err := writePublishTemp(t.PublishTmp, data, mode); err != nil {
			return RewindResult{OK: false, Error: err.Error()}, err
		}
	} else {
		t.Action = "delete"
		t.PublishTmp = ""
	}
	if fwd.Existed && s.blobs != nil {
		ref, perr := s.blobs.Put(fwd.Content)
		if perr != nil {
			return RewindResult{OK: false, Error: perr.Error()}, perr
		}
		t.ForwardBlob = ref
	} else if fwd.Existed {
		t.ForwardInline = clonePayload(fwd.Content)
	}
	tx.Targets = []TransactionTarget{t}
	if err := s.persistTransaction(tx); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	return s.commitTransaction(tx, nil, nil)
}

func sameFingerprint(a, b Fingerprint) bool {
	return a.Existed == b.Existed && a.IsDir == b.IsDir && a.IsSymlink == b.IsSymlink &&
		a.Nlink == b.Nlink && a.Mode == b.Mode && a.Size == b.Size && a.SHA256 == b.SHA256
}

func clonePayload(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	return out
}
