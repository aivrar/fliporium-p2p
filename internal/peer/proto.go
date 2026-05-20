// Wire protocol for the Fliporium peer link.
//
// Frames are length-prefixed JSON: 4-byte big-endian length, then a JSON
// Envelope object. The Envelope wraps a typed body so the reader can
// dispatch by Type without unmarshalling the body twice.
package peer

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const ProtocolVersion = "fliporium/0.10"

type MessageType string

const (
	TypeHello   MessageType = "HELLO"
	TypeMessage MessageType = "MESSAGE"
	TypeBye     MessageType = "BYE"

	// Flip = file transfer. v0.5 carries chunks as base64 inside JSON envelopes;
	// future versions will switch to native binary framing for efficiency.
	TypeFlipStart  MessageType = "FLIP_START"
	TypeFlipChunk  MessageType = "FLIP_CHUNK"
	TypeFlipEnd    MessageType = "FLIP_END"
	TypeFlipAck    MessageType = "FLIP_ACK"
	TypeFlipReject MessageType = "FLIP_REJECT"

	// Booths = multi-peer named chat rooms (v0.6).
	TypeBoothInvite MessageType = "BOOTH_INVITE"

	// Showtime = synchronized media playback in a booth (v0.7).
	TypeShowtimeStart MessageType = "SHOWTIME_START"
	TypeShowtimeState MessageType = "SHOWTIME_STATE"
	TypeShowtimeEnd   MessageType = "SHOWTIME_END"

	// Workshop = collaborative tools (v0.8 ships the shared notepad).
	TypeNotepadUpdate MessageType = "NOTEPAD_UPDATE"

	// Twin Mode (v0.9): one Fliporium instance relays its own 1:1 chat
	// history to a paired sibling instance owned by the same user.
	TypeTwinSyncMessage MessageType = "TWIN_SYNC_MESSAGE"

	// Round 1 (v0.10): chat ergonomics on top of MESSAGE.
	TypeMessageReaction MessageType = "MESSAGE_REACTION"
	TypeMessageEdit     MessageType = "MESSAGE_EDIT"
	TypeMessageDelete   MessageType = "MESSAGE_DELETE"
	TypeMessagePin      MessageType = "MESSAGE_PIN"

	// Round 5: rough presence broadcast.
	TypePeerStatus MessageType = "PEER_STATUS"
)

type Envelope struct {
	Type MessageType     `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

type Hello struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Message struct {
	UUID       string    `json:"uuid,omitempty"`        // sender-assigned; empty == legacy / unaddressable
	Text       string    `json:"text"`
	At         time.Time `json:"at"`
	BoothID    string    `json:"booth_id,omitempty"`    // empty for 1:1
	ParentUUID string    `json:"parent_uuid,omitempty"` // reply target, optional
}

// MessageReaction adds (Action="add") or removes (Action="remove") an emoji
// reaction on a previously-sent message.
type MessageReaction struct {
	MessageUUID string    `json:"message_uuid"`
	Emoji       string    `json:"emoji"`
	Action      string    `json:"action"` // "add" | "remove"
	BoothID     string    `json:"booth_id,omitempty"`
	At          time.Time `json:"at"`
}

// MessageEdit replaces the text of a previously-sent message. Only the
// original sender's edits are honored (enforced by the receiver: the edit
// must arrive over a connection whose remote name equals the original
// message's "peer" with direction="in", i.e. they sent it to us).
type MessageEdit struct {
	MessageUUID string    `json:"message_uuid"`
	Text        string    `json:"text"`
	BoothID     string    `json:"booth_id,omitempty"`
	At          time.Time `json:"at"`
}

// MessageDelete tombstones a previously-sent message. Same sender check as
// MessageEdit.
type MessageDelete struct {
	MessageUUID string    `json:"message_uuid"`
	BoothID     string    `json:"booth_id,omitempty"`
	At          time.Time `json:"at"`
}

// MessagePin toggles the pinned flag. Anyone in the conversation can pin —
// pinning isn't restricted to the author.
type MessagePin struct {
	MessageUUID string    `json:"message_uuid"`
	Pinned      bool      `json:"pinned"`
	BoothID     string    `json:"booth_id,omitempty"`
	At          time.Time `json:"at"`
}

// PeerStatus advertises this peer's presence state to connected peers.
// Status is one of "active", "idle", "away".
type PeerStatus struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

// BoothInvite seeds the recipient's local copy of a Booth.
// The sender is always one of the members.
type BoothInvite struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Founder   string    `json:"founder"`
	Members   []string  `json:"members"`
	Motto     string    `json:"motto,omitempty"`
	FoundedAt time.Time `json:"founded_at"`
}

// ShowtimeStart announces a new synchronized playback session in a booth.
// FlipID references a file that's already been flipped to every viewer
// (use booth-flip to seed it first). Leader is the peer hostname of whoever
// is broadcasting playback.
type ShowtimeStart struct {
	SessionID string    `json:"session_id"`
	BoothID   string    `json:"booth_id"`
	FlipID    string    `json:"flip_id"`
	Leader    string    `json:"leader"`
	Filename  string    `json:"filename,omitempty"` // hint for receivers that lack the flip
	Mime      string    `json:"mime,omitempty"`
	At        time.Time `json:"at"`
}

// ShowtimeState is the periodic + on-event sync update from the leader.
// Position is the media currentTime in seconds.
type ShowtimeState struct {
	SessionID string    `json:"session_id"`
	BoothID   string    `json:"booth_id"`
	Playing   bool      `json:"playing"`
	Position  float64   `json:"position"`
	At        time.Time `json:"at"`
}

// ShowtimeEnd closes a session.
type ShowtimeEnd struct {
	SessionID string    `json:"session_id"`
	BoothID   string    `json:"booth_id"`
	At        time.Time `json:"at"`
}

// NotepadUpdate carries the full text of a booth's shared notepad, plus a
// monotonically increasing Version for last-write-wins conflict resolution.
type NotepadUpdate struct {
	BoothID string    `json:"booth_id"`
	Text    string    `json:"text"`
	Version int64     `json:"version"`
	Editor  string    `json:"editor"`
	At      time.Time `json:"at"`
}

// TwinSyncMessage is one 1:1 chat row relayed from one of a user's devices
// to its paired Twin. OriginalPeer is the sender (if Direction == "in") or the
// recipient (if Direction == "out") on the originating device.
//
// TWIN_SYNC_MESSAGE never triggers further relays — it's a leaf in the
// propagation graph to avoid cycles.
type TwinSyncMessage struct {
	OriginalPeer string    `json:"original_peer"`
	Direction    string    `json:"direction"`
	Text         string    `json:"text"`
	At           time.Time `json:"at"`
	BoothID      string    `json:"booth_id,omitempty"` // reserved; not used in v0.9
}

type Bye struct {
	Reason string `json:"reason,omitempty"`
}

// FlipStart announces that the sender is about to begin streaming a file.
type FlipStart struct {
	ID       string `json:"id"`       // sender-chosen UUID
	Filename string `json:"filename"` // basename only; receiver decides path
	Size     int64  `json:"size"`     // bytes
	Mime     string `json:"mime,omitempty"`
}

// FlipChunk carries up to ChunkSize bytes of file data.
type FlipChunk struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"` // base64-encoded by encoding/json
}

// FlipEnd terminates a flip; the receiver verifies Sha256.
type FlipEnd struct {
	ID     string `json:"id"`
	Sha256 string `json:"sha256"` // hex-encoded
}

// FlipAck reports the cumulative number of bytes the receiver has on disk.
type FlipAck struct {
	ID       string `json:"id"`
	Received int64  `json:"received"`
}

// FlipReject is the receiver telling the sender to stop (e.g. user declined).
type FlipReject struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// ChunkSize is the size of each FlipChunk payload in bytes.
// 64KB strikes a balance between framing overhead and memory pressure.
const ChunkSize = 64 * 1024

// MaxFrame caps a single envelope to ~256KB after base64 expansion, leaving
// headroom around ChunkSize.
const MaxFrame = 256 * 1024

func WriteFrame(w io.Writer, t MessageType, body any) error {
	var bodyJSON json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyJSON = b
	}
	payload, err := json.Marshal(Envelope{Type: t, Body: bodyJSON})
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if uint32(len(payload)) > MaxFrame {
		return fmt.Errorf("frame too large: %d > %d", len(payload), MaxFrame)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func ReadFrame(r io.Reader) (Envelope, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return Envelope{}, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > MaxFrame {
		return Envelope{}, fmt.Errorf("frame too large: %d > %d", n, MaxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Envelope{}, fmt.Errorf("read payload: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	return env, nil
}
