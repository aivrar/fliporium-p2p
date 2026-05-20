package main

import (
	"context"
	cryptoRand "crypto/rand"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fliporium/internal/peer"
	"fliporium/internal/store"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"tailscale.com/tsnet"
)

const controlURL = "https://headscale.fliporium.com"

// AppState describes where the App is in its lifecycle.
type AppState string

const (
	StateInitializing AppState = "initializing"
	StateReady        AppState = "ready"
	StateError        AppState = "error"
)

// SelfInfo is what the UI shows about *us* in the title bar / status.
type SelfInfo struct {
	Hostname string   `json:"hostname"`
	DNSName  string   `json:"dnsName"`
	IPs      []string `json:"ips"`
	Online   bool     `json:"online"`
}

// PeerInfo summarises a peer for the Floor list.
type PeerInfo struct {
	Name          string `json:"name"`
	TailnetName   string `json:"tailnetName"`
	IPs           []string `json:"ips"`
	TailnetOnline bool   `json:"tailnetOnline"`
	Connected     bool   `json:"connected"`
	LastSeen      string `json:"lastSeen,omitempty"`
}

// MessageRecord is one persisted chat line as the frontend sees it.
type MessageRecord struct {
	ID        int64  `json:"id"`
	Peer      string `json:"peer"`
	Direction string `json:"direction"`
	Text      string `json:"text"`
	At        string `json:"at"`
	BoothID   string `json:"boothId,omitempty"`
}

// BoothRecord is a Booth surfaced to the frontend.
type BoothRecord struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Founder   string   `json:"founder"`
	Motto     string   `json:"motto,omitempty"`
	FoundedAt string   `json:"foundedAt"`
	Members   []string `json:"members"`
}

// NotepadRecord is the shared booth notepad surfaced to the frontend.
type NotepadRecord struct {
	BoothID      string `json:"boothId"`
	Text         string `json:"text"`
	Version      int64  `json:"version"`
	LastEditor   string `json:"lastEditor"`
	LastModified string `json:"lastModified,omitempty"`
}

// FlipRecord is what the frontend sees about a file transfer.
type FlipRecord struct {
	ID          string `json:"id"`
	Peer        string `json:"peer"`
	Direction   string `json:"direction"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	Mime        string `json:"mime"`
	Path        string `json:"path"`
	Status      string `json:"status"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt,omitempty"`
	// CatchURL is the in-app URL the frontend uses to render the file (only
	// set when status is complete and direction is in).
	CatchURL string `json:"catchUrl,omitempty"`
}

// AppStatus is the lightweight readiness probe the frontend polls / observes.
type AppStatus struct {
	State    AppState `json:"state"`
	Message  string   `json:"message,omitempty"`
	Self     SelfInfo `json:"self"`
}

// App is the Wails-bound object. Its exported methods become callable from JS.
type App struct {
	ctx context.Context

	mu       sync.RWMutex
	state    AppState
	stateMsg string
	self     SelfInfo

	srv    *tsnet.Server
	hub    *peer.Hub
	store  *store.Store
	ln     net.Listener
	tlsCfg *tls.Config

	hostname string
	dataDir  string
}

func NewApp() *App {
	return &App{state: StateInitializing}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// startup is called by Wails after the window is ready. We initialise
// asynchronously so the window paints immediately rather than blocking
// 10–30s on tsnet coming up.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.hostname = env("FLIPORIUM_HOSTNAME", "fliporium-gui")
	a.dataDir = env("FLIPORIUM_DIR", "./fliporium-data")
	log.Printf("startup: ctx=%v hostname=%s dir=%s", ctx != nil, a.hostname, a.dataDir)

	go a.initBackground()
}

func (a *App) shutdown(ctx context.Context) {
	if a.hub != nil {
		a.hub.ByeAll("window closed")
	}
	if a.ln != nil {
		a.ln.Close()
	}
	if a.srv != nil {
		a.srv.Close()
	}
	if a.store != nil {
		a.store.Close()
	}
}

func (a *App) setState(s AppState, msg string) {
	a.mu.Lock()
	a.state = s
	a.stateMsg = msg
	a.mu.Unlock()
	if a.ctx != nil {
		wailsruntime.EventsEmit(a.ctx, "app-state", a.Status())
	}
}

func (a *App) initBackground() {
	log.Printf("init: opening store at %s", a.dataDir)
	a.setState(StateInitializing, "opening store…")
	st, err := store.Open(a.dataDir)
	if err != nil {
		log.Printf("init: store err: %v", err)
		a.setState(StateError, "store: "+err.Error())
		return
	}
	a.store = st

	log.Printf("init: bringing up tsnet")
	a.setState(StateInitializing, "bringing up tsnet…")
	srv := &tsnet.Server{
		Hostname:   a.hostname,
		Dir:        a.dataDir,
		ControlURL: controlURL,
		AuthKey:    os.Getenv("FLIPORIUM_AUTHKEY"),
		Logf:       func(format string, args ...any) {},
		UserLogf:   func(format string, args ...any) {},
	}
	a.srv = srv

	bootCtx, cancelBoot := context.WithTimeout(a.ctx, 90*time.Second)
	tsStatus, err := srv.Up(bootCtx)
	cancelBoot()
	if err != nil {
		a.setState(StateError, "tsnet up: "+err.Error())
		return
	}

	if tsStatus.Self != nil {
		a.mu.Lock()
		a.self = SelfInfo{
			Hostname: tsStatus.Self.HostName,
			DNSName:  strings.TrimSuffix(tsStatus.Self.DNSName, "."),
			IPs:      ipsAsStrings(tsStatus.Self.TailscaleIPs),
			Online:   true,
		}
		a.mu.Unlock()
	}

	a.setState(StateInitializing, "starting peer listener…")
	tlsCfg, err := peer.NewTLSConfig(a.hostname)
	if err != nil {
		a.setState(StateError, "tls: "+err.Error())
		return
	}
	a.tlsCfg = tlsCfg

	a.hub = peer.NewHub()
	a.hub.CatchRoot = filepath.Join(a.dataDir, "catch")
	listenAddr := fmt.Sprintf(":%d", peer.Port)
	ln, err := srv.Listen("tcp", listenAddr)
	if err != nil {
		a.setState(StateError, "listen: "+err.Error())
		return
	}
	a.ln = ln

	go a.acceptLoop()
	go a.eventPump()

	log.Printf("init: ready (self=%+v)", a.self)
	a.setState(StateReady, "")
}

func (a *App) acceptLoop() {
	for {
		raw, err := a.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			acceptCtx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
			defer cancel()
			a.hub.Accept(acceptCtx, raw, a.tlsCfg, a.hostname)
		}()
	}
}

// eventPump bridges peer.Hub events to (a) the SQLite store and
// (b) wails event emission to the frontend.
func (a *App) eventPump() {
	for ev := range a.hub.Events {
		switch ev.Kind {
		case peer.EventConnect:
			_ = a.store.UpsertPeer(a.ctx, ev.Peer)
			wailsruntime.EventsEmit(a.ctx, "peer-state-changed", map[string]any{
				"peer":      ev.Peer,
				"connected": true,
				"detail":    ev.Text,
				"at":        ev.At.Format(time.RFC3339Nano),
			})
		case peer.EventDisconnect:
			wailsruntime.EventsEmit(a.ctx, "peer-state-changed", map[string]any{
				"peer":      ev.Peer,
				"connected": false,
				"at":        ev.At.Format(time.RFC3339Nano),
			})
		case peer.EventMessage:
			at := ev.At
			boothID := ""
			if md, ok := ev.Data.(*peer.MessageEventData); ok && md != nil {
				boothID = md.BoothID
			}
			_ = a.store.AppendMessageBooth(a.ctx, ev.Peer, store.DirectionIn, ev.Text, boothID, at)
			wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
				Peer:      ev.Peer,
				Direction: store.DirectionIn,
				Text:      ev.Text,
				At:        at.UTC().Format(time.RFC3339Nano),
				BoothID:   boothID,
			})
			// Twin relay: if we have a paired twin and this is a 1:1, send
			// the same row to our twin so both devices share history.
			if boothID == "" {
				a.relayToTwin(peer.TwinSyncMessage{
					OriginalPeer: ev.Peer,
					Direction:    store.DirectionIn,
					Text:         ev.Text,
					At:           at,
				})
			}
		case peer.EventInfo:
			wailsruntime.EventsEmit(a.ctx, "info", map[string]any{
				"peer": ev.Peer,
				"text": ev.Text,
				"at":   ev.At.Format(time.RFC3339Nano),
			})
		case peer.EventFlipStarted:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd == nil {
				continue
			}
			_ = a.store.AppendFlip(a.ctx, store.FlipRecord{
				ID:        fd.ID,
				Peer:      ev.Peer,
				Direction: fd.Direction,
				Filename:  fd.Filename,
				Size:      fd.Size,
				Mime:      fd.Mime,
				Path:      fd.Path,
				Status:    store.FlipStatusStarted,
				StartedAt: ev.At,
			})
			wailsruntime.EventsEmit(a.ctx, "flip", a.flipToRecord(ev.Peer, fd, store.FlipStatusStarted, ev.At, time.Time{}))
		case peer.EventFlipProgress:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd == nil {
				continue
			}
			wailsruntime.EventsEmit(a.ctx, "flip-progress", map[string]any{
				"id":    fd.ID,
				"peer":  ev.Peer,
				"bytes": fd.Bytes,
				"size":  fd.Size,
			})
		case peer.EventFlipCompleted:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd == nil {
				continue
			}
			_ = a.store.UpdateFlipStatus(a.ctx, fd.ID, store.FlipStatusComplete, fd.Sha256, ev.At)
			wailsruntime.EventsEmit(a.ctx, "flip", a.flipToRecord(ev.Peer, fd, store.FlipStatusComplete, time.Time{}, ev.At))
		case peer.EventFlipFailed:
			fd, _ := ev.Data.(*peer.FlipEventData)
			if fd == nil {
				continue
			}
			_ = a.store.UpdateFlipStatus(a.ctx, fd.ID, store.FlipStatusFailed, "", ev.At)
			wailsruntime.EventsEmit(a.ctx, "flip", a.flipToRecord(ev.Peer, fd, store.FlipStatusFailed, time.Time{}, ev.At))
		case peer.EventShowtimeStarted:
			s, _ := ev.Data.(*peer.ShowtimeStart)
			if s == nil {
				continue
			}
			wailsruntime.EventsEmit(a.ctx, "showtime-started", map[string]any{
				"sessionId": s.SessionID,
				"boothId":   s.BoothID,
				"flipId":    s.FlipID,
				"leader":    s.Leader,
				"filename":  s.Filename,
				"mime":      s.Mime,
				"at":        s.At.UTC().Format(time.RFC3339Nano),
			})
		case peer.EventShowtimeState:
			s, _ := ev.Data.(*peer.ShowtimeState)
			if s == nil {
				continue
			}
			wailsruntime.EventsEmit(a.ctx, "showtime-state", map[string]any{
				"sessionId": s.SessionID,
				"boothId":   s.BoothID,
				"playing":   s.Playing,
				"position":  s.Position,
				"at":        s.At.UTC().Format(time.RFC3339Nano),
			})
		case peer.EventShowtimeEnded:
			s, _ := ev.Data.(*peer.ShowtimeEnd)
			if s == nil {
				continue
			}
			wailsruntime.EventsEmit(a.ctx, "showtime-ended", map[string]any{
				"sessionId": s.SessionID,
				"boothId":   s.BoothID,
			})
		case peer.EventNotepadUpdated:
			n, _ := ev.Data.(*peer.NotepadUpdate)
			if n == nil {
				continue
			}
			applied, _ := a.store.UpdateBoothNotepad(a.ctx, store.BoothNotepad{
				BoothID:      n.BoothID,
				Text:         n.Text,
				Version:      n.Version,
				LastEditor:   n.Editor,
				LastModified: n.At,
			})
			if applied {
				wailsruntime.EventsEmit(a.ctx, "notepad", NotepadRecord{
					BoothID:      n.BoothID,
					Text:         n.Text,
					Version:      n.Version,
					LastEditor:   n.Editor,
					LastModified: n.At.UTC().Format(time.RFC3339Nano),
				})
			}
		case peer.EventTwinSyncedMessage:
			ts, _ := ev.Data.(*peer.TwinSyncMessage)
			if ts == nil {
				continue
			}
			twin, _ := a.store.GetSetting(a.ctx, store.SettingTwinHostname)
			if twin == "" || twin != ev.Peer {
				// Either we don't have a twin set, or this came from someone
				// other than our twin. Refuse to graft fake history.
				continue
			}
			_ = a.store.AppendMessageBooth(a.ctx, ts.OriginalPeer, ts.Direction, ts.Text, ts.BoothID, ts.At)
			wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
				Peer:      ts.OriginalPeer,
				Direction: ts.Direction,
				Text:      ts.Text,
				At:        ts.At.UTC().Format(time.RFC3339Nano),
				BoothID:   ts.BoothID,
			})
		case peer.EventBoothInvited:
			inv, _ := ev.Data.(*peer.BoothInvite)
			if inv == nil {
				continue
			}
			_ = a.store.UpsertBooth(a.ctx, store.Booth{
				ID:        inv.ID,
				Name:      inv.Name,
				Founder:   inv.Founder,
				FoundedAt: inv.FoundedAt,
				Motto:     inv.Motto,
			})
			for _, m := range inv.Members {
				_ = a.store.AddBoothMember(a.ctx, inv.ID, m, inv.FoundedAt)
			}
			wailsruntime.EventsEmit(a.ctx, "booth", BoothRecord{
				ID:        inv.ID,
				Name:      inv.Name,
				Founder:   inv.Founder,
				Motto:     inv.Motto,
				FoundedAt: inv.FoundedAt.UTC().Format(time.RFC3339Nano),
				Members:   append([]string{}, inv.Members...),
			})
		}
	}
}

func (a *App) flipToRecord(peerName string, fd *peer.FlipEventData, status string, startedAt, completedAt time.Time) FlipRecord {
	rec := FlipRecord{
		ID:        fd.ID,
		Peer:      peerName,
		Direction: fd.Direction,
		Filename:  fd.Filename,
		Size:      fd.Size,
		Mime:      fd.Mime,
		Path:      fd.Path,
		Status:    status,
	}
	if !startedAt.IsZero() {
		rec.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !completedAt.IsZero() {
		rec.CompletedAt = completedAt.UTC().Format(time.RFC3339Nano)
	}
	if status == store.FlipStatusComplete && fd.Direction == store.DirectionIn {
		rec.CatchURL = "/catch/" + fd.ID
	}
	return rec
}

func ipsAsStrings(ips any) []string {
	// tsnet returns []netip.Addr; we convert to strings without importing the type
	// here to keep this file free of the tailscale.com/util/dnsname etc. deps.
	switch v := ips.(type) {
	case []string:
		return v
	default:
		out := []string{}
		// Use fmt to render each addr.
		s := fmt.Sprintf("%v", v)
		s = strings.Trim(s, "[]")
		if s == "" {
			return out
		}
		for _, p := range strings.Split(s, " ") {
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
}

// ---------- bound methods (callable from JS) ----------

// Status returns the current readiness state plus our own identity.
func (a *App) Status() AppStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return AppStatus{State: a.state, Message: a.stateMsg, Self: a.self}
}

// ListPeers returns everyone Headscale knows about (tailnet peers), merged
// with whoever we've ever chatted with (store) and whoever we're connected
// to right now (hub).
func (a *App) ListPeers() ([]PeerInfo, error) {
	a.mu.RLock()
	state := a.state
	a.mu.RUnlock()
	if state != StateReady {
		return nil, fmt.Errorf("not ready: %s", state)
	}

	byName := map[string]*PeerInfo{}

	lc, err := a.srv.LocalClient()
	if err == nil {
		ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
		st, err := lc.Status(ctx)
		cancel()
		if err == nil {
			for _, p := range st.Peer {
				name := p.HostName
				if name == "" {
					continue
				}
				if name == a.hostname {
					continue
				}
				byName[name] = &PeerInfo{
					Name:          name,
					TailnetName:   strings.TrimSuffix(p.DNSName, "."),
					IPs:           ipsAsStrings(p.TailscaleIPs),
					TailnetOnline: p.Online,
				}
			}
		}
	}

	// Merge in store roster (peers we've ever talked to, even if offline now).
	if roster, err := a.store.Peers(a.ctx); err == nil {
		for _, r := range roster {
			if r.Name == a.hostname {
				continue
			}
			if existing, ok := byName[r.Name]; ok {
				existing.LastSeen = r.LastSeen.UTC().Format(time.RFC3339Nano)
			} else {
				byName[r.Name] = &PeerInfo{
					Name:     r.Name,
					LastSeen: r.LastSeen.UTC().Format(time.RFC3339Nano),
				}
			}
		}
	}

	// Mark currently-connected peers.
	for _, name := range a.hub.Names() {
		if p, ok := byName[name]; ok {
			p.Connected = true
		} else {
			byName[name] = &PeerInfo{Name: name, Connected: true}
		}
	}

	out := make([]PeerInfo, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Connected != out[j].Connected {
			return out[i].Connected
		}
		if out[i].TailnetOnline != out[j].TailnetOnline {
			return out[i].TailnetOnline
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ListMessages returns the last N (or all if limit <= 0) messages with the peer.
func (a *App) ListMessages(peerName string, limit int) ([]MessageRecord, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	msgs, err := a.store.Messages(a.ctx, peerName, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MessageRecord, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, MessageRecord{
			ID:        m.ID,
			Peer:      m.Peer,
			Direction: m.Direction,
			Text:      m.Text,
			At:        m.At.UTC().Format(time.RFC3339Nano),
		})
	}
	return out, nil
}

// SendMessage delivers a 1:1 chat line to a peer we're connected to, stores
// it locally as an outbound row, and emits a message event so the UI shows it.
func (a *App) SendMessage(peerName, text string) error {
	if a.hub == nil {
		return fmt.Errorf("hub not ready")
	}
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return fmt.Errorf("empty message")
	}
	if err := a.hub.Send(peerName, text); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := a.store.AppendMessage(a.ctx, peerName, store.DirectionOut, text, now); err != nil {
		log.Printf("append out message: %v", err)
	}
	wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
		Peer:      peerName,
		Direction: store.DirectionOut,
		Text:      text,
		At:        now.Format(time.RFC3339Nano),
	})
	a.relayToTwin(peer.TwinSyncMessage{
		OriginalPeer: peerName,
		Direction:    store.DirectionOut,
		Text:         text,
		At:           now,
	})
	return nil
}

// relayToTwin sends a 1:1 chat row to the paired twin (if one is set and
// currently connected). Errors are swallowed — twin sync is best-effort.
func (a *App) relayToTwin(m peer.TwinSyncMessage) {
	twin, _ := a.store.GetSetting(a.ctx, store.SettingTwinHostname)
	if twin == "" || twin == a.hostname {
		return
	}
	if a.hub.Get(twin) == nil {
		return
	}
	_ = a.hub.SendTwinSyncMessage(twin, m)
}

// ---------- Booth bindings ----------

// CreateBooth forms a new Booth with the given members. It persists the booth
// locally, sends a BOOTH_INVITE to each *other* member that's currently
// connected, and returns the new booth's id.
//
// Members are peer hostnames (your own hostname is added automatically if
// missing). Anything past creation (invites to peers who come online later)
// is on the caller to retry.
func (a *App) CreateBooth(name string, members []string) (string, error) {
	if a.store == nil || a.hub == nil {
		return "", fmt.Errorf("app not ready")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name required")
	}
	id, err := newBoothID()
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	cleaned := []string{}
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		cleaned = append(cleaned, m)
	}
	add(a.hostname)
	for _, m := range members {
		add(m)
	}

	now := time.Now().UTC()
	if err := a.store.UpsertBooth(a.ctx, store.Booth{
		ID:        id,
		Name:      name,
		Founder:   a.hostname,
		FoundedAt: now,
	}); err != nil {
		return "", err
	}
	for _, m := range cleaned {
		_ = a.store.AddBoothMember(a.ctx, id, m, now)
	}

	invite := peer.BoothInvite{
		ID:        id,
		Name:      name,
		Founder:   a.hostname,
		Members:   cleaned,
		FoundedAt: now,
	}
	for _, m := range cleaned {
		if m == a.hostname {
			continue
		}
		if c := a.hub.Get(m); c != nil {
			if err := a.hub.SendBoothInvite(m, invite); err != nil {
				log.Printf("invite %s to booth %s: %v", m, id, err)
			}
		}
	}

	wailsruntime.EventsEmit(a.ctx, "booth", BoothRecord{
		ID:        id,
		Name:      name,
		Founder:   a.hostname,
		FoundedAt: now.Format(time.RFC3339Nano),
		Members:   append([]string{}, cleaned...),
	})
	return id, nil
}

// ListBooths returns every booth this node knows about.
func (a *App) ListBooths() ([]BoothRecord, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	booths, err := a.store.ListBooths(a.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BoothRecord, 0, len(booths))
	for _, b := range booths {
		members, _ := a.store.BoothMembers(a.ctx, b.ID)
		names := make([]string, 0, len(members))
		for _, m := range members {
			names = append(names, m.PeerName)
		}
		out = append(out, BoothRecord{
			ID:        b.ID,
			Name:      b.Name,
			Founder:   b.Founder,
			Motto:     b.Motto,
			FoundedAt: b.FoundedAt.UTC().Format(time.RFC3339Nano),
			Members:   names,
		})
	}
	return out, nil
}

// ListBoothMessages returns the last `limit` messages in a Booth.
func (a *App) ListBoothMessages(boothID string, limit int) ([]MessageRecord, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	msgs, err := a.store.MessagesByBooth(a.ctx, boothID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MessageRecord, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, MessageRecord{
			ID:        m.ID,
			Peer:      m.Peer,
			Direction: m.Direction,
			Text:      m.Text,
			At:        m.At.UTC().Format(time.RFC3339Nano),
			BoothID:   m.BoothID,
		})
	}
	return out, nil
}

// SendBoothMessage fans out a message to every connected member of the booth
// (other than us), persists it as an outbound row, and emits a UI event.
// Members that aren't currently connected simply miss this message (Phase 6
// MVP — no store-and-forward).
func (a *App) SendBoothMessage(boothID, text string) error {
	if a.store == nil || a.hub == nil {
		return fmt.Errorf("app not ready")
	}
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return fmt.Errorf("empty message")
	}
	members, err := a.store.BoothMembers(a.ctx, boothID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var sendErrs []string
	delivered := 0
	for _, m := range members {
		if m.PeerName == a.hostname {
			continue
		}
		if c := a.hub.Get(m.PeerName); c == nil {
			continue
		}
		if err := a.hub.SendBooth(m.PeerName, boothID, text); err != nil {
			sendErrs = append(sendErrs, m.PeerName+": "+err.Error())
		} else {
			delivered++
		}
	}
	if err := a.store.AppendMessageBooth(a.ctx, a.hostname, store.DirectionOut, text, boothID, now); err != nil {
		log.Printf("append booth out: %v", err)
	}
	wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
		Peer:      a.hostname,
		Direction: store.DirectionOut,
		Text:      text,
		At:        now.Format(time.RFC3339Nano),
		BoothID:   boothID,
	})
	if delivered == 0 && len(sendErrs) > 0 {
		return fmt.Errorf("no peers reachable: %s", strings.Join(sendErrs, "; "))
	}
	return nil
}

func newBoothID() (string, error) {
	b := make([]byte, 16)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// ---------- Showtime bindings ----------

// StartShowtime broadcasts a SHOWTIME_START to every connected member of the
// booth (other than us), naming us as leader. Returns the new session id.
func (a *App) StartShowtime(boothID, flipID string) (string, error) {
	if a.hub == nil || a.store == nil {
		return "", fmt.Errorf("app not ready")
	}
	// Resolve the flip locally (must be an inbound or outbound flip we know
	// about — typically a file that's already been booth-flipped to everyone).
	f, err := a.store.GetFlip(a.ctx, flipID)
	if err != nil {
		return "", fmt.Errorf("unknown flip %q", flipID)
	}
	id, err := newBoothID() // reuse UUIDv4 helper
	if err != nil {
		return "", err
	}
	start := peer.ShowtimeStart{
		SessionID: id,
		BoothID:   boothID,
		FlipID:    flipID,
		Leader:    a.hostname,
		Filename:  f.Filename,
		Mime:      f.Mime,
		At:        time.Now().UTC(),
	}
	members, err := a.store.BoothMembers(a.ctx, boothID)
	if err != nil {
		return "", err
	}
	delivered := 0
	for _, m := range members {
		if m.PeerName == a.hostname {
			continue
		}
		if a.hub.Get(m.PeerName) == nil {
			continue
		}
		if err := a.hub.SendShowtimeStart(m.PeerName, start); err == nil {
			delivered++
		}
	}
	// Echo back to our own UI as if we received it.
	wailsruntime.EventsEmit(a.ctx, "showtime-started", map[string]any{
		"sessionId": id,
		"boothId":   boothID,
		"flipId":    flipID,
		"leader":    a.hostname,
		"filename":  f.Filename,
		"mime":      f.Mime,
		"at":        start.At.UTC().Format(time.RFC3339Nano),
	})
	if delivered == 0 {
		return id, fmt.Errorf("no booth members reachable; you'll watch alone")
	}
	return id, nil
}

// SendShowtimeState relays a state update from the leader to every connected
// booth member.
func (a *App) SendShowtimeState(sessionID, boothID string, playing bool, position float64) error {
	if a.hub == nil || a.store == nil {
		return fmt.Errorf("app not ready")
	}
	st := peer.ShowtimeState{
		SessionID: sessionID,
		BoothID:   boothID,
		Playing:   playing,
		Position:  position,
		At:        time.Now().UTC(),
	}
	members, err := a.store.BoothMembers(a.ctx, boothID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.PeerName == a.hostname {
			continue
		}
		if a.hub.Get(m.PeerName) == nil {
			continue
		}
		_ = a.hub.SendShowtimeState(m.PeerName, st)
	}
	return nil
}

// ---------- Twin Mode bindings ----------

// GetTwin returns the currently paired twin's hostname (or "" if unpaired).
func (a *App) GetTwin() (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("store not ready")
	}
	return a.store.GetSetting(a.ctx, store.SettingTwinHostname)
}

// SetTwin pairs this instance with another instance by hostname. The other
// instance must call SetTwin with this instance's hostname too — the link
// is symmetric but each side stores its own setting.
func (a *App) SetTwin(hostname string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return fmt.Errorf("hostname required")
	}
	if hostname == a.hostname {
		return fmt.Errorf("cannot pair with yourself")
	}
	return a.store.SetSetting(a.ctx, store.SettingTwinHostname, hostname)
}

// ClearTwin removes the twin pairing.
func (a *App) ClearTwin() error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	return a.store.DeleteSetting(a.ctx, store.SettingTwinHostname)
}

// ---------- Notepad bindings ----------

// GetNotepad returns the current shared notepad for a booth (or an empty one).
func (a *App) GetNotepad(boothID string) (NotepadRecord, error) {
	if a.store == nil {
		return NotepadRecord{}, fmt.Errorf("store not ready")
	}
	n, err := a.store.GetBoothNotepad(a.ctx, boothID)
	if err != nil {
		return NotepadRecord{}, err
	}
	rec := NotepadRecord{
		BoothID:    n.BoothID,
		Text:       n.Text,
		Version:    n.Version,
		LastEditor: n.LastEditor,
	}
	if !n.LastModified.IsZero() {
		rec.LastModified = n.LastModified.UTC().Format(time.RFC3339Nano)
	}
	return rec, nil
}

// UpdateNotepad commits a new version of the shared notepad locally and
// broadcasts the change to every connected booth member.
func (a *App) UpdateNotepad(boothID, text string) (NotepadRecord, error) {
	if a.store == nil || a.hub == nil {
		return NotepadRecord{}, fmt.Errorf("app not ready")
	}
	cur, _ := a.store.GetBoothNotepad(a.ctx, boothID)
	next := store.BoothNotepad{
		BoothID:      boothID,
		Text:         text,
		Version:      cur.Version + 1,
		LastEditor:   a.hostname,
		LastModified: time.Now().UTC(),
	}
	if _, err := a.store.UpdateBoothNotepad(a.ctx, next); err != nil {
		return NotepadRecord{}, err
	}
	members, _ := a.store.BoothMembers(a.ctx, boothID)
	upd := peer.NotepadUpdate{
		BoothID: boothID,
		Text:    text,
		Version: next.Version,
		Editor:  a.hostname,
		At:      next.LastModified,
	}
	for _, m := range members {
		if m.PeerName == a.hostname {
			continue
		}
		if a.hub.Get(m.PeerName) == nil {
			continue
		}
		_ = a.hub.SendNotepadUpdate(m.PeerName, upd)
	}
	rec := NotepadRecord{
		BoothID:      boothID,
		Text:         text,
		Version:      next.Version,
		LastEditor:   a.hostname,
		LastModified: next.LastModified.UTC().Format(time.RFC3339Nano),
	}
	wailsruntime.EventsEmit(a.ctx, "notepad", rec)
	return rec, nil
}

// EndShowtime broadcasts a SHOWTIME_END.
func (a *App) EndShowtime(sessionID, boothID string) error {
	if a.hub == nil || a.store == nil {
		return fmt.Errorf("app not ready")
	}
	end := peer.ShowtimeEnd{SessionID: sessionID, BoothID: boothID, At: time.Now().UTC()}
	members, err := a.store.BoothMembers(a.ctx, boothID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.PeerName == a.hostname {
			continue
		}
		if a.hub.Get(m.PeerName) == nil {
			continue
		}
		_ = a.hub.SendShowtimeEnd(m.PeerName, end)
	}
	wailsruntime.EventsEmit(a.ctx, "showtime-ended", map[string]any{
		"sessionId": sessionID,
		"boothId":   boothID,
	})
	return nil
}

// Connect dials a peer by tailnet hostname. The HELLO handshake makes them
// appear in ListPeers as Connected=true.
func (a *App) Connect(peerName string) error {
	if a.hub == nil {
		return fmt.Errorf("hub not ready")
	}
	if peerName == "" {
		return fmt.Errorf("peer name required")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	return a.hub.Dial(ctx, a.srv.Dial, a.tlsCfg, peerName, a.hostname)
}

// Disconnect closes the active connection to a peer (sending BYE if possible).
func (a *App) Disconnect(peerName string) error {
	if a.hub == nil {
		return fmt.Errorf("hub not ready")
	}
	c := a.hub.Get(peerName)
	if c == nil {
		return fmt.Errorf("not connected to %s", peerName)
	}
	_ = c.WriteFrame(peer.TypeBye, peer.Bye{Reason: "user disconnected"})
	c.Close()
	return nil
}

// SendFlip starts a file transfer to the named peer. Returns the new flip's id.
func (a *App) SendFlip(peerName, localPath string) (string, error) {
	if a.hub == nil {
		return "", fmt.Errorf("hub not ready")
	}
	if peerName == "" || localPath == "" {
		return "", fmt.Errorf("peer and path required")
	}
	return a.hub.SendFlip(peerName, localPath)
}

// SendBoothFlip sends the same local file to every connected member of a
// booth (other than us). All members receive the file under the SAME flip id
// so a subsequent showtime can reference one id that exists on every receiver.
// Returns the shared flip id.
func (a *App) SendBoothFlip(boothID, localPath string) (string, error) {
	if a.hub == nil || a.store == nil {
		return "", fmt.Errorf("app not ready")
	}
	members, err := a.store.BoothMembers(a.ctx, boothID)
	if err != nil {
		return "", err
	}
	id, err := newBoothID() // UUIDv4 helper, reused
	if err != nil {
		return "", err
	}
	delivered := 0
	var firstErr error
	for _, m := range members {
		if m.PeerName == a.hostname {
			continue
		}
		if a.hub.Get(m.PeerName) == nil {
			continue
		}
		if err := a.hub.SendFlipWithID(m.PeerName, localPath, id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			delivered++
		}
	}
	if delivered == 0 {
		if firstErr != nil {
			return id, firstErr
		}
		return id, fmt.Errorf("no booth members reachable")
	}
	return id, nil
}

// PickAndSendFlip pops the OS file picker, then flips the chosen file.
// Returns the new flip's id, or "" if the user cancelled.
func (a *App) PickAndSendFlip(peerName string) (string, error) {
	if a.hub == nil {
		return "", fmt.Errorf("hub not ready")
	}
	if peerName == "" {
		return "", fmt.Errorf("peer required")
	}
	path, err := wailsruntime.OpenFileDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Pick a file to flip",
	})
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", nil
	}
	return a.hub.SendFlip(peerName, path)
}

// ListFlips returns the flip history with the named peer.
func (a *App) ListFlips(peerName string) ([]FlipRecord, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	rows, err := a.store.FlipsByPeer(a.ctx, peerName)
	if err != nil {
		return nil, err
	}
	out := make([]FlipRecord, 0, len(rows))
	for _, r := range rows {
		rec := FlipRecord{
			ID:        r.ID,
			Peer:      r.Peer,
			Direction: r.Direction,
			Filename:  r.Filename,
			Size:      r.Size,
			Mime:      r.Mime,
			Path:      r.Path,
			Status:    r.Status,
			StartedAt: r.StartedAt.UTC().Format(time.RFC3339Nano),
		}
		if !r.CompletedAt.IsZero() {
			rec.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
		if r.Status == store.FlipStatusComplete && r.Direction == store.DirectionIn {
			rec.CatchURL = "/catch/" + r.ID
		}
		out = append(out, rec)
	}
	return out, nil
}

// OpenFlipExternally opens a caught file in the OS default application.
func (a *App) OpenFlipExternally(id string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	f, err := a.store.GetFlip(a.ctx, id)
	if err != nil {
		return fmt.Errorf("unknown flip %q", id)
	}
	if f.Path == "" {
		return fmt.Errorf("no local path for flip %q", id)
	}
	cmd := exec.Command("cmd", "/c", "start", "", f.Path)
	return cmd.Start()
}

// catchHandler serves /catch/<flip-id> by streaming the on-disk file. It only
// serves flips whose status is "complete" and direction is "in" — i.e. files
// the local user actually caught, never arbitrary local paths.
func (a *App) catchHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/catch/")
	if id == "" || strings.ContainsAny(id, "/\\") {
		http.NotFound(w, r)
		return
	}
	if a.store == nil {
		http.Error(w, "store not ready", http.StatusServiceUnavailable)
		return
	}
	f, err := a.store.GetFlip(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if f.Status != store.FlipStatusComplete {
		http.Error(w, "flip not complete", http.StatusNotFound)
		return
	}
	if f.Direction != store.DirectionIn {
		http.Error(w, "not an inbound flip", http.StatusNotFound)
		return
	}
	if f.Mime != "" {
		w.Header().Set("Content-Type", f.Mime)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", f.Filename))
	http.ServeFile(w, r, f.Path)
}
