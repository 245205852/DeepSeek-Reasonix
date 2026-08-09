package sessioninbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/fileutil"
	"reasonix/internal/store"
)

func (s *Store) mutableLocked() error {
	if s.closed {
		return ErrClosed
	}
	if s.readonly {
		return ErrSchemaReadonly
	}
	return nil
}

func (s *Store) blobPath(blobName string) string {
	return filepath.Join(s.dir, blobsDirName, blobName+blobSuffix)
}

// blobNameFor returns the on-disk blob stem for a meta entry.
func blobNameFor(meta InboxItemMeta) string {
	if name := strings.TrimSpace(meta.BlobName); name != "" {
		return name
	}
	return meta.ID
}

func (s *Store) writeBlobLocked(blobName string, data []byte) error {
	if err := os.MkdirAll(filepath.Join(s.dir, blobsDirName), 0o700); err != nil {
		return fmt.Errorf("sessioninbox: blobs dir: %w", err)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := s.blobPath(blobName)
	fileutil.Crash("inbox-blob-write", path)
	if err := fileutil.AtomicWriteFileStrict(path, data, 0o600); err != nil {
		return fmt.Errorf("sessioninbox: write blob: %w", err)
	}
	fileutil.Crash("inbox-blob-rename", path)
	return nil
}

func (s *Store) readBlobLocked(blobName, wantChecksum string) (PromptEnvelope, error) {
	data, err := os.ReadFile(s.blobPath(blobName))
	if err != nil {
		return PromptEnvelope{}, fmt.Errorf("sessioninbox: read blob: %w", err)
	}
	got := sha256Hex(data)
	if wantChecksum != "" && got != wantChecksum {
		return PromptEnvelope{}, fmt.Errorf("sessioninbox: blob checksum mismatch")
	}
	var env PromptEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return PromptEnvelope{}, fmt.Errorf("sessioninbox: decode blob: %w", err)
	}
	return env, nil
}

func (s *Store) commitManifestLocked(next *manifest) error {
	if next == nil {
		return fmt.Errorf("sessioninbox: nil manifest")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("sessioninbox: mkdir: %w", err)
	}
	next.SchemaVersion = SchemaVersion
	next.RunID = s.runID
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(s.dir, manifestName)
	fileutil.Crash("inbox-manifest-write", path)
	if err := fileutil.AtomicWriteFileStrict(path, data, 0o600); err != nil {
		return fmt.Errorf("sessioninbox: write manifest: %w", err)
	}
	fileutil.Crash("inbox-manifest-commit", path)
	// Best-effort directory fsync for durability of the rename.
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	s.man = next
	return nil
}

func (s *Store) quarantineFileLocked(path, tag string) error {
	qdir := filepath.Join(s.dir, quarantineName)
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return err
	}
	base := filepath.Base(path) + "." + tag + "." + fmt.Sprintf("%d", time.Now().UnixNano())
	return os.Rename(path, filepath.Join(qdir, base))
}

func (s *Store) gcOrphansLocked() {
	bdir := filepath.Join(s.dir, blobsDirName)
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(s.man.Items))
	for _, it := range s.man.Items {
		live[blobNameFor(it)] = struct{}{}
	}
	qdir := filepath.Join(s.dir, quarantineName)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, blobSuffix) {
			_ = s.quarantineUnknownLocked(filepath.Join(bdir, name))
			continue
		}
		stem := strings.TrimSuffix(name, blobSuffix)
		if _, ok := live[stem]; ok {
			continue
		}
		// Orphan blob → quarantine (do not delete silently: crash recovery).
		_ = os.MkdirAll(qdir, 0o700)
		_ = os.Rename(filepath.Join(bdir, name), filepath.Join(qdir, name+"."+fmt.Sprintf("%d", time.Now().UnixNano())))
	}
}

// salvageOrphanBlobsLocked rebuilds uncertain meta rows from blob files after a
// corrupt-manifest quarantine. Bodies stay on disk; the user reviews before resume.
func (s *Store) salvageOrphanBlobsLocked() []InboxItemMeta {
	bdir := filepath.Join(s.dir, blobsDirName)
	entries, err := os.ReadDir(bdir)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	var out []InboxItemMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), blobSuffix) {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), blobSuffix)
		data, err := os.ReadFile(filepath.Join(bdir, e.Name()))
		if err != nil {
			continue
		}
		var env PromptEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		preview := PreviewText(firstNonEmpty(env.DisplayText, env.SubmitText, env.RawText), DefaultPreviewRunes)
		if preview == "" {
			preview = "(salvaged body)"
		}
		// The corrupt manifest no longer provides a revision-to-item mapping.
		// Use the complete blob stem as a collision-free recovered item ID.
		itemID := stem
		out = append(out, InboxItemMeta{
			ID:          itemID,
			Intent:      IntentFollowup,
			State:       StateUncertain,
			BlobName:    stem,
			CreatedAt:   now,
			UpdatedAt:   now,
			Preview:     preview,
			ByteSize:    int64(len(data)),
			Checksum:    sha256Hex(data),
			RunID:       s.runID,
			BlockReason: "salvaged after corrupt manifest",
		})
	}
	return out
}

func (s *Store) quarantineUnknownLocked(path string) error {
	qdir := filepath.Join(s.dir, quarantineName)
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(qdir, filepath.Base(path)+"."+fmt.Sprintf("%d", time.Now().UnixNano())))
}

func (s *Store) notifyLocked(snap InboxSnapshot) {
	listeners := append([]func(InboxSnapshot){}, s.listeners...)
	// Unlock is held; notify asynchronously so listeners can re-enter.
	go func() {
		for _, fn := range listeners {
			fn(snap)
		}
	}()
}

func encodeEnvelope(env PromptEnvelope) (data []byte, checksum string, size int64, err error) {
	data, err = json.Marshal(env)
	if err != nil {
		return nil, "", 0, err
	}
	return data, sha256Hex(data), int64(len(data)), nil
}

func normalizeEnvelope(env PromptEnvelope) PromptEnvelope {
	env.DisplayText = strings.TrimSpace(env.DisplayText)
	env.RawText = strings.TrimSpace(env.RawText)
	env.SubmitText = strings.TrimSpace(env.SubmitText)
	env.Format = strings.TrimSpace(env.Format)
	env.Idempotency = strings.TrimSpace(env.Idempotency)
	env.Source = strings.TrimSpace(env.Source)
	return env
}

func refSummaries(refs []RefSnapshot) []RefSummary {
	if len(refs) == 0 {
		return nil
	}
	out := make([]RefSummary, 0, len(refs))
	for _, r := range refs {
		out = append(out, RefSummary{
			Kind:    r.Kind,
			Path:    firstNonEmpty(r.DisplayPath, r.Path),
			Commit:  r.Commit,
			Bytes:   int64(len(r.Content)),
			Preview: PreviewText(string(r.Content), 40),
		})
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func agentBranchID(sessionPath string) string {
	base := filepath.Base(sessionPath)
	return strings.TrimSuffix(base, ".jsonl")
}

// RemoveDir deletes the entire inbox directory (clear/delete session).
func RemoveDir(sessionPath string) error {
	dir := store.SessionInboxDir(sessionPath)
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// MigrateDir renames the inbox directory with a session path change.
func MigrateDir(oldPath, newPath string) error {
	oldDir := store.SessionInboxDir(oldPath)
	newDir := store.SessionInboxDir(newPath)
	if oldDir == "" || newDir == "" {
		return nil
	}
	if err := os.Rename(oldDir, newDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
