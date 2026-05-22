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
	"time"

	"fliporium/internal/rtc"
)

func main() {
	listenAddr := flag.String("listen", ":8090", "WebSocket listen address")
	flag.Parse()

	s := rtc.NewServer()
	s.Verbose = true
	log.Printf("flipsignal: listening on %s (ws path /ws)", *listenAddr)
	srv := &http.Server{Addr: *listenAddr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
