package sessioninbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"reasonix/internal/store"
)

const (
	manifestName   = "manifest.json"
	blobsDirName   = "blobs"
	quarantineName = "quarantine"
	blobSuffix     = ".json"
)

// Store is the transactional durable inbox for one session path.
// Disk I/O runs under store.mu only; callers must not hold Controller locks.
type Store struct {
	mu       sync.Mutex
	dir      string
	session  string // session transcript path
	runID    string
	limits   Limits
	man      *manifest
	readonly bool
	closed   bool
	// listeners receive revision bumps after durable commits (non-blocking).
	listeners []func(InboxSnapshot)
}

// Open binds a Store to the session's inbox directory. Missing dirs are created
// lazily on first write. Cross-process recovery marks uncertain items and pauses.
func Open(sessionPath string, limits Limits) (*Store, error) {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return nil, fmt.Errorf("sessioninbox: empty session path")
	}
	dir := store.SessionInboxDir(sessionPath)
	s := &Store{
		dir:     dir,
		session: sessionPath,
		runID:   ProcessRunID(),
		limits:  limits.withDefaults(),
		man:     emptyManifest(ProcessRunID()),
	}
	if err := s.loadOrInit(); err != nil {
		return nil, err
	}
	return s, nil
}

// Dir returns the on-disk inbox directory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// SessionPath returns the bound session transcript path.
func (s *Store) SessionPath() string {
	if s == nil {
		return ""
	}
	return s.session
}

// Rebind moves the store to a new session path without copying future work
// (used after rename migration that already relocated the directory).
func (s *Store) Rebind(sessionPath string) error {
	if s == nil {
		return ErrClosed
	}
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return fmt.Errorf("sessioninbox: empty session path")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.session = sessionPath
	s.dir = store.SessionInboxDir(sessionPath)
	return s.loadOrInitLocked()
}

// Close seals the store. Further mutations fail with ErrClosed.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// OnChange registers a non-blocking snapshot listener.
func (s *Store) OnChange(fn func(InboxSnapshot)) {
	if s == nil || fn == nil {
		return
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, fn)
	s.mu.Unlock()
}

func (s *Store) loadOrInit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadOrInitLocked()
}

func (s *Store) loadOrInitLocked() error {
	path := filepath.Join(s.dir, manifestName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.man = emptyManifest(s.runID)
		s.readonly = false
		return nil
	}
	if err != nil {
		return fmt.Errorf("sessioninbox: read manifest: %w", err)
	}
	man, err := decodeManifest(data)
	if err != nil {
		// Corrupt manifest → quarantine, salvage orphan blobs as uncertain
		// items, pause for user inspection. Never present "0 recovered".
		_ = s.quarantineFileLocked(path, "manifest-corrupt")
		salvaged := s.salvageOrphanBlobsLocked()
		s.man = emptyManifest(s.runID)
		s.man.Paused = true
		s.man.Recovered = true
		s.man.RecoveredN = len(salvaged)
		s.man.Items = salvaged
		return s.commitManifestLocked(s.man)
	}
	if man.SchemaVersion > SchemaVersion {
		s.man = man
		s.readonly = true
		s.man.Paused = true
		return nil
	}
	if man.SchemaVersion == 0 {
		man.SchemaVersion = SchemaVersion
	}
	// Cross-process recovery: another run left in-flight items.
	recovered := 0
	if man.RunID != "" && man.RunID != s.runID {
		for i := range man.Items {
			switch man.Items[i].State {
			case StateRunning, StateSteerAccepted, StateSteerConsumed:
				man.Items[i].State = StateUncertain
				man.Items[i].UpdatedAt = time.Now().UTC()
				recovered++
			case StateQueued, StateBlocked, StateUncertain:
				recovered++
			}
		}
		if recovered > 0 || len(man.Items) > 0 {
			man.Paused = true
			man.Recovered = true
			man.RecoveredN = recovered
		}
	}
	man.RunID = s.runID
	s.man = man
	s.readonly = false
	if recovered > 0 || man.Recovered {
		return s.commitManifestLocked(man)
	}
	// GC orphan blobs without holding callers longer than needed.
	s.gcOrphansLocked()
	return nil
}

// Snapshot returns a copy of current metadata.
func (s *Store) Snapshot() InboxSnapshot {
	if s == nil {
		return InboxSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() InboxSnapshot {
	m := s.man
	if m == nil {
		m = emptyManifest(s.runID)
	}
	items := append([]InboxItemMeta(nil), m.Items...)
	return InboxSnapshot{
		SchemaVersion: m.SchemaVersion,
		Revision:      m.Revision,
		Paused:        m.Paused,
		Recovered:     m.Recovered,
		RecoveredN:    m.RecoveredN,
		Readonly:      s.readonly,
		RunID:         m.RunID,
		Items:         items,
		Capacity: Capacity{
			Items:        len(items),
			MaxItems:     s.limits.MaxItems,
			Bytes:        m.totalBytes(),
			MaxBytes:     s.limits.MaxTotalBytes,
			MaxItemBytes: s.limits.MaxItemBytes,
		},
	}
}

// Enqueue durably appends an item. Only returns a receipt after blob+manifest
// commit succeed. Idempotent keys return the original item.
func (s *Store) Enqueue(req EnqueueRequest) (InboxReceipt, error) {
	if s == nil {
		return InboxReceipt{}, ErrClosed
	}
	env := normalizeEnvelope(req.Envelope)
	if strings.TrimSpace(env.SubmitText) == "" && strings.TrimSpace(env.DisplayText) == "" && strings.TrimSpace(env.RawText) == "" {
		return InboxReceipt{}, ErrEmpty
	}
	if env.SubmitText == "" {
		env.SubmitText = firstNonEmpty(env.RawText, env.DisplayText)
	}
	if env.DisplayText == "" {
		env.DisplayText = env.SubmitText
	}
	if env.RawText == "" {
		env.RawText = env.SubmitText
	}
	intent := req.Intent
	if intent != IntentSteer {
		intent = IntentFollowup
	}
	idem := strings.TrimSpace(firstNonEmpty(req.Idempotency, env.Idempotency))
	source := strings.TrimSpace(firstNonEmpty(req.Source, env.Source))

	blobBytes, checksum, byteSize, err := encodeEnvelope(env)
	if err != nil {
		return InboxReceipt{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return InboxReceipt{}, ErrClosed
	}
	if s.readonly {
		return InboxReceipt{}, ErrSchemaReadonly
	}
	if idem != "" {
		if id, ok := s.man.Idempotency[idem]; ok {
			if it, found := s.man.item(id); found {
				snap := s.snapshotLocked()
				return InboxReceipt{
					ItemID:      it.ID,
					Disposition: DispositionIdempotentHit,
					Position:    s.man.indexOf(it.ID) + 1,
					Paused:      s.man.Paused,
					Capacity:    snap.Capacity,
					Idempotent:  true,
				}, nil
			}
		}
	}
	if byteSize > s.limits.MaxItemBytes {
		return InboxReceipt{}, ErrItemTooLarge
	}
	if len(s.man.Items) >= s.limits.MaxItems {
		return InboxReceipt{}, ErrCapacityItems
	}
	if s.man.totalBytes()+byteSize > s.limits.MaxTotalBytes {
		return InboxReceipt{}, ErrCapacityBytes
	}

	id := newRandomID()
	blobName := id
	now := time.Now().UTC()
	meta := InboxItemMeta{
		ID:          id,
		SessionID:   firstNonEmpty(req.SessionID, agentBranchID(s.session)),
		Intent:      intent,
		State:       StateQueued,
		Revision:    s.man.Revision + 1,
		BlobName:    blobName,
		Source:      source,
		CreatedAt:   now,
		UpdatedAt:   now,
		Preview:     PreviewText(env.DisplayText, DefaultPreviewRunes),
		ByteSize:    byteSize,
		Checksum:    checksum,
		Idempotency: idem,
		Refs:        refSummaries(env.Refs),
		RunID:       s.runID,
	}

	// Transaction: write blob → commit manifest → receipt.
	if err := s.writeBlobLocked(blobName, blobBytes); err != nil {
		return InboxReceipt{}, err
	}
	next := s.man.clone()
	next.Items = append(next.Items, meta)
	if idem != "" {
		if next.Idempotency == nil {
			next.Idempotency = map[string]string{}
		}
		next.Idempotency[idem] = id
	}
	if err := s.commitManifestLocked(next); err != nil {
		_ = os.Remove(s.blobPath(blobName))
		return InboxReceipt{}, err
	}
	snap := s.snapshotLocked()
	s.notifyLocked(snap)
	return InboxReceipt{
		ItemID:      id,
		Disposition: DispositionQueuedFollowup,
		Position:    len(next.Items),
		Paused:      next.Paused,
		Capacity:    snap.Capacity,
	}, nil
}

// ReadItem loads a full PromptEnvelope by ID.
func (s *Store) ReadItem(id string) (InboxItemMeta, PromptEnvelope, error) {
	if s == nil {
		return InboxItemMeta{}, PromptEnvelope{}, ErrClosed
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return InboxItemMeta{}, PromptEnvelope{}, ErrClosed
	}
	meta, ok := s.man.item(id)
	if !ok {
		return InboxItemMeta{}, PromptEnvelope{}, ErrNotFound
	}
	env, err := s.readBlobLocked(blobNameFor(meta), meta.Checksum)
	if err != nil {
		return meta, PromptEnvelope{}, err
	}
	return meta, env, nil
}

// UpdateItem rewrites the envelope and re-freezes refs. State stays unless
// blocked items are refreshed successfully.
//
// Transaction order (crash-safe): write a NEW immutable blob under a unique
// revision name, switch the manifest pointer, then delete the old blob. A
// crash between blob write and manifest leaves an orphan (GC'd); a crash
// after manifest never leaves a checksum pointing at missing/wrong content.
func (s *Store) UpdateItem(id string, env PromptEnvelope) (InboxItemMeta, error) {
	if s == nil {
		return InboxItemMeta{}, ErrClosed
	}
	id = strings.TrimSpace(id)
	env = normalizeEnvelope(env)
	if strings.TrimSpace(env.SubmitText) == "" {
		return InboxItemMeta{}, ErrEmpty
	}
	blobBytes, checksum, byteSize, err := encodeEnvelope(env)
	if err != nil {
		return InboxItemMeta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mutableLocked(); err != nil {
		return InboxItemMeta{}, err
	}
	meta, ok := s.man.item(id)
	if !ok {
		return InboxItemMeta{}, ErrNotFound
	}
	if byteSize > s.limits.MaxItemBytes {
		return InboxItemMeta{}, ErrItemTooLarge
	}
	delta := byteSize - meta.ByteSize
	if s.man.totalBytes()+delta > s.limits.MaxTotalBytes {
		return InboxItemMeta{}, ErrCapacityBytes
	}
	oldBlob := blobNameFor(meta)
	newBlob := id + "." + newRandomID()
	if err := s.writeBlobLocked(newBlob, blobBytes); err != nil {
		return InboxItemMeta{}, err
	}
	next := s.man.clone()
	i := next.indexOf(id)
	next.Items[i].BlobName = newBlob
	next.Items[i].ByteSize = byteSize
	next.Items[i].Checksum = checksum
	next.Items[i].Preview = PreviewText(env.DisplayText, DefaultPreviewRunes)
	next.Items[i].Refs = refSummaries(env.Refs)
	next.Items[i].UpdatedAt = time.Now().UTC()
	next.Items[i].Revision = next.Revision + 1
	if next.Items[i].State == StateBlocked {
		next.Items[i].State = StateQueued
		next.Items[i].BlockReason = ""
	}
	if err := s.commitManifestLocked(next); err != nil {
		_ = os.Remove(s.blobPath(newBlob))
		return InboxItemMeta{}, err
	}
	if oldBlob != newBlob {
		_ = os.Remove(s.blobPath(oldBlob))
	}
	updated := next.Items[i]
	s.notifyLocked(s.snapshotLocked())
	return updated, nil
}
