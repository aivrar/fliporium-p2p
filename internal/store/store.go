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
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Direction of a stored message relative to the local node.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// Message is one persisted chat line.
//
// For a 1:1 conversation BoothID is empty and Peer is the conversation
// partner. For a Booth message BoothID is set and Peer is the *sender*
// (which can be us, in which case Direction is "out").
//
// UUID is the sender-assigned identifier used by reactions / edits / deletes /
// replies. Legacy rows from before v0.10 have an empty UUID and can't be
// addressed by those operations.
type Message struct {
	ID         int64
	UUID       string
	Peer       string
	Direction  string
	Text       string
	At         time.Time
	BoothID    string
	ParentUUID string
	EditedAt   time.Time // zero if never edited
	DeletedAt  time.Time // zero if not tombstoned
	Pinned     bool
}

// Reaction is one (message, peer, emoji) tuple. The same peer reacting with
// the same emoji twice is idempotent.
type Reaction struct {
	MessageUUID string
	Peer        string
	Emoji       string
	At          time.Time
}

// PeerRecord is what we remember about a peer between sessions.
type PeerRecord struct {
	Name      string
	Display   string // friendly name announced via HELLO ("" if unknown)
	FirstSeen time.Time
	LastSeen  time.Time
}

// Booth is a named multi-peer chat room. Each peer keeps its own copy of the
// booth + member list; the founder seeds it via BOOTH_INVITE messages.
type Booth struct {
	ID        string
	Name      string
	Founder   string // peer name of the creator
	FoundedAt time.Time
	Motto     string
}

// BoothMember pairs a peer name with the booth it joined.
type BoothMember struct {
	BoothID  string
	PeerName string
	JoinedAt time.Time
}

// BoothNotepad is the single shared text document per booth (Phase 8 v0.1).
// Conflict resolution is last-write-wins by Version; v0.2 will swap this for
// a CRDT (Y.js or Automerge) to support real-time concurrent editing.
type BoothNotepad struct {
	BoothID      string
	Text         string
	Version      int64
	LastEditor   string
	LastModified time.Time
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
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    peer         TEXT NOT NULL,
    direction    TEXT NOT NULL CHECK (direction IN ('in', 'out')),
    text         TEXT NOT NULL,
    at           TEXT NOT NULL,
    booth_id     TEXT,
    uuid         TEXT,
    parent_uuid  TEXT,
    edited_at    TEXT,
    deleted_at   TEXT,
    pinned       INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_messages_peer_at  ON messages(peer, at);
CREATE INDEX IF NOT EXISTS idx_messages_booth_at ON messages(booth_id, at);
CREATE INDEX IF NOT EXISTS idx_messages_uuid     ON messages(uuid) WHERE uuid IS NOT NULL;

CREATE TABLE IF NOT EXISTS message_reactions (
    message_uuid TEXT NOT NULL,
    peer         TEXT NOT NULL,
    emoji        TEXT NOT NULL,
    at           TEXT NOT NULL,
    PRIMARY KEY (message_uuid, peer, emoji)
);
CREATE INDEX IF NOT EXISTS idx_reactions_message ON message_reactions(message_uuid);

-- Full-text search over message text. Contentless mode so we control sync.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(text, content='messages', content_rowid='id');

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES('delete', old.id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, text) VALUES('delete', old.id, old.text);
    INSERT INTO messages_fts(rowid, text) VALUES (new.id, new.text);
END;

CREATE TABLE IF NOT EXISTS booths (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    founder    TEXT NOT NULL,
    founded_at TEXT NOT NULL,
    motto      TEXT
);

CREATE TABLE IF NOT EXISTS booth_members (
    booth_id  TEXT NOT NULL,
    peer_name TEXT NOT NULL,
    joined_at TEXT NOT NULL,
    PRIMARY KEY (booth_id, peer_name)
);

CREATE TABLE IF NOT EXISTS booth_notepads (
    booth_id      TEXT PRIMARY KEY,
    text          TEXT NOT NULL,
    version       INTEGER NOT NULL,
    last_editor   TEXT NOT NULL,
    last_modified TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

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
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Idempotent ALTERs for stores that predate later phases. SQLite errors
	// with "duplicate column name" if the column is already there, which we
	// catch and ignore.
	alters := []string{
		`ALTER TABLE messages ADD COLUMN booth_id TEXT`,        // v0.6
		`ALTER TABLE messages ADD COLUMN uuid TEXT`,            // v0.10
		`ALTER TABLE messages ADD COLUMN parent_uuid TEXT`,     // v0.10
		`ALTER TABLE messages ADD COLUMN edited_at TEXT`,       // v0.10
		`ALTER TABLE messages ADD COLUMN deleted_at TEXT`,      // v0.10
		`ALTER TABLE messages ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0`, // v0.10
		`ALTER TABLE peers ADD COLUMN display TEXT`,                         // v0.12 friendly names
	}
	for _, stmt := range alters {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	// Backfill the FTS index from any pre-existing messages that aren't
	// already indexed (only matters when upgrading a pre-FTS store; on a fresh
	// store messages_fts and messages are both empty).
	if _, err := db.Exec(`
		INSERT INTO messages_fts(rowid, text)
		SELECT id, text FROM messages
		WHERE NOT EXISTS (SELECT 1 FROM messages_fts WHERE rowid = messages.id)
	`); err != nil {
		return fmt.Errorf("backfill messages_fts: %w", err)
	}
	return nil
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

// SetPeerDisplay records a peer's friendly display name (no-op if display is
// empty, so we never clobber a known name with a blank one).
func (s *Store) SetPeerDisplay(ctx context.Context, name, display string) error {
	if display == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO peers (name, first_seen, last_seen, display) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET display = excluded.display, last_seen = excluded.last_seen
	`, name, now, now, display)
	return err
}

// Peers returns the known peer roster, most-recently-seen first.
func (s *Store) Peers(ctx context.Context) ([]PeerRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, COALESCE(display, ''), first_seen, last_seen FROM peers ORDER BY last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerRecord
	for rows.Next() {
		var p PeerRecord
		var first, last string
		if err := rows.Scan(&p.Name, &p.Display, &first, &last); err != nil {
			return nil, err
		}
		p.FirstSeen, _ = time.Parse(time.RFC3339Nano, first)
		p.LastSeen, _ = time.Parse(time.RFC3339Nano, last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PeerDisplays returns a name->display map for resolving friendly names.
func (s *Store) PeerDisplays(ctx context.Context) (map[string]string, error) {
	peers, err := s.Peers(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(peers))
	for _, p := range peers {
		if p.Display != "" {
			m[p.Name] = p.Display
		}
	}
	return m, nil
}

// AppendMessage stores a single 1:1 chat line. boothID + UUID + parentUUID
// are all empty.
func (s *Store) AppendMessage(ctx context.Context, peer, direction, text string, at time.Time) error {
	return s.AppendMessageFull(ctx, Message{Peer: peer, Direction: direction, Text: text, At: at})
}

// AppendMessageBooth is the booth-aware variant; UUID + parentUUID empty.
func (s *Store) AppendMessageBooth(ctx context.Context, peer, direction, text, boothID string, at time.Time) error {
	return s.AppendMessageFull(ctx, Message{Peer: peer, Direction: direction, Text: text, At: at, BoothID: boothID})
}

// AppendMessageFull persists a Message including UUID and ParentUUID.
// Idempotent on UUID: if a row with this UUID already exists, no-op (returns nil).
func (s *Store) AppendMessageFull(ctx context.Context, m Message) error {
	if m.Direction != DirectionIn && m.Direction != DirectionOut {
		return fmt.Errorf("invalid direction %q", m.Direction)
	}
	var boothCol, uuidCol, parentCol *string
	if m.BoothID != "" {
		boothCol = &m.BoothID
	}
	if m.UUID != "" {
		uuidCol = &m.UUID
		// Idempotency: skip if we already have this row by UUID.
		var existing int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE uuid = ?`, m.UUID).Scan(&existing); err == nil {
			return nil
		}
	}
	if m.ParentUUID != "" {
		parentCol = &m.ParentUUID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (peer, direction, text, at, booth_id, uuid, parent_uuid)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, m.Peer, m.Direction, m.Text, m.At.UTC().Format(time.RFC3339Nano), boothCol, uuidCol, parentCol)
	return err
}

// ApplyMessageEdit replaces text on the message with the given UUID and
// stamps edited_at. No-op if no such message. Returns whether a row was
// updated.
func (s *Store) ApplyMessageEdit(ctx context.Context, uuid, newText string, editedAt time.Time) (bool, error) {
	if uuid == "" {
		return false, fmt.Errorf("uuid required")
	}
	if editedAt.IsZero() {
		editedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE messages SET text = ?, edited_at = ?
		WHERE uuid = ? AND (edited_at IS NULL OR edited_at < ?)
	`, newText, editedAt.UTC().Format(time.RFC3339Nano), uuid, editedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ApplyMessageDelete tombstones the message with the given UUID.
func (s *Store) ApplyMessageDelete(ctx context.Context, uuid string, deletedAt time.Time) (bool, error) {
	if uuid == "" {
		return false, fmt.Errorf("uuid required")
	}
	if deletedAt.IsZero() {
		deletedAt = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE messages SET deleted_at = ? WHERE uuid = ? AND deleted_at IS NULL
	`, deletedAt.UTC().Format(time.RFC3339Nano), uuid)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetMessagePinned toggles the pinned flag on a message by UUID.
func (s *Store) SetMessagePinned(ctx context.Context, uuid string, pinned bool) error {
	if uuid == "" {
		return fmt.Errorf("uuid required")
	}
	val := 0
	if pinned {
		val = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET pinned = ? WHERE uuid = ?`, val, uuid)
	return err
}

// AddReaction is idempotent — if (uuid, peer, emoji) already exists, no-op.
func (s *Store) AddReaction(ctx context.Context, r Reaction) error {
	if r.MessageUUID == "" || r.Peer == "" || r.Emoji == "" {
		return fmt.Errorf("uuid, peer, emoji required")
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO message_reactions (message_uuid, peer, emoji, at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (message_uuid, peer, emoji) DO NOTHING
	`, r.MessageUUID, r.Peer, r.Emoji, r.At.UTC().Format(time.RFC3339Nano))
	return err
}

// RemoveReaction removes a (uuid, peer, emoji) reaction row.
func (s *Store) RemoveReaction(ctx context.Context, messageUUID, peer, emoji string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM message_reactions WHERE message_uuid = ? AND peer = ? AND emoji = ?
	`, messageUUID, peer, emoji)
	return err
}

// ReactionsForMessages bulk-loads reactions for a list of message UUIDs.
// Returns a map keyed by message UUID.
func (s *Store) ReactionsForMessages(ctx context.Context, uuids []string) (map[string][]Reaction, error) {
	out := map[string][]Reaction{}
	if len(uuids) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(uuids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(uuids))
	for i, u := range uuids {
		args[i] = u
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT message_uuid, peer, emoji, at FROM message_reactions WHERE message_uuid IN (`+placeholders+`) ORDER BY at ASC`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Reaction
		var atStr string
		if err := rows.Scan(&r.MessageUUID, &r.Peer, &r.Emoji, &atStr); err != nil {
			return nil, err
		}
		r.At, _ = time.Parse(time.RFC3339Nano, atStr)
		out[r.MessageUUID] = append(out[r.MessageUUID], r)
	}
	return out, rows.Err()
}

// SearchHit is one full-text result with snippet highlighting context.
type SearchHit struct {
	Message Message
	Snippet string // text fragment with the matching tokens
}

// SearchMessages does a full-text search over message bodies, ranked by FTS5's
// bm25 algorithm. Excludes tombstoned messages.
func (s *Store) SearchMessages(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, COALESCE(m.uuid,''), m.peer, m.direction, m.text, m.at,
		       COALESCE(m.booth_id,''), COALESCE(m.parent_uuid,''),
		       COALESCE(m.edited_at,''), COALESCE(m.deleted_at,''), COALESCE(m.pinned, 0),
		       snippet(messages_fts, 0, '<mark>', '</mark>', '...', 12)
		FROM messages_fts
		JOIN messages m ON m.id = messages_fts.rowid
		WHERE messages_fts MATCH ? AND m.deleted_at IS NULL
		ORDER BY bm25(messages_fts)
		LIMIT ?
	`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var m Message
		var atStr, editedStr, deletedStr, snippet string
		var pinnedInt int
		if err := rows.Scan(&m.ID, &m.UUID, &m.Peer, &m.Direction, &m.Text, &atStr,
			&m.BoothID, &m.ParentUUID, &editedStr, &deletedStr, &pinnedInt, &snippet); err != nil {
			return nil, err
		}
		m.At, _ = time.Parse(time.RFC3339Nano, atStr)
		if editedStr != "" {
			m.EditedAt, _ = time.Parse(time.RFC3339Nano, editedStr)
		}
		if deletedStr != "" {
			m.DeletedAt, _ = time.Parse(time.RFC3339Nano, deletedStr)
		}
		m.Pinned = pinnedInt != 0
		out = append(out, SearchHit{Message: m, Snippet: snippet})
	}
	return out, rows.Err()
}

// FindMessageByUUID looks up a single message; useful for the edit/delete
// sender-verification check and for jump-to-message search results.
func (s *Store) FindMessageByUUID(ctx context.Context, uuid string) (Message, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id,''),
		       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''),
		       COALESCE(pinned, 0)
		FROM messages WHERE uuid = ?
	`, uuid)
	return scanFullMessage(row)
}

// scanFullMessage scans one full Message row.
func scanFullMessage(row *sql.Row) (Message, error) {
	var m Message
	var atStr, editedStr, deletedStr string
	var pinnedInt int
	if err := row.Scan(&m.ID, &m.UUID, &m.Peer, &m.Direction, &m.Text, &atStr, &m.BoothID, &m.ParentUUID, &editedStr, &deletedStr, &pinnedInt); err != nil {
		return Message{}, err
	}
	m.At, _ = time.Parse(time.RFC3339Nano, atStr)
	if editedStr != "" {
		m.EditedAt, _ = time.Parse(time.RFC3339Nano, editedStr)
	}
	if deletedStr != "" {
		m.DeletedAt, _ = time.Parse(time.RFC3339Nano, deletedStr)
	}
	m.Pinned = pinnedInt != 0
	return m, nil
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

// Messages returns the last `limit` 1:1 messages with a given peer (booth_id is NULL).
// limit <= 0 means "all".
func (s *Store) Messages(ctx context.Context, peer string, limit int) ([]Message, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0)
			FROM (
				SELECT id, uuid, peer, direction, text, at, booth_id, parent_uuid, edited_at, deleted_at, pinned
				FROM messages WHERE peer = ? AND booth_id IS NULL
				ORDER BY id DESC LIMIT ?
			) ORDER BY id ASC
		`, peer, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0)
			FROM messages
			WHERE peer = ? AND booth_id IS NULL ORDER BY id ASC
		`, peer)
	}
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// MessagesByBooth returns the last `limit` messages in a Booth, oldest first.
// Includes messages from every sender (the "peer" column on each row is the sender).
func (s *Store) MessagesByBooth(ctx context.Context, boothID string, limit int) ([]Message, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0)
			FROM (
				SELECT id, uuid, peer, direction, text, at, booth_id, parent_uuid, edited_at, deleted_at, pinned
				FROM messages WHERE booth_id = ?
				ORDER BY id DESC LIMIT ?
			) ORDER BY id ASC
		`, boothID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0)
			FROM messages
			WHERE booth_id = ? ORDER BY id ASC
		`, boothID)
	}
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var atStr, editedStr, deletedStr string
		var pinnedInt int
		if err := rows.Scan(&m.ID, &m.UUID, &m.Peer, &m.Direction, &m.Text, &atStr, &m.BoothID, &m.ParentUUID, &editedStr, &deletedStr, &pinnedInt); err != nil {
			return nil, err
		}
		m.At, _ = time.Parse(time.RFC3339Nano, atStr)
		if editedStr != "" {
			m.EditedAt, _ = time.Parse(time.RFC3339Nano, editedStr)
		}
		if deletedStr != "" {
			m.DeletedAt, _ = time.Parse(time.RFC3339Nano, deletedStr)
		}
		m.Pinned = pinnedInt != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpsertBooth creates or updates a Booth row. Idempotent on (id).
func (s *Store) UpsertBooth(ctx context.Context, b Booth) error {
	if b.ID == "" || b.Name == "" {
		return fmt.Errorf("booth id and name required")
	}
	if b.FoundedAt.IsZero() {
		b.FoundedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO booths (id, name, founder, founded_at, motto) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			founder=excluded.founder,
			motto=excluded.motto
	`, b.ID, b.Name, b.Founder, b.FoundedAt.UTC().Format(time.RFC3339Nano), b.Motto)
	return err
}

// AddBoothMember inserts a (booth_id, peer_name) row idempotently.
func (s *Store) AddBoothMember(ctx context.Context, boothID, peerName string, joinedAt time.Time) error {
	if joinedAt.IsZero() {
		joinedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO booth_members (booth_id, peer_name, joined_at) VALUES (?, ?, ?)
		ON CONFLICT(booth_id, peer_name) DO NOTHING
	`, boothID, peerName, joinedAt.UTC().Format(time.RFC3339Nano))
	return err
}

// RemoveBoothMember deletes a member row.
func (s *Store) RemoveBoothMember(ctx context.Context, boothID, peerName string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM booth_members WHERE booth_id = ? AND peer_name = ?`, boothID, peerName)
	return err
}

// ListBooths returns every booth in the store, newest first.
func (s *Store) ListBooths(ctx context.Context) ([]Booth, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, founder, founded_at, COALESCE(motto, '') FROM booths
		ORDER BY founded_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Booth
	for rows.Next() {
		var b Booth
		var foundedStr string
		if err := rows.Scan(&b.ID, &b.Name, &b.Founder, &foundedStr, &b.Motto); err != nil {
			return nil, err
		}
		b.FoundedAt, _ = time.Parse(time.RFC3339Nano, foundedStr)
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBooth returns a single booth by id.
func (s *Store) GetBooth(ctx context.Context, id string) (Booth, error) {
	var b Booth
	var foundedStr string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, founder, founded_at, COALESCE(motto, '') FROM booths WHERE id = ?
	`, id).Scan(&b.ID, &b.Name, &b.Founder, &foundedStr, &b.Motto)
	if err != nil {
		return Booth{}, err
	}
	b.FoundedAt, _ = time.Parse(time.RFC3339Nano, foundedStr)
	return b, nil
}

// GetSetting returns the value for a key, or empty string if absent.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a single key/value pair.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

// DeleteSetting removes a key (no-op if missing).
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, key)
	return err
}

// Well-known app_settings keys.
const (
	SettingTwinHostname = "twin_hostname"
	SettingTheme        = "theme"        // "dark" | "light"
	SettingSoundsOn     = "sounds_on"    // "1" | "0"
	SettingTourDone     = "tour_done"    // "1" once the first-launch tour finishes
	SettingSeenPeers    = "seen_peers"   // comma-separated peer names for confetti dedup
	SettingDisplayName  = "display_name" // user-chosen label, separate from identity
)

// GetBoothNotepad returns the shared notepad for a booth (or an empty record
// if the booth has none yet).
func (s *Store) GetBoothNotepad(ctx context.Context, boothID string) (BoothNotepad, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT booth_id, text, version, last_editor, last_modified
		FROM booth_notepads WHERE booth_id = ?
	`, boothID)
	var n BoothNotepad
	var modStr string
	if err := row.Scan(&n.BoothID, &n.Text, &n.Version, &n.LastEditor, &modStr); err != nil {
		if err == sql.ErrNoRows {
			return BoothNotepad{BoothID: boothID}, nil
		}
		return BoothNotepad{}, err
	}
	n.LastModified, _ = time.Parse(time.RFC3339Nano, modStr)
	return n, nil
}

// UpdateBoothNotepad upserts the notepad if the incoming version is strictly
// greater than what's stored (last-write-wins). Returns true if applied.
func (s *Store) UpdateBoothNotepad(ctx context.Context, n BoothNotepad) (bool, error) {
	if n.BoothID == "" {
		return false, fmt.Errorf("booth id required")
	}
	if n.LastModified.IsZero() {
		n.LastModified = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO booth_notepads (booth_id, text, version, last_editor, last_modified)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(booth_id) DO UPDATE SET
			text          = excluded.text,
			version       = excluded.version,
			last_editor   = excluded.last_editor,
			last_modified = excluded.last_modified
		WHERE excluded.version > booth_notepads.version
	`, n.BoothID, n.Text, n.Version, n.LastEditor, n.LastModified.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// BoothMembers returns the peer names that belong to a booth.
func (s *Store) BoothMembers(ctx context.Context, boothID string) ([]BoothMember, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT booth_id, peer_name, joined_at FROM booth_members
		WHERE booth_id = ? ORDER BY joined_at ASC
	`, boothID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoothMember
	for rows.Next() {
		var bm BoothMember
		var joinedStr string
		if err := rows.Scan(&bm.BoothID, &bm.PeerName, &joinedStr); err != nil {
			return nil, err
		}
		bm.JoinedAt, _ = time.Parse(time.RFC3339Nano, joinedStr)
		out = append(out, bm)
	}
	return out, rows.Err()
}
