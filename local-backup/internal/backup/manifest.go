package backup

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Manifest is the SQLite catalogue of snapshots, entries and chunk references.
type Manifest struct {
	db *sql.DB
}

func OpenManifest(path string) (*Manifest, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	m := &Manifest{db: db}
	if err := m.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manifest) Close() error { return m.db.Close() }

func (m *Manifest) migrate() error {
	_, err := m.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS snapshots (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    root        TEXT NOT NULL,
    state       TEXT NOT NULL,           -- complete | incomplete | interrupted
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    finished_at TEXT,
    files       INTEGER NOT NULL DEFAULT 0,
    dirs        INTEGER NOT NULL DEFAULT 0,
    links       INTEGER NOT NULL DEFAULT 0,
    bytes       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS entries (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    relpath     TEXT NOT NULL,
    kind        TEXT NOT NULL,
    mode        INTEGER NOT NULL,
    uid         INTEGER NOT NULL,
    gid         INTEGER NOT NULL,
    mtime       TEXT NOT NULL,
    size        INTEGER NOT NULL DEFAULT 0,
    sha256      BLOB,
    link_target TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'valid'
);
CREATE INDEX IF NOT EXISTS idx_entries_snap ON entries(snapshot_id);
CREATE TABLE IF NOT EXISTS chunks (
    id         TEXT PRIMARY KEY,   -- sha256 hex of the chunk payload
    size       INTEGER NOT NULL,
    created_by INTEGER REFERENCES snapshots(id)
);
CREATE TABLE IF NOT EXISTS snapshot_chunks (
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id),
    chunk_id    TEXT NOT NULL REFERENCES chunks(id),
    PRIMARY KEY (snapshot_id, chunk_id)
);
CREATE TABLE IF NOT EXISTS file_chunks (
    entry_id  INTEGER NOT NULL REFERENCES entries(id),
    chunk_id  TEXT NOT NULL REFERENCES chunks(id),
    offset    INTEGER NOT NULL,
    length    INTEGER NOT NULL,
    seq       INTEGER NOT NULL,
    PRIMARY KEY (entry_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_filechunks_chunk ON file_chunks(chunk_id);
`)
	return err
}

// Poly returns the chunking polynomial, or 0 if not initialized yet.
func (m *Manifest) Poly() (uint64, error) {
	var s string
	err := m.db.QueryRow(`SELECT value FROM meta WHERE key='chunker_poly'`).Scan(&s)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("stored polynomial corrupt: %w", err)
	}
	return v, nil
}

func (m *Manifest) SetPoly(poly uint64) error {
	_, err := m.db.Exec(`INSERT OR IGNORE INTO meta(key,value) VALUES('chunker_poly', ?)`,
		fmt.Sprintf("%016x", poly))
	return err
}

// StartSnapshot inserts a snapshot row in the "interrupted" state up front,
// so that a crash mid-backup leaves a row a maintainer can diagnose.
func (m *Manifest) StartSnapshot(root string) (int64, error) {
	res, err := m.db.Exec(
		`INSERT INTO snapshots(root, state, created_at) VALUES(?, ?, ?)`,
		root, StatusInterrupted, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (m *Manifest) InsertEntry(tx *sql.Tx, e *Entry) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO entries(snapshot_id, relpath, kind, mode, uid, gid, mtime, size, sha256, link_target, state)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.SnapshotID, e.RelPath, e.Kind, e.Mode, e.UID, e.GID,
		e.Mtime.UTC().Format(time.RFC3339Nano), e.Size, e.Sha256, e.LinkTarget, e.State)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ChunkKnown reports whether a chunk is already present in the manifest.
func (m *Manifest) ChunkKnown(id string) (bool, error) {
	var n int
	if err := m.db.QueryRow(`SELECT count(*) FROM chunks WHERE id=?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (m *Manifest) InsertChunk(tx *sql.Tx, id string, size int64, snapshotID int64) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO chunks(id,size,created_by) VALUES(?,?,?)`, id, size, snapshotID)
	return err
}

func (m *Manifest) LinkSnapshotChunk(tx *sql.Tx, snapshotID int64, chunkID string) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO snapshot_chunks(snapshot_id, chunk_id) VALUES(?,?)`,
		snapshotID, chunkID)
	return err
}

func (m *Manifest) InsertFileChunk(tx *sql.Tx, entryID int64, seq int, c ChunkRef) error {
	_, err := tx.Exec(
		`INSERT INTO file_chunks(entry_id, chunk_id, offset, length, seq) VALUES(?,?,?,?,?)`,
		entryID, c.ChunkID, c.Offset, c.Length, seq)
	return err
}

func (m *Manifest) UpdateCounters(snapshotID int64, files, dirs, links int, bytes int64) error {
	_, err := m.db.Exec(
		`UPDATE snapshots SET files=?, dirs=?, links=?, bytes=? WHERE id=?`,
		files, dirs, links, bytes, snapshotID)
	return err
}

// FinalizeSnapshot marks the run complete or incomplete. The state is only
// ever "complete" when the caller has verified every referenced chunk.
func (m *Manifest) FinalizeSnapshot(snapshotID int64, state, reason string, files, dirs, links int, bytes int64) error {
	_, err := m.db.Exec(
		`UPDATE snapshots SET state=?, reason=?, finished_at=?, files=?, dirs=?, links=?, bytes=? WHERE id=?`,
		state, reason, time.Now().UTC().Format(time.RFC3339Nano), files, dirs, links, bytes, snapshotID)
	return err
}

func (m *Manifest) GetSnapshot(id int64) (*Snapshot, error) {
	s := &Snapshot{}
	var created, finished sql.NullString
	err := m.db.QueryRow(
		`SELECT id, root, state, reason, created_at, finished_at, files, dirs, links, bytes
		 FROM snapshots WHERE id=?`, id).
		Scan(&s.ID, &s.Root, &s.State, &s.Reason, &created, &finished,
			&s.Files, &s.Dirs, &s.Links, &s.Bytes)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("snapshot %d not found", id)
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, created.String)
	if finished.Valid {
		t, _ := time.Parse(time.RFC3339Nano, finished.String)
		s.FinishedAt = &t
	}
	return s, nil
}

func (m *Manifest) ListSnapshots() ([]Snapshot, error) {
	rows, err := m.db.Query(
		`SELECT id, root, state, reason, created_at, finished_at, files, dirs, links, bytes
		 FROM snapshots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var s Snapshot
		var created, finished sql.NullString
		if err := rows.Scan(&s.ID, &s.Root, &s.State, &s.Reason, &created, &finished,
			&s.Files, &s.Dirs, &s.Links, &s.Bytes); err != nil {
			return nil, err
		}
		s.CreatedAt, _ = time.Parse(time.RFC3339Nano, created.String)
		if finished.Valid {
			t, _ := time.Parse(time.RFC3339Nano, finished.String)
			s.FinishedAt = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// EntriesOf returns all entries of a snapshot ordered by path.
func (m *Manifest) EntriesOf(snapshotID int64) ([]Entry, error) {
	rows, err := m.db.Query(
		`SELECT id, snapshot_id, relpath, kind, mode, uid, gid, mtime, size, sha256, link_target, state
		 FROM entries WHERE snapshot_id=? ORDER BY relpath`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var sha sql.Null[string]
		var mt string
		if err := rows.Scan(&e.ID, &e.SnapshotID, &e.RelPath, &e.Kind, &e.Mode, &e.UID, &e.GID,
			&mt, &e.Size, &sha, &e.LinkTarget, &e.State); err != nil {
			return nil, err
		}
		e.Mtime, _ = time.Parse(time.RFC3339Nano, mt)
		if sha.Valid {
			e.Sha256 = []byte(sha.V)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FileChunksOf returns the ordered chunk list for one file entry.
func (m *Manifest) FileChunksOf(entryID int64) ([]ChunkRef, error) {
	rows, err := m.db.Query(
		`SELECT entry_id, chunk_id, offset, length FROM file_chunks
		 WHERE entry_id=? ORDER BY seq`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkRef
	for rows.Next() {
		var c ChunkRef
		if err := rows.Scan(&c.EntryID, &c.ChunkID, &c.Offset, &c.Length); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SnapshotChunkIDs lists every distinct chunk referenced by the snapshot.
func (m *Manifest) SnapshotChunkIDs(snapshotID int64) ([]string, error) {
	rows, err := m.db.Query(
		`SELECT chunk_id FROM snapshot_chunks WHERE snapshot_id=? ORDER BY chunk_id`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MissingChunksForSnapshot joins chunk references with entries, so the report
// can say not just "chunk X missing" but which files of the snapshot need it.
func (m *Manifest) MissingChunksForSnapshot(snapshotID int64, exists func(string) bool) ([]MissingChunk, error) {
	rows, err := m.db.Query(
		`SELECT fc.chunk_id, e.relpath
		 FROM file_chunks fc
		 JOIN entries e ON e.id = fc.entry_id
		 WHERE e.snapshot_id = ?
		 ORDER BY fc.chunk_id, e.relpath`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byID := map[string][]string{}
	var order []string
	for rows.Next() {
		var id, rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return nil, err
		}
		if exists(id) {
			continue
		}
		if _, ok := byID[id]; !ok {
			order = append(order, id)
		}
		byID[id] = append(byID[id], rel)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]MissingChunk, 0, len(order))
	for _, id := range order {
		out = append(out, MissingChunk{ChunkID: id, Entries: dedupe(byID[id])})
	}
	return out, nil
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// begin exposes a transaction for the backup/restore flows.
func (m *Manifest) begin() (*sql.Tx, error) { return m.db.Begin() }

func (m *Manifest) dbQuerySize(id string, n *int64) error {
	return m.db.QueryRow(`SELECT size FROM chunks WHERE id=?`, id).Scan(n)
}

func (m *Manifest) chunkSizeQ(id string) (int64, error) {
	var n int64
	if err := m.dbQuerySize(id, &n); err != nil {
		return 0, err
	}
	return n, nil
}

var _ = strings.TrimSpace // keep strings imported for future query builders
