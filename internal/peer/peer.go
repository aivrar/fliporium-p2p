// Peer transport: a TLS-wrapped TCP connection over the tailnet,
// with a HELLO handshake. The TLS layer is self-signed and unverified at
// the X509 layer because peer identity is already established by the
// underlying Tailscale (WireGuard) transport — TLS here is defense in
// depth plus the brief's "TLS-wrapped TCP" wording.
package peer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"time"
)

// Port is the fixed TCP port the Fliporium peer protocol listens on.
const Port = 41642

// NewTLSConfig produces a fresh in-memory TLS config with a self-signed cert.
// Both server and client sides use InsecureSkipVerify because we trust the
// Tailscale layer for peer authentication.
func NewTLSConfig(hostname string) (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("gen key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("gen serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hostname, Organization: []string{"fliporium"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create cert: %w", err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}, nil
}

// PeerConn is one live peer connection after the HELLO handshake. The
// transport is any io.ReadWriteCloser: a TLS-over-tailnet net.Conn (legacy)
// or a detached WebRTC DataChannel — runLoop/WriteFrame/Close only need
// Read/Write/Close, so the same machinery serves both.
type PeerConn struct {
	Name    string // remote's announced display name
	Addr    string // remote net address for logging
	Version string // remote protocol version
	conn    io.ReadWriteCloser
	key     *[32]byte // room key; nil = no encryption (e.g. plaintext HELLO bootstrap)
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

	EventShowtimeStarted HubEventKind = "showtime-started"
	EventShowtimeState   HubEventKind = "showtime-state"
	EventShowtimeEnded   HubEventKind = "showtime-ended"

	EventNotepadUpdated HubEventKind = "notepad-updated"

	EventTwinSyncedMessage HubEventKind = "twin-synced-message"

	EventMessageReaction HubEventKind = "message-reaction"
	EventMessageEdit     HubEventKind = "message-edit"
	EventMessageDelete   HubEventKind = "message-delete"
	EventMessagePin      HubEventKind = "message-pin"
	EventPeerStatus      HubEventKind = "peer-status"
)

// MessageEventData accompanies EventMessage so the app layer can route by Booth
// and address messages by UUID for reactions / edits / deletes.
type MessageEventData struct {
	BoothID    string
	UUID       string
	ParentUUID string
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
}

// HubEvent is a single async event to surface to the UI.
type HubEvent struct {
	Kind HubEventKind
	Peer string
	Text string
	Data any // typed payload (e.g. *FlipEventData)
	At   time.Time
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

	keyMu   sync.Mutex
	roomKey *[32]byte // current room's E2E key; new PeerConns inherit it

	flipMu   sync.Mutex
	inFlips  map[string]*incomingFlip
	outFlips map[string]*outgoingFlip
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
		Events:   make(chan HubEvent, 64),
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
// The caller is responsible for iterating the Booth's members and calling
// SendBooth for each one that's reachable.
func (h *Hub) SendBooth(peerName, boothID, text string) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessage, Message{Text: text, At: time.Now().UTC(), BoothID: boothID})
}

// SendBoothInvite hands a Booth's full state to one peer.
func (h *Hub) SendBoothInvite(peerName string, inv BoothInvite) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeBoothInvite, inv)
}

// SendShowtimeStart broadcasts a SHOWTIME_START to one peer.
func (h *Hub) SendShowtimeStart(peerName string, s ShowtimeStart) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeShowtimeStart, s)
}

// SendShowtimeState broadcasts a SHOWTIME_STATE to one peer.
func (h *Hub) SendShowtimeState(peerName string, s ShowtimeState) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeShowtimeState, s)
}

// SendShowtimeEnd broadcasts a SHOWTIME_END to one peer.
func (h *Hub) SendShowtimeEnd(peerName string, s ShowtimeEnd) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeShowtimeEnd, s)
}

// SendNotepadUpdate broadcasts a NOTEPAD_UPDATE to one peer.
func (h *Hub) SendNotepadUpdate(peerName string, n NotepadUpdate) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeNotepadUpdate, n)
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
				Kind: EventMessage, Peer: c.Name, Text: m.Text,
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
		case TypeShowtimeStart:
			var s ShowtimeStart
			if err := json.Unmarshal(body, &s); err == nil {
				h.emit(HubEvent{Kind: EventShowtimeStarted, Peer: c.Name, Text: s.FlipID, Data: &s})
			}
		case TypeShowtimeState:
			var s ShowtimeState
			if err := json.Unmarshal(body, &s); err == nil {
				h.emit(HubEvent{Kind: EventShowtimeState, Peer: c.Name, Data: &s})
			}
		case TypeShowtimeEnd:
			var s ShowtimeEnd
			if err := json.Unmarshal(body, &s); err == nil {
				h.emit(HubEvent{Kind: EventShowtimeEnded, Peer: c.Name, Data: &s})
			}
		case TypeNotepadUpdate:
			var n NotepadUpdate
			if err := json.Unmarshal(body, &n); err == nil {
				h.emit(HubEvent{Kind: EventNotepadUpdated, Peer: c.Name, Data: &n})
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
		}
	}
}

// Accept welcomes a new inbound conn: TLS handshake, exchange HELLO, register.
func (h *Hub) Accept(ctx context.Context, raw net.Conn, tlsCfg *tls.Config, selfName string) {
	tlsConn := tls.Server(raw, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		raw.Close()
		h.emit(HubEvent{Kind: EventInfo, Text: "inbound tls handshake failed: " + err.Error()})
		return
	}
	env, err := ReadFrame(tlsConn)
	if err != nil || env.Type != TypeHello {
		tlsConn.Close()
		h.emit(HubEvent{Kind: EventInfo, Text: "inbound HELLO missing or malformed"})
		return
	}
	var remote Hello
	if err := json.Unmarshal(env.Body, &remote); err != nil {
		tlsConn.Close()
		return
	}
	if err := WriteFrame(tlsConn, TypeHello, Hello{Name: selfName, Version: ProtocolVersion}); err != nil {
		tlsConn.Close()
		return
	}
	pc := &PeerConn{
		Name:    remote.Name,
		Addr:    raw.RemoteAddr().String(),
		Version: remote.Version,
		conn:    tlsConn,
	}
	h.add(pc)
	h.emit(HubEvent{Kind: EventConnect, Peer: pc.Name, Text: "inbound from " + pc.Addr})
	go h.runLoop(pc)
}

// Dial connects out: TLS handshake, exchange HELLO, register.
func (h *Hub) Dial(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), tlsCfg *tls.Config, target, selfName string) error {
	addr := fmt.Sprintf("%s:%d", target, Port)
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	tlsConn := tls.Client(raw, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return fmt.Errorf("tls handshake to %s: %w", addr, err)
	}
	if err := WriteFrame(tlsConn, TypeHello, Hello{Name: selfName, Version: ProtocolVersion}); err != nil {
		tlsConn.Close()
		return fmt.Errorf("send HELLO: %w", err)
	}
	env, err := ReadFrame(tlsConn)
	if err != nil {
		tlsConn.Close()
		return fmt.Errorf("receive HELLO: %w", err)
	}
	if env.Type != TypeHello {
		tlsConn.Close()
		return fmt.Errorf("expected HELLO, got %q", env.Type)
	}
	var remote Hello
	if err := json.Unmarshal(env.Body, &remote); err != nil {
		tlsConn.Close()
		return fmt.Errorf("decode HELLO: %w", err)
	}
	pc := &PeerConn{
		Name:    remote.Name,
		Addr:    addr,
		Version: remote.Version,
		conn:    tlsConn,
	}
	h.add(pc)
	h.emit(HubEvent{Kind: EventConnect, Peer: pc.Name, Text: "outbound to " + addr})
	go h.runLoop(pc)
	return nil
}
