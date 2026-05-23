// Package rtc is the Phase-0 WebRTC transport: a tiny signaling client and a
// helper that brings up a pion PeerConnection with a detached DataChannel.
//
// The DataChannel is detached so it exposes a plain io.ReadWriteCloser — which
// means the existing length-prefixed envelope protocol (peer.WriteFrame /
// peer.ReadFrame) can run over it unchanged. That's the whole point of the
// proof: swap the transport, keep the protocol and the message dispatch.
//
// This package deliberately handles ONE remote peer per Connect call. The
// many-peers room hub is Phase 1.
package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Signal message types exchanged with the signaling server over WebSocket.
// The server only ever moves these between peers; it never sees DataChannel
// payloads (chat/file content), which flow peer-to-peer.
const (
	SigJoin       = "join"        // client -> server: join a room as `From`
	SigPeers      = "peers"       // server -> client: who else is already here
	SigPeerJoined = "peer-joined" // server -> client: a new peer arrived
	SigPeerLeft   = "peer-left"   // server -> client: a peer disconnected
	SigOffer      = "offer"       // relayed SDP offer (From -> To)
	SigAnswer     = "answer"      // relayed SDP answer (From -> To)
	SigICE        = "ice"         // relayed trickle ICE candidate (From -> To)
	SigStore      = "store"       // client -> server: append an encrypted blob to the room backlog
	SigBacklog    = "backlog"     // server -> client: the room's stored encrypted blobs (on join)
	SigError      = "error"       // server -> client: something went wrong
)

// Sig is the on-the-wire signaling message. Fields are populated per Type.
type Sig struct {
	Type  string          `json:"type"`
	Room  string          `json:"room,omitempty"`
	From  string          `json:"from,omitempty"`  // sender peer id
	To    string          `json:"to,omitempty"`    // target peer id (offer/answer/ice)
	Peers []string        `json:"peers,omitempty"` // membership (peers/peer-joined replies)
	SDP   string          `json:"sdp,omitempty"`   // offer/answer
	Cand  json.RawMessage `json:"cand,omitempty"`  // ICE candidate (webrtc.ICECandidateInit)
	Blob  string          `json:"blob,omitempty"`  // store: one encrypted message blob
	Blobs []string        `json:"blobs,omitempty"` // backlog: the room's stored encrypted blobs
	Turn  *TurnCreds      `json:"turn,omitempty"`  // peers: short-lived TURN relay credentials
	Msg   string          `json:"msg,omitempty"`   // error detail
}

// TurnCreds are short-lived TURN relay credentials minted by the signaling
// server (coturn use-auth-secret / TURN REST API style). Used by peers behind
// hard NATs that can't hole-punch directly.
type TurnCreds struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// SignalClient is a WebSocket connection to the signaling server. Incoming
// messages are delivered on In; outgoing messages go through Send (serialized).
type SignalClient struct {
	conn *websocket.Conn
	self string
	In   chan Sig

	writeMu sync.Mutex
}

// DialSignal connects to the signaling server, joins `room` as `self`, and
// starts a read pump that feeds the In channel until the connection drops.
// The dial + join handshake is bounded by a timeout so an unreachable or slow
// server can't wedge the caller (callers serialize joins, so a hung dial would
// otherwise stall every room). The established connection then lives for as
// long as ctx (the read pump is independent of the dial deadline).
func DialSignal(ctx context.Context, url, room, self string) (*SignalClient, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial signaling %q: %w", url, err)
	}
	// SDP offers can exceed the 32KB default; allow generous frames.
	conn.SetReadLimit(1 << 20)

	c := &SignalClient{conn: conn, self: self, In: make(chan Sig, 32)}
	if err := c.Send(dialCtx, Sig{Type: SigJoin, Room: room, From: self}); err != nil {
		conn.Close(websocket.StatusInternalError, "join failed")
		return nil, err
	}
	go c.readPump()
	return c, nil
}

func (c *SignalClient) readPump() {
	defer close(c.In)
	for {
		var m Sig
		if err := wsjson.Read(context.Background(), c.conn, &m); err != nil {
			return
		}
		c.In <- m
	}
}

// Send writes one signaling message. Safe for concurrent callers.
func (c *SignalClient) Send(ctx context.Context, m Sig) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return wsjson.Write(ctx, c.conn, m)
}

// Self returns this client's peer id.
func (c *SignalClient) Self() string { return c.self }

// Close shuts the signaling connection down.
func (c *SignalClient) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "bye")
}
