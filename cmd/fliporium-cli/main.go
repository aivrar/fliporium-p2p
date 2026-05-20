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
//	FLIPORIUM_AUTOPEER  — comma-separated MagicDNS names to auto-connect on startup.
//	FLIPORIUM_AUTOSAY   — optional text; sent to each auto-peered connection
//	                      after the HELLO completes. Handy for scripted tests.
//	FLIPORIUM_AUTOFLIP  — optional file path; flipped to each auto-peered peer
//	                      after the HELLO completes. Handy for scripted tests.
//	FLIPORIUM_AUTOBOOTH — "name|p1,p2,..." — after auto-peering, create a booth
//	                      and send invites. Scripted-test convenience.
//	FLIPORIUM_AUTOBOOTHSAY
//	                    — text broadcast into the auto-created booth.
//	FLIPORIUM_HEADLESS  — when set, skip the interactive REPL and just run as
//	                      a listening peer until SIGINT. Used by scripted tests.
//	FLIPORIUM_AUTOQUIT_SECONDS
//	                    — when set, trigger a clean shutdown after N seconds.
//	                      Used by scripted tests to demonstrate BYE end-to-end.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fliporium/internal/peer"
	"fliporium/internal/store"

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

	db, err := store.Open(dir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer db.Close()

	hub := peer.NewHub()
	hub.CatchRoot = filepath.Join(dir, "catch")

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
		displayEvents(ctx, db, hostname, hub.Events)
	}()

	if autoPeer != "" {
		autoSay := os.Getenv("FLIPORIUM_AUTOSAY")
		autoFlip := os.Getenv("FLIPORIUM_AUTOFLIP")
		autoBooth := os.Getenv("FLIPORIUM_AUTOBOOTH")
		autoBoothSay := os.Getenv("FLIPORIUM_AUTOBOOTHSAY")
		go func() {
			for _, target := range strings.Split(autoPeer, ",") {
				target = strings.TrimSpace(target)
				if target == "" {
					continue
				}
				dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
				if err := hub.Dial(dialCtx, srv.Dial, tlsCfg, target, hostname); err != nil {
					fmt.Fprintf(os.Stderr, "autopeer %s: %v\n", target, err)
				}
				dialCancel()
			}
			if autoSay != "" {
				time.Sleep(500 * time.Millisecond)
				for _, name := range hub.Names() {
					_ = hub.Send(name, autoSay)
				}
			}
			if autoFlip != "" {
				time.Sleep(500 * time.Millisecond)
				for _, name := range hub.Names() {
					if _, err := hub.SendFlip(name, autoFlip); err != nil {
						fmt.Fprintf(os.Stderr, "autoflip %s: %v\n", autoFlip, err)
					}
				}
			}
			if autoBooth != "" {
				parts := strings.SplitN(autoBooth, "|", 2)
				name := parts[0]
				memberList := []string{}
				if len(parts) == 2 {
					for _, m := range strings.Split(parts[1], ",") {
						if m = strings.TrimSpace(m); m != "" {
							memberList = append(memberList, m)
						}
					}
				}
				seen := map[string]bool{hostname: true}
				cleaned := []string{hostname}
				for _, m := range memberList {
					if !seen[m] {
						seen[m] = true
						cleaned = append(cleaned, m)
					}
				}
				id, err := cliNewBoothID()
				if err != nil {
					fmt.Fprintf(os.Stderr, "autobooth id: %v\n", err)
					return
				}
				now := time.Now().UTC()
				_ = db.UpsertBooth(ctx, store.Booth{ID: id, Name: name, Founder: hostname, FoundedAt: now})
				for _, m := range cleaned {
					_ = db.AddBoothMember(ctx, id, m, now)
				}
				inv := peer.BoothInvite{ID: id, Name: name, Founder: hostname, Members: cleaned, FoundedAt: now}
				for _, m := range cleaned {
					if m == hostname {
						continue
					}
					if hub.Get(m) != nil {
						_ = hub.SendBoothInvite(m, inv)
					}
				}
				fmt.Fprintf(os.Stderr, "autobooth: created %q (id=%s, %d members)\n", name, id[:8], len(cleaned))
				if autoBoothSay != "" {
					time.Sleep(500 * time.Millisecond)
					for _, m := range cleaned {
						if m == hostname {
							continue
						}
						if hub.Get(m) != nil {
							_ = hub.SendBooth(m, id, autoBoothSay)
						}
					}
					_ = db.AppendMessageBooth(ctx, hostname, store.DirectionOut, autoBoothSay, id, time.Now().UTC())
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
			runREPL(ctx, srv, hub, tlsCfg, hostname, db)
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

func displayEvents(ctx context.Context, st *store.Store, selfName string, events <-chan peer.HubEvent) {
	for ev := range events {
		ts := ev.At.Local().Format("15:04:05")
		switch ev.Kind {
		case peer.EventMessage:
			boothID := ""
			if md, ok := ev.Data.(*peer.MessageEventData); ok && md != nil {
				boothID = md.BoothID
			}
			_ = st.AppendMessageBooth(ctx, ev.Peer, store.DirectionIn, ev.Text, boothID, ev.At)
			if boothID != "" {
				booth, _ := st.GetBooth(ctx, boothID)
				name := booth.Name
				if name == "" {
					name = boothID[:8]
				}
				fmt.Printf("\r\033[K[%s] [%s] %s: %s\n> ", ts, name, ev.Peer, ev.Text)
			} else {
				fmt.Printf("\r\033[K[%s] %s: %s\n> ", ts, ev.Peer, ev.Text)
			}
		case peer.EventConnect:
			fmt.Printf("\r\033[K[%s] *** %s connected (%s)\n> ", ts, ev.Peer, ev.Text)
		case peer.EventDisconnect:
			fmt.Printf("\r\033[K[%s] *** %s disconnected\n> ", ts, ev.Peer)
		case peer.EventInfo:
			fmt.Printf("\r\033[K[%s] info: %s\n> ", ts, ev.Text)
		case peer.EventFlipStarted:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd != nil {
				fmt.Printf("\r\033[K[%s] flip %s %s %s (%d bytes)\n> ", ts, fd.Direction, fd.Filename, fd.ID[:8], fd.Size)
				_ = st.AppendFlip(ctx, store.FlipRecord{
					ID: fd.ID, Peer: ev.Peer, Direction: fd.Direction, Filename: fd.Filename,
					Size: fd.Size, Mime: fd.Mime, Path: fd.Path, Status: store.FlipStatusStarted, StartedAt: ev.At,
				})
			}
		case peer.EventFlipCompleted:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd != nil {
				fmt.Printf("\r\033[K[%s] flip done %s %s -> %s\n> ", ts, fd.Direction, fd.Filename, fd.Path)
				_ = st.UpdateFlipStatus(ctx, fd.ID, store.FlipStatusComplete, fd.Sha256, ev.At)
			}
		case peer.EventFlipFailed:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd != nil {
				fmt.Printf("\r\033[K[%s] flip FAILED %s %s: %s\n> ", ts, fd.Direction, fd.Filename, fd.Reason)
				_ = st.UpdateFlipStatus(ctx, fd.ID, store.FlipStatusFailed, "", ev.At)
			}
		case peer.EventBoothInvited:
			inv, _ := ev.Data.(*peer.BoothInvite)
			if inv == nil {
				continue
			}
			_ = st.UpsertBooth(ctx, store.Booth{
				ID: inv.ID, Name: inv.Name, Founder: inv.Founder, FoundedAt: inv.FoundedAt, Motto: inv.Motto,
			})
			for _, m := range inv.Members {
				_ = st.AddBoothMember(ctx, inv.ID, m, inv.FoundedAt)
			}
			fmt.Printf("\r\033[K[%s] *** invited to booth %q by %s (members: %s)\n> ", ts, inv.Name, ev.Peer, strings.Join(inv.Members, ", "))
		}
	}
}

func runREPL(ctx context.Context, srv *tsnet.Server, hub *peer.Hub, tlsCfg *tls.Config, selfName string, st *store.Store) {
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
		case "flip":
			handleFlip(hub, rest)
		case "booth":
			handleBooth(ctx, hub, st, selfName, rest)
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
	fmt.Println("  flip <path>                 send a file to the only open peer")
	fmt.Println("  flip @<peer> <path>         send a file to a specific peer")
	fmt.Println("  booth list                  list known booths")
	fmt.Println("  booth members <id|name>     show a booth's members")
	fmt.Println("  booth create <name> <p1,p2> create a booth with the listed members")
	fmt.Println("  booth send <id|name> <txt>  send a message to a booth")
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

func handleFlip(hub *peer.Hub, rest string) {
	names := hub.Names()
	if len(names) == 0 {
		fmt.Println("not connected to any peer; use 'connect <hostname>' first")
		return
	}
	var target, path string
	if strings.HasPrefix(rest, "@") {
		parts := strings.SplitN(rest[1:], " ", 2)
		if len(parts) < 2 {
			fmt.Println("usage: flip @<peer> <path>")
			return
		}
		target = parts[0]
		path = parts[1]
	} else {
		if len(names) > 1 {
			fmt.Println("multiple peers connected; use 'flip @<peer> <path>'")
			return
		}
		target = names[0]
		path = rest
	}
	if path == "" {
		fmt.Println("usage: flip [@<peer>] <path>")
		return
	}
	id, err := hub.SendFlip(target, path)
	if err != nil {
		fmt.Println("flip:", err)
		return
	}
	fmt.Printf("started flip %s -> %s (id=%s)\n", path, target, id[:8])
}

func handleBooth(ctx context.Context, hub *peer.Hub, st *store.Store, selfName, rest string) {
	sub, args := splitCmd(rest)
	switch sub {
	case "list":
		booths, err := st.ListBooths(ctx)
		if err != nil {
			fmt.Println("booth list:", err)
			return
		}
		if len(booths) == 0 {
			fmt.Println("(no booths)")
			return
		}
		for _, b := range booths {
			members, _ := st.BoothMembers(ctx, b.ID)
			names := make([]string, 0, len(members))
			for _, m := range members {
				names = append(names, m.PeerName)
			}
			fmt.Printf("  %s  %q  founder=%s  members=[%s]\n", b.ID[:8], b.Name, b.Founder, strings.Join(names, ", "))
		}
	case "members":
		b, ok := findBooth(ctx, st, args)
		if !ok {
			return
		}
		members, _ := st.BoothMembers(ctx, b.ID)
		for _, m := range members {
			fmt.Printf("  %s  joined=%s\n", m.PeerName, m.JoinedAt.Local().Format("15:04:05"))
		}
	case "create":
		parts := strings.SplitN(args, " ", 2)
		if len(parts) < 2 {
			fmt.Println("usage: booth create <name> <peer1,peer2,...>")
			return
		}
		name := parts[0]
		memberList := strings.Split(parts[1], ",")
		seen := map[string]bool{selfName: true}
		cleaned := []string{selfName}
		for _, m := range memberList {
			m = strings.TrimSpace(m)
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			cleaned = append(cleaned, m)
		}
		id, err := cliNewBoothID()
		if err != nil {
			fmt.Println("create:", err)
			return
		}
		now := time.Now().UTC()
		if err := st.UpsertBooth(ctx, store.Booth{ID: id, Name: name, Founder: selfName, FoundedAt: now}); err != nil {
			fmt.Println("create:", err)
			return
		}
		for _, m := range cleaned {
			_ = st.AddBoothMember(ctx, id, m, now)
		}
		invite := peer.BoothInvite{ID: id, Name: name, Founder: selfName, Members: cleaned, FoundedAt: now}
		sent := 0
		for _, m := range cleaned {
			if m == selfName {
				continue
			}
			if hub.Get(m) != nil {
				if err := hub.SendBoothInvite(m, invite); err != nil {
					fmt.Printf("  invite %s: %v\n", m, err)
				} else {
					sent++
				}
			} else {
				fmt.Printf("  invite %s: not connected; they'll miss the invite for now\n", m)
			}
		}
		fmt.Printf("created booth %s (id %s); invites sent to %d/%d members\n", name, id[:8], sent, len(cleaned)-1)
	case "send":
		parts := strings.SplitN(args, " ", 2)
		if len(parts) < 2 {
			fmt.Println("usage: booth send <id|name> <text>")
			return
		}
		b, ok := findBooth(ctx, st, parts[0])
		if !ok {
			return
		}
		text := parts[1]
		members, _ := st.BoothMembers(ctx, b.ID)
		now := time.Now().UTC()
		delivered := 0
		for _, m := range members {
			if m.PeerName == selfName {
				continue
			}
			if hub.Get(m.PeerName) == nil {
				continue
			}
			if err := hub.SendBooth(m.PeerName, b.ID, text); err != nil {
				fmt.Printf("  %s: %v\n", m.PeerName, err)
			} else {
				delivered++
			}
		}
		_ = st.AppendMessageBooth(ctx, selfName, store.DirectionOut, text, b.ID, now)
		fmt.Printf("sent to %d/%d members of %q\n", delivered, len(members)-1, b.Name)
	default:
		fmt.Println("usage: booth list | members <id|name> | create <name> <p1,p2,...> | send <id|name> <text>")
	}
}

// findBooth resolves a booth by id prefix or case-insensitive name match.
func findBooth(ctx context.Context, st *store.Store, ref string) (store.Booth, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		fmt.Println("usage: <id|name> required")
		return store.Booth{}, false
	}
	booths, _ := st.ListBooths(ctx)
	refLower := strings.ToLower(ref)
	for _, b := range booths {
		if strings.HasPrefix(b.ID, ref) || strings.EqualFold(b.Name, ref) || strings.ToLower(b.Name) == refLower {
			return b, true
		}
	}
	fmt.Printf("no booth matching %q\n", ref)
	return store.Booth{}, false
}

func cliNewBoothID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
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
