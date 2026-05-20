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

// PeerConn is one live tailnet peer connection after the HELLO handshake.
type PeerConn struct {
	Name    string // remote's announced display name
	Addr    string // remote net address for logging
	Version string // remote protocol version
	conn    net.Conn
	mu      sync.Mutex // serializes writes
	closeMu sync.Mutex
	closed  bool
}

func (c *PeerConn) WriteFrame(t MessageType, body any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return WriteFrame(c.conn, t, body)
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
	EventConnect    HubEventKind = "connect"
	EventDisconnect HubEventKind = "disconnect"
	EventMessage    HubEventKind = "message"
	EventInfo       HubEventKind = "info"
)

// HubEvent is a single async event to surface to the UI.
type HubEvent struct {
	Kind HubEventKind
	Peer string
	Text string
	At   time.Time
}

// Hub owns all live peer connections and a single events channel for the UI.
type Hub struct {
	mu     sync.RWMutex
	conns  map[string]*PeerConn
	Events chan HubEvent
}

func NewHub() *Hub {
	return &Hub{
		conns:  map[string]*PeerConn{},
		Events: make(chan HubEvent, 64),
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

// Send queues a MESSAGE to a named peer.
func (h *Hub) Send(peerName, text string) error {
	c := h.Get(peerName)
	if c == nil {
		return fmt.Errorf("no active connection to %q", peerName)
	}
	return c.WriteFrame(TypeMessage, Message{Text: text, At: time.Now().UTC()})
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
		switch env.Type {
		case TypeMessage:
			var m Message
			if err := json.Unmarshal(env.Body, &m); err != nil {
				continue
			}
			h.emit(HubEvent{Kind: EventMessage, Peer: c.Name, Text: m.Text})
		case TypeBye:
			var b Bye
			_ = json.Unmarshal(env.Body, &b)
			reason := b.Reason
			if reason == "" {
				reason = "(no reason)"
			}
			h.emit(HubEvent{Kind: EventInfo, Peer: c.Name, Text: "BYE: " + reason})
			return
		case TypeHello:
			// extra HELLO post-handshake — ignore
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
