// Package store persists Fliporium's chat history and peer roster in SQLite.
//
// Uses modernc.org/sqlite (pure Go, no cgo) so the binary stays single-step
// to build. Timestamps are stored as RFC3339Nano strings — easier to inspect
// by hand and avoids gotchas with SQLite's lack of a real date type.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Direction of a stored message relative to the local node.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// Message is one persisted chat line.
type Message struct {
	ID        int64
	Peer      string
	Direction string
	Text      string
	At        time.Time
}

// PeerRecord is what we remember about a peer between sessions.
type PeerRecord struct {
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
}

// FlipStatus tracks where a transfer is in its lifecycle.
const (
	FlipStatusStarted   = "started"
	FlipStatusReceiving = "receiving"
	FlipStatusComplete  = "complete"
	FlipStatusFailed    = "failed"
	FlipStatusCancelled = "cancelled"
)

// FlipRecord is a row in the flips table.
type FlipRecord struct {
	ID          string
	Peer        string
	Direction   string // "in" or "out"
	Filename    string
	Size        int64
	Mime        string
	Path        string // absolute path on this machine
	Sha256      string // empty until completed
	Status      string
	StartedAt   time.Time
	CompletedAt time.Time // zero if not yet complete
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens or creates store.db inside dir. Creates the dir if missing.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", dir, err)
	}
	path := filepath.Join(dir, "store.db")
	// WAL gives concurrent readers without blocking the single writer.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %q: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite at %q: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS peers (
    name       TEXT PRIMARY KEY,
    first_seen TEXT NOT NULL,
    last_seen  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    peer      TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('in', 'out')),
    text      TEXT NOT NULL,
    at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_peer_at ON messages(peer, at);

CREATE TABLE IF NOT EXISTS flips (
    id           TEXT PRIMARY KEY,
    peer         TEXT NOT NULL,
    direction    TEXT NOT NULL CHECK (direction IN ('in', 'out')),
    filename     TEXT NOT NULL,
    size         INTEGER NOT NULL,
    mime         TEXT,
    path         TEXT NOT NULL,
    sha256       TEXT,
    status       TEXT NOT NULL,
    started_at   TEXT NOT NULL,
    completed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_flips_peer_started ON flips(peer, started_at);
`

func migrate(db *sql.DB) error {
	_, err := db.Exec(schema)
	return err
}

// UpsertPeer marks the peer as seen now; sets first_seen on first contact.
func (s *Store) UpsertPeer(ctx context.Context, name string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO peers (name, first_seen, last_seen) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET last_seen = excluded.last_seen
	`, name, now, now)
	return err
}

// Peers returns the known peer roster, most-recently-seen first.
func (s *Store) Peers(ctx context.Context) ([]PeerRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, first_seen, last_seen FROM peers ORDER BY last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerRecord
	for rows.Next() {
		var p PeerRecord
		var first, last string
		if err := rows.Scan(&p.Name, &first, &last); err != nil {
			return nil, err
		}
		p.FirstSeen, _ = time.Parse(time.RFC3339Nano, first)
		p.LastSeen, _ = time.Parse(time.RFC3339Nano, last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AppendMessage stores a single chat line.
func (s *Store) AppendMessage(ctx context.Context, peer, direction, text string, at time.Time) error {
	if direction != DirectionIn && direction != DirectionOut {
		return fmt.Errorf("invalid direction %q", direction)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (peer, direction, text, at) VALUES (?, ?, ?, ?)
	`, peer, direction, text, at.UTC().Format(time.RFC3339Nano))
	return err
}

// AppendFlip records a brand-new transfer (status = started).
func (s *Store) AppendFlip(ctx context.Context, f FlipRecord) error {
	if f.Direction != DirectionIn && f.Direction != DirectionOut {
		return fmt.Errorf("invalid flip direction %q", f.Direction)
	}
	if f.Status == "" {
		f.Status = FlipStatusStarted
	}
	if f.StartedAt.IsZero() {
		f.StartedAt = time.Now().UTC()
	}
	var completedAt *string
	if !f.CompletedAt.IsZero() {
		s := f.CompletedAt.UTC().Format(time.RFC3339Nano)
		completedAt = &s
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO flips (id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			filename=excluded.filename,
			size=excluded.size,
			mime=excluded.mime,
			path=excluded.path,
			sha256=excluded.sha256,
			status=excluded.status,
			completed_at=excluded.completed_at
	`,
		f.ID, f.Peer, f.Direction, f.Filename, f.Size, f.Mime, f.Path, f.Sha256, f.Status,
		f.StartedAt.UTC().Format(time.RFC3339Nano), completedAt)
	return err
}

// UpdateFlipStatus changes the status (and optionally completion fields) of a flip.
func (s *Store) UpdateFlipStatus(ctx context.Context, id, status, sha256 string, completedAt time.Time) error {
	var completed *string
	if !completedAt.IsZero() {
		s := completedAt.UTC().Format(time.RFC3339Nano)
		completed = &s
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE flips SET status = ?, sha256 = COALESCE(NULLIF(?, ''), sha256), completed_at = COALESCE(?, completed_at)
		WHERE id = ?
	`, status, sha256, completed, id)
	return err
}

// FlipsByPeer returns flips with the given peer, oldest first.
func (s *Store) FlipsByPeer(ctx context.Context, peer string) ([]FlipRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at
		FROM flips WHERE peer = ? ORDER BY started_at ASC
	`, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlipRecord
	for rows.Next() {
		var f FlipRecord
		var mime, sha sql.NullString
		var startedAt string
		var completedAt sql.NullString
		if err := rows.Scan(&f.ID, &f.Peer, &f.Direction, &f.Filename, &f.Size, &mime, &f.Path, &sha, &f.Status, &startedAt, &completedAt); err != nil {
			return nil, err
		}
		f.Mime = mime.String
		f.Sha256 = sha.String
		f.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAt)
		if completedAt.Valid {
			f.CompletedAt, _ = time.Parse(time.RFC3339Nano, completedAt.String)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFlip looks up a single flip by id.
func (s *Store) GetFlip(ctx context.Context, id string) (FlipRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at
		FROM flips WHERE id = ?
	`, id)
	var f FlipRecord
	var mime, sha sql.NullString
	var startedAt string
	var completedAt sql.NullString
	if err := row.Scan(&f.ID, &f.Peer, &f.Direction, &f.Filename, &f.Size, &mime, &f.Path, &sha, &f.Status, &startedAt, &completedAt); err != nil {
		return FlipRecord{}, err
	}
	f.Mime = mime.String
	f.Sha256 = sha.String
	f.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAt)
	if completedAt.Valid {
		f.CompletedAt, _ = time.Parse(time.RFC3339Nano, completedAt.String)
	}
	return f, nil
}

// Messages returns the last `limit` messages with a given peer, oldest first.
// limit <= 0 means "all".
func (s *Store) Messages(ctx context.Context, peer string, limit int) ([]Message, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		// Get last N (newest), then reverse to oldest-first for chat display.
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, peer, direction, text, at FROM (
				SELECT id, peer, direction, text, at
				FROM messages WHERE peer = ?
				ORDER BY id DESC LIMIT ?
			) ORDER BY id ASC
		`, peer, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, peer, direction, text, at FROM messages
			WHERE peer = ? ORDER BY id ASC
		`, peer)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var atStr string
		if err := rows.Scan(&m.ID, &m.Peer, &m.Direction, &m.Text, &atStr); err != nil {
			return nil, err
		}
		m.At, _ = time.Parse(time.RFC3339Nano, atStr)
		out = append(out, m)
	}
	return out, rows.Err()
}
