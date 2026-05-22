package peer

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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

	go func() { _ = ha.RunWebRTC(ctx, wsURL, "room", "alice", nil) }()
	go func() { _ = hb.RunWebRTC(ctx, wsURL, "room", "bob", nil) }()

	// Both sides must see the peer-connect before we exercise anything.
	waitEvent(t, ha, EventConnect, 20*time.Second)
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
	if err := hb.SendBooth("alice", "booth-1", "in the booth"); err != nil {
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
