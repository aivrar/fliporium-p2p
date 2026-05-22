package peer

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
)

// Room-level end-to-end encryption. Every member derives the same 32-byte
// room key from the secret carried in the invite link (which the signaling
// server never sees). Message bodies are sealed with NaCl secretbox so that
// nothing in the middle — signaling server, TURN relay, or a future
// store-and-forward relay — can read them. WebRTC's DTLS already encrypts the
// live hop; this adds application-layer E2E that survives relaying.
//
// HELLO stays in the clear (it's the bootstrap handshake and carries only a
// name + version); everything after is sealed.

// sealBody encrypts plaintext under key and returns it as a JSON string value
// (base64 of nonce||ciphertext) so it can ride in an Envelope.Body unchanged.
func sealBody(key *[32]byte, plain []byte) (json.RawMessage, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	sealed := secretbox.Seal(nonce[:], plain, &nonce, key)
	b, err := json.Marshal(base64.StdEncoding.EncodeToString(sealed))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// openBody reverses sealBody.
func openBody(key *[32]byte, body json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("encrypted body is not a string: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	if len(raw) < 24 {
		return nil, fmt.Errorf("ciphertext too short")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	plain, ok := secretbox.Open(nil, raw[24:], &nonce, key)
	if !ok {
		return nil, fmt.Errorf("decryption failed (wrong room key?)")
	}
	return plain, nil
}
