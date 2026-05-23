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

	// Round 14: link unfurling. The sender unfurls a link in a message it sent
	// (only the sender's device contacts the third-party site), then hands the
	// resulting card to recipients so their devices never touch the link.
	TypeMessageCard MessageType = "MESSAGE_CARD"

	// Round 15: identity proof. After HELLO, each side signs the other's nonce
	// with its Ed25519 key, proving it owns the key the routing id is derived
	// from. Without this, a peer could claim anyone's id and impersonate them.
	TypeAuth MessageType = "AUTH"
)

type Envelope struct {
	Type MessageType     `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

type Hello struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	DisplayName string `json:"displayName,omitempty"` // friendly label; routing still keys on Name
	Avatar      string `json:"avatar,omitempty"`      // small self-contained data: URI (downscaled square JPEG), like DisplayName
	PubKey      []byte `json:"pubkey,omitempty"`      // Ed25519 public key the routing id is derived from
	Nonce       []byte `json:"nonce,omitempty"`       // fresh challenge the peer must sign to prove key ownership
}

// Auth proves identity: Sig is the sender's Ed25519 signature over the nonce
// the *other* side sent in its HELLO. Verifying it (against the HELLO PubKey,
// whose fingerprint must equal the claimed routing id) prevents impersonation.
type Auth struct {
	Sig []byte `json:"sig"`
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

// LinkCard is an unfurled preview of a URL found in a message. The sender's
// device fetches the metadata (so only the sender ever contacts the link's
// host); Image is a small self-contained data: URI (a downscaled JPEG) so
// recipients render the thumbnail without reaching out to anyone. The JSON
// field names double as the shape the frontend consumes.
type LinkCard struct {
	URL         string `json:"url"`
	Kind        string `json:"kind"`                  // "link" | "youtube"
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`       // data: URI thumbnail (already downscaled)
	SiteName    string `json:"siteName,omitempty"`
	VideoID     string `json:"videoId,omitempty"`     // YouTube id, for click-to-load embed
}

// MessageCard attaches an unfurled LinkCard to a previously-sent message.
// Honored only when it arrives from the message's original sender (same check
// as MessageEdit).
type MessageCard struct {
	MessageUUID string   `json:"message_uuid"`
	BoothID     string   `json:"booth_id,omitempty"`
	Card        LinkCard `json:"card"`
}

// BoothInvite seeds the recipient's local copy of a Booth.
// The sender is always one of the members.
//
// Secret carries the booth's end-to-end room key so the recipient can actually
// join the encrypted mesh without a separately-pasted invite link. It only
// rides this envelope, which travels over the already-authenticated +
// E2E-encrypted P2P channel to a single verified peer — the signaling server
// never sees it. Empty Secret means "metadata only" (legacy/group reshare).
type BoothInvite struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Founder   string    `json:"founder"`
	Members   []string  `json:"members"`
	Motto     string    `json:"motto,omitempty"`
	Secret    string    `json:"secret,omitempty"`
	FoundedAt time.Time `json:"founded_at"`
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
	BoothID  string `json:"booth_id,omitempty"` // conversation scope; "" = 1:1 (keeps files from leaking across rooms that share a member)
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
	return writeEnvelope(w, Envelope{Type: t, Body: bodyJSON})
}

// writeEnvelope length-prefixes and writes a pre-built envelope (used by the
// encrypted write path, which seals the body before wrapping it).
func writeEnvelope(w io.Writer, env Envelope) error {
	payload, err := json.Marshal(env)
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
