// Phase 2: minimal tsnet-based binary that joins the Fliporium tailnet,
// prints its identity, lists peers, and stays alive until Ctrl+C.
//
// Configuration (env vars):
//
//	FLIPORIUM_AUTHKEY   — Headscale pre-auth key (required on first run only;
//	                      identity persists in the data dir afterwards).
//	FLIPORIUM_HOSTNAME  — node hostname (default "fliporium-test").
//	FLIPORIUM_DIR       — data dir for persisted identity (default "./fliporium-data").
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tailscale.com/tsnet"
)

const controlURL = "https://headscale.fliporium.com"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	log.SetFlags(log.LstdFlags)

	hostname := env("FLIPORIUM_HOSTNAME", "fliporium-test")
	dir := env("FLIPORIUM_DIR", "./fliporium-data")
	authKey := os.Getenv("FLIPORIUM_AUTHKEY")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("create data dir %q: %v", dir, err)
	}

	s := &tsnet.Server{
		Hostname:   hostname,
		Dir:        dir,
		ControlURL: controlURL,
		AuthKey:    authKey,
		Logf:       func(format string, args ...any) { log.Printf("tsnet: "+format, args...) },
	}
	defer s.Close()

	fmt.Printf("Bringing up tsnet node %q against %s\n", hostname, controlURL)
	fmt.Printf("Identity dir: %s\n\n", dir)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	st, err := s.Up(ctx)
	if err != nil {
		log.Fatalf("tsnet up: %v", err)
	}

	fmt.Println("=== Self ===")
	if st.Self != nil {
		fmt.Printf("  HostName : %s\n", st.Self.HostName)
		fmt.Printf("  DNSName  : %s\n", st.Self.DNSName)
		fmt.Printf("  IPs      : %v\n", st.Self.TailscaleIPs)
	} else {
		fmt.Println("  (self status not yet populated)")
	}

	fmt.Println()
	fmt.Println("=== Peers ===")
	if len(st.Peer) == 0 {
		fmt.Println("  (no peers visible yet — may take a few seconds to populate)")
	} else {
		for _, p := range st.Peer {
			state := "offline"
			if p.Online {
				state = "online"
			}
			fmt.Printf("  %-32s %-12s %v\n", p.HostName, state, p.TailscaleIPs)
		}
	}

	fmt.Println()
	fmt.Println("Node is up. Press Ctrl+C to exit (state persists in", dir+").")

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	<-sigC
	fmt.Println("\nshutting down…")
}
