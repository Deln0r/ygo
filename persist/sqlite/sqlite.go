// Package sqlite is the reference persist.Store implementation
// backed by modernc.org/sqlite. modernc.org/sqlite is a pure-Go
// translation of SQLite — no CGO — so binaries built against this
// package preserve the project's gomobile-friendly stance (see
// DESIGN.md).
//
// Use cases:
//
//   - Production single-process storage where SQLite's
//     single-writer model is acceptable. Read concurrency is
//     unlimited; writes serialize at the file lock.
//
//   - Tests, via Open(":memory:"). Each in-memory database is
//     isolated to the *sql.DB connection pool that owns it; tests
//     do not interfere even when run in parallel.
//
//   - Reference for porting a different backing engine. The schema,
//     write path, and Flush transaction here are the canonical
//     shape; deviations should be justified.
//
// The schema is the smallest viable form: an append-only log of
// (doc_name, opaque blob) tuples ordered by autoincrement primary
// key. No additional indexes beyond the doc_name lookup; no separate
// meta table; no clientID stored alongside. The log alone suffices
// because all wire-format invariants (clock-per-client, state vector,
// delete set) live inside the V1 update blobs themselves.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	// modernc.org/sqlite registers itself as the "sqlite" driver.
	_ "modernc.org/sqlite"

	"github.com/Deln0r/ygo/persist"
)

// memoryDSN is the dsn modernc.org/sqlite recognises for ephemeral
// in-process databases. Each connection opened against this dsn gets
// its OWN private database — there is no shared in-memory pool — so
// callers passing this path must either accept the per-connection
// isolation or rely on Open's SetMaxOpenConns(1) clamp below.
const memoryDSN = ":memory:"

// schemaSQL bootstraps the storage layout. Run on every Open via
// IF NOT EXISTS so opening an existing database is a no-op.
//
// Index choice: a single composite index on (doc_name, id) is enough
// because every lookup either filters on doc_name (GetUpdates,
// DocumentExists, ClearDocument, Flush) or scans distinct doc_name
// (ListDocuments). The id column is already the primary key so it
// has its own implicit index; the composite form lets GetUpdates
// satisfy WHERE doc_name = ? ORDER BY id ASC without a separate sort.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS ygo_updates (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_name TEXT NOT NULL,
    update_blob BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ygo_updates_doc_name ON ygo_updates(doc_name, id);
`

// Store implements persist.Store on top of a SQLite database.
//
// Concurrency: safe for concurrent use from multiple goroutines.
// modernc.org/sqlite + database/sql serialize writes at the file
// lock; reads are unbounded. Callers do not need to synchronize.
type Store struct {
	db *sql.DB

	closeMu sync.Mutex
	closed  bool
}

// Compile-time guarantee that *Store satisfies persist.Store. If
// this line stops compiling, the interface has shifted and Store
// needs a matching update.
var _ persist.Store = (*Store)(nil)

// Open opens (or creates) a SQLite database at path and returns a
// ready-to-use Store. The schema is created on first open and is a
// no-op on subsequent opens of the same database.
//
// Use ":memory:" for an ephemeral in-memory database. The Store
// clamps the connection pool to a single connection in that mode
// because modernc.org/sqlite (like reference SQLite) gives each
// connection its own private in-memory database. Without the clamp,
// a write on one connection would land in a different database
// than a read on another connection — every test would mysteriously
// fail with "no such table".
//
// Concurrency note: although Store is safe for concurrent use, opening
// the SAME on-disk path twice from the same process creates two
// independent *sql.DB pools. SQLite's file-lock serializes their
// writes correctly, but they will not see each other's uncommitted
// transactions. Prefer a single Store per database file per process.
func Open(path string) (*Store, error) {
	dsn := path
	if path != memoryDSN {
		// On-disk: run every pooled connection in WAL mode with a busy
		// timeout. Without a busy timeout, concurrent writers contending
		// on the file lock fail immediately with SQLITE_BUSY ("database
		// is locked") and their updates are lost; the timeout makes them
		// wait for the lock instead. WAL additionally lets readers run
		// without blocking the writer. Both are set through the DSN so
		// they apply to each connection database/sql opens in the pool
		// (busy_timeout is a per-connection setting).
		//
		// The pragmas are appended as a query WITHOUT a "file:" prefix on
		// purpose: a file: URI would reinterpret '#', '?' and '%' in the
		// path and silently write to the wrong file, whereas a bare path
		// stays a literal filename while the driver still applies the
		// pragmas. WAL needs a local filesystem and a writable directory
		// (for the -wal / -shm sidecar files); it is not suitable over a
		// network filesystem such as NFS.
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite.Open(%q): %w", path, err)
	}
	if path == memoryDSN {
		// One connection means one database — the per-connection
		// isolation of ":memory:" becomes a non-issue.
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite.Open(%q) init schema: %w", path, err)
	}
	if _, err := db.Exec(versionsSchemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite.Open(%q) init versions schema: %w", path, err)
	}
	return &Store{db: db}, nil
}

// StoreUpdate appends an opaque update blob to docName's log.
// Returns persist.ErrEmptyUpdate on a zero-length blob.
func (s *Store) StoreUpdate(ctx context.Context, docName string, update []byte) error {
	if len(update) == 0 {
		return persist.ErrEmptyUpdate
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ygo_updates (doc_name, update_blob) VALUES (?, ?)`,
		docName, update)
	if err != nil {
		return fmt.Errorf("sqlite.StoreUpdate(%q): %w", docName, err)
	}
	return nil
}

// GetUpdates returns every update blob stored for docName, in
// insertion order. Returns (nil, nil) — empty slice, no error — for
// an unknown document.
func (s *Store) GetUpdates(ctx context.Context, docName string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT update_blob FROM ygo_updates WHERE doc_name = ? ORDER BY id ASC`,
		docName)
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetUpdates(%q): %w", docName, err)
	}
	defer rows.Close()

	var out [][]byte
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("sqlite.GetUpdates(%q) scan: %w", docName, err)
		}
		out = append(out, blob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.GetUpdates(%q) iterate: %w", docName, err)
	}
	return out, nil
}

// Flush replaces all updates for docName with a single merged
// snapshot. The read-merge-replace runs inside one write transaction
// opened with BEGIN IMMEDIATE, so a concurrent StoreUpdate either
// commits before Flush takes the write lock (and is folded into the
// snapshot) or blocks on that lock until Flush commits and then appends
// to the now-shorter log. A torn intermediate state is never observable.
//
// The transaction is IMMEDIATE, not the default deferred, on purpose. A
// deferred transaction would take only a read snapshot for the SELECT and
// then try to upgrade to a writer at the DELETE; on a WAL database a
// StoreUpdate committing in that gap invalidates the snapshot and the
// upgrade fails at once with SQLITE_BUSY_SNAPSHOT, which the busy_timeout
// does NOT service, so Flush would fail a large fraction of the time on a
// live document. Taking the write lock up front routes contention through
// busy_timeout instead (a concurrent writer waits, bounded, rather than
// failing the flush), which is what makes Flush reliable on a hot doc.
//
// Idempotent: no-op for documents with zero or one stored updates.
// Returns the error from persist.MergeUpdates without touching the
// database when the snapshot cannot be computed; the original log
// stays canonical. Under extreme, sustained write contention BEGIN
// IMMEDIATE may itself exceed busy_timeout and return a transient busy
// error; the log is untouched and the caller may retry.
func (s *Store) Flush(ctx context.Context, docName string) error {
	// A dedicated connection lets us drive BEGIN/COMMIT/ROLLBACK by hand so
	// the transaction is IMMEDIATE (see the doc comment); database/sql's
	// BeginTx opens a deferred transaction with no portable way to request
	// IMMEDIATE across drivers.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite.Flush(%q) conn: %w", docName, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite.Flush(%q) begin: %w", docName, err)
	}
	committed := false
	// Roll back unless an explicit COMMIT was reached; a no-op afterward.
	// Runs before conn.Close (deferred earlier, so it releases the lock
	// before the connection returns to the pool).
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx,
		`SELECT update_blob FROM ygo_updates WHERE doc_name = ? ORDER BY id ASC`,
		docName)
	if err != nil {
		return fmt.Errorf("sqlite.Flush(%q) read: %w", docName, err)
	}
	var updates [][]byte
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite.Flush(%q) scan: %w", docName, err)
		}
		updates = append(updates, blob)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("sqlite.Flush(%q) iterate: %w", docName, err)
	}
	rows.Close()

	if len(updates) < 2 {
		// Zero or one update — already optimal. Commit the empty txn so
		// the write lock is released cleanly.
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("sqlite.Flush(%q) commit: %w", docName, err)
		}
		committed = true
		return nil
	}

	snapshot, err := persist.MergeUpdates(updates)
	if err != nil {
		// Snapshot computation failed — leave the original log alone.
		// Returning here triggers the deferred ROLLBACK.
		return fmt.Errorf("sqlite.Flush(%q) merge: %w", docName, err)
	}

	if _, err := conn.ExecContext(ctx,
		`DELETE FROM ygo_updates WHERE doc_name = ?`, docName); err != nil {
		return fmt.Errorf("sqlite.Flush(%q) delete: %w", docName, err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO ygo_updates (doc_name, update_blob) VALUES (?, ?)`,
		docName, snapshot); err != nil {
		return fmt.Errorf("sqlite.Flush(%q) insert: %w", docName, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqlite.Flush(%q) commit: %w", docName, err)
	}
	committed = true
	return nil
}

// DocumentExists reports whether docName has any stored updates.
func (s *Store) DocumentExists(ctx context.Context, docName string) (bool, error) {
	var n int
	row := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM ygo_updates WHERE doc_name = ? LIMIT 1`, docName)
	err := row.Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite.DocumentExists(%q): %w", docName, err)
	}
	return true, nil
}

// ListDocuments returns the names of every document with at least
// one stored update. Order is by doc_name ascending — not part of
// the persist.Store contract, but deterministic ordering keeps
// tests stable.
func (s *Store) ListDocuments(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT doc_name FROM ygo_updates ORDER BY doc_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListDocuments: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite.ListDocuments scan: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.ListDocuments iterate: %w", err)
	}
	return out, nil
}

// ClearDocument removes every update for docName. Idempotent.
func (s *Store) ClearDocument(ctx context.Context, docName string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM ygo_updates WHERE doc_name = ?`, docName)
	if err != nil {
		return fmt.Errorf("sqlite.ClearDocument(%q): %w", docName, err)
	}
	return nil
}

// Close releases the underlying *sql.DB. Safe to call more than
// once; subsequent calls return nil without touching the database.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("sqlite.Close: %w", err)
	}
	return nil
}
