package identity

import "testing"

func TestLoadIsStableAcrossReloads(t *testing.T) {
	dir := t.TempDir()

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if a.ID() != b.ID() {
		t.Fatalf("id changed across reload: %s != %s", a.ID(), b.ID())
	}
	if a.PubHex() != b.PubHex() {
		t.Fatal("public key changed across reload")
	}
	if len(a.ID()) != 16 {
		t.Fatalf("id should be 16 hex chars, got %q", a.ID())
	}

	// A different dir must yield a different identity.
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	if c.ID() == a.ID() {
		t.Fatal("distinct dirs produced the same identity")
	}
}
