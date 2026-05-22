package rtc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/pion/webrtc/v4"
)

// DefaultSTUN is Google's public STUN server — fine for the proof. Production
// adds a TURN relay (minted by the signaling server) for hard NATs.
var DefaultSTUN = []string{"stun:stun.l.google.com:19302"}

// STUNServers builds an ICE server list from STUN URLs (no credentials).
func STUNServers(urls []string) []webrtc.ICEServer {
	if len(urls) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{URLs: urls}}
}

// Connect establishes a single WebRTC peer connection to `remote` and returns
// the opened DataChannel as an io.ReadWriteCloser (detached), so callers can
// run the existing peer.WriteFrame / peer.ReadFrame protocol over it.
//
//   - `send` writes a signaling message (the caller owns the SignalClient).
//   - `in` delivers signaling messages already filtered to those FROM `remote`.
//   - `offerer` decides who initiates (exactly one side must be the offerer).
//
// It blocks until the DataChannel opens or ctx is cancelled.
func Connect(ctx context.Context, send func(Sig) error, in <-chan Sig, self, remote string, offerer bool, ice []webrtc.ICEServer) (io.ReadWriteCloser, error) {
	se := webrtc.SettingEngine{}
	se.DetachDataChannels() // makes dc.Detach() return an io.ReadWriteCloser
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	// No ICE servers when none are configured (e.g. same-machine tests use host
	// candidates only).
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	// Instrument connectivity so we can see direct-vs-relay behaviour and
	// diagnose NAT-traversal failures (the top risk for a consumer install).
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("rtc: ice state %s <-> %s: %s", self, remote, s)
	})

	// Trickle local ICE candidates to the remote via signaling.
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // end-of-candidates
		}
		raw, err := json.Marshal(c.ToJSON())
		if err != nil {
			return
		}
		_ = send(Sig{Type: SigICE, From: self, To: remote, Cand: raw})
	})

	opened := make(chan io.ReadWriteCloser, 1)
	failed := make(chan error, 1)

	wireDC := func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			log.Printf("rtc: datachannel open %s <-> %s; detaching", self, remote)
			raw, err := dc.Detach()
			if err != nil {
				log.Printf("rtc: detach %s <-> %s failed: %v", self, remote, err)
				failed <- fmt.Errorf("detach data channel: %w", err)
				return
			}
			opened <- raw
		})
	}

	if offerer {
		dc, err := pc.CreateDataChannel("data", nil) // ordered + reliable
		if err != nil {
			pc.Close()
			return nil, fmt.Errorf("create data channel: %w", err)
		}
		wireDC(dc)
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			pc.Close()
			return nil, fmt.Errorf("create offer: %w", err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			pc.Close()
			return nil, fmt.Errorf("set local offer: %w", err)
		}
		if err := send(Sig{Type: SigOffer, From: self, To: remote, SDP: offer.SDP}); err != nil {
			pc.Close()
			return nil, fmt.Errorf("send offer: %w", err)
		}
	} else {
		pc.OnDataChannel(wireDC)
	}

	// Pump signaling for this remote until the channel opens. ICE candidates
	// that arrive before the remote description is set must be queued, since
	// AddICECandidate errors otherwise.
	go func() {
		var (
			mu        sync.Mutex
			remoteSet bool
			pending   []webrtc.ICECandidateInit
		)
		addICE := func(init webrtc.ICECandidateInit) {
			mu.Lock()
			defer mu.Unlock()
			if !remoteSet {
				pending = append(pending, init)
				return
			}
			_ = pc.AddICECandidate(init)
		}
		markRemoteSet := func() {
			mu.Lock()
			defer mu.Unlock()
			remoteSet = true
			for _, init := range pending {
				_ = pc.AddICECandidate(init)
			}
			pending = nil
		}

		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-in:
				if !ok {
					return
				}
				switch m.Type {
				case SigOffer: // answerer path
					if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
						failed <- fmt.Errorf("set remote offer: %w", err)
						return
					}
					markRemoteSet()
					answer, err := pc.CreateAnswer(nil)
					if err != nil {
						failed <- fmt.Errorf("create answer: %w", err)
						return
					}
					if err := pc.SetLocalDescription(answer); err != nil {
						failed <- fmt.Errorf("set local answer: %w", err)
						return
					}
					_ = send(Sig{Type: SigAnswer, From: self, To: remote, SDP: answer.SDP})
				case SigAnswer: // offerer path
					if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP}); err != nil {
						failed <- fmt.Errorf("set remote answer: %w", err)
						return
					}
					markRemoteSet()
				case SigICE:
					var init webrtc.ICECandidateInit
					if err := json.Unmarshal(m.Cand, &init); err == nil {
						addICE(init)
					}
				}
			}
		}
	}()

	select {
	case rwc := <-opened:
		return rwc, nil
	case err := <-failed:
		pc.Close()
		return nil, err
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	}
}
