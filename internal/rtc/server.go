package rtc

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Server is the signaling/matchmaking server. Clients join rooms over a
// WebSocket; the server relays the WebRTC handshake (SDP + ICE) between
// members and announces join/leave. It never sees DataChannel payloads.
//
// The logic lives here (not in package main) so it can be started in-process
// by tests via Handler().
type Server struct {
	mu    sync.Mutex
	rooms map[string]map[string]*serverClient // roomID -> peerID -> client
	// Verbose logs each relayed signal (handy for the proof; off by default).
	Verbose bool
}

func NewServer() *Server {
	return &Server{rooms: map[string]map[string]*serverClient{}}
}

type serverClient struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *serverClient) send(m Sig) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = wsjson.Write(ctx, c.conn, m)
}

func (s *Server) join(room string, c *serverClient) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.rooms[room]
	if m == nil {
		m = map[string]*serverClient{}
		s.rooms[room] = m
	}
	others := make([]string, 0, len(m))
	for id := range m {
		others = append(others, id)
	}
	m[c.id] = c
	return others
}

func (s *Server) leave(room, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.rooms[room]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(s.rooms, room)
		}
	}
}

func (s *Server) broadcast(room, except string, m Sig) {
	s.mu.Lock()
	targets := make([]*serverClient, 0)
	for id, c := range s.rooms[room] {
		if id != except {
			targets = append(targets, c)
		}
	}
	s.mu.Unlock()
	for _, c := range targets {
		c.send(m)
	}
}

func (s *Server) relayTo(room, to string, m Sig) {
	s.mu.Lock()
	c := s.rooms[room][to]
	s.mu.Unlock()
	if c != nil {
		c.send(m)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Verbose {
		log.Printf(format, args...)
	}
}

// Handler returns an http.Handler serving /ws (signaling) and /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 20)
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	var first Sig
	if err := wsjson.Read(r.Context(), conn, &first); err != nil {
		return
	}
	if first.Type != SigJoin || first.Room == "" || first.From == "" {
		_ = wsjson.Write(r.Context(), conn, Sig{Type: SigError, Msg: "first message must be join with room+from"})
		return
	}

	c := &serverClient{id: first.From, conn: conn}
	room := first.Room
	others := s.join(room, c)
	s.logf("join: peer=%s room=%s (now %d members)", c.id, room, len(others)+1)

	c.send(Sig{Type: SigPeers, Room: room, Peers: others})
	s.broadcast(room, c.id, Sig{Type: SigPeerJoined, Room: room, From: c.id})

	defer func() {
		s.leave(room, c.id)
		s.broadcast(room, c.id, Sig{Type: SigPeerLeft, Room: room, From: c.id})
		s.logf("leave: peer=%s room=%s", c.id, room)
	}()

	for {
		var m Sig
		if err := wsjson.Read(context.Background(), conn, &m); err != nil {
			return
		}
		m.From = c.id // trust the connection's id, not the client's claim
		switch m.Type {
		case SigOffer, SigAnswer, SigICE:
			s.logf("relay: %s %s->%s room=%s", m.Type, c.id, m.To, room)
			s.relayTo(room, m.To, m)
		}
	}
}
