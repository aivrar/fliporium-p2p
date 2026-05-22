// p2pprobe is the Phase-0 proof: two instances join the same room on the
// signaling server, establish a direct WebRTC DataChannel, and exchange a
// real Fliporium MESSAGE envelope over it using the EXISTING wire protocol
// (peer.WriteFrame / peer.ReadFrame). If the message crosses, the transport
// swap is proven without touching any domain logic.
//
// Usage (two terminals, signaling server already running):
//
//	go run ./cmd/p2pprobe -id alice -room demo
//	go run ./cmd/p2pprobe -id bob   -room demo
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"fliporium/internal/peer"
	"fliporium/internal/rtc"
)

func main() {
	signalURL := flag.String("signal", "ws://127.0.0.1:8090/ws", "signaling server WebSocket URL")
	room := flag.String("room", "demo", "room id to join")
	self := flag.String("id", "", "this peer's id (required, must be unique in the room)")
	flag.Parse()
	if *self == "" {
		log.Fatal("-id is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	sig, err := rtc.DialSignal(ctx, *signalURL, *room, *self)
	if err != nil {
		log.Fatalf("signaling: %v", err)
	}
	defer sig.Close()
	log.Printf("[%s] joined room %q via %s", *self, *room, *signalURL)

	var (
		remote   string
		peerChan = make(chan rtc.Sig, 32)
		started  bool
	)
	send := func(m rtc.Sig) error { return sig.Send(ctx, m) }

	start := func(offerer bool) {
		started = true
		go func() {
			role := "answerer"
			if offerer {
				role = "offerer"
			}
			log.Printf("[%s] connecting to %q as %s", *self, remote, role)
			rwc, err := rtc.Connect(ctx, send, peerChan, *self, remote, offerer, rtc.STUNServers(rtc.DefaultSTUN))
			if err != nil {
				log.Fatalf("[%s] connect: %v", *self, err)
			}
			log.Printf("[%s] DataChannel OPEN — peer-to-peer link established", *self)

			// Send one MESSAGE envelope using the real protocol.
			out := peer.Message{Text: fmt.Sprintf("hello from %s", *self), At: time.Now()}
			if err := peer.WriteFrame(rwc, peer.TypeMessage, out); err != nil {
				log.Fatalf("[%s] write frame: %v", *self, err)
			}
			log.Printf("[%s] sent MESSAGE: %q", *self, out.Text)

			// Read one MESSAGE envelope back.
			env, err := peer.ReadFrame(rwc)
			if err != nil {
				log.Fatalf("[%s] read frame: %v", *self, err)
			}
			var in peer.Message
			if err := json.Unmarshal(env.Body, &in); err != nil {
				log.Fatalf("[%s] unmarshal body: %v", *self, err)
			}
			log.Printf("[%s] RECEIVED %s over P2P: %q (sent at %s)", *self, env.Type, in.Text, in.At.Format(time.RFC3339))
			log.Printf("[%s] PROOF OK — message crossed peer-to-peer", *self)
			cancel()
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-sig.In:
			if !ok {
				return
			}
			switch m.Type {
			case rtc.SigPeers:
				if len(m.Peers) > 0 && !started {
					remote = m.Peers[0]
					start(true) // we arrived second → we offer
				}
			case rtc.SigPeerJoined:
				if !started {
					remote = m.From
					start(false) // someone arrived after us → they offer, we answer
				}
			case rtc.SigOffer, rtc.SigAnswer, rtc.SigICE:
				if m.From == remote {
					peerChan <- m
				}
			case rtc.SigPeerLeft:
				if m.From == remote {
					log.Printf("[%s] remote %q left", *self, remote)
				}
			}
		}
	}
}
