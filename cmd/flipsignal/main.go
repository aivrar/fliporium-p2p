// flipsignal is the Fliporium signaling/matchmaking server. Its only job is to
// help peers find each other and exchange the WebRTC handshake (SDP + ICE) so
// they can open a direct peer-to-peer DataChannel. Once that channel is up,
// chat/files/everything flows directly between peers — this server never sees
// any of it. The actual logic lives in internal/rtc so it can also be started
// in-process by tests.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"fliporium/internal/rtc"
)

func main() {
	listenAddr := flag.String("listen", ":8090", "WebSocket listen address")
	flag.Parse()

	s := rtc.NewServer()
	s.Verbose = true

	// TURN: if a coturn shared secret + URLs are configured, mint short-lived
	// relay credentials for each joining peer. The secret stays server-side.
	s.TurnSecret = os.Getenv("FLIPSIGNAL_TURN_SECRET")
	if urls := os.Getenv("FLIPSIGNAL_TURN_URLS"); urls != "" {
		s.TurnURLs = strings.Split(urls, ",")
	}
	if s.TurnSecret != "" && len(s.TurnURLs) > 0 {
		log.Printf("flipsignal: TURN enabled (%v)", s.TurnURLs)
	}

	log.Printf("flipsignal: listening on %s (ws path /ws)", *listenAddr)
	srv := &http.Server{Addr: *listenAddr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
