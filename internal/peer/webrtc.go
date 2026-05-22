// WebRTC transport glue. This wires the transport-only rtc package (signaling
// + DataChannel mesh) into the existing Hub: each opened DataChannel becomes a
// PeerConn driven by the same runLoop, so every feature (chat, flips, booths,
// showtime, reactions...) works peer-to-peer with zero protocol changes. The
// 4-byte length-prefixed envelopes ride the DataChannel as-is.
package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"fliporium/internal/rtc"
)

// AttachDataChannel runs the HELLO handshake over an already-open transport
// (a detached WebRTC DataChannel) and registers the resulting peer, then
// starts its runLoop. Mirrors Dial/Accept but works on any io.ReadWriteCloser.
//
// Both sides write HELLO then read it; DataChannel writes are buffered, so
// there's no handshake deadlock.
func (h *Hub) AttachDataChannel(rwc io.ReadWriteCloser, selfName string) (string, error) {
	if err := WriteFrame(rwc, TypeHello, Hello{Name: selfName, Version: ProtocolVersion, DisplayName: h.selfDisp()}); err != nil {
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
	pc := &PeerConn{Name: remote.Name, Display: remote.DisplayName, Addr: "webrtc", Version: remote.Version, conn: rwc, key: h.currentKey()}
	h.add(pc)
	h.emit(HubEvent{Kind: EventConnect, Peer: pc.Name, Display: pc.Display, Text: "webrtc peer"})
	go h.runLoop(pc)
	return remote.Name, nil
}

// JoinRoom connects to a signaling room and maintains a WebRTC mesh, attaching
// every peer's DataChannel to the Hub. Non-blocking: it returns the live Room
// so the caller can Close() it to leave (then call Hub.CloseAllPeers to drop
// the room's connections). The room's signaling pump runs until ctx is
// cancelled or the room is closed.
func (h *Hub) JoinRoom(ctx context.Context, signalURL, roomID, selfName string, stun []string) (*rtc.Room, error) {
	r, err := rtc.JoinRoom(ctx, signalURL, roomID, selfName, stun)
	if err != nil {
		return nil, err
	}
	r.OnPeer = func(remoteID string, rwc io.ReadWriteCloser, initiator bool) {
		log.Printf("hub: datachannel to %s open (initiator=%v); running HELLO", remoteID, initiator)
		if name, err := h.AttachDataChannel(rwc, selfName); err != nil {
			log.Printf("hub: attach %s failed: %v", remoteID, err)
			h.emit(HubEvent{Kind: EventInfo, Text: "webrtc attach " + remoteID + ": " + err.Error()})
		} else {
			log.Printf("hub: attached peer %q", name)
		}
	}
	r.OnPeerLeft = func(remoteID string) {
		// remoteID is the signaling id, which equals the HELLO name for now.
		if c := h.Get(remoteID); c != nil {
			c.Close() // runLoop observes EOF → emits EventDisconnect + removes
		}
	}
	// Drain the room's offline backlog: decrypt each stored blob with the room
	// key and surface it as a normal message event (the app dedupes by UUID).
	r.OnBacklog = func(blobs []string) {
		key := h.currentKey()
		if key == nil {
			return
		}
		for _, b := range blobs {
			plain, err := Open(key, b)
			if err != nil {
				continue
			}
			var bm backlogMsg
			if json.Unmarshal(plain, &bm) != nil {
				continue
			}
			h.emit(HubEvent{
				Kind: EventMessage, Peer: bm.Sender, Display: bm.Disp, Text: bm.Text, At: bm.At,
				Data: &MessageEventData{BoothID: bm.BoothID, UUID: bm.UUID, Backlog: true},
			})
		}
	}
	go func() { _ = r.Run(ctx) }()
	return r, nil
}

// backlogMsg is the cleartext shape of an offline-backlog blob (sealed before
// it leaves the device).
type backlogMsg struct {
	Sender  string    `json:"s"`           // sender's routing id
	Disp    string    `json:"d,omitempty"` // sender's friendly name
	UUID    string    `json:"u"`
	Text    string    `json:"t"`
	BoothID string    `json:"b"`
	At      time.Time `json:"at"`
}

// StoreMessage seals a room message and appends it to the room's offline
// backlog so members who join later receive it. No-op if the room isn't
// encrypted (no key) — we never hand the relay plaintext.
func (h *Hub) StoreMessage(ctx context.Context, r *rtc.Room, sender, display, uuid, text, boothID string, at time.Time) error {
	key := h.currentKey()
	if key == nil || r == nil {
		return nil
	}
	plain, err := json.Marshal(backlogMsg{Sender: sender, Disp: display, UUID: uuid, Text: text, BoothID: boothID, At: at})
	if err != nil {
		return err
	}
	blob, err := Seal(key, plain)
	if err != nil {
		return err
	}
	return r.Store(ctx, blob)
}

// RunWebRTC joins a room and blocks until ctx is cancelled, then leaves. A
// convenience wrapper around JoinRoom for callers (and tests) that want a
// single fixed room for the process lifetime.
func (h *Hub) RunWebRTC(ctx context.Context, signalURL, roomID, selfName string, stun []string) error {
	r, err := h.JoinRoom(ctx, signalURL, roomID, selfName, stun)
	if err != nil {
		return err
	}
	<-ctx.Done()
	r.Close()
	return ctx.Err()
}
