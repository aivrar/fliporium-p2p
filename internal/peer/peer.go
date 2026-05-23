// Peer hub: the transport-agnostic protocol layer. A PeerConn wraps any
// io.ReadWriteCloser (today a detached WebRTC DataChannel) and runLoop
// dispatches the length-prefixed JSON envelopes to Hub events. The WebRTC
// signaling/handshake wiring lives in webrtc.go; this file is the Hub,
// PeerConn, and the protocol dispatch loop.
package peer

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// PeerConn is one live peer connection after the HELLO handshake. The
// transport is any io.ReadWriteCloser (a detached WebRTC DataChannel):
// runLoop/WriteFrame/Close only need Read/Write/Close.
type PeerConn struct {
	Name    string // remote's stable routing id (the HELLO Name)
	Display string // remote's friendly display name (the HELLO DisplayName)
	Avatar  string // remote's avatar data URI (the HELLO Avatar), validated
	Addr    string // remote net address for logging
	Version string // remote protocol version
	conn    io.ReadWriteCloser
	key     *[32]byte  // room key; nil = no encryption (e.g. plaintext HELLO bootstrap)
	mu      sync.Mutex // serializes writes
	closeMu sync.Mutex
	closed  bool
}

func (c *PeerConn) WriteFrame(t MessageType, body any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == nil {
		return WriteFrame(c.conn, t, body)
	}
	plain, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}
	sealed, err := sealBody(c.key, plain)
	if err != nil {
		return fmt.Errorf("seal body: %w", err)
	}
	return writeEnvelope(c.conn, Envelope{Type: t, Body: sealed})
}

func (c *PeerConn) Close() {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.conn.Close()
}

// HubEventKind describes what happened on a peer connection.
type HubEventKind string

const (
	EventConnect       HubEventKind = "connect"
	EventDisconnect    HubEventKind = "disconnect"
	EventMessage       HubEventKind = "message"
	EventInfo          HubEventKind = "info"
	EventFlipStarted   HubEventKind = "flip-started"
	EventFlipProgress  HubEventKind = "flip-progress"
	EventFlipCompleted HubEventKind = "flip-completed"
	EventFlipFailed    HubEventKind = "flip-failed"
	EventBoothInvited  HubEventKind = "booth-invited"

	EventTwinSyncedMessage HubEventKind = "twin-synced-message"

	EventMessageReaction HubEventKind = "message-reaction"
	EventMessageEdit     HubEventKind = "message-edit"
	EventMessageDelete   HubEventKind = "message-delete"
	EventMessagePin      HubEventKind = "message-pin"
	EventPeerStatus      HubEventKind = "peer-status"
	EventMessageCard     HubEventKind = "message-card"
)

// MessageEventData accompanies EventMessage so the app layer can route by Booth
// and address messages by UUID for reactions / edits / deletes.
type MessageEventData struct {
	BoothID    string
	UUID       string
	ParentUUID string
	Backlog    bool // true if replayed from the offline relay (dedupe by UUID)
}

// FlipEventData is the structured payload attached to Event Flip* events.
type FlipEventData struct {
	ID        string
	Direction string // "in" or "out"
	Filename  string
	Size      int64
	Mime      string
	Path      string // absolute local path (where we wrote, or where we read)
	Bytes     int64  // total transferred so far
	Sha256    string
	Reason    string // for FlipFailed
	BoothID   string // conversation scope; "" = 1:1
}

// HubEvent is a single async event to surface to the UI.
type HubEvent struct {
	Kind    HubEventKind
	Peer    string
	Display string // friendly name of Peer ("" if unknown)
	Avatar  string // avatar data URI of Peer ("" if unknown); set on EventConnect
	Text    string
	Data    any // typed payload (e.g. *FlipEventData)
	At      time.Time
}

// Hub owns all live peer connections and a single events channel for the UI.
type Hub struct {
	mu     sync.RWMutex
	conns  map[string]*PeerConn
	Events chan HubEvent

	// CatchRoot is the directory inbound flips are written into.
	// Files land at CatchRoot/<peer-name>/<filename>. Must be set before any
	// inbound flip can succeed; if empty, inbound flips are rejected.
	CatchRoot string

	keyMu       sync.Mutex
	roomKey     *[32]byte // current room's E2E key; new PeerConns inherit it
	selfDisplay string    // our own friendly name, announced in HELLO
	selfAvatar  string    // our own avatar data URI, announced in HELLO

	blockMu sync.Mutex
	blocked map[string]bool // peer ids we refuse to connect to (client-side block)

	idMu    sync.Mutex
	privKey ed25519.PrivateKey // for signing the peer-auth challenge
	pubKey  ed25519.PublicKey

	flipMu   sync.Mutex
	inFlips  map[string]*incomingFlip
	outFlips map[string]*outgoingFlip
}

// SetSelfDisplay sets the friendly name announced to peers in the HELLO
// handshake. Set it before connecting; reconnecting peers pick up changes.
func (h *Hub) SetSelfDisplay(name string) {
	h.keyMu.Lock()
	h.selfDisplay = name
	h.keyMu.Unlock()
}

func (h *Hub) selfDisp() string {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	return h.selfDisplay
}

// SetSelfAvatar sets the small avatar data URI announced to peers in the HELLO
// handshake. Like the display name, peers pick up changes on their next
// (re)connect. Pass "" to announce no avatar.
func (h *Hub) SetSelfAvatar(uri string) {
	h.keyMu.Lock()
	h.selfAvatar = uri
	h.keyMu.Unlock()
}

func (h *Hub) selfAv() string {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	return h.selfAvatar
}

// SetBlocked replaces the set of peer ids this hub refuses to connect to.
// Applied at the HELLO handshake (AttachDataChannel) so a blocked peer never
// becomes a live connection.
func (h *Hub) SetBlocked(ids []string) {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	h.blockMu.Lock()
	h.blocked = m
	h.blockMu.Unlock()
}

// IsBlocked reports whether a peer id is on this hub's blocklist.
func (h *Hub) IsBlocked(name string) bool {
	h.blockMu.Lock()
	defer h.blockMu.Unlock()
	return h.blocked[name]
}

// SetIdentity gives the hub the Ed25519 keypair used to prove identity in the
// peer handshake. Set before connecting. If unset, the hub doesn't present a
// key (used by tests/dev with non-"fp-" names, where proof isn't enforced).
func (h *Hub) SetIdentity(priv ed25519.PrivateKey, pub ed25519.PublicKey) {
	h.idMu.Lock()
	h.privKey = priv
	h.pubKey = pub
	h.idMu.Unlock()
}

func (h *Hub) identity() (ed25519.PrivateKey, ed25519.PublicKey) {
	h.idMu.Lock()
	defer h.idMu.Unlock()
	return h.privKey, h.pubKey
}

// SetRoomKey sets the E2E key applied to peers that connect from now on. Pass
// nil to disable encryption (e.g. when leaving a room). Existing connections
// keep whatever key they were created with.
func (h *Hub) SetRoomKey(key *[32]byte) {
	h.keyMu.Lock()
	h.roomKey = key
	h.keyMu.Unlock()
}

func (h *Hub) currentKey() *[32]byte {
	h.keyMu.Lock()
	defer h.keyMu.Unlock()
	return h.roomKey
}

func NewHub() *Hub {
	return &Hub{
		conns:    map[string]*PeerConn{},
		Events:   make(chan HubEvent, 256),
		inFlips:  map[string]*incomingFlip{},
		outFlips: map[string]*outgoingFlip{},
	}
}

func (h *Hub) emit(ev HubEvent) {
	ev.At = time.Now()
	select {
	case h.Events <- ev:
	default:
	}
}

func (h *Hub) Names() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	names := make([]string, 0, len(h.conns))
	for n := range h.conns {
		names = append(names, n)
	}
	return names
}

func (h *Hub) Get(name string) *PeerConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[name]
}

func (h *Hub) add(c *PeerConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.conns[c.Name]; ok {
		existing.Close()
	}
	h.conns[c.Name] = c
}

func (h *Hub) remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, name)
}

// Send queues a 1:1 MESSAGE to a named peer.
func (h *Hub) Send(peerName, text string) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessage, Message{Text: text, At: time.Now().UTC()})
}

// SendBooth queues a Booth-scoped MESSAGE to one specific connected peer.
// The caller iterates the Booth's reachable members; uuid lets receivers
// dedupe (important once the same message can also arrive via the backlog).
func (h *Hub) SendBooth(peerName, boothID, text, uuid, parentUUID string) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessage, Message{UUID: uuid, Text: text, At: time.Now().UTC(), BoothID: boothID, ParentUUID: parentUUID})
}

// SendBoothInvite hands a Booth's full state to one peer.
func (h *Hub) SendBoothInvite(peerName string, inv BoothInvite) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeBoothInvite, inv)
}

// SendTwinSyncMessage sends a 1:1 message relay to a paired twin.
func (h *Hub) SendTwinSyncMessage(peerName string, m TwinSyncMessage) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeTwinSyncMessage, m)
}

// SendMessageReaction broadcasts a single reaction add/remove.
func (h *Hub) SendMessageReaction(peerName string, r MessageReaction) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessageReaction, r)
}

// SendMessageEdit broadcasts an edit to a previously-sent message.
func (h *Hub) SendMessageEdit(peerName string, e MessageEdit) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessageEdit, e)
}

// SendMessageDelete broadcasts a tombstone for a previously-sent message.
func (h *Hub) SendMessageDelete(peerName string, d MessageDelete) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessageDelete, d)
}

// SendMessagePin broadcasts a pin/unpin toggle.
func (h *Hub) SendMessagePin(peerName string, p MessagePin) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessagePin, p)
}

// SendMessageCard hands an unfurled link card for a previously-sent message
// to one peer.
func (h *Hub) SendMessageCard(peerName string, mc MessageCard) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessageCard, mc)
}

// SendPeerStatus advertises a presence state change to one peer.
func (h *Hub) SendPeerStatus(peerName string, s PeerStatus) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypePeerStatus, s)
}

// BroadcastPeerStatus sends a status to every currently-connected peer.
func (h *Hub) BroadcastPeerStatus(s PeerStatus) {
	for _, name := range h.Names() {
		_ = h.SendPeerStatus(name, s)
	}
}

// CloseAllPeers closes every active connection without sending BYE. Used when
// leaving a room: each runLoop observes the close, emits EventDisconnect, and
// removes itself from the map.
func (h *Hub) CloseAllPeers() {
	h.mu.RLock()
	conns := make([]*PeerConn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		c.Close()
	}
}

// ByeAll sends BYE to every peer and closes their connections.
func (h *Hub) ByeAll(reason string) {
	h.mu.RLock()
	conns := make([]*PeerConn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		_ = c.WriteFrame(TypeBye, Bye{Reason: reason})
		c.Close()
	}
}

// runLoop reads frames from a peer until the connection ends.
func (h *Hub) runLoop(c *PeerConn) {
	defer func() {
		c.Close()
		h.remove(c.Name)
		h.emit(HubEvent{Kind: EventDisconnect, Peer: c.Name})
	}()
	for {
		env, err := ReadFrame(c.conn)
		if err != nil {
			if err != io.EOF {
				h.emit(HubEvent{Kind: EventInfo, Peer: c.Name, Text: "read err: " + err.Error()})
			}
			return
		}
		// Decrypt the body for everything except the cleartext HELLO bootstrap.
		body := env.Body
		if c.key != nil && env.Type != TypeHello {
			plain, derr := openBody(c.key, env.Body)
			if derr != nil {
				h.emit(HubEvent{Kind: EventInfo, Peer: c.Name, Text: "decrypt: " + derr.Error()})
				continue
			}
			body = plain
		}
		switch env.Type {
		case TypeMessage:
			var m Message
			if err := json.Unmarshal(body, &m); err != nil {
				continue
			}
			h.emit(HubEvent{
				Kind: EventMessage, Peer: c.Name, Display: c.Display, Text: m.Text,
				Data: &MessageEventData{BoothID: m.BoothID, UUID: m.UUID, ParentUUID: m.ParentUUID},
			})
		case TypeBye:
			var b Bye
			_ = json.Unmarshal(body, &b)
			reason := b.Reason
			if reason == "" {
				reason = "(no reason)"
			}
			h.emit(HubEvent{Kind: EventInfo, Peer: c.Name, Text: "BYE: " + reason})
			return
		case TypeHello:
			// extra HELLO post-handshake — ignore
		case TypeFlipStart:
			var s FlipStart
			if err := json.Unmarshal(body, &s); err == nil {
				h.handleFlipStart(c.Name, s)
			}
		case TypeFlipChunk:
			var ch FlipChunk
			if err := json.Unmarshal(body, &ch); err == nil {
				h.handleFlipChunk(c.Name, ch)
			}
		case TypeFlipEnd:
			var e FlipEnd
			if err := json.Unmarshal(body, &e); err == nil {
				h.handleFlipEnd(c.Name, e)
			}
		case TypeFlipReject:
			var r FlipReject
			if err := json.Unmarshal(body, &r); err == nil {
				h.handleFlipReject(c.Name, r)
			}
		case TypeBoothInvite:
			var inv BoothInvite
			if err := json.Unmarshal(body, &inv); err == nil {
				h.emit(HubEvent{Kind: EventBoothInvited, Peer: c.Name, Text: inv.Name, Data: &inv})
			}
		case TypeTwinSyncMessage:
			var t TwinSyncMessage
			if err := json.Unmarshal(body, &t); err == nil {
				h.emit(HubEvent{Kind: EventTwinSyncedMessage, Peer: c.Name, Data: &t})
			}
		case TypeMessageReaction:
			var r MessageReaction
			if err := json.Unmarshal(body, &r); err == nil {
				h.emit(HubEvent{Kind: EventMessageReaction, Peer: c.Name, Data: &r})
			}
		case TypeMessageEdit:
			var e MessageEdit
			if err := json.Unmarshal(body, &e); err == nil {
				h.emit(HubEvent{Kind: EventMessageEdit, Peer: c.Name, Data: &e})
			}
		case TypeMessageDelete:
			var d MessageDelete
			if err := json.Unmarshal(body, &d); err == nil {
				h.emit(HubEvent{Kind: EventMessageDelete, Peer: c.Name, Data: &d})
			}
		case TypeMessagePin:
			var p MessagePin
			if err := json.Unmarshal(body, &p); err == nil {
				h.emit(HubEvent{Kind: EventMessagePin, Peer: c.Name, Data: &p})
			}
		case TypePeerStatus:
			var s PeerStatus
			if err := json.Unmarshal(body, &s); err == nil {
				h.emit(HubEvent{Kind: EventPeerStatus, Peer: c.Name, Text: s.Status, Data: &s})
			}
		case TypeMessageCard:
			var mc MessageCard
			if err := json.Unmarshal(body, &mc); err == nil {
				h.emit(HubEvent{Kind: EventMessageCard, Peer: c.Name, Data: &mc})
			}
		}
	}
}
