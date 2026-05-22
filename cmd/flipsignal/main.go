// flipsignal is the Phase-0 signaling server for Fliporium's WebRTC transport.
//
// Its only job is matchmaking: clients connect over a WebSocket, join a room,
// and the server relays the WebRTC handshake (SDP offers/answers + ICE
// candidates) between the members so they can open a direct peer-to-peer
// DataChannel. Once that channel is up, chat/file/everything flows directly
// between peers — this server never sees any of it. It only ever moves the
// signaling messages defined in internal/rtc.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fliporium/internal/rtc"
)

var listenAddr = flag.String("listen", ":8090", "WebSocket listen address")

type client struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *client) send(m rtc.Sig) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = wsjson.Write(ctx, c.conn, m)
}

type hub struct {
	mu    sync.Mutex
	rooms map[string]map[string]*client // roomID -> peerID -> client
}

func newHub() *hub { return &hub{rooms: map[string]map[string]*client{}} }

// join adds c to room and returns the ids of peers already present.
func (h *hub) join(room string, c *client) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.rooms[room]
	if m == nil {
		m = map[string]*client{}
		h.rooms[room] = m
	}
	others := make([]string, 0, len(m))
	for id := range m {
		others = append(others, id)
	}
	m[c.id] = c
	return others
}

func (h *hub) leave(room string, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.rooms[room]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(h.rooms, room)
		}
	}
}

// broadcast sends m to every member of room except `except`.
func (h *hub) broadcast(room, except string, m rtc.Sig) {
	h.mu.Lock()
	targets := make([]*client, 0)
	for id, c := range h.rooms[room] {
		if id != except {
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()
	for _, c := range targets {
		c.send(m)
	}
}

// relayTo forwards m to a single member `to` of room.
func (h *hub) relayTo(room, to string, m rtc.Sig) {
	h.mu.Lock()
	c := h.rooms[room][to]
	h.mu.Unlock()
	if c != nil {
		c.send(m)
	}
}

func (h *hub) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	// First message must be a join.
	var first rtc.Sig
	if err := wsjson.Read(r.Context(), conn, &first); err != nil {
		return
	}
	if first.Type != rtc.SigJoin || first.Room == "" || first.From == "" {
		_ = wsjson.Write(r.Context(), conn, rtc.Sig{Type: rtc.SigError, Msg: "first message must be join with room+from"})
		return
	}

	c := &client{id: first.From, conn: conn}
	room := first.Room
	others := h.join(room, c)
	log.Printf("join: peer=%s room=%s (now %d members)", c.id, room, len(others)+1)

	// Tell the newcomer who's already here, and tell the others someone arrived.
	c.send(rtc.Sig{Type: rtc.SigPeers, Room: room, Peers: others})
	h.broadcast(room, c.id, rtc.Sig{Type: rtc.SigPeerJoined, Room: room, From: c.id})

	defer func() {
		h.leave(room, c.id)
		h.broadcast(room, c.id, rtc.Sig{Type: rtc.SigPeerLeft, Room: room, From: c.id})
		log.Printf("leave: peer=%s room=%s", c.id, room)
	}()

	for {
		var m rtc.Sig
		if err := wsjson.Read(context.Background(), conn, &m); err != nil {
			return
		}
		m.From = c.id // trust the connection's id, not the client's claim
		switch m.Type {
		case rtc.SigOffer, rtc.SigAnswer, rtc.SigICE:
			// Pure relay. The server does not inspect or store SDP/ICE beyond
			// moving it to the named target.
			log.Printf("relay: %s %s->%s room=%s", m.Type, c.id, m.To, room)
			h.relayTo(room, m.To, m)
		default:
			log.Printf("ignoring unexpected message type %q from %s", m.Type, c.id)
		}
	}
}

func main() {
	flag.Parse()
	h := newHub()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.handle)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	log.Printf("flipsignal: listening on %s (ws path /ws)", *listenAddr)
	srv := &http.Server{Addr: *listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
