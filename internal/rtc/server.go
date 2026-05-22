package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	// maxRoomSize caps a room's mesh. Rooms are friends-sized; a full WebRTC
	// mesh past this gets expensive (N^2 connections), and it bounds abuse.
	maxRoomSize = 16
	// maxTotalConns bounds total concurrent signaling connections so a flood
	// can't exhaust the process.
	maxTotalConns = 5000
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
	total int                                 // concurrent connections across all rooms
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

func (s *Server) join(room string, c *serverClient) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxTotalConns {
		return nil, fmt.Errorf("server is at capacity, try again later")
	}
	m := s.rooms[room]
	if m == nil {
		m = map[string]*serverClient{}
		s.rooms[room] = m
	}
	if len(m) >= maxRoomSize {
		if len(m) == 0 {
			delete(s.rooms, room) // we created it above; don't leak an empty room
		}
		return nil, fmt.Errorf("room is full (max %d people)", maxRoomSize)
	}
	others := make([]string, 0, len(m))
	for id := range m {
		others = append(others, id)
	}
	m[c.id] = c
	s.total++
	return others, nil
}

func (s *Server) leave(room, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.rooms[room]; m != nil {
		if _, ok := m[id]; ok {
			delete(m, id)
			s.total--
		}
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

// Stats is a snapshot of live signaling activity (aggregate only — no
// per-user data).
type Stats struct {
	Rooms int `json:"rooms"`
	Peers int `json:"peers"`
}

// Stats counts active rooms and connected peers right now.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	peers := 0
	for _, m := range s.rooms {
		peers += len(m)
	}
	return Stats{Rooms: len(s.rooms), Peers: peers}
}

// Handler returns an http.Handler serving /ws (signaling), /stats, /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.Stats())
	})
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
	others, err := s.join(room, c)
	if err != nil {
		_ = wsjson.Write(r.Context(), conn, Sig{Type: SigError, Msg: err.Error()})
		return
	}
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
