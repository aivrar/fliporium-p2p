// WebRTC transport glue. This wires the transport-only rtc package (signaling
// + DataChannel mesh) into the existing Hub: each opened DataChannel becomes a
// PeerConn driven by the same runLoop, so every feature (chat, flips, booths,
// reactions...) works peer-to-peer with zero protocol changes. The
// 4-byte length-prefixed envelopes ride the DataChannel as-is.
package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"fliporium/internal/identity"
	"fliporium/internal/rtc"
)

// AttachDataChannel runs the HELLO handshake over an already-open transport
// (a detached WebRTC DataChannel) and registers the resulting peer, then
// starts its runLoop. Mirrors Dial/Accept but works on any io.ReadWriteCloser.
//
// Both sides write HELLO then read it; DataChannel writes are buffered, so
// there's no handshake deadlock.
func (h *Hub) AttachDataChannel(rwc io.ReadWriteCloser, selfName string) (string, error) {
	// Run the (blocking, multi-frame) handshake under a deadline so a stalled or
	// hostile peer can't hang the attach forever. The channel is buffered so the
	// goroutine never leaks even if we time out and move on.
	type result struct {
		remote Hello
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		remote, err := h.handshake(rwc, selfName)
		ch <- result{remote, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(15 * time.Second):
		rwc.Close()
		return "", fmt.Errorf("handshake timeout")
	}
	if res.err != nil {
		rwc.Close()
		return "", res.err
	}
	remote := res.remote
	pc := &PeerConn{Name: remote.Name, Display: remote.DisplayName, Avatar: remote.Avatar, Addr: "webrtc", Version: remote.Version, conn: rwc, key: h.currentKey()}
	h.add(pc)
	h.emit(HubEvent{Kind: EventConnect, Peer: pc.Name, Display: pc.Display, Avatar: pc.Avatar, Text: "webrtc peer"})
	go h.runLoop(pc)
	return remote.Name, nil
}

// handshake runs the HELLO exchange plus mutual identity proof, returning the
// verified remote HELLO (no side effects — the caller registers the peer).
//
// A peer whose routing id starts with "fp-" MUST present the Ed25519 key that
// id is derived from AND sign our fresh nonce, or we refuse the connection.
// This is what prevents one room member from impersonating another (forging
// messages, or editing/deleting someone else's messages by claiming their id).
func (h *Hub) handshake(rwc io.ReadWriteCloser, selfName string) (Hello, error) {
	priv, pub := h.identity()
	var myNonce []byte
	hello := Hello{Name: selfName, Version: ProtocolVersion, DisplayName: h.selfDisp(), Avatar: h.selfAv()}
	if priv != nil && pub != nil {
		myNonce = make([]byte, 32)
		if _, err := rand.Read(myNonce); err != nil {
			return Hello{}, err
		}
		hello.PubKey = pub
		hello.Nonce = myNonce
	}
	if err := WriteFrame(rwc, TypeHello, hello); err != nil {
		return Hello{}, fmt.Errorf("send HELLO: %w", err)
	}
	env, err := ReadFrame(rwc)
	if err != nil {
		return Hello{}, fmt.Errorf("receive HELLO: %w", err)
	}
	if env.Type != TypeHello {
		return Hello{}, fmt.Errorf("expected HELLO, got %q", env.Type)
	}
	var remote Hello
	if err := json.Unmarshal(env.Body, &remote); err != nil {
		return Hello{}, fmt.Errorf("decode HELLO: %w", err)
	}
	if h.IsBlocked(remote.Name) {
		return Hello{}, fmt.Errorf("blocked peer %q", remote.Name)
	}
	// Bound peer-controlled strings so a hostile peer can't bloat the UI/store
	// with a megabyte "display name".
	if len(remote.DisplayName) > 80 {
		remote.DisplayName = remote.DisplayName[:80]
	}
	if len(remote.Name) > 128 {
		return Hello{}, fmt.Errorf("peer id too long")
	}
	// The avatar is rendered in an <img src>, so accept only a small, well-formed
	// image data: URI — never an arbitrary (e.g. javascript:) URI, and never
	// something big enough to bloat the handshake/store.
	remote.Avatar = sanitizeAvatar(remote.Avatar)

	// Any peer claiming an "fp-" identity must prove it owns the matching key.
	mustVerify := strings.HasPrefix(remote.Name, "fp-")
	if mustVerify {
		if len(remote.PubKey) != ed25519.PublicKeySize {
			return Hello{}, fmt.Errorf("peer %q presented no valid key", remote.Name)
		}
		if "fp-"+identity.FingerprintID(remote.PubKey) != remote.Name {
			return Hello{}, fmt.Errorf("peer id %q does not match its key", remote.Name)
		}
		if len(remote.Nonce) == 0 {
			return Hello{}, fmt.Errorf("peer %q sent no challenge", remote.Name)
		}
		if myNonce == nil {
			return Hello{}, fmt.Errorf("cannot verify peer %q: no local identity", remote.Name)
		}
	}
	// Prove ourselves: sign a challenge bound to BOTH nonces (theirs and ours),
	// not just theirs. Signing the bare peer nonce makes the proof relayable — a
	// man-in-the-middle could get us to sign a challenge it was itself handed by
	// a third party and replay our signature to impersonate us. Mixing in our own
	// fresh nonce (which the attacker can't control) ties the signature to this
	// specific pairing. (Only if we have a key and they challenged us.)
	if priv != nil && len(remote.Nonce) > 0 {
		if err := WriteFrame(rwc, TypeAuth, Auth{Sig: ed25519.Sign(priv, authChallenge(remote.Nonce, myNonce))}); err != nil {
			return Hello{}, fmt.Errorf("send AUTH: %w", err)
		}
	}
	// Verify them: they must sign the challenge bound to OUR nonce (which they
	// received) and THEIR nonce (which they sent), with the key matching their id.
	if mustVerify {
		authEnv, err := ReadFrame(rwc)
		if err != nil {
			return Hello{}, fmt.Errorf("receive AUTH: %w", err)
		}
		if authEnv.Type != TypeAuth {
			return Hello{}, fmt.Errorf("expected AUTH from %q, got %q", remote.Name, authEnv.Type)
		}
		var au Auth
		if err := json.Unmarshal(authEnv.Body, &au); err != nil {
			return Hello{}, fmt.Errorf("decode AUTH: %w", err)
		}
		if !ed25519.Verify(ed25519.PublicKey(remote.PubKey), authChallenge(myNonce, remote.Nonce), au.Sig) {
			return Hello{}, fmt.Errorf("peer %q failed identity proof", remote.Name)
		}
	}
	return remote, nil
}

// authChallenge binds an identity proof to BOTH session nonces so a signature
// can't be relayed into a different session (a classic MITM on a bare
// challenge-response). peerNonce is the nonce we received from the other side;
// ownNonce is the one we sent. Each side feeds the same two values in opposite
// received/sent roles, so the signer computes H(peerNonce‖ownNonce) and the
// verifier checks H(theirReceived‖theirSent) — the identical byte string —
// while a relay attacker, unable to control the honest party's own fresh nonce,
// can never make the two line up.
func authChallenge(peerNonce, ownNonce []byte) []byte {
	h := sha256.New()
	h.Write([]byte("fliporium-auth-v1"))
	writeLenPrefixed(h, peerNonce)
	writeLenPrefixed(h, ownNonce)
	return h.Sum(nil)
}

// writeLenPrefixed writes a 4-byte big-endian length then the bytes, so that
// concatenated fields can't be confused with a different split of the same
// total bytes when hashed.
func writeLenPrefixed(w io.Writer, b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	_, _ = w.Write(n[:])
	_, _ = w.Write(b)
}

// backlogSigBytes is the canonical message an offline-backlog entry's author
// signs (and verifiers re-derive). It binds the entry to the sender id, message
// id, room, text, and timestamp so a holder of the shared room key can't forge
// a stored message attributed to a different member.
func backlogSigBytes(sender, uuid, boothID, text, atRFC string) []byte {
	h := sha256.New()
	h.Write([]byte("fliporium-backlog-v1"))
	for _, s := range []string{sender, uuid, boothID, text, atRFC} {
		writeLenPrefixed(h, []byte(s))
	}
	return h.Sum(nil)
}

// maxAvatarBytes caps a peer-supplied avatar so a hostile peer can't bloat the
// handshake or our store. Avatars are downscaled to a few KB; 48KB is generous.
const maxAvatarBytes = 48 * 1024

// sanitizeAvatar returns the avatar only if it's a small, well-formed image
// data: URI; otherwise "". The avatar is rendered in an <img src>, so this is
// the gate that keeps a hostile peer from supplying a javascript:/arbitrary URI
// or an oversized blob.
func sanitizeAvatar(uri string) string {
	if len(uri) == 0 || len(uri) > maxAvatarBytes {
		return ""
	}
	switch {
	case strings.HasPrefix(uri, "data:image/jpeg;base64,"),
		strings.HasPrefix(uri, "data:image/png;base64,"),
		strings.HasPrefix(uri, "data:image/webp;base64,"),
		strings.HasPrefix(uri, "data:image/gif;base64,"):
		return uri
	}
	return ""
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
			// Authorship check: the shared room key proves only that the storer is
			// a room member, NOT who wrote the message — so without this any member
			// could store a backlog entry impersonating another. Identity-bearing
			// senders ("fp-…") must carry their key (fingerprint == claimed id) and
			// a valid signature over the entry. Non-"fp-" dev senders pass through.
			if strings.HasPrefix(bm.Sender, "fp-") {
				if len(bm.Pub) != ed25519.PublicKeySize ||
					"fp-"+identity.FingerprintID(bm.Pub) != bm.Sender ||
					!ed25519.Verify(ed25519.PublicKey(bm.Pub), backlogSigBytes(bm.Sender, bm.UUID, bm.BoothID, bm.Text, bm.At.UTC().Format(time.RFC3339Nano)), bm.Sig) {
					continue
				}
			}
			h.emit(HubEvent{
				Kind: EventMessage, Peer: bm.Sender, Display: bm.Disp, Text: bm.Text, At: bm.At,
				Data: &MessageEventData{BoothID: bm.BoothID, UUID: bm.UUID, ParentUUID: bm.Parent, Backlog: true},
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
	Parent  string    `json:"pp,omitempty"` // parent message UUID, if this was a reply
	Pub     []byte    `json:"pk,omitempty"` // sender's Ed25519 key; fingerprint must equal Sender
	Sig     []byte    `json:"sg,omitempty"` // signature binding the entry to the sender's identity
}

// StoreMessage seals a room message and appends it to the room's offline
// backlog so members who join later receive it. No-op if the room isn't
// encrypted (no key) — we never hand the relay plaintext.
func (h *Hub) StoreMessage(ctx context.Context, r *rtc.Room, sender, display, uuid, text, boothID, parentUUID string, at time.Time) error {
	key := h.currentKey()
	if key == nil || r == nil {
		return nil
	}
	bm := backlogMsg{Sender: sender, Disp: display, UUID: uuid, Text: text, BoothID: boothID, At: at, Parent: parentUUID}
	// Sign the entry to our identity so other room members (who all hold the
	// shared room key) can't later forge a stored message "from" us.
	if priv, pub := h.identity(); priv != nil {
		bm.Pub = pub
		bm.Sig = ed25519.Sign(priv, backlogSigBytes(sender, uuid, boothID, text, at.UTC().Format(time.RFC3339Nano)))
	}
	plain, err := json.Marshal(bm)
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
