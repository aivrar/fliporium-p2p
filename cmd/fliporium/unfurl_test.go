package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"net"
	"testing"

	"fliporium/internal/peer"
	"fliporium/internal/store"
)

func TestSanitizeLinkCardRejectsScriptURL(t *testing.T) {
	if _, ok := sanitizeLinkCard(peer.LinkCard{URL: "javascript:alert(1)", Kind: "link"}); ok {
		t.Fatal("javascript: link card was accepted")
	}
}

func TestSanitizeLinkCardStripsUnsafeImageData(t *testing.T) {
	card, ok := sanitizeLinkCard(peer.LinkCard{
		URL:   "https://example.com/page",
		Kind:  "link",
		Image: "data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+",
	})
	if !ok {
		t.Fatal("valid https card was rejected")
	}
	if card.Image != "" {
		t.Fatalf("unsafe image data URI survived: %q", card.Image)
	}
}

// TestIsNonPublicIP locks the SSRF guard: only publicly-routable addresses may
// be dialed when unfurling a link, so a posted link can't probe localhost, the
// LAN, or cloud metadata.
func TestIsNonPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.5", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", // link-local / cloud metadata
		"100.64.0.1",      // carrier-grade NAT
		"0.0.0.0",
		"::1", "fc00::1", "fe80::1",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: unparseable IP %q", s)
		}
		if !isNonPublicIP(ip) {
			t.Errorf("isNonPublicIP(%s) = false, want true (must be blocked)", s)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range public {
		ip := net.ParseIP(s)
		if isNonPublicIP(ip) {
			t.Errorf("isNonPublicIP(%s) = true, want false (must be allowed)", s)
		}
	}
}

// TestDecodableSize locks the decompression-bomb guard: an image whose declared
// dimensions blow past the pixel ceiling is rejected before image.Decode would
// allocate the full buffer.
func TestDecodableSize(t *testing.T) {
	if decodableSize([]byte("definitely not an image")) {
		t.Error("decodableSize accepted garbage bytes")
	}
	if !decodableSize(pngHeader(100, 100)) {
		t.Error("decodableSize rejected a sane 100x100 image")
	}
	if decodableSize(pngHeader(60000, 60000)) {
		t.Error("decodableSize accepted a 60000x60000 decompression bomb")
	}
}

// pngHeader builds just enough of a PNG (signature + IHDR with a valid CRC) for
// image.DecodeConfig to read its dimensions — no pixel data needed.
func pngHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type: truecolor
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(ihdr)))
	b.Write(length[:])
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(chunk))
	b.Write(crc[:])
	return b.Bytes()
}

// TestInSameConversation locks reaction/pin scoping: a control frame is only
// honored for a message in the conversation its connection rides.
func TestInSameConversation(t *testing.T) {
	booth := store.Message{BoothID: "room-1", Peer: "fp-alice"}
	if !inSameConversation(booth, "fp-anyone", "room-1") {
		t.Error("booth message on its own room should be allowed")
	}
	if inSameConversation(booth, "fp-anyone", "room-2") {
		t.Error("booth message must NOT be honored on a different room's connection")
	}
	dm := store.Message{BoothID: "", Peer: "fp-bob"}
	if !inSameConversation(dm, "fp-bob", "room-ignored") {
		t.Error("1:1 message from the counterpart should be allowed")
	}
	if inSameConversation(dm, "fp-eve", "room-ignored") {
		t.Error("1:1 message must NOT be honored from a third party")
	}
}
