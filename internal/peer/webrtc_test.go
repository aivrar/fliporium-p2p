package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"fliporium/internal/identity"
	"fliporium/internal/rtc"
)

// waitEvent reads the hub's event stream until an event of `kind` arrives,
// skipping others, or fails on timeout.
func waitEvent(t *testing.T, h *Hub, kind HubEventKind, timeout time.Duration) HubEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-h.Events:
			if ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %s", timeout, kind)
		}
	}
}

// TestSanitizeAvatar verifies the avatar gate: only small, well-formed image
// data: URIs survive. Anything else (a script-bearing svg, a javascript: URI,
// a remote URL, or an oversized blob) must be dropped, since the avatar is
// rendered in an <img src> on every peer that connects.
func TestSanitizeAvatar(t *testing.T) {
	good := []string{
		"data:image/jpeg;base64,/9j/AAAQSkZJRg==",
		"data:image/png;base64,iVBORw0KGgo=",
		"data:image/gif;base64,R0lGODlh",
		"data:image/webp;base64,UklGRg==",
	}
	for _, g := range good {
		if got := sanitizeAvatar(g); got != g {
			t.Fatalf("sanitizeAvatar(%q) = %q, want it kept", g, got)
		}
	}
	bad := []string{
		"",
		"javascript:alert(1)",
		"http://example.com/x.png",
		"data:text/html;base64,PHNjcmlwdD4=",
		"data:image/svg+xml;base64,PHN2Zz4=", // svg can carry script
		"definitely not a uri",
	}
	for _, b := range bad {
		if got := sanitizeAvatar(b); got != "" {
			t.Fatalf("sanitizeAvatar(%q) = %q, want \"\" (rejected)", b, got)
		}
	}
	if got := sanitizeAvatar("data:image/jpeg;base64," + strings.Repeat("A", maxAvatarBytes)); got != "" {
		t.Fatal("oversized avatar accepted; want rejected")
	}
}

// TestWebRTCHubParity proves the existing Hub machinery — 1:1 messages, booth
// messages, and chunked file flips — works unchanged over the WebRTC transport,
// using an in-process signaling server and host-only ICE (no network needed).
func TestWebRTCHubParity(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	catchDir := t.TempDir()
	ha := NewHub()
	hb := NewHub()
	hb.CatchRoot = catchDir
	ha.SetSelfDisplay("Alice")
	hb.SetSelfDisplay("Bob")
	hb.SetSelfAvatar("data:image/png;base64,iVBORw0KGgoAAAA=")

	go func() { _ = ha.RunWebRTC(ctx, wsURL, "room", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "room", "bob", nil) }()

	// Both sides must see the peer-connect before we exercise anything.
	connectEv := waitEvent(t, ha, EventConnect, 20*time.Second)
	if connectEv.Display != "Bob" {
		t.Fatalf("alice saw peer display %q, want Bob", connectEv.Display)
	}
	if connectEv.Avatar != "data:image/png;base64,iVBORw0KGgoAAAA=" {
		t.Fatalf("alice saw peer avatar %q, want bob's announced avatar", connectEv.Avatar)
	}
	waitEvent(t, hb, EventConnect, 20*time.Second)

	if ha.Get("bob") == nil {
		t.Fatal("alice's hub has no connection to bob after connect")
	}
	if hb.Get("alice") == nil {
		t.Fatal("bob's hub has no connection to alice after connect")
	}

	// 1:1 message alice -> bob.
	if err := ha.Send("bob", "hello over webrtc"); err != nil {
		t.Fatalf("alice send: %v", err)
	}
	ev := waitEvent(t, hb, EventMessage, 10*time.Second)
	if ev.Text != "hello over webrtc" {
		t.Fatalf("bob got %q, want %q", ev.Text, "hello over webrtc")
	}
	if ev.Peer != "alice" {
		t.Fatalf("bob saw sender %q, want alice", ev.Peer)
	}

	// Booth-scoped message bob -> alice.
	if err := hb.SendBooth("alice", "booth-1", "in the booth", "booth-msg-uuid", ""); err != nil {
		t.Fatalf("bob send booth: %v", err)
	}
	ev = waitEvent(t, ha, EventMessage, 10*time.Second)
	if ev.Text != "in the booth" {
		t.Fatalf("alice got %q, want %q", ev.Text, "in the booth")
	}
	if d, ok := ev.Data.(*MessageEventData); !ok || d.BoothID != "booth-1" {
		t.Fatalf("alice booth message missing BoothID booth-1: %+v", ev.Data)
	}

	// File flip alice -> bob, verify the bytes land intact on bob's disk.
	want := bytes.Repeat([]byte("fliporium-flip-payload\n"), 5000) // ~115 KB, multi-chunk
	src, err := os.CreateTemp(t.TempDir(), "flip-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Write(want); err != nil {
		t.Fatal(err)
	}
	src.Close()

	if _, err := ha.SendFlip("bob", src.Name()); err != nil {
		t.Fatalf("alice flip: %v", err)
	}
	done := waitEvent(t, hb, EventFlipCompleted, 20*time.Second)
	fd, ok := done.Data.(*FlipEventData)
	if !ok {
		t.Fatalf("flip-completed missing FlipEventData: %+v", done.Data)
	}
	got, err := os.ReadFile(fd.Path)
	if err != nil {
		t.Fatalf("read caught file %q: %v", fd.Path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("caught file content mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

// TestWebRTCEncryptedRoom proves messages round-trip when both peers share a
// room key, and that with the key set the data still flows (E2E layer on).
func TestWebRTCEncryptedRoom(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var key [32]byte
	copy(key[:], []byte("a-shared-room-key-32-bytes-long!"))

	ha := NewHub()
	hb := NewHub()
	ha.SetRoomKey(&key)
	hb.SetRoomKey(&key)
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "enc", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "enc", "bob", nil) }()

	waitEvent(t, ha, EventConnect, 20*time.Second)
	waitEvent(t, hb, EventConnect, 20*time.Second)

	if err := ha.Send("bob", "secret hello"); err != nil {
		t.Fatalf("alice send: %v", err)
	}
	ev := waitEvent(t, hb, EventMessage, 10*time.Second)
	if ev.Text != "secret hello" {
		t.Fatalf("bob got %q, want %q", ev.Text, "secret hello")
	}
}

// TestWebRTCKeyMismatch proves a peer with the wrong room key cannot read
// messages — they fail to decrypt rather than arriving as plaintext.
func TestWebRTCKeyMismatch(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var k1, k2 [32]byte
	copy(k1[:], []byte("room-key-number-one-padding-32!!"))
	copy(k2[:], []byte("room-key-number-two-padding-32!!"))

	ha := NewHub()
	hb := NewHub()
	ha.SetRoomKey(&k1)
	hb.SetRoomKey(&k2) // different key
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "mismatch", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "mismatch", "bob", nil) }()

	waitEvent(t, ha, EventConnect, 20*time.Second)
	waitEvent(t, hb, EventConnect, 20*time.Second)

	if err := ha.Send("bob", "you can't read this"); err != nil {
		t.Fatalf("alice send: %v", err)
	}
	// bob can't decrypt → surfaces an info event mentioning decrypt, never a
	// clean EventMessage.
	ev := waitEvent(t, hb, EventInfo, 10*time.Second)
	if !strings.Contains(ev.Text, "decrypt") {
		t.Fatalf("expected a decrypt failure, got info %q", ev.Text)
	}
}

// TestWebRTCBacklogMessage proves offline delivery: a message stored while no
// one else is in the room is replayed (decrypted) to a peer who joins later.
func TestWebRTCBacklogMessage(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var key [32]byte
	copy(key[:], []byte("backlog-room-key-32-bytes-pad!!!"))

	ha := NewHub()
	ha.SetRoomKey(&key)
	rA, err := ha.JoinRoom(ctx, wsURL, "bk", "alice", nil)
	if err != nil {
		t.Fatalf("alice join: %v", err)
	}
	// Alice posts while she's alone; it goes only to the encrypted backlog.
	if err := ha.StoreMessage(ctx, rA, "alice", "Alice", "uuid-1", "offline hi", "bk", "", time.Now().UTC()); err != nil {
		t.Fatalf("store: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // let the server persist it

	hb := NewHub()
	hb.SetRoomKey(&key)
	if _, err := hb.JoinRoom(ctx, wsURL, "bk", "bob", nil); err != nil {
		t.Fatalf("bob join: %v", err)
	}

	ev := waitEvent(t, hb, EventMessage, 10*time.Second)
	if ev.Text != "offline hi" {
		t.Fatalf("bob backlog got %q, want %q", ev.Text, "offline hi")
	}
	if ev.Display != "Alice" {
		t.Fatalf("backlog display = %q, want %q", ev.Display, "Alice")
	}
	d, ok := ev.Data.(*MessageEventData)
	if !ok || !d.Backlog || d.UUID != "uuid-1" {
		t.Fatalf("expected backlog message uuid-1, got %+v", ev.Data)
	}
}

// TestWebRTCFlipSniffsMime proves a file with no extension still gets a real
// mime (content-sniffed) AND that sniffing's read-then-rewind doesn't corrupt
// the transfer — the bytes must land intact.
func TestWebRTCFlipSniffsMime(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	catchDir := t.TempDir()
	ha := NewHub()
	hb := NewHub()
	hb.CatchRoot = catchDir
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "flip", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "flip", "bob", nil) }()
	waitEvent(t, ha, EventConnect, 20*time.Second)
	waitEvent(t, hb, EventConnect, 20*time.Second)

	// A PNG header + filler, written to a file with NO extension ("noext-*"
	// yields names like "noext-1234567" — no dot, so TypeByExtension fails and
	// the sniffing path runs).
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("payload-bytes!"), 1000)...)
	src, err := os.CreateTemp(t.TempDir(), "noext-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Write(png); err != nil {
		t.Fatal(err)
	}
	src.Close()

	if _, err := ha.SendFlip("bob", src.Name()); err != nil {
		t.Fatalf("send flip: %v", err)
	}
	// The SENDER's completed event must also carry the mime, or the sender's
	// own preview (served from their original file) won't render.
	sent := waitEvent(t, ha, EventFlipCompleted, 20*time.Second)
	if sfd, ok := sent.Data.(*FlipEventData); !ok || sfd.Mime != "image/png" {
		t.Fatalf("sender completed mime = %+v, want image/png", sent.Data)
	}
	done := waitEvent(t, hb, EventFlipCompleted, 20*time.Second)
	fd, ok := done.Data.(*FlipEventData)
	if !ok {
		t.Fatalf("no FlipEventData: %+v", done.Data)
	}
	if fd.Mime != "image/png" {
		t.Fatalf("sniffed mime = %q, want image/png", fd.Mime)
	}
	got, err := os.ReadFile(fd.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("received %d bytes, want %d (rewind after sniff likely dropped the head)", len(got), len(png))
	}
}

// TestWebRTCBlockedPeerRefused proves a blocked peer never becomes a live
// connection: Alice blocks Bob, so even though they share a room and the mesh
// negotiates a DataChannel, Alice's HELLO handshake refuses him — and the
// connection collapses on both sides, so no data can flow either way.
func TestWebRTCBlockedPeerRefused(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ha := NewHub()
	hb := NewHub()
	ha.SetBlocked([]string{"bob"}) // alice blocks bob
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "blk", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "blk", "bob", nil) }()

	// Give the mesh ample time to negotiate (and collapse the refused channel).
	time.Sleep(3 * time.Second)

	if ha.Get("bob") != nil {
		t.Fatalf("alice has a live connection to a blocked peer: %v", ha.Names())
	}
	if hb.Get("alice") != nil {
		t.Fatalf("bob still connected to alice who blocked him: %v", hb.Names())
	}
	if !ha.IsBlocked("bob") {
		t.Fatal("IsBlocked(bob) should be true")
	}
}

// TestWebRTCFlipCarriesBoothID proves a booth-scoped flip carries its booth id
// to the receiver, so the file files under the right conversation and can't
// leak into other rooms that happen to share a member.
func TestWebRTCFlipCarriesBoothID(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	catchDir := t.TempDir()
	ha := NewHub()
	hb := NewHub()
	hb.CatchRoot = catchDir
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "scope", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "scope", "bob", nil) }()
	waitEvent(t, ha, EventConnect, 20*time.Second)
	waitEvent(t, hb, EventConnect, 20*time.Second)

	src, err := os.CreateTemp(t.TempDir(), "scoped-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Write(bytes.Repeat([]byte("x"), 2048)); err != nil {
		t.Fatal(err)
	}
	src.Close()

	if err := ha.SendFlipWithID("bob", src.Name(), "flip-scope-1", "booth-XYZ"); err != nil {
		t.Fatalf("send booth flip: %v", err)
	}
	done := waitEvent(t, hb, EventFlipCompleted, 20*time.Second)
	fd, ok := done.Data.(*FlipEventData)
	if !ok {
		t.Fatalf("no FlipEventData: %+v", done.Data)
	}
	if fd.BoothID != "booth-XYZ" {
		t.Fatalf("received flip BoothID = %q, want booth-XYZ", fd.BoothID)
	}
}

// TestWebRTCIdentityAuth proves two peers with real Ed25519 identities and
// "fp-" routing ids authenticate each other and exchange a message.
func TestWebRTCIdentityAuth(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	nameA := "fp-" + identity.FingerprintID(pubA)
	nameB := "fp-" + identity.FingerprintID(pubB)

	ha := NewHub()
	ha.SetIdentity(privA, pubA)
	hb := NewHub()
	hb.SetIdentity(privB, pubB)
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "auth", nameA, nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "auth", nameB, nil) }()

	waitEvent(t, ha, EventConnect, 20*time.Second)
	waitEvent(t, hb, EventConnect, 20*time.Second)
	if ha.Get(nameB) == nil {
		t.Fatalf("alice not connected to authenticated bob")
	}
	if err := ha.Send(nameB, "authed hi"); err != nil {
		t.Fatalf("send: %v", err)
	}
	ev := waitEvent(t, hb, EventMessage, 10*time.Second)
	if ev.Text != "authed hi" || ev.Peer != nameA {
		t.Fatalf("bob got %q from %q, want %q from %q", ev.Text, ev.Peer, "authed hi", nameA)
	}
}

// TestWebRTCImpersonationRejected proves the core anti-impersonation property:
// an attacker who knows the victim's PUBLIC key (it's broadcast in HELLOs) and
// claims the victim's id — but lacks the private key — cannot pass the signed
// challenge, so an honest peer never accepts the impersonated connection.
func TestWebRTCImpersonationRejected(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pubVictim, _, _ := ed25519.GenerateKey(rand.Reader) // private key NOT known to attacker
	_, privAtk, _ := ed25519.GenerateKey(rand.Reader)
	victimID := "fp-" + identity.FingerprintID(pubVictim)

	pubC, privC, _ := ed25519.GenerateKey(rand.Reader)
	carolID := "fp-" + identity.FingerprintID(pubC)

	hCarol := NewHub()
	hCarol.SetIdentity(privC, pubC)
	// Attacker presents the victim's public key (correct fingerprint for
	// victimID) but can only sign with its OWN private key.
	hAtk := NewHub()
	hAtk.SetIdentity(privAtk, pubVictim)
	go func() { _ = hCarol.RunWebRTC(ctx, wsURL, "imp", carolID, nil) }()
	go func() { _ = hAtk.RunWebRTC(ctx, wsURL, "imp", victimID, nil) }()

	time.Sleep(4 * time.Second) // let the mesh negotiate + the handshake be refused
	if hCarol.Get(victimID) != nil {
		t.Fatalf("carol accepted an impersonator claiming %s", victimID)
	}
}

// TestAuthChallengeBinding locks the property that makes the identity proof
// non-relayable: the signed challenge depends on BOTH nonces and on their
// order (received vs sent). If it were symmetric, or ignored either nonce, a
// man-in-the-middle could relay a signature from one session into another.
func TestAuthChallengeBinding(t *testing.T) {
	a := []byte("nonce-aaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	b := []byte("nonce-bbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	c := []byte("nonce-cccccccccccccccccccccccccccc")
	if bytes.Equal(authChallenge(a, b), authChallenge(b, a)) {
		t.Fatal("authChallenge must not be symmetric in its two nonces")
	}
	if bytes.Equal(authChallenge(a, b), authChallenge(a, c)) {
		t.Fatal("changing the own-nonce must change the challenge")
	}
	if bytes.Equal(authChallenge(a, b), authChallenge(c, b)) {
		t.Fatal("changing the peer-nonce must change the challenge")
	}
}

// TestWebRTCBacklogForgeryRejected proves a room member — who necessarily holds
// the shared room key — cannot forge an offline-backlog entry attributed to
// another member. A genuine signed entry from Bob is delivered; an entry sealed
// with the same room key but claiming to be from a victim (with no/invalid
// signature) is dropped on drain.
func TestWebRTCBacklogForgeryRejected(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var key [32]byte
	copy(key[:], []byte("forge-room-key-32-bytes-padding!"))

	pubVictim, _, _ := ed25519.GenerateKey(rand.Reader) // attacker does NOT own this
	victimID := "fp-" + identity.FingerprintID(pubVictim)
	pubAtk, privAtk, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	bobID := "fp-" + identity.FingerprintID(pubB)

	// Attacker (holds the room key) stores two forgeries claiming to be the victim.
	attacker := NewHub()
	attacker.SetRoomKey(&key)
	rAtk, err := attacker.JoinRoom(ctx, wsURL, "fk", "attacker", nil)
	if err != nil {
		t.Fatalf("attacker join: %v", err)
	}
	// (1) no signature at all.
	noSig := backlogMsg{Sender: victimID, Disp: "Victim", UUID: "forged-nosig", Text: "i never said this", BoothID: "fk", At: time.Now().UTC()}
	// (2) a valid signature, but made with the attacker's OWN key while still
	//     claiming the victim's id (fingerprint won't match).
	now := time.Now().UTC()
	wrongKey := backlogMsg{Sender: victimID, Disp: "Victim", UUID: "forged-wrongkey", Text: "neither this", BoothID: "fk", At: now,
		Pub: pubAtk, Sig: ed25519.Sign(privAtk, backlogSigBytes(victimID, "forged-wrongkey", "fk", "neither this", now.Format(time.RFC3339Nano)))}
	for _, m := range []backlogMsg{noSig, wrongKey} {
		blob, err := Seal(&key, mustJSONBacklog(t, m))
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		if err := rAtk.Store(ctx, blob); err != nil {
			t.Fatalf("store forged: %v", err)
		}
	}

	// Bob (a genuine member) stores a properly-signed message.
	bob := NewHub()
	bob.SetRoomKey(&key)
	bob.SetIdentity(privB, pubB)
	rBob, err := bob.JoinRoom(ctx, wsURL, "fk", bobID, nil)
	if err != nil {
		t.Fatalf("bob join: %v", err)
	}
	if err := bob.StoreMessage(ctx, rBob, bobID, "Bob", "bob-real", "hello, this is really bob", "fk", "", time.Now().UTC()); err != nil {
		t.Fatalf("bob store: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // let the server persist all three

	// Alice joins and drains the backlog. She must see Bob's real message and
	// never either forgery.
	alice := NewHub()
	alice.SetRoomKey(&key)
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	alice.SetIdentity(privA, pubA)
	if _, err := alice.JoinRoom(ctx, wsURL, "fk", "fp-"+identity.FingerprintID(pubA), nil); err != nil {
		t.Fatalf("alice join: %v", err)
	}

	gotBob := false
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-alice.Events:
			if ev.Kind != EventMessage {
				continue
			}
			d, _ := ev.Data.(*MessageEventData)
			if d != nil && (d.UUID == "forged-nosig" || d.UUID == "forged-wrongkey") {
				t.Fatalf("forged backlog entry %q surfaced — impersonation not blocked", d.UUID)
			}
			if d != nil && d.UUID == "bob-real" {
				gotBob = true
			}
		case <-deadline:
			if !gotBob {
				t.Fatal("never received Bob's genuine signed backlog message")
			}
			return
		}
	}
}

func mustJSONBacklog(t *testing.T, m backlogMsg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal backlogMsg: %v", err)
	}
	return b
}

// TestWebRTCRoomIsolation proves peers only mesh-connect within their own
// signaling room — the basis for invite-link rooms being separate spaces.
func TestWebRTCRoomIsolation(t *testing.T) {
	srv := httptest.NewServer(rtc.NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ha := NewHub()
	hb := NewHub()
	hc := NewHub()
	go func() { _ = ha.RunWebRTC(ctx, wsURL, "roomA", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "roomA", "bob", nil) }()
	go func() { _ = hc.RunWebRTC(ctx, wsURL, "roomB", "carol", nil) }()

	// alice and bob share roomA → they connect.
	waitEvent(t, ha, EventConnect, 20*time.Second)
	if ha.Get("bob") == nil {
		t.Fatal("alice should be connected to bob in roomA")
	}
	// carol is alone in roomB → alice must never see her.
	time.Sleep(500 * time.Millisecond)
	if ha.Get("carol") != nil {
		t.Fatalf("alice connected to carol across rooms: %v", ha.Names())
	}
}
