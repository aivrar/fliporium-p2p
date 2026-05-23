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
	Card       string // JSON of an unfurled link card, "" if none
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
	Avatar    string // avatar data URI announced via HELLO ("" if none)
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

// FlipStatus tracks where a transfer is in its lifecycle.
const (
	FlipStatusStarted   = "started"
	FlipStatusReceiving = "receiving"
	FlipStatusComplete  = "complete"
	FlipStatusFailed    = "failed"
	FlipStatusCancelled = "cancelled"
	// FlipStatusQueued is a file added to a room while no one else was present.
	// It's stored locally (peer = room id) and sent when someone joins.
	FlipStatusQueued = "queued"
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
	BoothID     string    // conversation scope; "" = 1:1
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
-- Indexes on columns that older stores gain via ALTER (booth_id, uuid) are
-- created after those ALTERs run, in migrate() — not here — so opening a
-- pre-existing DB whose table predates the column doesn't fail before the
-- ALTER can add it.

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
    completed_at TEXT,
    booth_id     TEXT
);

CREATE INDEX IF NOT EXISTS idx_flips_peer_started ON flips(peer, started_at);
-- idx_flips_booth (on the ALTER-added booth_id column) is created in migrate().

CREATE TABLE IF NOT EXISTS blocked (
    peer TEXT PRIMARY KEY,
    at   TEXT NOT NULL
);
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
		`ALTER TABLE messages ADD COLUMN card TEXT`,                         // v0.14 link cards
		`ALTER TABLE flips ADD COLUMN booth_id TEXT`,                        // v0.15 booth-scoped flips
		`ALTER TABLE peers ADD COLUMN avatar TEXT`,                          // v0.16 custom avatars
	}
	for _, stmt := range alters {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	// Indexes on columns the ALTERs above add. These MUST run after the ALTERs:
	// putting them in the base schema breaks opening an older DB whose table
	// predates the column (CREATE INDEX fails with "no such column" before the
	// ALTER can add it).
	postAlterIndexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_messages_booth_at ON messages(booth_id, at)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_uuid ON messages(uuid) WHERE uuid IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_flips_booth ON flips(booth_id)`,
	}
	for _, stmt := range postAlterIndexes {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate index %q: %w", stmt, err)
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

// SetPeerAvatar records a peer's avatar data URI (no-op if empty, so we never
// clobber a known avatar with a blank one — same policy as SetPeerDisplay).
func (s *Store) SetPeerAvatar(ctx context.Context, name, avatar string) error {
	if avatar == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO peers (name, first_seen, last_seen, avatar) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET avatar = excluded.avatar, last_seen = excluded.last_seen
	`, name, now, now, avatar)
	return err
}

// Peers returns the known peer roster, most-recently-seen first.
func (s *Store) Peers(ctx context.Context) ([]PeerRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, COALESCE(display, ''), COALESCE(avatar, ''), first_seen, last_seen FROM peers ORDER BY last_seen DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerRecord
	for rows.Next() {
		var p PeerRecord
		var first, last string
		if err := rows.Scan(&p.Name, &p.Display, &p.Avatar, &first, &last); err != nil {
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

// PeerAvatars returns a name->avatar (data URI) map for rendering peer pictures.
func (s *Store) PeerAvatars(ctx context.Context) (map[string]string, error) {
	peers, err := s.Peers(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(peers))
	for _, p := range peers {
		if p.Avatar != "" {
			m[p.Name] = p.Avatar
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
	var cardCol *string
	if m.Card != "" {
		cardCol = &m.Card
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (peer, direction, text, at, booth_id, uuid, parent_uuid, card)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, m.Peer, m.Direction, m.Text, m.At.UTC().Format(time.RFC3339Nano), boothCol, uuidCol, parentCol, cardCol)
	return err
}

// SetMessageCard stores (or replaces) the unfurled link-card JSON on a message
// identified by UUID. Returns whether a row was updated.
func (s *Store) SetMessageCard(ctx context.Context, uuid, cardJSON string) (bool, error) {
	if uuid == "" {
		return false, fmt.Errorf("uuid required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET card = ? WHERE uuid = ?`, cardJSON, uuid)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
		       snippet(messages_fts, 0, char(2), char(3), '...', 12)
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
		       COALESCE(pinned, 0), COALESCE(card,'')
		FROM messages WHERE uuid = ?
	`, uuid)
	return scanFullMessage(row)
}

// scanFullMessage scans one full Message row.
func scanFullMessage(row *sql.Row) (Message, error) {
	var m Message
	var atStr, editedStr, deletedStr string
	var pinnedInt int
	if err := row.Scan(&m.ID, &m.UUID, &m.Peer, &m.Direction, &m.Text, &atStr, &m.BoothID, &m.ParentUUID, &editedStr, &deletedStr, &pinnedInt, &m.Card); err != nil {
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
	var boothCol *string
	if f.BoothID != "" {
		boothCol = &f.BoothID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO flips (id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at, booth_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			filename=excluded.filename,
			size=excluded.size,
			mime=excluded.mime,
			path=excluded.path,
			sha256=excluded.sha256,
			status=excluded.status,
			completed_at=excluded.completed_at,
			booth_id=COALESCE(excluded.booth_id, flips.booth_id)
	`,
		f.ID, f.Peer, f.Direction, f.Filename, f.Size, f.Mime, f.Path, f.Sha256, f.Status,
		f.StartedAt.UTC().Format(time.RFC3339Nano), completedAt, boothCol)
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
		SELECT id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at, COALESCE(booth_id, '')
		FROM flips WHERE peer = ? ORDER BY started_at ASC
	`, peer)
	if err != nil {
		return nil, err
	}
	return scanFlips(rows)
}

// FlipsByBooth returns flips scoped to a booth (its conversation), oldest first.
func (s *Store) FlipsByBooth(ctx context.Context, boothID string) ([]FlipRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at, COALESCE(booth_id, '')
		FROM flips WHERE booth_id = ? ORDER BY started_at ASC
	`, boothID)
	if err != nil {
		return nil, err
	}
	return scanFlips(rows)
}

func scanFlips(rows *sql.Rows) ([]FlipRecord, error) {
	defer rows.Close()
	var out []FlipRecord
	for rows.Next() {
		var f FlipRecord
		var mime, sha sql.NullString
		var startedAt string
		var completedAt sql.NullString
		if err := rows.Scan(&f.ID, &f.Peer, &f.Direction, &f.Filename, &f.Size, &mime, &f.Path, &sha, &f.Status, &startedAt, &completedAt, &f.BoothID); err != nil {
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
		SELECT id, peer, direction, filename, size, mime, path, sha256, status, started_at, completed_at, COALESCE(booth_id, '')
		FROM flips WHERE id = ?
	`, id)
	var f FlipRecord
	var mime, sha sql.NullString
	var startedAt string
	var completedAt sql.NullString
	if err := row.Scan(&f.ID, &f.Peer, &f.Direction, &f.Filename, &f.Size, &mime, &f.Path, &sha, &f.Status, &startedAt, &completedAt, &f.BoothID); err != nil {
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

// DeleteFlip removes a flip record (the local file is the caller's concern).
func (s *Store) DeleteFlip(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM flips WHERE id = ?`, id)
	return err
}

// DeleteMessageByID hard-deletes one message row from this device only (the
// AFTER DELETE trigger keeps the search index in sync). Also clears its
// reactions. This is a local "remove my copy", distinct from the tombstone
// delete that propagates to peers.
func (s *Store) DeleteMessageByID(ctx context.Context, id int64) error {
	var uuid string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(uuid,'') FROM messages WHERE id = ?`, id).Scan(&uuid)
	if uuid != "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM message_reactions WHERE message_uuid = ?`, uuid)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id)
	return err
}

// Messages returns the last `limit` 1:1 messages with a given peer (booth_id is NULL).
// limit <= 0 means "all".
func (s *Store) Messages(ctx context.Context, peer string, limit int) ([]Message, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0), COALESCE(card,'')
			FROM (
				SELECT id, uuid, peer, direction, text, at, booth_id, parent_uuid, edited_at, deleted_at, pinned, card
				FROM messages WHERE peer = ? AND booth_id IS NULL
				ORDER BY id DESC LIMIT ?
			) ORDER BY id ASC
		`, peer, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0), COALESCE(card,'')
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
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0), COALESCE(card,'')
			FROM (
				SELECT id, uuid, peer, direction, text, at, booth_id, parent_uuid, edited_at, deleted_at, pinned, card
				FROM messages WHERE booth_id = ?
				ORDER BY id DESC LIMIT ?
			) ORDER BY id ASC
		`, boothID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, COALESCE(uuid,''), peer, direction, text, at, COALESCE(booth_id, ''),
			       COALESCE(parent_uuid,''), COALESCE(edited_at,''), COALESCE(deleted_at,''), COALESCE(pinned, 0), COALESCE(card,'')
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
		if err := rows.Scan(&m.ID, &m.UUID, &m.Peer, &m.Direction, &m.Text, &atStr, &m.BoothID, &m.ParentUUID, &editedStr, &deletedStr, &pinnedInt, &m.Card); err != nil {
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

// BoothLastActivity returns the most recent message time in each booth, keyed
// by booth id. Booths with no messages are absent from the map. Used to sort /
// auto-hide inactive rooms in the UI.
func (s *Store) BoothLastActivity(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT booth_id, MAX(at) FROM messages
		WHERE booth_id IS NOT NULL AND booth_id != ''
		GROUP BY booth_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, atStr string
		if err := rows.Scan(&id, &atStr); err != nil {
			return nil, err
		}
		if t, perr := time.Parse(time.RFC3339Nano, atStr); perr == nil {
			out[id] = t
		}
	}
	return out, rows.Err()
}

// BlockPeer adds a peer id to the local blocklist (idempotent).
func (s *Store) BlockPeer(ctx context.Context, peer string) error {
	if strings.TrimSpace(peer) == "" {
		return fmt.Errorf("peer required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blocked (peer, at) VALUES (?, ?)
		ON CONFLICT(peer) DO NOTHING
	`, peer, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// UnblockPeer removes a peer id from the blocklist.
func (s *Store) UnblockPeer(ctx context.Context, peer string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM blocked WHERE peer = ?`, peer)
	return err
}

// BlockedPeers returns every blocked peer id.
func (s *Store) BlockedPeers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT peer FROM blocked ORDER BY at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteBooth removes a booth row and its membership list (history rows in
// messages/flips keyed by this id are left intact — they reappear if the user
// rejoins via the invite link; call DeleteMessagesByBooth to purge them).
func (s *Store) DeleteBooth(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM booth_members WHERE booth_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM booths WHERE id = ?`, id)
	return err
}

// DeleteMessagesByBooth purges all messages in a booth from this device (the
// AFTER DELETE trigger keeps the FTS index in sync).
func (s *Store) DeleteMessagesByBooth(ctx context.Context, boothID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE booth_id = ?`, boothID)
	return err
}

// DeleteFlipsByPeer removes all flip rows keyed by a given peer/room id (used to
// purge a room's parked files, which are keyed by the room id).
func (s *Store) DeleteFlipsByPeer(ctx context.Context, peer string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM flips WHERE peer = ?`, peer)
	return err
}
