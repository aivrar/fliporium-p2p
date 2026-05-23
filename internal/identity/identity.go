// Package identity gives each Fliporium install a stable cryptographic
// identity: an Ed25519 keypair generated on first launch and persisted in the
// data dir. The public-key fingerprint is the stable peer id (so two people
// can both call themselves "alex" without colliding), and the keypair is what
// Phase 3's end-to-end encryption will use for sealed-box room-key exchange.
//
// The display name is separate and mutable — it's just a label, not identity.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

const keyFile = "identity.key"

// Identity is a device's long-lived Ed25519 keypair.
type Identity struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// Load returns the identity stored at <dir>/identity.key, generating and
// persisting a fresh one on first use. The key file is written 0600.
func Load(dir string) (Identity, error) {
	path := filepath.Join(dir, keyFile)
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != ed25519.PrivateKeySize {
			return Identity{}, fmt.Errorf("identity key %q is corrupt (%d bytes)", path, len(b))
		}
		priv := ed25519.PrivateKey(append([]byte(nil), b...))
		return Identity{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate keypair: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, err
	}
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		return Identity{}, fmt.Errorf("persist identity: %w", err)
	}
	return Identity{Priv: priv, Pub: pub}, nil
}

// ID is the short, stable fingerprint of the public key (16 hex chars). Used
// as the peer id at the signaling and routing layers.
func (i Identity) ID() string {
	return FingerprintID(i.Pub)
}

// FingerprintID derives the short, stable id from any Ed25519 public key. The
// peer-auth handshake uses it to verify that a peer claiming "fp-<id>" actually
// holds the matching key (sha256 over 8 bytes = 64-bit binding; forging a key
// with a chosen fingerprint is infeasible), which is what stops impersonation.
func FingerprintID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// PubHex is the full public key in hex, for key exchange.
func (i Identity) PubHex() string {
	return hex.EncodeToString(i.Pub)
}
