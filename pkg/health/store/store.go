// Package store persists the health monitor state in a SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // Register the pure-Go "sqlite" driver.
)

const (
	// FileName is the database file name inside the state directory.
	FileName = "health.db"

	driverName  = "sqlite"
	busyTimeout = 5000 // milliseconds
)

// ErrSchemaTooNew is returned when the database was written by a newer version.
var ErrSchemaTooNew = errors.New("database schema is newer than this build supports")

// Store is the SQLite-backed state store.
type Store struct {
	db   *sql.DB
	ckpt checkpointer
}

// Open opens (creating if needed) <stateDir>/health.db in WAL mode with a single
// connection and applies pending schema migrations.
func Open(ctx context.Context, stateDir string) (*Store, error) {
	path, err := dbPath(stateDir)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driverName, dsn(path, "rwc",
		"journal_mode(WAL)", "synchronous(NORMAL)"))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// OpenReadOnly opens an existing <stateDir>/health.db without creating or
// modifying anything: no migration, no journal-mode change. It fails when the
// file is missing or its schema version is not the one this build expects.
func OpenReadOnly(ctx context.Context, stateDir string) (*Store, error) {
	path, err := dbPath(stateDir)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(path); err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	mode := "ro"
	if _, err = os.Stat(path + "-wal"); errors.Is(err, os.ErrNotExist) {
		// No WAL file means no writer left anything behind: the daemon is stopped
		// (a clean close removes the WAL). Open immutable so SQLite does not create
		// -wal/-shm files next to the database. Remaining race: a daemon that starts
		// between this Stat and the end of the read; benign, the worst case is one
		// stale or wrong check --once result.
		mode = "ro&immutable=1"
	}
	db, err := sql.Open(driverName, dsn(path, mode, "query_only(1)"))
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	version, err := userVersion(ctx, db)
	if err == nil && version != len(migrations()) {
		err = fmt.Errorf("schema version %d, want %d", version, len(migrations()))
		if version > len(migrations()) {
			err = fmt.Errorf("schema version %d: %w", version, ErrSchemaTooNew)
		}
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close releases the database. SQLite checkpoints the WAL and removes its files
// when the last connection to a writable database closes.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}
	return nil
}

// dbPath returns the absolute database path; a relative path would be parsed as
// a URI authority by SQLite.
func dbPath(stateDir string) (string, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state dir %q: %w", stateDir, err)
	}

	return filepath.Join(abs, FileName), nil
}

func dsn(path, mode string, pragmas ...string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout))
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode + "&" + q.Encode()}

	return u.String()
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// migrate applies the forward-only migrations above the stored user_version,
// each in its own transaction.
func migrate(ctx context.Context, db *sql.DB) error {
	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	steps := migrations()
	if current > len(steps) {
		return fmt.Errorf("schema version %d: %w", current, ErrSchemaTooNew)
	}
	for i := current; i < len(steps); i++ {
		if err := applyStep(ctx, db, i+1, steps[i]); err != nil {
			return err
		}
	}

	return nil
}

func applyStep(ctx context.Context, db *sql.DB, version int, ddl string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("set user_version %d: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", version, err)
	}

	return nil
}

// migrations returns the ordered schema steps; step N brings user_version to N.
func migrations() []string {
	return []string{schemaV1}
}

const schemaV1 = `
CREATE TABLE conditions (
  key TEXT PRIMARY KEY, check_name TEXT NOT NULL,
  streak_start INTEGER, bad_for_s INTEGER NOT NULL DEFAULT 0,
  last_reading_at INTEGER, last_text TEXT NOT NULL DEFAULT '',
  open INTEGER NOT NULL DEFAULT 0, opened_at INTEGER
);
CREATE TABLE baselines (
  serial TEXT PRIMARY KEY, model TEXT, bus TEXT,
  data TEXT NOT NULL,              -- JSON: counters, self-test fields
  first_seen_at INTEGER NOT NULL, last_seen_at INTEGER NOT NULL,
  retired INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL); -- kernel cursor, probe state, rate marks, first-run flag
CREATE TABLE alerts (
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('problem','resolved','event','fix','retired','cleared','checker_error')),
  check_name TEXT NOT NULL, key TEXT NOT NULL, text TEXT NOT NULL
);
CREATE INDEX alerts_at ON alerts(at);
CREATE INDEX alerts_check_at ON alerts(check_name, at);
CREATE TABLE samples (at INTEGER NOT NULL, metric TEXT NOT NULL, value REAL NOT NULL);
CREATE INDEX samples_metric_at ON samples(metric, at);
CREATE TABLE samples_daily (
  day INTEGER NOT NULL, metric TEXT NOT NULL,  -- day = epoch of 00:00 local
  min REAL NOT NULL, avg REAL NOT NULL, max REAL NOT NULL, n INTEGER NOT NULL,
  PRIMARY KEY (day, metric)
);
CREATE TABLE queue (
  id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL,
  channel TEXT NOT NULL CHECK (channel IN ('tlg','sms','email')),
  text TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0
);
`
