package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func names(t *testing.T, db *sql.DB, typ string) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%'`, typ)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	return got
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("pragma %s: %v", name, err)
	}

	return v
}

func kvValue(t *testing.T, db *sql.DB, k string) string {
	t.Helper()
	var v string
	if err := db.QueryRowContext(context.Background(), `SELECT v FROM kv WHERE k = ?`, k).Scan(&v); err != nil {
		t.Fatalf("select kv %s: %v", k, err)
	}

	return v
}

func listDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(e.Name() + ":" + info.ModTime().String() + ";")
	}

	return out.String()
}

func TestOpenCreatesSchema(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := os.Stat(filepath.Join(dir, "health.db")); err != nil {
		t.Fatalf("health.db missing: %v", err)
	}
	tables := names(t, s.db, "table")
	for _, n := range []string{"conditions", "baselines", "kv", "alerts", "samples", "samples_daily", "queue"} {
		if !tables[n] {
			t.Errorf("table %s missing", n)
		}
	}
	indexes := names(t, s.db, "index")
	for _, n := range []string{"alerts_at", "alerts_check_at", "samples_metric_at"} {
		if !indexes[n] {
			t.Errorf("index %s missing", n)
		}
	}
	if v := pragma(t, s.db, "user_version"); v != "3" {
		t.Errorf("user_version = %s, want 3", v)
	}
	if v := pragma(t, s.db, "journal_mode"); v != "wal" {
		t.Errorf("journal_mode = %s, want wal", v)
	}
	if v := pragma(t, s.db, "busy_timeout"); v != "5000" {
		t.Errorf("busy_timeout = %s, want 5000", v)
	}
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", got)
	}
}

func TestOpenConstraints(t *testing.T) {
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	exec := func(q string) error {
		_, execErr := s.db.ExecContext(ctx, q)
		return execErr
	}
	if exec(`INSERT INTO alerts(at, kind, check_name, key, text) VALUES (1, 'bogus', 'c', 'k', 't')`) == nil {
		t.Error("alerts accepted invalid kind")
	}
	if exec(`INSERT INTO queue(created_at, channel, text) VALUES (1, 'fax', 't')`) == nil {
		t.Error("queue accepted invalid channel")
	}
	valid := `INSERT INTO alerts(at, kind, check_name, key, text) VALUES (1, 'problem', 'c', 'k', 't')`
	if err = exec(valid); err != nil {
		t.Errorf("valid alert rejected: %v", err)
	}
}

func TestOpenIsIdempotentAndKeepsData(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES ('a', 'b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	if got := kvValue(t, s.db, "a"); got != "b" {
		t.Errorf("kv a = %q, want b", got)
	}
	if got := pragma(t, s.db, "user_version"); got != "3" {
		t.Errorf("user_version = %s, want 3", got)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatalf("set version: %v", err)
	}
	_ = s.Close()

	if _, err = Open(ctx, dir); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("Open error = %v, want ErrSchemaTooNew", err)
	}
	if _, err = OpenReadOnly(ctx, dir); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("OpenReadOnly error = %v, want ErrSchemaTooNew", err)
	}
}

func TestOpenBadDir(t *testing.T) {
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "missing", "sub")); err == nil {
		t.Error("Open succeeded in a missing directory")
	}
}

func TestOpenReadOnlyMissingDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenReadOnly(context.Background(), dir); err == nil {
		t.Fatal("OpenReadOnly succeeded without a database")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("OpenReadOnly created files: %v", entries)
	}
}

func TestOpenReadOnlyReadsAndRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES ('a', 'b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := listDir(t, dir)

	ro, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if got := kvValue(t, ro.db, "a"); got != "b" {
		t.Errorf("kv a = %q, want b", got)
	}
	if _, err := ro.db.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES ('c', 'd')`); err == nil {
		t.Error("read-only store accepted a write")
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("Close ro: %v", err)
	}
	if after := listDir(t, dir); after != before {
		t.Errorf("directory changed: before %q after %q", before, after)
	}
}

func TestOpenReadOnlyWhileWriterOpen(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.db.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES ('a', 'b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ro, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = ro.Close() }()
	if got := kvValue(t, ro.db, "a"); got != "b" {
		t.Errorf("kv a = %q, want b", got)
	}
}

func TestRelativeStateDir(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	ctx := context.Background()
	s, err := Open(ctx, "state")
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded before the directory exists")
	}
	if err = os.Mkdir("state", 0o750); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, "state")
	if err != nil {
		t.Fatalf("Open relative: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, "state")
	if err != nil {
		t.Fatalf("OpenReadOnly relative: %v", err)
	}
	_ = ro.Close()
}

func TestOpenReadOnlyWithLeftoverWAL(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO kv(k, v) VALUES ('a', 'b')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crashed daemon: a -wal file remains next to the database.
	if err = os.WriteFile(filepath.Join(dir, FileName+"-wal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dbFile := filepath.Join(dir, FileName)
	before, err := os.Stat(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	_ = ro.Close()
	after, err := os.Stat(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("health.db mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestOpenReadOnlyUnmigrated(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.ExecContext(ctx, "CREATE TABLE x (a INTEGER)"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	ro, err := OpenReadOnly(ctx, dir)
	if err == nil {
		_ = ro.Close()
		t.Fatal("OpenReadOnly accepted user_version 0")
	}
	if errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("user_version 0 reported as too new: %v", err)
	}
}

// seedV1 creates a version-1 database holding one queued message.
func seedV1(t *testing.T, dir string) {
	t.Helper()
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	for _, q := range []string{
		schemaV1,
		`INSERT INTO queue (id, created_at, channel, text, attempts) VALUES (3, 100, 'tlg', 'old', 1)`,
		"PRAGMA user_version = 1",
	} {
		if _, err = raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateV1ToCurrent(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	seedV1(t, dir)

	ro, err := OpenReadOnly(ctx, dir)
	if err == nil {
		_ = ro.Close()
		t.Fatal("OpenReadOnly accepted a version-1 database")
	}
	if !errors.Is(err, ErrSchemaOld) || errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("OpenReadOnly error = %v, want ErrSchemaOld", err)
	}

	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v := pragma(t, s.db, "user_version"); v != "3" {
		t.Errorf("user_version = %s, want 3", v)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Queue) != 1 || st.Queue[0].ID != 3 || st.Queue[0].Kind != "event" ||
		st.Queue[0].Check != "" || st.Queue[0].Key != "" || st.Queue[0].Plain != "" || !st.Queue[0].Since.IsZero() {
		t.Errorf("queue = %+v, want the 001 row read back as event", st.Queue)
	}
}

// seedV2 creates a version-2 database holding one queued PROBLEM.
func seedV2(t *testing.T, dir string) {
	t.Helper()
	raw, err := sql.Open(driverName, "file:"+filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	for _, q := range []string{
		schemaV1,
		schemaV2,
		`INSERT INTO queue (id, created_at, channel, text, attempts, kind, check_name, key)
		VALUES (4, 100, 'tlg', 'h: PROBLEM: hot (since 10:00)', 1, 'problem', 'disks', 'disks/a')`,
		"PRAGMA user_version = 2",
	} {
		if _, err = raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrateV2ToV3: migration 3 adds the queue item's plain text and since;
// a row written before it reads back with both empty, so the notifier falls
// back to parsing its rendered text.
func TestMigrateV2ToV3(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	seedV2(t, dir)

	ro, err := OpenReadOnly(ctx, dir)
	if err == nil {
		_ = ro.Close()
		t.Fatal("OpenReadOnly accepted a version-2 database")
	}
	var old *OldSchemaError
	if !errors.As(err, &old) || old.Version != 2 {
		t.Errorf("OpenReadOnly error = %v, want OldSchemaError at version 2", err)
	}

	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if v := pragma(t, s.db, "user_version"); v != "3" {
		t.Errorf("user_version = %s, want 3", v)
	}
	st, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Queue) != 1 {
		t.Fatalf("queue = %+v, want the 002 row", st.Queue)
	}
	q := st.Queue[0]
	if q.ID != 4 || q.Kind != "problem" || q.Key != "disks/a" || q.Text != "h: PROBLEM: hot (since 10:00)" ||
		q.Plain != "" || !q.Since.IsZero() {
		t.Errorf("queue row = %+v, want the 002 row with no plain text and no since", q)
	}
}
