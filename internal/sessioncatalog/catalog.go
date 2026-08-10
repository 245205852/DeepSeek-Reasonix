package sessioncatalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/projectiondb"
)

const defaultMissingGrace = 30 * time.Second

type Catalog struct {
	db               *sql.DB
	opts             Options
	revision         atomic.Uint64
	statusMu         sync.RWMutex
	status           Status
	writeCh          chan string
	writeMu          sync.Mutex
	writeQueued      map[string]SessionRecord
	mutationMu       sync.RWMutex
	removedPaths     sync.Map
	repairCh         chan string
	repairQueued     sync.Map
	reconcileCh      chan DirectoryTarget
	reconcileQueued  sync.Map
	pathCh           chan sessionPathRequest
	pathQueued       sync.Map
	directoryLocksMu sync.Mutex
	directoryLocks   map[string]*sync.Mutex
	workerCtx        context.Context
	workerCancel     context.CancelFunc
	stop             chan struct{}
	stopOnce         sync.Once
	workers          sync.WaitGroup
	closeDBOnce      sync.Once
}

type sessionPathRequest struct {
	target DirectoryTarget
	path   string
}

type pageCursor struct {
	Pinned   int    `json:"p"`
	Activity int64  `json:"a"`
	TopicID  string `json:"t"`
}

func Open(ctx context.Context, opts Options) (*Catalog, error) {
	if opts.Path == "" {
		opts.Path = DefaultPath()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MissingGrace <= 0 {
		opts.MissingGrace = defaultMissingGrace
	}
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = 1024
	}
	// An empty path (no cache dir) or explicit memory flag must never write a
	// relative session-catalog file into the current project directory.
	if strings.TrimSpace(opts.Path) == "" {
		opts.Path = ""
		opts.InMemory = true
	}
	if !opts.InMemory {
		if env := strings.TrimSpace(os.Getenv("REASONIX_SESSION_CATALOG_MEMORY")); env == "1" {
			opts.InMemory = true
		}
	}

	c := &Catalog{
		opts:           opts,
		writeCh:        make(chan string, opts.QueueCapacity),
		writeQueued:    map[string]SessionRecord{},
		repairCh:       make(chan string, opts.QueueCapacity),
		reconcileCh:    make(chan DirectoryTarget, 64),
		pathCh:         make(chan sessionPathRequest, opts.QueueCapacity),
		directoryLocks: map[string]*sync.Mutex{},
		stop:           make(chan struct{}),
		status:         Status{State: StateOpening, Path: opts.Path},
	}
	handle, err := projectiondb.Open(ctx, projectiondb.OpenOptions{
		Path:         opts.Path,
		MemoryName:   "session-catalog",
		Migrations:   sessionMigrations(),
		InMemory:     opts.InMemory,
		MaxOpenConns: 4,
		Now:          opts.Now,
	})
	if err != nil {
		return nil, err
	}
	c.db = handle.DB
	c.status.Mode = Mode(handle.Status.Mode)
	c.status.State = State(handle.Status.State)
	if c.status.State == "" {
		c.status.State = StateReady
	}
	if c.status.Mode == ModeMemory {
		c.status.Path = ""
	} else {
		c.status.Path = handle.Status.Path
	}
	c.status.LastError = handle.Status.LastError
	c.status.QuarantinedPath = handle.Status.QuarantinedPath
	if err := c.loadStatus(ctx); err != nil {
		_ = c.db.Close()
		return nil, err
	}
	c.workerCtx, c.workerCancel = context.WithCancel(context.Background())
	c.workers.Add(1)
	go c.writerLoop()
	c.workers.Add(1)
	go c.reconcileLoop()
	c.workers.Add(1)
	go c.sessionPathLoop()
	if !opts.DisableRepair {
		c.workers.Add(1)
		go c.repairLoop()
		c.enqueuePersistedRepairs(ctx)
	}
	return c, nil
}

func (c *Catalog) loadStatus(ctx context.Context) error {
	var revision uint64
	if err := c.db.QueryRowContext(ctx, `SELECT revision FROM catalog_state WHERE id=1`).Scan(&revision); err != nil {
		return err
	}
	c.revision.Store(revision)
	c.statusMu.Lock()
	c.status.Revision = revision
	c.statusMu.Unlock()
	c.refreshCounts(ctx)
	return nil
}

func (c *Catalog) Status() Status {
	if c == nil {
		return Status{State: StateDegraded, Mode: ModeMemory, LastError: "session catalog unavailable"}
	}
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

func (c *Catalog) refreshCounts(ctx context.Context) {
	if c == nil || c.db == nil {
		return
	}
	var indexed, pending, total int64
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions`).Scan(&indexed)
	_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE turns_state='unknown'`).Scan(&pending)
	_ = c.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total), 0) FROM catalog_directories`).Scan(&total)
	c.statusMu.Lock()
	c.status.Indexed = indexed
	c.status.Total = total
	c.status.RepairPending = pending
	c.status.Revision = c.revision.Load()
	c.statusMu.Unlock()
}

func normalizeScope(scope, root string) (string, string) {
	if strings.TrimSpace(scope) != "project" {
		return "global", ""
	}
	return "project", strings.TrimSpace(root)
}

func normalizeSessionRecord(record SessionRecord) SessionRecord {
	record.Path = filepath.Clean(record.Path)
	if record.Directory == "" {
		record.Directory = filepath.Dir(record.Path)
	}
	record.Directory = filepath.Clean(record.Directory)
	record.Scope, record.WorkspaceRoot = normalizeScope(record.Scope, record.WorkspaceRoot)
	if record.TurnsState == "" {
		record.TurnsState = TurnsUnknown
	}
	if record.Health == "" {
		record.Health = HealthOK
	}
	return record
}

func (c *Catalog) EnqueueSession(record SessionRecord) bool {
	if c == nil {
		return false
	}
	record = normalizeSessionRecord(record)
	c.removedPaths.Delete(record.Path)
	c.writeMu.Lock()
	if _, loaded := c.writeQueued[record.Path]; loaded {
		c.writeQueued[record.Path] = record
		c.writeMu.Unlock()
		return true
	}
	c.writeQueued[record.Path] = record
	select {
	case <-c.stop:
		delete(c.writeQueued, record.Path)
		c.writeMu.Unlock()
		return false
	case c.writeCh <- record.Path:
		c.writeMu.Unlock()
		return true
	default:
		delete(c.writeQueued, record.Path)
		c.writeMu.Unlock()
		return false
	}
}

func (c *Catalog) takeQueuedWrite(path string) (SessionRecord, bool) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	record, ok := c.writeQueued[path]
	if ok {
		delete(c.writeQueued, path)
	}
	return record, ok
}

func (c *Catalog) writerLoop() {
	defer c.workers.Done()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	pending := map[string]SessionRecord{}
	flush := func() {
		if len(pending) == 0 {
			return
		}
		records := make([]SessionRecord, 0, len(pending))
		for _, record := range pending {
			records = append(records, record)
		}
		pending = map[string]SessionRecord{}
		ctx, cancel := context.WithTimeout(c.workerCtx, time.Second)
		_ = c.upsertSessions(ctx, records, nil, "write")
		cancel()
	}
	for {
		select {
		case path := <-c.writeCh:
			if record, ok := c.takeQueuedWrite(path); ok {
				pending[path] = record
			}
			if len(pending) >= 64 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.stop:
			for {
				select {
				case path := <-c.writeCh:
					if record, ok := c.takeQueuedWrite(path); ok {
						pending[path] = record
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (c *Catalog) UpsertSession(ctx context.Context, record SessionRecord) error {
	return c.upsertSessions(ctx, []SessionRecord{normalizeSessionRecord(record)}, nil, "write")
}

func (c *Catalog) upsertSessions(ctx context.Context, records []SessionRecord, generations map[string]int64, reason string) error {
	if len(records) == 0 {
		return nil
	}
	c.mutationMu.RLock()
	defer c.mutationMu.RUnlock()
	filtered := records[:0]
	for _, record := range records {
		if _, removed := c.removedPaths.Load(filepath.Clean(record.Path)); !removed {
			filtered = append(filtered, record)
		}
	}
	records = filtered
	if len(records) == 0 {
		return nil
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	affected := map[TopicKey]struct{}{}
	roots := map[string]struct{}{}
	directoryGenerations := map[string]int64{}
	for _, raw := range records {
		record := normalizeSessionRecord(raw)
		var previous TopicKey
		if err := tx.QueryRowContext(ctx, `SELECT scope,workspace_root,topic_id FROM catalog_sessions WHERE path=?`, record.Path).
			Scan(&previous.Scope, &previous.WorkspaceRoot, &previous.TopicID); err == nil && previous.TopicID != "" {
			affected[previous] = struct{}{}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return err
		}
		generation := int64(0)
		if generations != nil {
			generation = generations[record.Path]
		} else if cached, ok := directoryGenerations[record.Directory]; ok {
			generation = cached
		} else {
			_ = tx.QueryRowContext(ctx, `SELECT scan_generation FROM catalog_directories WHERE path=?`, record.Directory).Scan(&generation)
			directoryGenerations[record.Directory] = generation
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_sessions(
            path,directory,scope,workspace_root,topic_id,topic_title,custom_title,
            created_at,last_activity_at,preview,turns,turns_state,recovered,
            recovery_reason,recovery_digest,parent_id,content_fingerprint,
            meta_fingerprint,health,missing_since,seen_generation
        ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(path) DO UPDATE SET
            directory=excluded.directory, scope=excluded.scope,
            workspace_root=excluded.workspace_root, topic_id=excluded.topic_id,
            topic_title=excluded.topic_title, custom_title=excluded.custom_title,
            created_at=excluded.created_at, last_activity_at=excluded.last_activity_at,
            preview=excluded.preview, turns=excluded.turns,
            turns_state=excluded.turns_state, recovered=excluded.recovered,
            recovery_reason=excluded.recovery_reason,
            recovery_digest=excluded.recovery_digest, parent_id=excluded.parent_id,
            content_fingerprint=excluded.content_fingerprint,
            meta_fingerprint=excluded.meta_fingerprint, health=excluded.health,
            missing_since=0, seen_generation=MAX(catalog_sessions.seen_generation, excluded.seen_generation)`,
			record.Path, record.Directory, record.Scope, record.WorkspaceRoot,
			record.TopicID, record.TopicTitle, record.CustomTitle, record.CreatedAt,
			record.LastActivityAt, record.Preview, record.Turns, record.TurnsState,
			record.Recovered, record.RecoveryReason, record.RecoveryDigest,
			record.ParentID, record.ContentFingerprint, record.MetaFingerprint,
			record.Health, 0, generation); err != nil {
			_ = tx.Rollback()
			return err
		}
		if record.TopicID != "" {
			affected[TopicKey{Scope: record.Scope, WorkspaceRoot: record.WorkspaceRoot, TopicID: record.TopicID}] = struct{}{}
		}
		roots[record.WorkspaceRoot] = struct{}{}
	}
	for key := range affected {
		if err := recomputeTopic(ctx, tx, key); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	revision, err := bumpRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.publishRevision(revision, mapKeys(roots), reason)
	c.refreshCounts(ctx)
	return nil
}

// RemoveSession immediately removes one projection row after an authoritative
// archive/delete. It never touches the transcript or sidecars. A tombstone
// suppresses writer work captured before the removal; a later exact index,
// save, or fresh directory scan clears it when the path is recreated.
func (c *Catalog) RemoveSession(ctx context.Context, path, reason string) error {
	if c == nil || c.db == nil {
		return nil
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil
	}
	// Serialize an authoritative removal with directory reconciliation. Without
	// this boundary a scan that captured the old path just before an archive
	// could clear the tombstone and reinsert the stale projection afterwards.
	directoryLock := c.directoryLock(filepath.Dir(path))
	directoryLock.Lock()
	defer directoryLock.Unlock()
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	c.removedPaths.Store(path, struct{}{})
	c.writeMu.Lock()
	delete(c.writeQueued, path)
	c.writeMu.Unlock()
	c.pathQueued.Delete(path)
	c.repairQueued.Delete(path)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var key TopicKey
	err = tx.QueryRowContext(ctx, `SELECT scope,workspace_root,topic_id FROM catalog_sessions WHERE path=?`, path).
		Scan(&key.Scope, &key.WorkspaceRoot, &key.TopicID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalog_sessions WHERE path=?`, path); err != nil {
		_ = tx.Rollback()
		return err
	}
	if key.TopicID != "" {
		if err := recomputeTopic(ctx, tx, key); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	revision, err := bumpRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.publishRevision(revision, []string{key.WorkspaceRoot}, reason)
	c.refreshCounts(ctx)
	return nil
}

func recomputeTopic(ctx context.Context, tx *sql.Tx, key TopicKey) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_sessions WHERE scope=? AND workspace_root=? AND topic_id=?`, key.Scope, key.WorkspaceRoot, key.TopicID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM catalog_topics WHERE scope=? AND workspace_root=? AND topic_id=?`, key.Scope, key.WorkspaceRoot, key.TopicID)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO catalog_topics(
        scope,workspace_root,topic_id,title,turns,turns_state,created_at,
        last_activity_at,recovery_state,health
    ) SELECT ?,?,?,
        COALESCE(NULLIF((SELECT COALESCE(NULLIF(custom_title,''), NULLIF(topic_title,''), preview, '')
            FROM catalog_sessions WHERE scope=? AND workspace_root=? AND topic_id=?
            ORDER BY last_activity_at DESC, path ASC LIMIT 1),''), ?),
        COALESCE(SUM(CASE WHEN turns_state='valid' THEN turns ELSE 0 END),0),
        CASE WHEN SUM(CASE WHEN turns_state='corrupt' THEN 1 ELSE 0 END)>0 THEN 'corrupt'
             WHEN SUM(CASE WHEN turns_state='unknown' THEN 1 ELSE 0 END)>0 THEN 'unknown'
             ELSE 'valid' END,
        COALESCE(MIN(NULLIF(created_at,0)),0), COALESCE(MAX(last_activity_at),0),
        CASE WHEN SUM(CASE WHEN recovered=1 THEN 1 ELSE 0 END)=COUNT(*) THEN 'recovery_only' ELSE '' END,
        CASE WHEN SUM(CASE WHEN health='corrupt' THEN 1 ELSE 0 END)>0 THEN 'corrupt'
             WHEN SUM(CASE WHEN health='missing' THEN 1 ELSE 0 END)>0 THEN 'missing'
             ELSE 'ok' END
      FROM catalog_sessions WHERE scope=? AND workspace_root=? AND topic_id=?
    ON CONFLICT(scope,workspace_root,topic_id) DO UPDATE SET
        title=excluded.title, turns=excluded.turns, turns_state=excluded.turns_state,
        created_at=excluded.created_at, last_activity_at=excluded.last_activity_at,
        recovery_state=excluded.recovery_state, health=excluded.health`,
		key.Scope, key.WorkspaceRoot, key.TopicID,
		key.Scope, key.WorkspaceRoot, key.TopicID, key.TopicID,
		key.Scope, key.WorkspaceRoot, key.TopicID)
	return err
}

func bumpRevision(ctx context.Context, tx *sql.Tx) (uint64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE catalog_state SET revision=revision+1 WHERE id=1`); err != nil {
		return 0, err
	}
	var revision uint64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM catalog_state WHERE id=1`).Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func (c *Catalog) publishRevision(revision uint64, roots []string, reason string) {
	c.revision.Store(revision)
	c.statusMu.Lock()
	c.status.Revision = revision
	c.statusMu.Unlock()
	if c.opts.OnRevision != nil {
		c.opts.OnRevision(revision, roots, reason)
	}
}

func mapKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}

func (c *Catalog) ListTopics(ctx context.Context, req TopicPageRequest) (TopicPage, error) {
	out := TopicPage{Items: []TopicRecord{}, Revision: c.revision.Load()}
	req.Scope, req.WorkspaceRoot = normalizeScope(req.Scope, req.WorkspaceRoot)
	if req.Limit <= 0 {
		req.Limit = DefaultLimit
	}
	if req.Limit > MaxLimit {
		req.Limit = MaxLimit
	}
	cursor, err := decodeCursor(req.Cursor)
	if err != nil {
		return out, err
	}
	args := []any{req.Scope, req.WorkspaceRoot}
	where := `scope=? AND workspace_root=?`
	if query := strings.TrimSpace(req.Query); query != "" {
		where += ` AND lower(title) LIKE ?`
		args = append(args, "%"+strings.ToLower(query)+"%")
	}
	if cutoff := timeFilterCutoff(req.TimeFilter, c.opts.Now()); cutoff > 0 {
		where += ` AND last_activity_at>=?`
		args = append(args, cutoff)
	}
	if cursor != nil {
		where += ` AND (pinned<? OR (pinned=? AND last_activity_at<?) OR (pinned=? AND last_activity_at=? AND topic_id>?))`
		args = append(args, cursor.Pinned, cursor.Pinned, cursor.Activity, cursor.Pinned, cursor.Activity, cursor.TopicID)
	}
	args = append(args, req.Limit+1)
	rows, err := c.db.QueryContext(ctx, `SELECT scope,workspace_root,topic_id,title,pinned,sort_order,
        turns,turns_state,created_at,last_activity_at,recovery_state,health
        FROM catalog_topics WHERE `+where+`
        ORDER BY pinned DESC,last_activity_at DESC,topic_id ASC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var item TopicRecord
		if err := rows.Scan(&item.Scope, &item.WorkspaceRoot, &item.TopicID, &item.Title,
			&item.Pinned, &item.SortOrder, &item.Turns, &item.TurnsState,
			&item.CreatedAt, &item.LastActivityAt, &item.RecoveryState, &item.Health); err != nil {
			return out, err
		}
		item.Sessions = []SessionRecord{}
		out.Items = append(out.Items, item)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	more := len(out.Items) > req.Limit
	if more {
		out.Items = out.Items[:req.Limit]
	}
	for i := range out.Items {
		sessions, err := c.listTopicSessions(ctx, TopicKey{Scope: out.Items[i].Scope, WorkspaceRoot: out.Items[i].WorkspaceRoot, TopicID: out.Items[i].TopicID})
		if err != nil {
			return TopicPage{Items: []TopicRecord{}, Revision: out.Revision}, err
		}
		out.Items[i].Sessions = sessions
	}
	if more && len(out.Items) > 0 {
		last := out.Items[len(out.Items)-1]
		pinned := 0
		if last.Pinned {
			pinned = 1
		}
		out.NextCursor = encodeCursor(pageCursor{Pinned: pinned, Activity: last.LastActivityAt, TopicID: last.TopicID})
	}
	return out, nil
}

func (c *Catalog) listTopicSessions(ctx context.Context, key TopicKey) ([]SessionRecord, error) {
	// Bound per-topic payload size so a recovery-heavy topic cannot emit an
	// unbounded Wails payload on a single page fetch.
	rows, err := c.db.QueryContext(ctx, `SELECT path,directory,scope,workspace_root,topic_id,topic_title,
        custom_title,created_at,last_activity_at,preview,turns,turns_state,recovered,
        recovery_reason,recovery_digest,parent_id,content_fingerprint,meta_fingerprint,
        health,missing_since FROM catalog_sessions
        WHERE scope=? AND workspace_root=? AND topic_id=?
        ORDER BY last_activity_at DESC,path ASC LIMIT ?`, key.Scope, key.WorkspaceRoot, key.TopicID, MaxLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionRecord{}
	for rows.Next() {
		var record SessionRecord
		if err := rows.Scan(&record.Path, &record.Directory, &record.Scope, &record.WorkspaceRoot,
			&record.TopicID, &record.TopicTitle, &record.CustomTitle, &record.CreatedAt,
			&record.LastActivityAt, &record.Preview, &record.Turns, &record.TurnsState,
			&record.Recovered, &record.RecoveryReason, &record.RecoveryDigest,
			&record.ParentID, &record.ContentFingerprint, &record.MetaFingerprint,
			&record.Health, &record.MissingSince); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (c *Catalog) GetTopic(ctx context.Context, key TopicKey) (TopicRecord, bool, error) {
	key.Scope, key.WorkspaceRoot = normalizeScope(key.Scope, key.WorkspaceRoot)
	key.TopicID = strings.TrimSpace(key.TopicID)
	item := TopicRecord{Sessions: []SessionRecord{}}
	err := c.db.QueryRowContext(ctx, `SELECT scope,workspace_root,topic_id,title,pinned,sort_order,
        turns,turns_state,created_at,last_activity_at,recovery_state,health
        FROM catalog_topics WHERE scope=? AND workspace_root=? AND topic_id=?`,
		key.Scope, key.WorkspaceRoot, key.TopicID).Scan(
		&item.Scope, &item.WorkspaceRoot, &item.TopicID, &item.Title,
		&item.Pinned, &item.SortOrder, &item.Turns, &item.TurnsState,
		&item.CreatedAt, &item.LastActivityAt, &item.RecoveryState, &item.Health)
	if errors.Is(err, sql.ErrNoRows) {
		return item, false, nil
	}
	if err != nil {
		return item, false, err
	}
	item.Sessions, err = c.listTopicSessions(ctx, key)
	if err != nil {
		return TopicRecord{Sessions: []SessionRecord{}}, false, err
	}
	return item, true, nil
}

func encodeCursor(cursor pageCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(encoded string) (*pageCursor, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid session catalog cursor: %w", err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(b, &cursor); err != nil || cursor.TopicID == "" {
		return nil, errors.New("invalid session catalog cursor")
	}
	return &cursor, nil
}

func timeFilterCutoff(filter string, now time.Time) int64 {
	var duration time.Duration
	value := strings.TrimSpace(strings.ToLower(filter))
	switch value {
	case "day", "24h":
		duration = 24 * time.Hour
	case "week", "7d":
		duration = 7 * 24 * time.Hour
	case "month", "30d":
		duration = 30 * 24 * time.Hour
	default:
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0
		}
		duration = parsed
	}
	return now.Add(-duration).UnixMilli()
}

func (c *Catalog) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		if c.workerCancel != nil {
			c.workerCancel()
		}
		close(c.stop)
	})
	done := make(chan struct{})
	go func() {
		c.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		var err error
		c.closeDBOnce.Do(func() { err = c.db.Close() })
		c.statusMu.Lock()
		c.status.State = StateClosed
		c.statusMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
