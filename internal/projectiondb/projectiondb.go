// Package projectiondb owns the lifecycle of disposable SQLite projections.
// Business data must remain authoritative outside the database: callers are
// expected to be able to discard and rebuild every database opened here.
package projectiondb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	moderncsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type Mode string

const (
	ModeDisk   Mode = "disk"
	ModeMemory Mode = "memory"
)

type State string

const (
	StateReady    State = "ready"
	StateDegraded State = "degraded"
)

type Status struct {
	State           State  `json:"state"`
	Mode            Mode   `json:"mode"`
	Path            string `json:"path,omitempty"`
	Revision        uint64 `json:"revision"`
	Indexed         int64  `json:"indexed"`
	Total           int64  `json:"total"`
	Pending         int64  `json:"pending"`
	Failed          int64  `json:"failed"`
	LastError       string `json:"lastError,omitempty"`
	QuarantinedPath string `json:"quarantinedPath,omitempty"`
}

type Migration struct {
	Version int
	Apply   func(context.Context, *sql.Tx) error
}

type OpenOptions struct {
	Path         string
	MemoryName   string
	Migrations   []Migration
	InMemory     bool
	MaxOpenConns int
	Now          func() time.Time
	SecureDelete bool
	AutoVacuum   bool
}

type Handle struct {
	DB     *sql.DB
	Status Status
}

type FutureSchemaError struct {
	Found     int
	Supported int
}

func (e *FutureSchemaError) Error() string {
	return fmt.Sprintf("projection schema %d is newer than supported %d", e.Found, e.Supported)
}

func Open(ctx context.Context, opts OpenOptions) (*Handle, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MemoryName == "" {
		opts.MemoryName = "projection"
	}
	mode := ModeDisk
	status := Status{State: StateReady, Mode: mode, Path: opts.Path}
	useMemory := opts.InMemory || PathLooksRemote(opts.Path)
	if !useMemory {
		if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
			useMemory = true
			status.LastError = err.Error()
		} else {
			_ = os.Chmod(filepath.Dir(opts.Path), 0o700)
			if filesystemRemote(filepath.Dir(opts.Path)) {
				useMemory = true
				status.LastError = "projection cache is on a remote filesystem; using memory"
			}
		}
	}
	if useMemory {
		mode = ModeMemory
	}

	db, err := open(ctx, opts, mode)
	if err != nil && mode == ModeDisk {
		var future *FutureSchemaError
		switch {
		case errors.As(err, &future):
			// A newer process wrote this projection. Keep the file intact and
			// serve an empty memory projection so startup never quarantines a
			// healthy future schema.
			status.State = StateDegraded
			status.LastError = err.Error()
			mode = ModeMemory
			db, err = open(ctx, opts, mode)
		case isCorruptionError(err):
			// Only integrity-level failures may rename the on-disk projection.
			status.QuarantinedPath = Quarantine(opts.Path, opts.Now())
			db, err = open(ctx, opts, mode)
			if err != nil {
				status.State = StateDegraded
				status.LastError = err.Error()
				mode = ModeMemory
				db, err = open(ctx, opts, mode)
			}
		default:
			// Busy, permission, IO, or transient open errors must never rename
			// a healthy database. Fall back to memory for this process only.
			status.State = StateDegraded
			status.LastError = err.Error()
			mode = ModeMemory
			db, err = open(ctx, opts, mode)
		}
	}
	if err != nil {
		return nil, err
	}
	status.Mode = mode
	if mode == ModeMemory {
		status.Path = ""
	}
	return &Handle{DB: db, Status: status}, nil
}

func open(ctx context.Context, opts OpenOptions, mode Mode) (*sql.DB, error) {
	var dsn string
	if mode == ModeMemory {
		dsn = fmt.Sprintf("file:reasonix-%s-%d?mode=memory&cache=shared", url.PathEscape(opts.MemoryName), opts.Now().UnixNano())
	} else {
		u := &url.URL{Scheme: "file", Path: opts.Path}
		dsn = u.String() + "?_pragma=busy_timeout%28150%29&_pragma=foreign_keys%281%29"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 4
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(min(maxOpen, 2))
	fail := func(err error) (*sql.DB, error) {
		_ = db.Close()
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		return fail(err)
	}
	if mode == ModeDisk {
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
			return fail(err)
		}
	}
	for _, pragma := range []string{`PRAGMA synchronous=NORMAL`, `PRAGMA foreign_keys=ON`, `PRAGMA busy_timeout=150`} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fail(err)
		}
	}
	if opts.SecureDelete {
		if _, err := db.ExecContext(ctx, `PRAGMA secure_delete=ON`); err != nil {
			return fail(err)
		}
	}
	if opts.AutoVacuum {
		if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
			return fail(err)
		}
	}
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fail(err)
	}
	if integrity != "ok" {
		return fail(fmt.Errorf("projection integrity check: %s", integrity))
	}
	if err := ApplyMigrations(ctx, db, opts.Migrations, opts.Now); err != nil {
		return fail(err)
	}
	if mode == ModeDisk {
		_ = os.Chmod(opts.Path, 0o600)
		_ = os.Chmod(opts.Path+"-wal", 0o600)
		_ = os.Chmod(opts.Path+"-shm", 0o600)
	}
	return db, nil
}

func ApplyMigrations(ctx context.Context, db *sql.DB, migrations []Migration, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version INTEGER PRIMARY KEY,
        applied_at INTEGER NOT NULL
    )`); err != nil {
		return err
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	supported := 0
	for _, migration := range migrations {
		if migration.Version > supported {
			supported = migration.Version
		}
	}
	if current > supported {
		return &FutureSchemaError{Found: current, Supported: supported}
	}
	for _, migration := range migrations {
		if migration.Version <= current {
			continue
		}
		if migration.Version != current+1 || migration.Apply == nil {
			return fmt.Errorf("projection migration gap after version %d", current)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := migration.Apply(ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply projection migration %d: %w", migration.Version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, migration.Version, now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		current = migration.Version
	}
	return nil
}

type Inspection struct {
	Exists    bool   `json:"exists"`
	Path      string `json:"path"`
	Schema    int    `json:"schema"`
	Integrity string `json:"integrity,omitempty"`
	Size      int64  `json:"size"`
	Error     string `json:"error,omitempty"`
}

// Inspect is deliberately read-only: it never creates, migrates, repairs, or
// quarantines a database.
func Inspect(ctx context.Context, path string) Inspection {
	out := Inspection{Path: path}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Exists = true
	out.Size = info.Size()
	u := &url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&out.Integrity); err != nil {
		out.Error = err.Error()
		return out
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&out.Schema); err != nil {
		out.Error = err.Error()
	}
	return out
}

func Quarantine(path string, now time.Time) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	quarantined := fmt.Sprintf("%s.corrupt-%d", path, now.UnixMilli())
	if err := os.Rename(path, quarantined); err != nil {
		return ""
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(path+suffix, quarantined+suffix)
	}
	return quarantined
}

// isCorruptionError reports whether err proves the on-disk projection is unsafe
// to keep open. Temporary busy/permission/IO failures must return false so a
// multi-process client never renames a healthy database out from under peers.
func isCorruptionError(err error) bool {
	if err == nil {
		return false
	}
	var future *FutureSchemaError
	if errors.As(err, &future) {
		return false
	}
	var se *moderncsqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "integrity check") ||
		strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "file is not a database") ||
		strings.Contains(msg, "not a database")
}

func PathLooksRemote(path string) bool {
	clean := filepath.Clean(path)
	if runtime.GOOS == "windows" && strings.HasPrefix(clean, `\\`) {
		return true
	}
	slash := filepath.ToSlash(clean)
	return strings.HasPrefix(slash, "/net/") || strings.HasPrefix(slash, "/nfs/") || strings.HasPrefix(slash, "/afs/")
}

// Rebuild constructs and validates a replacement beside the live database,
// then swaps it into place. The old projection remains untouched if building,
// validation, or the platform rename fails (notably an open database on
// Windows). Rebuild never touches authoritative business files.
func Rebuild(ctx context.Context, opts OpenOptions, populate func(context.Context, *sql.DB) error) error {
	if strings.TrimSpace(opts.Path) == "" || opts.InMemory {
		return errors.New("projection rebuild requires a disk path")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	temporary := fmt.Sprintf("%s.rebuild-%d", opts.Path, opts.Now().UnixNano())
	replacement := opts
	replacement.Path = temporary
	replacement.InMemory = false
	handle, err := Open(ctx, replacement)
	if err != nil {
		return fmt.Errorf("open projection replacement: %w", err)
	}
	cleanupTemporary := func() {
		_ = os.Remove(temporary)
		_ = os.Remove(temporary + "-wal")
		_ = os.Remove(temporary + "-shm")
	}
	if handle.Status.Mode != ModeDisk {
		_ = handle.DB.Close()
		cleanupTemporary()
		return errors.New("projection replacement could not use disk storage")
	}
	if populate != nil {
		if err := populate(ctx, handle.DB); err != nil {
			_ = handle.DB.Close()
			cleanupTemporary()
			return fmt.Errorf("populate projection replacement: %w", err)
		}
	}
	var integrity string
	if err := handle.DB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		_ = handle.DB.Close()
		cleanupTemporary()
		if err != nil {
			return fmt.Errorf("validate projection replacement: %w", err)
		}
		return fmt.Errorf("validate projection replacement: %s", integrity)
	}
	_, _ = handle.DB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err := handle.DB.Close(); err != nil {
		cleanupTemporary()
		return err
	}

	backup := fmt.Sprintf("%s.replaced-%d", opts.Path, opts.Now().UnixNano())
	hadOld := false
	if _, err := os.Stat(opts.Path); err == nil {
		if err := os.Rename(opts.Path, backup); err != nil {
			cleanupTemporary()
			return fmt.Errorf("projection database is busy: %w", err)
		}
		hadOld = true
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := os.Rename(opts.Path+suffix, backup+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				_ = os.Rename(backup, opts.Path)
				for _, restored := range []string{"-wal", "-shm"} {
					_ = os.Rename(backup+restored, opts.Path+restored)
				}
				cleanupTemporary()
				return fmt.Errorf("projection database is busy: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanupTemporary()
		return err
	}
	if err := os.Rename(temporary, opts.Path); err != nil {
		if hadOld {
			_ = os.Rename(backup, opts.Path)
			for _, suffix := range []string{"-wal", "-shm"} {
				_ = os.Rename(backup+suffix, opts.Path+suffix)
			}
		}
		cleanupTemporary()
		return fmt.Errorf("install projection replacement: %w", err)
	}
	_ = os.Chmod(opts.Path, 0o600)
	if hadOld {
		_ = os.Remove(backup)
		_ = os.Remove(backup + "-wal")
		_ = os.Remove(backup + "-shm")
	}
	cleanupTemporary()
	return nil
}
