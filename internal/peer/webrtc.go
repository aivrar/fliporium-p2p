// WebRTC transport glue. This wires the transport-only rtc package (signaling
// + DataChannel mesh) into the existing Hub: each opened DataChannel becomes a
// PeerConn driven by the same runLoop, so every feature (chat, flips, booths,
// showtime, notepad, reactions...) works peer-to-peer with zero protocol
// changes. The 4-byte length-prefixed envelopes ride the DataChannel as-is.
package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"fliporium/internal/rtc"
)

// AttachDataChannel runs the HELLO handshake over an already-open transport
// (a detached WebRTC DataChannel) and registers the resulting peer, then
// starts its runLoop. Mirrors Dial/Accept but works on any io.ReadWriteCloser.
//
// Both sides write HELLO then read it; DataChannel writes are buffered, so
// there's no handshake deadlock.
func (h *Hub) AttachDataChannel(rwc io.ReadWriteCloser, selfName string) (string, error) {
	if err := WriteFrame(rwc, TypeHello, Hello{Name: selfName, Version: ProtocolVersion}); err != nil {
		rwc.Close()
		return "", fmt.Errorf("send HELLO: %w", err)
	}
	env, err := ReadFrame(rwc)
	if err != nil {
		rwc.Close()
		return "", fmt.Errorf("receive HELLO: %w", err)
	}
	if env.Type != TypeHello {
		rwc.Close()
		return "", fmt.Errorf("expected HELLO, got %q", env.Type)
	}
	var remote Hello
	if err := json.Unmarshal(env.Body, &remote); err != nil {
		rwc.Close()
		return "", fmt.Errorf("decode HELLO: %w", err)
	}
	pc := &PeerConn{Name: remote.Name, Addr: "webrtc", Version: remote.Version, conn: rwc}
	h.add(pc)
	h.emit(HubEvent{Kind: EventConnect, Peer: pc.Name, Text: "webrtc peer"})
	go h.runLoop(pc)
	return remote.Name, nil
}

// RunWebRTC joins a signaling room and maintains a WebRTC mesh, attaching every
// peer's DataChannel to the Hub. Blocks until ctx is cancelled or signaling
// drops. selfName is both this peer's signaling id and its HELLO display name
// (in Phase 1 these are the same; identity becomes a pubkey in Phase 2).
func (h *Hub) RunWebRTC(ctx context.Context, signalURL, room, selfName string, stun []string) error {
	r, err := rtc.JoinRoom(ctx, signalURL, room, selfName, stun)
	if err != nil {
		return err
	}
	defer r.Close()

	r.OnPeer = func(remoteID string, rwc io.ReadWriteCloser, initiator bool) {
		if _, err := h.AttachDataChannel(rwc, selfName); err != nil {
			h.emit(HubEvent{Kind: EventInfo, Text: "webrtc attach " + remoteID + ": " + err.Error()})
		}
	}
	r.OnPeerLeft = func(remoteID string) {
		// remoteID is the signaling id, which equals the HELLO name in Phase 1.
		if c := h.Get(remoteID); c != nil {
			c.Close() // runLoop observes EOF → emits EventDisconnect + removes
		}
	}
	return r.Run(ctx)
}
