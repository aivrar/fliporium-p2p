package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSearchSnippetSentinels confirms full-text search works and wraps matches
// in control-char sentinels (\x02 .. \x03) rather than literal <mark> tags, so
// the frontend can HTML-escape the snippet before turning sentinels into marks
// (the fix for snippet XSS).
func TestSearchSnippetSentinels(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	if err := s.AppendMessageFull(ctx, Message{
		UUID: "u1", Peer: "p", Direction: DirectionIn,
		Text: "hello secret world", At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := s.SearchMessages(ctx, "secret", 10)
	if err != nil {
		t.Fatalf("search errored (char(2)/char(3) snippet markers rejected?): %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d", len(hits))
	}
	snip := hits[0].Snippet
	if !strings.Contains(snip, "\x02") || !strings.Contains(snip, "\x03") {
		t.Fatalf("snippet missing sentinel markers: %q", snip)
	}
	if strings.Contains(snip, "<mark>") {
		t.Fatalf("snippet should use sentinels, not literal <mark>: %q", snip)
	}
}
