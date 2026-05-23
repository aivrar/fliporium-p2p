package rtc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// TestBacklogDeliveredOnJoin proves the offline backlog: blobs stored by one
// member are handed to a member who joins later.
func TestBacklogDeliveredOnJoin(t *testing.T) {
	srv := httptest.NewServer(NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a, err := JoinRoom(ctx, wsURL, "r", "a", nil)
	if err != nil {
		t.Fatalf("a join: %v", err)
	}
	go func() { _ = a.Run(ctx) }()
	if err := a.Store(ctx, "blob-1"); err != nil {
		t.Fatalf("store 1: %v", err)
	}
	if err := a.Store(ctx, "blob-2"); err != nil {
		t.Fatalf("store 2: %v", err)
	}
	// Let the server process the stores before b joins.
	time.Sleep(300 * time.Millisecond)

	got := make(chan []string, 1)
	b, err := JoinRoom(ctx, wsURL, "r", "b", nil)
	if err != nil {
		t.Fatalf("b join: %v", err)
	}
	b.OnBacklog = func(blobs []string) { got <- blobs }
	go func() { _ = b.Run(ctx) }()

	select {
	case blobs := <-got:
		if len(blobs) != 2 || blobs[0] != "blob-1" || blobs[1] != "blob-2" {
			t.Fatalf("backlog = %v, want [blob-1 blob-2]", blobs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never received backlog")
	}
}

func TestCrossOriginWebSocketRejected(t *testing.T) {
	srv := httptest.NewServer(NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "unexpected success")
		t.Fatal("cross-origin websocket dial succeeded")
	}
}

func TestDuplicatePeerIDRejected(t *testing.T) {
	srv := httptest.NewServer(NewServer().Handler())
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	first, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close(websocket.StatusNormalClosure, "done")
	if err := wsjson.Write(ctx, first, Sig{Type: SigJoin, Room: "r", From: "a"}); err != nil {
		t.Fatal(err)
	}
	var peers Sig
	if err := wsjson.Read(ctx, first, &peers); err != nil {
		t.Fatal(err)
	}
	if peers.Type != SigPeers {
		t.Fatalf("first join got %q, want %q", peers.Type, SigPeers)
	}

	dup, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dup.Close(websocket.StatusNormalClosure, "done")
	if err := wsjson.Write(ctx, dup, Sig{Type: SigJoin, Room: "r", From: "a"}); err != nil {
		t.Fatal(err)
	}
	var got Sig
	if err := wsjson.Read(ctx, dup, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != SigError {
		t.Fatalf("duplicate join got %q, want %q", got.Type, SigError)
	}
}
