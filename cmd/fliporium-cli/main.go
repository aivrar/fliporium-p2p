// fliporium-cli is the terminal-mode peer that proved the protocol in
// Phase 3. It now lives alongside the GUI binary as a small, scriptable
// surface for testing.
//
// Configuration (env vars):
//
//	FLIPORIUM_AUTHKEY   — Headscale pre-auth key (required on first run only;
//	                      identity persists in the data dir afterwards).
//	FLIPORIUM_HOSTNAME  — node hostname (default "fliporium-cli").
//	FLIPORIUM_DIR       — data dir for persisted identity (default "./fliporium-data").
//	FLIPORIUM_AUTOPEER  — optional MagicDNS name to auto-connect on startup.
//	FLIPORIUM_AUTOSAY   — optional text; sent to each auto-peered connection
//	                      after the HELLO completes. Handy for scripted tests.
//	FLIPORIUM_HEADLESS  — when set, skip the interactive REPL and just run as
//	                      a listening peer until SIGINT. Used by scripted tests.
//	FLIPORIUM_AUTOQUIT_SECONDS
//	                    — when set, trigger a clean shutdown after N seconds.
//	                      Used by scripted tests to demonstrate BYE end-to-end.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fliporium/internal/peer"

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
	log.SetOutput(os.Stderr)

	hostname := env("FLIPORIUM_HOSTNAME", "fliporium-cli")
	dir := env("FLIPORIUM_DIR", "./fliporium-data")
	authKey := os.Getenv("FLIPORIUM_AUTHKEY")
	autoPeer := os.Getenv("FLIPORIUM_AUTOPEER")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("create data dir %q: %v", dir, err)
	}

	tsLog := func(format string, args ...any) {}
	if os.Getenv("FLIPORIUM_TSNET_LOG") != "" {
		tsLog = func(format string, args ...any) { log.Printf("tsnet: "+format, args...) }
	}

	srv := &tsnet.Server{
		Hostname:   hostname,
		Dir:        dir,
		ControlURL: controlURL,
		AuthKey:    authKey,
		Logf:       tsLog,
		UserLogf:   tsLog,
	}
	defer srv.Close()

	fmt.Printf("Bringing up %q against %s\n", hostname, controlURL)
	fmt.Printf("Identity dir: %s\n\n", dir)

	bootCtx, cancelBoot := context.WithTimeout(context.Background(), 90*time.Second)
	st, err := srv.Up(bootCtx)
	cancelBoot()
	if err != nil {
		log.Fatalf("tsnet up: %v", err)
	}

	if st.Self != nil {
		fmt.Printf("Self: %s  %v\n", st.Self.HostName, st.Self.TailscaleIPs)
		fmt.Printf("DNS : %s\n", st.Self.DNSName)
	}

	tlsCfg, err := peer.NewTLSConfig(hostname)
	if err != nil {
		log.Fatalf("tls config: %v", err)
	}

	hub := peer.NewHub()

	listenAddr := fmt.Sprintf(":%d", peer.Port)
	ln, err := srv.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", listenAddr, err)
	}
	fmt.Printf("Listening on tailnet %s\n\n", listenAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				acceptCtx, acceptCancel := context.WithTimeout(ctx, 15*time.Second)
				defer acceptCancel()
				hub.Accept(acceptCtx, raw, tlsCfg, hostname)
			}()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		displayEvents(hub.Events)
	}()

	if autoPeer != "" {
		autoSay := os.Getenv("FLIPORIUM_AUTOSAY")
		go func() {
			dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
			defer dialCancel()
			if err := hub.Dial(dialCtx, srv.Dial, tlsCfg, autoPeer, hostname); err != nil {
				fmt.Fprintf(os.Stderr, "autopeer %s: %v\n", autoPeer, err)
				return
			}
			if autoSay != "" {
				time.Sleep(500 * time.Millisecond)
				for _, name := range hub.Names() {
					_ = hub.Send(name, autoSay)
				}
			}
		}()
	}

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	if v := os.Getenv("FLIPORIUM_AUTOQUIT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			go func() {
				time.Sleep(time.Duration(n) * time.Second)
				sigC <- syscall.SIGTERM
			}()
		}
	}

	headless := os.Getenv("FLIPORIUM_HEADLESS") != ""
	replDone := make(chan struct{})
	if headless {
		fmt.Println("(headless — REPL disabled; waiting for signal)")
	} else {
		go func() {
			runREPL(ctx, srv, hub, tlsCfg, hostname)
			close(replDone)
		}()
	}

	select {
	case <-sigC:
		fmt.Println("\n(received signal — saying BYE)")
	case <-replDone:
	}

	hub.ByeAll("shutting down")
	ln.Close()
	cancel()
	wg.Wait()
	close(hub.Events)
}

func displayEvents(events <-chan peer.HubEvent) {
	for ev := range events {
		ts := ev.At.Local().Format("15:04:05")
		switch ev.Kind {
		case peer.EventMessage:
			fmt.Printf("\r\033[K[%s] %s: %s\n> ", ts, ev.Peer, ev.Text)
		case peer.EventConnect:
			fmt.Printf("\r\033[K[%s] *** %s connected (%s)\n> ", ts, ev.Peer, ev.Text)
		case peer.EventDisconnect:
			fmt.Printf("\r\033[K[%s] *** %s disconnected\n> ", ts, ev.Peer)
		case peer.EventInfo:
			fmt.Printf("\r\033[K[%s] info: %s\n> ", ts, ev.Text)
		}
	}
}

func runREPL(ctx context.Context, srv *tsnet.Server, hub *peer.Hub, tlsCfg *tls.Config, selfName string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	fmt.Print("> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}
		cmd, rest := splitCmd(line)
		switch cmd {
		case "help", "?":
			printHelp()
		case "peers":
			showPeers(ctx, srv)
		case "connections", "conns":
			names := hub.Names()
			if len(names) == 0 {
				fmt.Println("(no active connections)")
			} else {
				for _, n := range names {
					c := hub.Get(n)
					fmt.Printf("  %s  %s  %s\n", n, c.Addr, c.Version)
				}
			}
		case "connect":
			if rest == "" {
				fmt.Println("usage: connect <hostname>")
				break
			}
			cCtx, cCancel := context.WithTimeout(ctx, 15*time.Second)
			err := hub.Dial(cCtx, srv.Dial, tlsCfg, rest, selfName)
			cCancel()
			if err != nil {
				fmt.Println("connect:", err)
			}
		case "say", "msg":
			handleSay(hub, rest)
		case "disconnect":
			handleDisconnect(hub, rest)
		case "quit", "exit":
			return
		default:
			fmt.Printf("unknown command %q — try 'help'\n", cmd)
		}
		fmt.Print("> ")
	}
}

func splitCmd(line string) (string, string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func printHelp() {
	fmt.Println("commands:")
	fmt.Println("  peers                       list tailnet peers visible to Headscale")
	fmt.Println("  connections                 list currently-open chat connections")
	fmt.Println("  connect <hostname>          dial peer (e.g. fliporium-node2)")
	fmt.Println("  say <text>                  send to the only open peer (or use @peer)")
	fmt.Println("  say @<peer> <text>          send to a specific peer")
	fmt.Println("  disconnect [<peer>]         close one or the only open peer")
	fmt.Println("  quit                        BYE all peers and exit")
}

func showPeers(ctx context.Context, srv *tsnet.Server) {
	lc, err := srv.LocalClient()
	if err != nil {
		fmt.Println("localclient:", err)
		return
	}
	st, err := lc.Status(ctx)
	if err != nil {
		fmt.Println("status:", err)
		return
	}
	if len(st.Peer) == 0 {
		fmt.Println("(no peers visible)")
		return
	}
	for _, p := range st.Peer {
		state := "offline"
		if p.Online {
			state = "online"
		}
		fmt.Printf("  %-32s %-8s %v\n", p.HostName, state, p.TailscaleIPs)
	}
}

func handleSay(hub *peer.Hub, rest string) {
	names := hub.Names()
	if len(names) == 0 {
		fmt.Println("not connected to any peer; use 'connect <hostname>' first")
		return
	}
	var target, text string
	if strings.HasPrefix(rest, "@") {
		parts := strings.SplitN(rest[1:], " ", 2)
		if len(parts) < 2 {
			fmt.Println("usage: say @<peer> <text>")
			return
		}
		target = parts[0]
		text = parts[1]
	} else {
		if len(names) > 1 {
			fmt.Println("multiple peers connected; use 'say @<peer> <text>'")
			return
		}
		target = names[0]
		text = rest
	}
	if err := hub.Send(target, text); err != nil {
		fmt.Println("send:", err)
	}
}

func handleDisconnect(hub *peer.Hub, rest string) {
	target := rest
	if target == "" {
		names := hub.Names()
		if len(names) != 1 {
			fmt.Println("usage: disconnect <peer>")
			return
		}
		target = names[0]
	}
	c := hub.Get(target)
	if c == nil {
		fmt.Printf("not connected to %s\n", target)
		return
	}
	_ = c.WriteFrame(peer.TypeBye, peer.Bye{Reason: "user disconnected"})
	c.Close()
}
