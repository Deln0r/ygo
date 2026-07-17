package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Deln0r/ygo/persist"
)

// versionsSchemaSQL bootstraps the versioned-history table. Run on
// every Open via IF NOT EXISTS, same as the main log schema. Versions
// live in their own table so the live log and the history are fully
// independent: Flush / ClearDocument never touch versions, and
// DeleteVersion never touches the log.
const versionsSchemaSQL = `
CREATE TABLE IF NOT EXISTS ygo_versions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_name TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    state BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ygo_versions_doc_name ON ygo_versions(doc_name, id);
`

// Compile-time guarantee that *Store satisfies the versioned-history
// extension alongside the base contract.
var _ persist.VersionedStore = (*Store)(nil)

// SaveVersion stores state as a new version of docName and returns
// its ID. The creation timestamp is stamped here (UTC, second
// precision).
func (s *Store) SaveVersion(ctx context.Context, docName, label string, state []byte) (int64, error) {
	if len(state) == 0 {
		return 0, persist.ErrEmptyUpdate
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ygo_versions (doc_name, label, created_at, state) VALUES (?, ?, ?, ?)`,
		docName, label, time.Now().UTC().Unix(), state)
	if err != nil {
		return 0, fmt.Errorf("sqlite.SaveVersion(%q): %w", docName, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite.SaveVersion(%q) id: %w", docName, err)
	}
	return id, nil
}

// ListVersions returns the metadata of every version of docName,
// oldest first. (nil, nil) when the document has no versions.
func (s *Store) ListVersions(ctx context.Context, docName string) ([]persist.VersionInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, label, created_at, length(state) FROM ygo_versions
		 WHERE doc_name = ? ORDER BY id ASC`, docName)
	if err != nil {
		return nil, fmt.Errorf("sqlite.ListVersions(%q): %w", docName, err)
	}
	defer rows.Close()

	var out []persist.VersionInfo
	for rows.Next() {
		var info persist.VersionInfo
		var createdAt int64
		if err := rows.Scan(&info.ID, &info.Label, &createdAt, &info.Size); err != nil {
			return nil, fmt.Errorf("sqlite.ListVersions(%q) scan: %w", docName, err)
		}
		info.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite.ListVersions(%q) iterate: %w", docName, err)
	}
	return out, nil
}

// GetVersionState returns the state blob of one version.
func (s *Store) GetVersionState(ctx context.Context, docName string, versionID int64) ([]byte, error) {
	var state []byte
	row := s.db.QueryRowContext(ctx,
		`SELECT state FROM ygo_versions WHERE doc_name = ? AND id = ?`,
		docName, versionID)
	err := row.Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, persist.ErrVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite.GetVersionState(%q, %d): %w", docName, versionID, err)
	}
	return state, nil
}

// DeleteVersion removes one version. Idempotent on unknown versions.
func (s *Store) DeleteVersion(ctx context.Context, docName string, versionID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM ygo_versions WHERE doc_name = ? AND id = ?`,
		docName, versionID)
	if err != nil {
		return fmt.Errorf("sqlite.DeleteVersion(%q, %d): %w", docName, versionID, err)
	}
	return nil
}

// RestoreVersion replaces docName's live update log with the version's
// state blob, inside one write transaction opened with BEGIN IMMEDIATE:
// concurrent readers and writers see either the old log or the restored
// single-blob log, never a torn intermediate state, and a concurrent
// StoreUpdate blocks (bounded by busy_timeout) rather than failing the
// restore. Like Flush, a deferred transaction here would take only a read
// snapshot for the SELECT and then fail the DELETE's writer upgrade with
// SQLITE_BUSY_SNAPSHOT — which busy_timeout does not service — whenever a
// write committed in the gap.
//
// The restored log starts a new history for connected clients: the
// document's state vector regresses to the version's, so live sync
// sessions must resynchronize from scratch. Intended for
// administrative restore, not for use mid-session.
func (s *Store) RestoreVersion(ctx context.Context, docName string, versionID int64) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite.RestoreVersion(%q, %d) conn: %w", docName, versionID, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite.RestoreVersion(%q, %d) begin: %w", docName, versionID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var state []byte
	row := conn.QueryRowContext(ctx,
		`SELECT state FROM ygo_versions WHERE doc_name = ? AND id = ?`,
		docName, versionID)
	err = row.Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return persist.ErrVersionNotFound
	}
	if err != nil {
		return fmt.Errorf("sqlite.RestoreVersion(%q, %d) read: %w", docName, versionID, err)
	}

	if _, err := conn.ExecContext(ctx,
		`DELETE FROM ygo_updates WHERE doc_name = ?`, docName); err != nil {
		return fmt.Errorf("sqlite.RestoreVersion(%q, %d) clear: %w", docName, versionID, err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO ygo_updates (doc_name, update_blob) VALUES (?, ?)`,
		docName, state); err != nil {
		return fmt.Errorf("sqlite.RestoreVersion(%q, %d) insert: %w", docName, versionID, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqlite.RestoreVersion(%q, %d) commit: %w", docName, versionID, err)
	}
	committed = true
	return nil
}
