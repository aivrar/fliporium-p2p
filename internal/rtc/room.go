package rtc

import (
	"context"
	"io"
	"sync"
)

// Room joins a signaling room and maintains a full WebRTC mesh: it opens a
// DataChannel to every other member and reports each one via OnPeer. It stays
// transport-only — it hands the caller an io.ReadWriteCloser per peer and
// never imports the higher-level peer/protocol package (no import cycle).
//
// Connection roles avoid a double-offer: a member already present when we
// arrive — learned via SigPeers — gets offered TO by us; a member who arrives
// AFTER us — learned via SigPeerJoined — offers to us, and we answer.
type Room struct {
	sig  *SignalClient
	self string
	room string
	stun []string

	// OnPeer fires (in its own goroutine) when a DataChannel to remoteID opens.
	OnPeer func(remoteID string, rwc io.ReadWriteCloser, initiator bool)
	// OnPeerLeft fires when a remote leaves the room or its connection drops.
	OnPeerLeft func(remoteID string)
	// OnBacklog fires once on join with the room's stored encrypted blobs.
	OnBacklog func(blobs []string)

	mu    sync.Mutex
	peers map[string]chan Sig // remoteID -> per-peer signaling inbox
}

// Store appends an encrypted blob to the room's offline backlog on the server.
func (r *Room) Store(ctx context.Context, blob string) error {
	return r.sig.Send(ctx, Sig{Type: SigStore, Room: r.room, Blob: blob})
}

// JoinRoom dials the signaling server and joins `room` as `self`.
func JoinRoom(ctx context.Context, signalURL, room, self string, stun []string) (*Room, error) {
	sig, err := DialSignal(ctx, signalURL, room, self)
	if err != nil {
		return nil, err
	}
	return &Room{
		sig:   sig,
		self:  self,
		room:  room,
		stun:  stun,
		peers: map[string]chan Sig{},
	}, nil
}

// Self returns this member's id.
func (r *Room) Self() string { return r.self }

// Close tears down the signaling connection.
func (r *Room) Close() error { return r.sig.Close() }

// Run pumps signaling until ctx is cancelled or the connection drops.
func (r *Room) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-r.sig.In:
			if !ok {
				return nil
			}
			switch m.Type {
			case SigPeers:
				for _, id := range m.Peers {
					r.startPeer(ctx, id, true) // we arrived later → we offer
				}
			case SigPeerJoined:
				r.startPeer(ctx, m.From, false) // they arrived later → they offer
			case SigOffer, SigAnswer, SigICE:
				r.route(ctx, m)
			case SigPeerLeft:
				r.dropPeer(m.From)
			case SigBacklog:
				if r.OnBacklog != nil && len(m.Blobs) > 0 {
					r.OnBacklog(m.Blobs)
				}
			}
		}
	}
}

// startPeer begins (or no-ops if already underway) a connection to remote and
// returns the per-peer signaling inbox.
func (r *Room) startPeer(ctx context.Context, remote string, offerer bool) chan Sig {
	r.mu.Lock()
	if ch, exists := r.peers[remote]; exists {
		r.mu.Unlock()
		return ch
	}
	ch := make(chan Sig, 32)
	r.peers[remote] = ch
	r.mu.Unlock()

	go func() {
		send := func(s Sig) error { return r.sig.Send(ctx, s) }
		rwc, err := Connect(ctx, send, ch, r.self, remote, offerer, r.stun)
		if err != nil {
			r.dropPeer(remote)
			return
		}
		if r.OnPeer != nil {
			r.OnPeer(remote, rwc, offerer)
		}
	}()
	return ch
}

func (r *Room) route(ctx context.Context, m Sig) {
	r.mu.Lock()
	ch, ok := r.peers[m.From]
	r.mu.Unlock()
	if !ok {
		// An offer can race ahead of the peer-joined notice; start as answerer.
		if m.Type != SigOffer {
			return
		}
		ch = r.startPeer(ctx, m.From, false)
	}
	select {
	case ch <- m:
	case <-ctx.Done():
	}
}

func (r *Room) dropPeer(remote string) {
	r.mu.Lock()
	ch, ok := r.peers[remote]
	if ok {
		delete(r.peers, remote)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	close(ch)
	if r.OnPeerLeft != nil {
		r.OnPeerLeft(remote)
	}
}
