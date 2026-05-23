package main

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	mimepkg "mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"fliporium/internal/identity"
	"fliporium/internal/peer"
	"fliporium/internal/rtc"
	"fliporium/internal/store"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// defaultSignalURL is the public signaling server (served by Caddy on the main
// domain). Override with FLIPORIUM_SIGNAL for local development.
const defaultSignalURL = "wss://fliporium.com/ws"

// AppState describes where the App is in its lifecycle.
type AppState string

const (
	StateInitializing AppState = "initializing"
	StateReady        AppState = "ready"
	StateError        AppState = "error"
)

// SelfInfo is what the UI shows about *us* in the title bar / status.
type SelfInfo struct {
	Hostname    string `json:"hostname"`
	DisplayName string `json:"displayName"`
	ID          string `json:"id"`     // stable pubkey fingerprint
	Avatar      string `json:"avatar"` // our avatar data URI ("" if none)
	Online      bool   `json:"online"`
}

// PeerInfo summarises a peer for the Floor list.
type PeerInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	Connected   bool   `json:"connected"`
	LastSeen    string `json:"lastSeen,omitempty"`
}

// MessageRecord is one persisted chat line as the frontend sees it.
type MessageRecord struct {
	ID          int64               `json:"id"`
	UUID        string              `json:"uuid,omitempty"`
	Peer        string              `json:"peer"`
	DisplayName string              `json:"displayName,omitempty"`
	Direction   string              `json:"direction"`
	Text        string              `json:"text"`
	At          string              `json:"at"`
	BoothID     string              `json:"boothId,omitempty"`
	ParentUUID  string              `json:"parentUuid,omitempty"`
	EditedAt    string              `json:"editedAt,omitempty"`
	DeletedAt   string              `json:"deletedAt,omitempty"`
	Pinned      bool                `json:"pinned,omitempty"`
	Reactions   map[string][]string `json:"reactions,omitempty"` // emoji -> peer-names
	Card        *peer.LinkCard      `json:"card,omitempty"`      // unfurled link preview, if any
}

func (a *App) toMessageRecord(m store.Message, displays map[string]string) MessageRecord {
	// DisplayName is the SENDER's friendly name. Outgoing lines are ours;
	// incoming lines are from m.Peer (the sender, for both 1:1 and rooms).
	sender := a.displayFor(m.Peer, displays)
	if m.Direction == store.DirectionOut {
		sender = a.displayName()
	}
	r := MessageRecord{
		ID:          m.ID,
		UUID:        m.UUID,
		Peer:        m.Peer,
		DisplayName: sender,
		Direction:   m.Direction,
		Text:        m.Text,
		At:          m.At.UTC().Format(time.RFC3339Nano),
		BoothID:     m.BoothID,
		ParentUUID:  m.ParentUUID,
		Pinned:      m.Pinned,
	}
	if !m.EditedAt.IsZero() {
		r.EditedAt = m.EditedAt.UTC().Format(time.RFC3339Nano)
	}
	if !m.DeletedAt.IsZero() {
		r.DeletedAt = m.DeletedAt.UTC().Format(time.RFC3339Nano)
	}
	if m.Card != "" {
		var c peer.LinkCard
		if json.Unmarshal([]byte(m.Card), &c) == nil && c.URL != "" {
			if safe, ok := sanitizeLinkCard(c); ok {
				r.Card = &safe
			}
		}
	}
	return r
}

// displayFor resolves a routing id to a friendly name: our own name for
// ourselves, the roster's known name otherwise, and "" if unknown (the
// frontend then falls back to showing the id).
func (a *App) displayFor(peerID string, displays map[string]string) string {
	if peerID == a.hostname {
		return a.displayName()
	}
	if displays != nil {
		if d := displays[peerID]; d != "" {
			return d
		}
	}
	return ""
}

// displayResolve is the event-path variant: it prefers an explicit name hint
// carried on the event (the live or backlog sender's announced name).
func (a *App) displayResolve(peerID, hint string) string {
	if peerID == a.hostname {
		return a.displayName()
	}
	return hint
}

func collectUUIDs(msgs []store.Message) []string {
	out := []string{}
	for _, m := range msgs {
		if m.UUID != "" {
			out = append(out, m.UUID)
		}
	}
	return out
}

// attachReactions decorates a slice of MessageRecord with reactions in bulk.
func (a *App) attachReactions(records []MessageRecord, msgs []store.Message) {
	uuids := collectUUIDs(msgs)
	if len(uuids) == 0 {
		return
	}
	byUUID, err := a.store.ReactionsForMessages(a.ctx, uuids)
	if err != nil || len(byUUID) == 0 {
		return
	}
	for i := range records {
		if records[i].UUID == "" {
			continue
		}
		rs := byUUID[records[i].UUID]
		if len(rs) == 0 {
			continue
		}
		m := map[string][]string{}
		for _, r := range rs {
			m[r.Emoji] = append(m[r.Emoji], r.Peer)
		}
		records[i].Reactions = m
	}
}

// newUUID returns a v4 UUID. (Same algorithm as the booth/flip helpers; kept
// inline so the call sites stay self-contained.)
func newUUID() (string, error) { return newBoothID() }

// BoothRecord is a Booth surfaced to the frontend.
type BoothRecord struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Founder   string   `json:"founder"`
	Motto     string   `json:"motto,omitempty"`
	FoundedAt    string   `json:"foundedAt"`
	Members      []string `json:"members"`
	Pending      bool     `json:"pending,omitempty"`      // an invite awaiting accept/decline
	LastActivity string   `json:"lastActivity,omitempty"` // most recent message time ("" if none)
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
	BoothID     string `json:"boothId,omitempty"` // conversation scope; "" = 1:1
	// CatchURL is the in-app URL the frontend uses to render the file (only
	// set when status is complete and direction is in).
	CatchURL string `json:"catchUrl,omitempty"`
}

// AppStatus is the lightweight readiness probe the frontend polls / observes.
type AppStatus struct {
	State   AppState `json:"state"`
	Message string   `json:"message,omitempty"`
	Self    SelfInfo `json:"self"`
}

// App is the Wails-bound object. Its exported methods become callable from JS.
type App struct {
	ctx context.Context

	mu       sync.RWMutex
	state    AppState
	stateMsg string
	self     SelfInfo

	store *store.Store

	hostname string
	dataDir  string
	identity identity.Identity

	// WebRTC transport state. We hold one live session (its own signaling
	// room + WebRTC mesh + Hub) per booth the user is in, all live at once.
	signalURL  string
	stun       []string
	joinMu     sync.Mutex // serializes joins so the same room is never dialed twice at once
	sessionsMu sync.Mutex
	sessions   map[string]*roomSession // roomID -> live session

	statusMu   sync.Mutex
	peerStatus map[string]string // peer name -> "active"|"idle"|"away"
	myStatus   string
}

// roomSession is one live booth: its own signaling room, WebRTC mesh, and Hub
// (with that room's end-to-end key). cancel stops the room's pump + event loop.
type roomSession struct {
	id     string
	hub    *peer.Hub
	room   *rtc.Room
	cancel context.CancelFunc
}

func NewApp() *App {
	return &App{state: StateInitializing, sessions: map[string]*roomSession{}}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// startup is called by Wails after the window is ready. We initialise
// asynchronously so the window paints immediately rather than blocking on
// the transport coming up.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.dataDir = env("FLIPORIUM_DIR", defaultDataDir())
	// The routing/signaling id MUST be unique per install — two peers sharing
	// an id collide on the signaling server and can never connect. Default to
	// the install's stable Ed25519 fingerprint; FLIPORIUM_HOSTNAME overrides
	// (used by tests/dev to run several peers on one box).
	routingID := env("FLIPORIUM_HOSTNAME", "")
	if id, err := identity.Load(a.dataDir); err == nil {
		a.identity = id
		if routingID == "" {
			routingID = "fp-" + id.ID()
		}
		log.Printf("startup: identity %s", id.ID())
	} else {
		log.Printf("startup: identity load failed: %v", err)
	}
	if routingID == "" {
		// No identity and no override: fall back to a random id so we never
		// collide with another install.
		var b [8]byte
		_, _ = cryptoRand.Read(b[:])
		routingID = "fp-" + base64.RawURLEncoding.EncodeToString(b[:])
	}
	a.hostname = routingID
	log.Printf("startup: ctx=%v id=%s dir=%s", ctx != nil, a.hostname, a.dataDir)

	go a.initBackground()
}

func (a *App) shutdown(ctx context.Context) {
	a.closeAllSessions("window closed")
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

	signalURL := env("FLIPORIUM_SIGNAL", defaultSignalURL)
	a.initWebRTC(signalURL)
}

// initWebRTC brings the app up on the WebRTC transport: peers meet in a
// signaling room and connect peer-to-peer over DataChannels.
func (a *App) initWebRTC(signalURL string) {
	a.signalURL = signalURL
	a.stun = []string{"stun:stun.l.google.com:19302"}
	if s := os.Getenv("FLIPORIUM_STUN"); s != "" {
		a.stun = strings.Split(s, ",")
	}

	a.mu.Lock()
	a.self = SelfInfo{Hostname: a.hostname, DisplayName: a.displayName(), ID: a.identity.ID(), Avatar: a.selfAvatar(), Online: true}
	a.mu.Unlock()

	a.setState(StateReady, "")

	log.Printf("init: webrtc transport ready (signal=%s host=%s)", signalURL, a.hostname)
	go a.joinAllRooms()
}

// joinAllRooms goes live in every booth the user is in (the all-live model:
// you get real-time messages + unread across all your rooms at once). A dev/
// test FLIPORIUM_ROOM that isn't a saved booth is joined too.
func (a *App) joinAllRooms() {
	joined := map[string]bool{}
	if booths, err := a.store.ListBooths(a.ctx); err == nil {
		for _, b := range booths {
			joined[b.ID] = true
			if p, _ := a.store.GetSetting(a.ctx, roomPendingKey(b.ID)); p == "1" {
				continue // an invite we haven't accepted yet — don't connect
			}
			if err := a.joinSessionQuiet(b.ID); err != nil {
				log.Printf("webrtc: join booth %q: %v", b.ID, err)
			}
		}
	}
	if r := env("FLIPORIUM_ROOM", ""); r != "" && !joined[r] {
		if err := a.joinSessionQuiet(r); err != nil {
			log.Printf("webrtc: join FLIPORIUM_ROOM %q: %v", r, err)
		}
	}
}

// joinSessionQuiet is joinSession without emitting the room-changed focus event
// (used for bulk auto-join at launch).
func (a *App) joinSessionQuiet(roomID string) error {
	_, err := a.joinSessionFocus(roomID, false)
	return err
}

// transportReady reports whether the WebRTC transport has been initialised.
func (a *App) transportReady() bool { return a.signalURL != "" && a.store != nil }

const settingCurrentRoom = "current_room"

// maxInviteMembers bounds how many member ids a single BOOTH_INVITE may seed,
// so a peer can't pad a room's roster with a flood of fabricated ids. Rooms are
// friend-sized (the mesh caps at 16); 32 is a lenient ceiling.
const maxInviteMembers = 32

// maxReactionEmojiBytes bounds an inbound reaction's emoji so a peer can't spam
// the store with giant strings (real emoji, even ZWJ sequences, are tiny).
const maxReactionEmojiBytes = 64

// inSameConversation reports whether a control frame (reaction/pin) for message
// `orig`, arriving from `sender` over room `roomID`'s connection, legitimately
// belongs to that conversation: a booth message must arrive on that booth's
// link, and a 1:1 message must come from the counterpart. This stops a peer you
// share one room with from reacting to or pinning messages in other rooms (or
// your 1:1s) just by learning a message UUID.
func inSameConversation(orig store.Message, sender, roomID string) bool {
	if orig.BoothID != "" {
		return orig.BoothID == roomID
	}
	return orig.Peer == sender
}

// RoomInfo describes a room and its shareable invite link.
type RoomInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Link string `json:"link"`
}

// joinSession brings a booth live: its own Hub (keyed with the room's E2E
// secret), its own signaling room + mesh, and its own event pump. Idempotent —
// if the room is already live, returns the existing session. focus=true also
// records it as the "last room" and emits room-changed (a user-initiated open).
func (a *App) joinSessionFocus(roomID string, focus bool) (*roomSession, error) {
	if !a.transportReady() {
		return nil, fmt.Errorf("transport not ready")
	}
	// Serialize joins. Without this, two concurrent callers for the same room
	// (e.g. joinAllRooms racing a user clicking that booth at startup) both pass
	// the "already joined?" check and both dial a websocket. The loser leaks,
	// and because the signaling server keys members by id (last-writer-wins,
	// and leave deletes by id), the duplicate also corrupts the server's view
	// of who's in the room. Holding joinMu — not sessionsMu — keeps sends fast.
	a.joinMu.Lock()
	defer a.joinMu.Unlock()

	if s := a.sessionFor(roomID); s != nil {
		if focus {
			_ = a.store.SetSetting(a.ctx, settingCurrentRoom, roomID)
		}
		return s, nil
	}

	hub := peer.NewHub()
	hub.CatchRoot = filepath.Join(a.dataDir, "catch")
	hub.SetSelfDisplay(a.displayName())
	hub.SetSelfAvatar(a.selfAvatar())
	hub.SetBlocked(a.blockedIDs())
	hub.SetIdentity(a.identity.Priv, a.identity.Pub) // proves our id to peers + lets us verify theirs
	// Set the room's end-to-end key (derived from the invite-link secret) before
	// connecting, so peers that join inherit it. No secret -> no encryption.
	if secret, _ := a.store.GetSetting(a.ctx, roomSecretKey(roomID)); secret != "" {
		if k, err := secretToKey(secret); err == nil {
			hub.SetRoomKey(k)
		}
	}
	ctx, cancel := context.WithCancel(a.ctx)
	r, err := hub.JoinRoom(ctx, a.signalURL, roomID, a.hostname, a.stun)
	if err != nil {
		cancel()
		return nil, err
	}
	go a.eventPump(ctx, roomID, hub)
	s := &roomSession{id: roomID, hub: hub, room: r, cancel: cancel}
	a.sessionsMu.Lock()
	a.sessions[roomID] = s
	a.sessionsMu.Unlock()
	if focus {
		_ = a.store.SetSetting(a.ctx, settingCurrentRoom, roomID)
		wailsruntime.EventsEmit(a.ctx, "room-changed", roomID)
	}
	return s, nil
}

// joinSession is the focused (user-initiated) variant.
func (a *App) joinSession(roomID string) (*roomSession, error) {
	return a.joinSessionFocus(roomID, true)
}

// leaveSession drops a booth's live session (mesh + signaling + pump). The
// signaling server broadcasts our departure to the other members. Reversible:
// rejoin via joinSession.
func (a *App) leaveSession(roomID string) {
	a.sessionsMu.Lock()
	s := a.sessions[roomID]
	delete(a.sessions, roomID)
	a.sessionsMu.Unlock()
	if s == nil {
		return
	}
	if s.room != nil {
		s.room.Close()
	}
	s.hub.CloseAllPeers()
	s.cancel()
}

// closeAllSessions tears every live session down (shutdown / burn).
func (a *App) closeAllSessions(reason string) {
	a.sessionsMu.Lock()
	all := make([]*roomSession, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	a.sessions = map[string]*roomSession{}
	a.sessionsMu.Unlock()
	for _, s := range all {
		if s.room != nil {
			s.room.Close()
		}
		s.hub.ByeAll(reason)
		s.cancel()
	}
}

// sessionFor returns the live session for a room, or nil if not joined.
func (a *App) sessionFor(roomID string) *roomSession {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	return a.sessions[roomID]
}

// roomHub returns the Hub for a live room, or nil if the room isn't joined.
func (a *App) roomHub(roomID string) *peer.Hub {
	if s := a.sessionFor(roomID); s != nil {
		return s.hub
	}
	return nil
}

// hubForPeer finds the live session Hub that currently has an open connection
// to peerName (a peer can be reachable in any room you share). nil if none.
func (a *App) hubForPeer(peerName string) *peer.Hub {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	for _, s := range a.sessions {
		if s.hub.Get(peerName) != nil {
			return s.hub
		}
	}
	return nil
}

// eachHub runs fn against every live session's Hub.
func (a *App) eachHub(fn func(*peer.Hub)) {
	a.sessionsMu.Lock()
	hubs := make([]*peer.Hub, 0, len(a.sessions))
	for _, s := range a.sessions {
		hubs = append(hubs, s.hub)
	}
	a.sessionsMu.Unlock()
	for _, h := range hubs {
		fn(h)
	}
}

func roomSecretKey(roomID string) string { return "roomsecret:" + roomID }

// newRoomSecret returns 32 fresh random bytes, base64url-encoded for the link.
func newRoomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := cryptoRand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// secretToKey decodes a room secret into a 32-byte symmetric key.
func secretToKey(secret string) (*[32]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("invalid room secret")
	}
	var k [32]byte
	copy(k[:], b)
	return &k, nil
}

// CreateRoom makes a new room (a booth joined over a fresh signaling room),
// switches to it, and returns its shareable invite link.
func (a *App) CreateRoom(name string) (RoomInfo, error) {
	if !a.transportReady() {
		return RoomInfo{}, fmt.Errorf("app not ready")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Untitled room"
	}
	id, err := newBoothID()
	if err != nil {
		return RoomInfo{}, err
	}
	secret, err := newRoomSecret()
	if err != nil {
		return RoomInfo{}, err
	}
	now := time.Now().UTC()
	if err := a.store.UpsertBooth(a.ctx, store.Booth{ID: id, Name: name, Founder: a.hostname, FoundedAt: now}); err != nil {
		return RoomInfo{}, err
	}
	_ = a.store.SetSetting(a.ctx, roomSecretKey(id), secret)
	_ = a.store.AddBoothMember(a.ctx, id, a.hostname, now)
	if _, err := a.joinSession(id); err != nil {
		return RoomInfo{}, err
	}
	wailsruntime.EventsEmit(a.ctx, "booth", BoothRecord{
		ID: id, Name: name, Founder: a.hostname,
		FoundedAt: now.Format(time.RFC3339Nano), Members: []string{a.hostname},
	})
	return RoomInfo{ID: id, Name: name, Link: roomLink(id, name, secret)}, nil
}

// JoinRoomByLink parses an invite link, records the room locally, and switches
// to it. The mesh connects to whoever else is currently in the room.
func (a *App) JoinRoomByLink(link string) (RoomInfo, error) {
	if !a.transportReady() {
		return RoomInfo{}, fmt.Errorf("app not ready")
	}
	id, name, secret := parseRoomLink(link)
	if id == "" {
		return RoomInfo{}, fmt.Errorf("that doesn't look like a valid invite link")
	}
	if name == "" {
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		name = "Room " + short
	}
	now := time.Now().UTC()
	if err := a.store.UpsertBooth(a.ctx, store.Booth{ID: id, Name: name, FoundedAt: now}); err != nil {
		return RoomInfo{}, err
	}
	if secret != "" {
		_ = a.store.SetSetting(a.ctx, roomSecretKey(id), secret)
	}
	_ = a.store.AddBoothMember(a.ctx, id, a.hostname, now)
	_ = a.store.DeleteSetting(a.ctx, roomPendingKey(id)) // joining via link == accepting
	if _, err := a.joinSession(id); err != nil {
		return RoomInfo{}, err
	}
	wailsruntime.EventsEmit(a.ctx, "booth", BoothRecord{
		ID: id, Name: name, FoundedAt: now.Format(time.RFC3339Nano), Members: []string{a.hostname},
	})
	return RoomInfo{ID: id, Name: name, Link: roomLink(id, name, secret)}, nil
}

// SwitchRoom focuses a room the user already knows about (by booth id),
// bringing it live if it isn't already. With the all-live model the room is
// usually already joined; this just makes sure and records the focus.
func (a *App) SwitchRoom(roomID string) error {
	if strings.TrimSpace(roomID) == "" {
		return fmt.Errorf("room id required")
	}
	_, err := a.joinSession(roomID)
	return err
}

// CurrentRoom returns the id of the last room the user focused ("" if none).
func (a *App) CurrentRoom() (string, error) {
	if a.store == nil {
		return "", nil
	}
	return a.store.GetSetting(a.ctx, settingCurrentRoom)
}

// LeaveRoom drops a room's live session, stops participating, and removes it
// from the Floor (the other members see us leave). Local message/file history
// is KEPT — it reappears if you rejoin with the invite link. DeleteRoom purges
// it. Reversible.
func (a *App) LeaveRoom(roomID string) error {
	if strings.TrimSpace(roomID) == "" {
		return fmt.Errorf("room id required")
	}
	a.leaveSession(roomID)
	if a.store != nil {
		_ = a.store.DeleteBooth(a.ctx, roomID)
		_ = a.store.DeleteSetting(a.ctx, roomPendingKey(roomID))
		cur, _ := a.store.GetSetting(a.ctx, settingCurrentRoom)
		if cur == roomID {
			_ = a.store.DeleteSetting(a.ctx, settingCurrentRoom)
		}
	}
	wailsruntime.EventsEmit(a.ctx, "booth-removed", roomID)
	return nil
}

// DeleteRoom leaves a room AND purges all of its local data — messages, parked
// files, and the room key. Permanent; no undo. (Files you caught from members
// may remain under the People view, since flips are keyed per sender.)
func (a *App) DeleteRoom(roomID string) error {
	if strings.TrimSpace(roomID) == "" {
		return fmt.Errorf("room id required")
	}
	a.leaveSession(roomID)
	if a.store != nil {
		_ = a.store.DeleteMessagesByBooth(a.ctx, roomID)
		_ = a.store.DeleteFlipsByPeer(a.ctx, roomID) // parked files keyed by room id
		_ = a.store.DeleteBooth(a.ctx, roomID)
		_ = a.store.DeleteSetting(a.ctx, roomSecretKey(roomID))
		_ = a.store.DeleteSetting(a.ctx, roomPendingKey(roomID))
		cur, _ := a.store.GetSetting(a.ctx, settingCurrentRoom)
		if cur == roomID {
			_ = a.store.DeleteSetting(a.ctx, settingCurrentRoom)
		}
	}
	wailsruntime.EventsEmit(a.ctx, "booth-removed", roomID)
	return nil
}

func roomPendingKey(roomID string) string { return "roompending:" + roomID }

// blockedIDs returns the local blocklist (peer ids we refuse to connect to).
func (a *App) blockedIDs() []string {
	if a.store == nil {
		return nil
	}
	ids, _ := a.store.BlockedPeers(a.ctx)
	return ids
}

// BlockPeer blocks a peer id everywhere: refuses future connections, drops any
// current one, and stops delivering/receiving anything to/from them. Local and
// reversible (UnblockPeer). The server is never involved.
func (a *App) BlockPeer(peerID string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	peerID = strings.TrimSpace(peerID)
	if peerID == "" || peerID == a.hostname {
		return fmt.Errorf("invalid peer")
	}
	if err := a.store.BlockPeer(a.ctx, peerID); err != nil {
		return err
	}
	ids := a.blockedIDs()
	a.eachHub(func(h *peer.Hub) {
		h.SetBlocked(ids)
		if c := h.Get(peerID); c != nil {
			c.Close() // drop any live connection to them now
		}
	})
	wailsruntime.EventsEmit(a.ctx, "blocklist-changed", peerID)
	return nil
}

// UnblockPeer removes a peer from the blocklist (they can connect again).
func (a *App) UnblockPeer(peerID string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	if err := a.store.UnblockPeer(a.ctx, peerID); err != nil {
		return err
	}
	a.applyBlocklistToHubs()
	wailsruntime.EventsEmit(a.ctx, "blocklist-changed", peerID)
	return nil
}

func (a *App) applyBlocklistToHubs() {
	ids := a.blockedIDs()
	a.eachHub(func(h *peer.Hub) { h.SetBlocked(ids) })
}

// ListBlocked returns the blocked peer ids paired with their last-known display
// names (for a manageable blocklist UI).
func (a *App) ListBlocked() ([]PeerInfo, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	ids, err := a.store.BlockedPeers(a.ctx)
	if err != nil {
		return nil, err
	}
	displays, _ := a.store.PeerDisplays(a.ctx)
	out := make([]PeerInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, PeerInfo{Name: id, DisplayName: displays[id]})
	}
	return out, nil
}

// AcceptInvite accepts a pending room invite: clears its pending mark and goes
// live in it.
func (a *App) AcceptInvite(roomID string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	if _, err := a.joinSession(roomID); err != nil {
		return err // keep it pending so the user can retry
	}
	_ = a.store.DeleteSetting(a.ctx, roomPendingKey(roomID))
	wailsruntime.EventsEmit(a.ctx, "invite-resolved", roomID)
	return nil
}

// DeclineInvite rejects a pending invite: removes the booth without ever
// connecting.
func (a *App) DeclineInvite(roomID string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	_ = a.store.DeleteSetting(a.ctx, roomPendingKey(roomID))
	_ = a.store.DeleteBooth(a.ctx, roomID)
	wailsruntime.EventsEmit(a.ctx, "booth-removed", roomID)
	return nil
}

// StartDM opens a private 2-person booth with a peer you're connected to and
// hands them the invite over the existing peer-to-peer link — no link to paste.
// They get an accept/decline prompt; on accept the mesh connects the two of you
// in a fresh end-to-end-encrypted room only you two hold the key to. If a DM
// with this peer already exists, it's reused (focused) instead of duplicated.
func (a *App) StartDM(peerID string) (RoomInfo, error) {
	if !a.transportReady() {
		return RoomInfo{}, fmt.Errorf("app not ready")
	}
	peerID = strings.TrimSpace(peerID)
	if peerID == "" || peerID == a.hostname {
		return RoomInfo{}, fmt.Errorf("invalid peer")
	}
	// The invite rides the live P2P link, so they must be connected right now.
	h := a.hubForPeer(peerID)
	if h == nil || h.Get(peerID) == nil {
		return RoomInfo{}, fmt.Errorf("you can only start a private booth with someone you're connected to")
	}
	displays, _ := a.store.PeerDisplays(a.ctx)

	// Reuse an existing DM with this peer rather than spawning duplicates.
	if existing := a.existingDM(peerID); existing != "" {
		if _, err := a.joinSession(existing); err != nil {
			return RoomInfo{}, err
		}
		b, _ := a.store.GetBooth(a.ctx, existing)
		secret, _ := a.store.GetSetting(a.ctx, roomSecretKey(existing))
		return RoomInfo{ID: existing, Name: b.Name, Link: roomLink(existing, b.Name, secret)}, nil
	}

	id, err := newBoothID()
	if err != nil {
		return RoomInfo{}, err
	}
	secret, err := newRoomSecret()
	if err != nil {
		return RoomInfo{}, err
	}
	now := time.Now().UTC()
	name := a.displayName() + " & " + a.displayFor(peerID, displays)
	members := []string{a.hostname, peerID}

	if err := a.store.UpsertBooth(a.ctx, store.Booth{ID: id, Name: name, Founder: a.hostname, FoundedAt: now}); err != nil {
		return RoomInfo{}, err
	}
	_ = a.store.SetSetting(a.ctx, roomSecretKey(id), secret)
	_ = a.store.AddBoothMember(a.ctx, id, a.hostname, now)
	_ = a.store.AddBoothMember(a.ctx, id, peerID, now)
	if _, err := a.joinSession(id); err != nil {
		return RoomInfo{}, err
	}

	// Hand the invite (with the room key) to the one peer over the encrypted link.
	if err := h.SendBoothInvite(peerID, peer.BoothInvite{
		ID: id, Name: name, Founder: a.hostname, Members: members, Secret: secret, FoundedAt: now,
	}); err != nil {
		return RoomInfo{}, fmt.Errorf("couldn't deliver the invite: %w", err)
	}

	wailsruntime.EventsEmit(a.ctx, "booth", BoothRecord{
		ID: id, Name: name, Founder: a.hostname,
		FoundedAt: now.Format(time.RFC3339Nano), Members: members,
	})
	return RoomInfo{ID: id, Name: name, Link: roomLink(id, name, secret)}, nil
}

// existingDM returns the id of a 2-person booth whose only members are us and
// peerID, or "" if there's none.
func (a *App) existingDM(peerID string) string {
	booths, err := a.store.ListBooths(a.ctx)
	if err != nil {
		return ""
	}
	for _, b := range booths {
		members, _ := a.store.BoothMembers(a.ctx, b.ID)
		if len(members) != 2 {
			continue
		}
		var hasSelf, hasPeer bool
		for _, m := range members {
			switch m.PeerName {
			case a.hostname:
				hasSelf = true
			case peerID:
				hasPeer = true
			}
		}
		if hasSelf && hasPeer {
			return b.ID
		}
	}
	return ""
}

// RoomLinkFor returns the shareable invite link for a room the user is in.
func (a *App) RoomLinkFor(roomID string) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("store not ready")
	}
	b, err := a.store.GetBooth(a.ctx, roomID)
	if err != nil {
		return "", err
	}
	secret, _ := a.store.GetSetting(a.ctx, roomSecretKey(roomID))
	return roomLink(b.ID, b.Name, secret), nil
}

// roomLink builds the shareable invite URL. The room id, name, and the E2E
// secret all live in the URL fragment (after #), which browsers never send to
// the server — so the signaling server can match-make by id but can't read
// any room's traffic.
func roomLink(id, name, secret string) string {
	v := url.Values{}
	v.Set("r", id)
	if secret != "" {
		v.Set("k", secret)
	}
	if name != "" {
		v.Set("n", name)
	}
	return "https://fliporium.com/join#" + v.Encode()
}

// parseRoomLink extracts the room id, name, and E2E secret from an invite
// link. Accepts the full https://fliporium.com/join#r=...&k=...&n=... URL, a
// bare fragment, or a bare room id.
func parseRoomLink(link string) (id, name, secret string) {
	link = strings.TrimSpace(link)
	frag := link
	if i := strings.LastIndex(link, "#"); i >= 0 {
		frag = link[i+1:]
	}
	if v, err := url.ParseQuery(frag); err == nil {
		if r := v.Get("r"); r != "" {
			return r, v.Get("n"), v.Get("k")
		}
	}
	if link != "" && !strings.ContainsAny(link, "/ #") {
		return link, "", "" // bare id
	}
	return "", "", ""
}

// eventPump bridges one room's peer.Hub events to (a) the SQLite store and
// (b) wails event emission to the frontend. One pump runs per live session;
// roomID is the booth this hub belongs to. Stops when the session's ctx is
// cancelled (leave / shutdown).
func (a *App) eventPump(ctx context.Context, roomID string, hub *peer.Hub) {
	for {
		var ev peer.HubEvent
		select {
		case <-ctx.Done():
			return
		case e, ok := <-hub.Events:
			if !ok {
				return
			}
			ev = e
		}
		switch ev.Kind {
		case peer.EventConnect:
			_ = a.store.UpsertPeer(a.ctx, ev.Peer)
			_ = a.store.SetPeerDisplay(a.ctx, ev.Peer, ev.Display)
			_ = a.store.SetPeerAvatar(a.ctx, ev.Peer, ev.Avatar)
			// A connecting peer becomes a member of THIS room so message/flip
			// fan-out reaches them, and we deliver anything parked while empty.
			_ = a.store.AddBoothMember(a.ctx, roomID, ev.Peer, ev.At)
			a.deliverQueuedFlips(hub, roomID, ev.Peer)
			wailsruntime.EventsEmit(a.ctx, "peer-state-changed", map[string]any{
				"peer":      ev.Peer,
				"connected": true,
				"detail":    ev.Text,
				"display":   ev.Display,
				"avatar":    ev.Avatar,
				"at":        ev.At.Format(time.RFC3339Nano),
			})
		case peer.EventDisconnect:
			// Drop their cached presence so the status map can't grow without
			// bound over the app's lifetime.
			a.statusMu.Lock()
			delete(a.peerStatus, ev.Peer)
			a.statusMu.Unlock()
			wailsruntime.EventsEmit(a.ctx, "peer-state-changed", map[string]any{
				"peer":      ev.Peer,
				"connected": false,
				"at":        ev.At.Format(time.RFC3339Nano),
			})
		case peer.EventMessage:
			at := ev.At
			boothID := ""
			uuid := ""
			parentUUID := ""
			backlog := false
			if md, ok := ev.Data.(*peer.MessageEventData); ok && md != nil {
				boothID = md.BoothID
				uuid = md.UUID
				parentUUID = md.ParentUUID
				backlog = md.Backlog
			}
			// A peer's connection belongs to exactly one room. Don't let a message
			// claim a *different* booth than the one it arrived on — otherwise a
			// peer you share one room with could inject chat into the history of
			// other rooms (even ones they're not a member of). "" stays "" (a 1:1
			// line riding a shared connection).
			if boothID != "" && boothID != roomID {
				boothID = roomID
			}
			// Dedupe by UUID: a message can arrive both live and via the offline
			// backlog, or twice from the mesh. If we already have it, skip.
			if uuid != "" {
				if _, err := a.store.FindMessageByUUID(a.ctx, uuid); err == nil {
					continue
				}
			}
			// Backlog messages we sent ourselves shouldn't reappear as inbound.
			if backlog && ev.Peer == a.hostname {
				continue
			}
			// Learn the sender's friendly name (covers backlog senders we never
			// got a connect event from).
			_ = a.store.SetPeerDisplay(a.ctx, ev.Peer, ev.Display)
			_ = a.store.AppendMessageFull(a.ctx, store.Message{
				UUID: uuid, Peer: ev.Peer, Direction: store.DirectionIn, Text: ev.Text, At: at, BoothID: boothID, ParentUUID: parentUUID,
			})
			wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
				UUID:        uuid,
				Peer:        ev.Peer,
				DisplayName: a.displayResolve(ev.Peer, ev.Display),
				Direction:   store.DirectionIn,
				Text:        ev.Text,
				At:          at.UTC().Format(time.RFC3339Nano),
				BoothID:     boothID,
				ParentUUID:  parentUUID,
			})
			// Twin relay: live messages only (not historical backlog replays).
			if !backlog {
				a.relayToTwin(peer.TwinSyncMessage{
					OriginalPeer: ev.Peer,
					Direction:    store.DirectionIn,
					Text:         ev.Text,
					At:           at,
					BoothID:      boothID,
				})
			}
		case peer.EventMessageReaction:
			r, _ := ev.Data.(*peer.MessageReaction)
			if r == nil {
				continue
			}
			if len(r.Emoji) == 0 || len(r.Emoji) > maxReactionEmojiBytes {
				continue
			}
			// Scope it: only react to a message we actually have, and only when it
			// belongs to the conversation this connection rides — otherwise a peer
			// who learns a UUID could react in rooms they aren't part of.
			if orig, ferr := a.store.FindMessageByUUID(a.ctx, r.MessageUUID); ferr != nil || !inSameConversation(orig, ev.Peer, roomID) {
				continue
			}
			if r.Action == "remove" {
				_ = a.store.RemoveReaction(a.ctx, r.MessageUUID, ev.Peer, r.Emoji)
			} else {
				_ = a.store.AddReaction(a.ctx, store.Reaction{MessageUUID: r.MessageUUID, Peer: ev.Peer, Emoji: r.Emoji, At: r.At})
			}
			wailsruntime.EventsEmit(a.ctx, "reaction", map[string]any{
				"uuid": r.MessageUUID, "emoji": r.Emoji, "peer": ev.Peer, "action": r.Action, "boothId": r.BoothID,
			})
		case peer.EventMessageEdit:
			e, _ := ev.Data.(*peer.MessageEdit)
			if e == nil {
				continue
			}
			// Sender check: the original message's peer column (we stored the
			// sender as Peer with direction=in) must match the connection's
			// peer. Otherwise reject — someone else trying to edit a message
			// they didn't author.
			orig, ferr := a.store.FindMessageByUUID(a.ctx, e.MessageUUID)
			if ferr != nil {
				continue
			}
			if orig.Direction != store.DirectionIn || orig.Peer != ev.Peer {
				continue
			}
			if applied, _ := a.store.ApplyMessageEdit(a.ctx, e.MessageUUID, e.Text, e.At); applied {
				wailsruntime.EventsEmit(a.ctx, "message-edited", map[string]any{
					"uuid": e.MessageUUID, "text": e.Text, "boothId": e.BoothID, "editedAt": e.At.UTC().Format(time.RFC3339Nano),
				})
			}
		case peer.EventMessageDelete:
			d, _ := ev.Data.(*peer.MessageDelete)
			if d == nil {
				continue
			}
			orig, ferr := a.store.FindMessageByUUID(a.ctx, d.MessageUUID)
			if ferr != nil {
				continue
			}
			if orig.Direction != store.DirectionIn || orig.Peer != ev.Peer {
				continue
			}
			if applied, _ := a.store.ApplyMessageDelete(a.ctx, d.MessageUUID, d.At); applied {
				wailsruntime.EventsEmit(a.ctx, "message-deleted", map[string]any{
					"uuid": d.MessageUUID, "boothId": d.BoothID, "deletedAt": d.At.UTC().Format(time.RFC3339Nano),
				})
			}
		case peer.EventMessagePin:
			p, _ := ev.Data.(*peer.MessagePin)
			if p == nil {
				continue
			}
			// Anyone in the conversation can pin — but it must BE in this
			// conversation: only a message we have that belongs to the room/peer
			// this connection rides (stops pinning across rooms via a guessed UUID).
			orig, ferr := a.store.FindMessageByUUID(a.ctx, p.MessageUUID)
			if ferr != nil || !inSameConversation(orig, ev.Peer, roomID) {
				continue
			}
			if err := a.store.SetMessagePinned(a.ctx, p.MessageUUID, p.Pinned); err == nil {
				wailsruntime.EventsEmit(a.ctx, "message-pinned", map[string]any{
					"uuid": p.MessageUUID, "pinned": p.Pinned, "boothId": p.BoothID,
				})
			}
		case peer.EventMessageCard:
			mc, _ := ev.Data.(*peer.MessageCard)
			if mc == nil || mc.MessageUUID == "" {
				continue
			}
			// Only the message's original sender may attach its card (same
			// authorship check as edits/deletes).
			orig, ferr := a.store.FindMessageByUUID(a.ctx, mc.MessageUUID)
			if ferr != nil || orig.Direction != store.DirectionIn || orig.Peer != ev.Peer {
				continue
			}
			card, ok := sanitizeLinkCard(mc.Card)
			if !ok {
				continue
			}
			mc.Card = card
			cardJSON, err := json.Marshal(mc.Card)
			if err != nil {
				continue
			}
			if ok, _ := a.store.SetMessageCard(a.ctx, mc.MessageUUID, string(cardJSON)); ok {
				wailsruntime.EventsEmit(a.ctx, "message-card", map[string]any{
					"uuid": mc.MessageUUID, "boothId": mc.BoothID, "card": mc.Card,
				})
			}
		case peer.EventPeerStatus:
			s, _ := ev.Data.(*peer.PeerStatus)
			if s == nil {
				continue
			}
			a.statusMu.Lock()
			if a.peerStatus == nil {
				a.peerStatus = map[string]string{}
			}
			a.peerStatus[ev.Peer] = s.Status
			a.statusMu.Unlock()
			wailsruntime.EventsEmit(a.ctx, "peer-status", map[string]any{
				"peer": ev.Peer, "status": s.Status, "at": s.At.UTC().Format(time.RFC3339Nano),
			})
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
			// Same scoping as inbound messages: a received flip belongs to the
			// room its connection rides, never a booth the sender names off the
			// wire — otherwise a peer could plant a file in another room's view.
			if fd.Direction == store.DirectionIn && fd.BoothID != "" && fd.BoothID != roomID {
				fd.BoothID = roomID
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
				BoothID:   fd.BoothID,
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
			if fd.Direction == store.DirectionIn && fd.BoothID != "" && fd.BoothID != roomID {
				fd.BoothID = roomID
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
		case peer.EventTwinSyncedMessage:
			ts, _ := ev.Data.(*peer.TwinSyncMessage)
			if ts == nil {
				continue
			}
			twin, _ := a.store.GetSetting(a.ctx, store.SettingTwinHostname)
			if twin == "" || twin != ev.Peer {
				continue
			}
			_ = a.store.AppendMessageBooth(a.ctx, ts.OriginalPeer, ts.Direction, ts.Text, ts.BoothID, ts.At)
			twinDisplays, _ := a.store.PeerDisplays(a.ctx)
			wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
				Peer:        ts.OriginalPeer,
				DisplayName: a.displayFor(ts.OriginalPeer, twinDisplays),
				Direction:   ts.Direction,
				Text:        ts.Text,
				At:          ts.At.UTC().Format(time.RFC3339Nano),
				BoothID:     ts.BoothID,
			})
		case peer.EventBoothInvited:
			inv, _ := ev.Data.(*peer.BoothInvite)
			if inv == nil {
				continue
			}
			// Drop invites from blocked peers silently.
			if a.store != nil {
				if blocked, _ := a.store.BlockedPeers(a.ctx); contains(blocked, ev.Peer) {
					continue
				}
			}
			// Don't let an invite rewrite a booth we already know: otherwise a
			// member could re-send an invite to rename your room, change its motto,
			// or swap the room key out from under you (locking you out of
			// decryption). We adopt the invite's name/founder/motto only the FIRST
			// time we learn of the booth; later invites can't mutate it.
			existing, exErr := a.store.GetBooth(a.ctx, inv.ID)
			boothKnown := exErr == nil
			name, founder, motto := inv.Name, inv.Founder, inv.Motto
			if boothKnown {
				name, founder, motto = existing.Name, existing.Founder, existing.Motto
			} else {
				_ = a.store.UpsertBooth(a.ctx, store.Booth{
					ID:        inv.ID,
					Name:      inv.Name,
					Founder:   inv.Founder,
					FoundedAt: inv.FoundedAt,
					Motto:     inv.Motto,
				})
			}
			// A P2P invite carries the room's E2E key, so accepting can join the
			// encrypted mesh without a separately-pasted link — but only adopt it if
			// we don't already hold a key for this room (a re-invite must not be
			// able to replace our key).
			if inv.Secret != "" {
				if cur, _ := a.store.GetSetting(a.ctx, roomSecretKey(inv.ID)); cur == "" {
					_ = a.store.SetSetting(a.ctx, roomSecretKey(inv.ID), inv.Secret)
				}
			}
			// Cap how many members a single invite can seed, so a peer can't pad
			// your roster with a flood of bogus ids.
			members := inv.Members
			if len(members) > maxInviteMembers {
				members = members[:maxInviteMembers]
			}
			for _, m := range members {
				_ = a.store.AddBoothMember(a.ctx, inv.ID, m, inv.FoundedAt)
			}
			// If we already know this room (live session or stored), it's not a new
			// invite — refresh only, no pending prompt.
			alreadyIn := a.sessionFor(inv.ID) != nil
			pending := false
			if !alreadyIn && !boothKnown {
				// Genuinely new: hold it for explicit accept/decline (consent).
				_ = a.store.SetSetting(a.ctx, roomPendingKey(inv.ID), "1")
				pending = true
			}
			wailsruntime.EventsEmit(a.ctx, "booth", BoothRecord{
				ID:        inv.ID,
				Name:      name,
				Founder:   founder,
				Motto:     motto,
				FoundedAt: inv.FoundedAt.UTC().Format(time.RFC3339Nano),
				Members:   append([]string{}, members...),
				Pending:   pending,
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
		BoothID:   fd.BoothID,
	}
	if !startedAt.IsZero() {
		rec.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)
	}
	if !completedAt.IsZero() {
		rec.CompletedAt = completedAt.UTC().Format(time.RFC3339Nano)
	}
	// Both directions get a preview URL: inbound serves the caught file,
	// outbound serves the sender's own original (local-only, so the sender
	// also sees a thumbnail of what they sent).
	if status == store.FlipStatusComplete {
		rec.CatchURL = "/catch/" + fd.ID
	}
	return rec
}

// ---------- bound methods (callable from JS) ----------

// Status returns the current readiness state plus our own identity.
func (a *App) Status() AppStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return AppStatus{State: a.state, Message: a.stateMsg, Self: a.self}
}

// displayName returns the user-chosen label, defaulting to the OS username so
// peers see something readable without any setup (the routing id is an ugly
// fingerprint and shouldn't be shown).
func (a *App) displayName() string {
	if a.store != nil {
		if dn, _ := a.store.GetSetting(a.ctx, store.SettingDisplayName); dn != "" {
			return dn
		}
	}
	// No name chosen yet. Use a neutral handle derived from this install's
	// stable identity — deliberately NOT the OS account name, which would leak
	// the user's real Windows/login name to peers and the signaling server.
	// The first-launch prompt asks the user to pick a real one.
	if id := a.identity.ID(); id != "" {
		if len(id) > 4 {
			id = id[:4]
		}
		return "guest-" + id
	}
	return "guest"
}

// SetDisplayName updates the user-chosen label (separate from the device's
// cryptographic identity) and refreshes the UI's self view.
func (a *App) SetDisplayName(name string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	if err := a.store.SetSetting(a.ctx, store.SettingDisplayName, name); err != nil {
		return err
	}
	a.eachHub(func(h *peer.Hub) { h.SetSelfDisplay(name) }) // peers pick it up on their next (re)connect
	a.mu.Lock()
	a.self.DisplayName = name
	a.mu.Unlock()
	if a.ctx != nil {
		wailsruntime.EventsEmit(a.ctx, "app-state", a.Status())
	}
	return nil
}

// ListPeers returns everyone we've ever chatted with (the store roster),
// merged with whoever we're connected to right now across all live rooms
// (the hubs).
func (a *App) ListPeers() ([]PeerInfo, error) {
	a.mu.RLock()
	state := a.state
	a.mu.RUnlock()
	if state != StateReady {
		return nil, fmt.Errorf("not ready: %s", state)
	}

	byName := map[string]*PeerInfo{}

	// Peers come from the live hub (currently connected in this room) plus the
	// store roster (people seen before).
	// Merge in store roster (peers we've ever talked to, even if offline now).
	if roster, err := a.store.Peers(a.ctx); err == nil {
		for _, r := range roster {
			if r.Name == a.hostname {
				continue
			}
			if existing, ok := byName[r.Name]; ok {
				existing.LastSeen = r.LastSeen.UTC().Format(time.RFC3339Nano)
				if existing.DisplayName == "" {
					existing.DisplayName = r.Display
				}
				if existing.Avatar == "" {
					existing.Avatar = r.Avatar
				}
			} else {
				byName[r.Name] = &PeerInfo{
					Name:        r.Name,
					DisplayName: r.Display,
					Avatar:      r.Avatar,
					LastSeen:    r.LastSeen.UTC().Format(time.RFC3339Nano),
				}
			}
		}
	}

	// Mark peers currently connected in ANY of our live rooms.
	connected := map[string]bool{}
	a.eachHub(func(h *peer.Hub) {
		for _, n := range h.Names() {
			connected[n] = true
		}
	})
	for name := range connected {
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
	displays, _ := a.store.PeerDisplays(a.ctx)
	out := make([]MessageRecord, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, a.toMessageRecord(m, displays))
	}
	a.attachReactions(out, msgs)
	return out, nil
}

// SendMessage delivers a 1:1 chat line to a peer we're connected to, stores
// it locally as an outbound row with a fresh UUID, and emits a message event.
func (a *App) SendMessage(peerName, text string) error {
	return a.SendReply(peerName, text, "")
}

// SendReply is SendMessage with an optional parent message UUID.
func (a *App) SendReply(peerName, text, parentUUID string) error {
	if !a.transportReady() {
		return fmt.Errorf("transport not ready")
	}
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return fmt.Errorf("empty message")
	}
	uuid, err := newUUID()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	hub := a.hubForPeer(peerName)
	if hub == nil {
		return fmt.Errorf("not connected to %s", peerName)
	}
	c := hub.Get(peerName)
	if c == nil {
		return fmt.Errorf("not connected to %s", peerName)
	}
	if err := c.WriteFrame(peer.TypeMessage, peer.Message{UUID: uuid, Text: text, At: now, ParentUUID: parentUUID}); err != nil {
		return err
	}
	if err := a.store.AppendMessageFull(a.ctx, store.Message{
		UUID: uuid, Peer: peerName, Direction: store.DirectionOut, Text: text, At: now, ParentUUID: parentUUID,
	}); err != nil {
		log.Printf("append out message: %v", err)
	}
	wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
		UUID: uuid, Peer: peerName, DisplayName: a.displayName(), Direction: store.DirectionOut, Text: text,
		At: now.Format(time.RFC3339Nano), ParentUUID: parentUUID,
	})
	a.relayToTwin(peer.TwinSyncMessage{
		OriginalPeer: peerName, Direction: store.DirectionOut, Text: text, At: now,
	})
	go a.enrichWithCard(uuid, text, peerName, "")
	return nil
}

// enrichWithCard unfurls the first link in a just-sent message (sender-side
// only), then persists the card, updates our own UI, and hands it to connected
// recipients via MESSAGE_CARD so their devices never touch the link. Runs in a
// goroutine so sending stays instant; a no-op if the text has no link or the
// unfurl yields nothing. peerName is set for 1:1, boothID for a room.
func (a *App) enrichWithCard(uuid, text, peerName, boothID string) {
	if !a.transportReady() {
		return
	}
	link := firstURL(text)
	if link == "" {
		return
	}
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	card := unfurlLink(ctx, link)
	if card == nil {
		return
	}
	if safe, ok := sanitizeLinkCard(*card); ok {
		card = &safe
	} else {
		return
	}
	if cardJSON, err := json.Marshal(card); err == nil {
		_, _ = a.store.SetMessageCard(a.ctx, uuid, string(cardJSON))
	}
	wailsruntime.EventsEmit(a.ctx, "message-card", map[string]any{
		"uuid": uuid, "boothId": boothID, "card": card,
	})
	mc := peer.MessageCard{MessageUUID: uuid, BoothID: boothID, Card: *card}
	if boothID != "" {
		if hub := a.roomHub(boothID); hub != nil {
			members, _ := a.store.BoothMembers(a.ctx, boothID)
			for _, m := range members {
				if m.PeerName == a.hostname {
					continue
				}
				if hub.Get(m.PeerName) != nil {
					_ = hub.SendMessageCard(m.PeerName, mc)
				}
			}
		}
	} else if peerName != "" {
		if hub := a.hubForPeer(peerName); hub != nil {
			_ = hub.SendMessageCard(peerName, mc)
		}
	}
}

// fanoutToConversation delivers a per-message control frame (edit/delete/
// reaction/pin) to the right peers: every connected member of a booth, or just
// the counterpart of a 1:1. send is invoked with the owning hub + target peer.
func (a *App) fanoutToConversation(boothID, otherPeer string, send func(h *peer.Hub, target string)) {
	if boothID != "" {
		hub := a.roomHub(boothID)
		if hub == nil {
			return
		}
		members, _ := a.store.BoothMembers(a.ctx, boothID)
		for _, m := range members {
			if m.PeerName == a.hostname {
				continue
			}
			if hub.Get(m.PeerName) != nil {
				send(hub, m.PeerName)
			}
		}
		return
	}
	if hub := a.hubForPeer(otherPeer); hub != nil {
		send(hub, otherPeer)
	}
}

// EditMessage replaces the text of one of our own previously-sent messages
// and broadcasts the edit to its recipients. Returns error if the message
// wasn't sent by us.
func (a *App) EditMessage(uuid, newText string) error {
	if !a.transportReady() {
		return fmt.Errorf("app not ready")
	}
	if uuid == "" || strings.TrimSpace(newText) == "" {
		return fmt.Errorf("uuid and non-empty text required")
	}
	orig, err := a.store.FindMessageByUUID(a.ctx, uuid)
	if err != nil {
		return fmt.Errorf("no such message")
	}
	if orig.Direction != store.DirectionOut {
		return fmt.Errorf("can only edit your own messages")
	}
	if !orig.DeletedAt.IsZero() {
		return fmt.Errorf("message already deleted")
	}
	now := time.Now().UTC()
	if _, err := a.store.ApplyMessageEdit(a.ctx, uuid, newText, now); err != nil {
		return err
	}
	edit := peer.MessageEdit{MessageUUID: uuid, Text: newText, BoothID: orig.BoothID, At: now}
	a.fanoutToConversation(orig.BoothID, orig.Peer, func(h *peer.Hub, target string) {
		_ = h.SendMessageEdit(target, edit)
	})
	wailsruntime.EventsEmit(a.ctx, "message-edited", map[string]any{
		"uuid": uuid, "text": newText, "boothId": orig.BoothID, "editedAt": now.Format(time.RFC3339Nano),
	})
	return nil
}

// DeleteMessage tombstones one of our own messages and broadcasts the delete.
func (a *App) DeleteMessage(uuid string) error {
	if !a.transportReady() {
		return fmt.Errorf("app not ready")
	}
	if uuid == "" {
		return fmt.Errorf("uuid required")
	}
	orig, err := a.store.FindMessageByUUID(a.ctx, uuid)
	if err != nil {
		return fmt.Errorf("no such message")
	}
	if orig.Direction != store.DirectionOut {
		return fmt.Errorf("can only delete your own messages")
	}
	now := time.Now().UTC()
	if _, err := a.store.ApplyMessageDelete(a.ctx, uuid, now); err != nil {
		return err
	}
	d := peer.MessageDelete{MessageUUID: uuid, BoothID: orig.BoothID, At: now}
	a.fanoutToConversation(orig.BoothID, orig.Peer, func(h *peer.Hub, target string) {
		_ = h.SendMessageDelete(target, d)
	})
	wailsruntime.EventsEmit(a.ctx, "message-deleted", map[string]any{
		"uuid": uuid, "boothId": orig.BoothID, "deletedAt": now.Format(time.RFC3339Nano),
	})
	return nil
}

// RemoveMessageLocally hard-deletes a message from THIS device only — any
// message, yours or theirs, received or sent. It does not notify anyone; the
// other person keeps their copy. For getting rid of something you don't want
// on your own machine.
func (a *App) RemoveMessageLocally(id int64) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	return a.store.DeleteMessageByID(a.ctx, id)
}

// RemoveFlipLocally removes a file transfer from THIS device. For files you
// RECEIVED, it also deletes the caught copy from your catch folder (you said
// you don't want it). For files you SENT or parked, it only forgets the record
// — your original file on disk is never touched.
func (a *App) RemoveFlipLocally(id string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	f, err := a.store.GetFlip(a.ctx, id)
	if err != nil {
		return fmt.Errorf("no such file")
	}
	if f.Direction == store.DirectionIn && f.Path != "" {
		if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			log.Printf("remove caught file %q: %v", f.Path, err)
		}
	}
	return a.store.DeleteFlip(a.ctx, id)
}

// ToggleReaction toggles an emoji reaction by the local user on a message.
// Returns the new state ("added" or "removed").
func (a *App) ToggleReaction(messageUUID, emoji string) (string, error) {
	if !a.transportReady() {
		return "", fmt.Errorf("app not ready")
	}
	if messageUUID == "" || emoji == "" {
		return "", fmt.Errorf("uuid + emoji required")
	}
	orig, err := a.store.FindMessageByUUID(a.ctx, messageUUID)
	if err != nil {
		return "", fmt.Errorf("no such message")
	}
	// Check existing
	existing, _ := a.store.ReactionsForMessages(a.ctx, []string{messageUUID})
	has := false
	for _, r := range existing[messageUUID] {
		if r.Peer == a.hostname && r.Emoji == emoji {
			has = true
			break
		}
	}
	now := time.Now().UTC()
	action := "add"
	if has {
		action = "remove"
		_ = a.store.RemoveReaction(a.ctx, messageUUID, a.hostname, emoji)
	} else {
		_ = a.store.AddReaction(a.ctx, store.Reaction{MessageUUID: messageUUID, Peer: a.hostname, Emoji: emoji, At: now})
	}
	r := peer.MessageReaction{MessageUUID: messageUUID, Emoji: emoji, Action: action, BoothID: orig.BoothID, At: now}
	a.fanoutToConversation(orig.BoothID, orig.Peer, func(h *peer.Hub, target string) {
		_ = h.SendMessageReaction(target, r)
	})
	wailsruntime.EventsEmit(a.ctx, "reaction", map[string]any{
		"uuid": messageUUID, "emoji": emoji, "peer": a.hostname, "action": action,
		"boothId": orig.BoothID,
	})
	return action, nil
}

// relayToTwin sends a 1:1 chat row to the paired twin (if one is set and
// currently connected). Errors are swallowed — twin sync is best-effort.
func (a *App) relayToTwin(m peer.TwinSyncMessage) {
	twin, _ := a.store.GetSetting(a.ctx, store.SettingTwinHostname)
	if twin == "" || twin == a.hostname {
		return
	}
	hub := a.hubForPeer(twin)
	if hub == nil {
		return
	}
	_ = hub.SendTwinSyncMessage(twin, m)
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
	if !a.transportReady() {
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
		if h := a.hubForPeer(m); h != nil {
			if err := h.SendBoothInvite(m, invite); err != nil {
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
	acts, _ := a.store.BoothLastActivity(a.ctx)
	out := make([]BoothRecord, 0, len(booths))
	for _, b := range booths {
		members, _ := a.store.BoothMembers(a.ctx, b.ID)
		names := make([]string, 0, len(members))
		for _, m := range members {
			names = append(names, m.PeerName)
		}
		pendingVal, _ := a.store.GetSetting(a.ctx, roomPendingKey(b.ID))
		rec := BoothRecord{
			ID:        b.ID,
			Name:      b.Name,
			Founder:   b.Founder,
			Motto:     b.Motto,
			FoundedAt: b.FoundedAt.UTC().Format(time.RFC3339Nano),
			Members:   names,
			Pending:   pendingVal == "1",
		}
		if t, ok := acts[b.ID]; ok {
			rec.LastActivity = t.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, rec)
	}
	return out, nil
}

// contains reports whether ss includes s.
func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
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
	displays, _ := a.store.PeerDisplays(a.ctx)
	out := make([]MessageRecord, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, a.toMessageRecord(m, displays))
	}
	a.attachReactions(out, msgs)
	return out, nil
}

// SendBoothMessage fans out a message to every connected member of the booth
// (other than us), persists it, and also stores it (encrypted) in the room's
// offline backlog so absent members receive it on reconnect. parentUUID is the
// message being replied to ("" for a normal message).
func (a *App) SendBoothMessage(boothID, text, parentUUID string) error {
	if !a.transportReady() {
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
	uuid, err := newUUID()
	if err != nil {
		return err
	}
	session := a.sessionFor(boothID)
	if session != nil {
		for _, m := range members {
			if m.PeerName == a.hostname {
				continue
			}
			if session.hub.Get(m.PeerName) == nil {
				continue
			}
			_ = session.hub.SendBooth(m.PeerName, boothID, text, uuid, parentUUID)
		}
	}
	// Persist with a UUID and push the (encrypted) message to the room's
	// offline backlog so members who join later still receive it.
	if err := a.store.AppendMessageFull(a.ctx, store.Message{
		UUID: uuid, Peer: a.hostname, Direction: store.DirectionOut, Text: text, BoothID: boothID, ParentUUID: parentUUID, At: now,
	}); err != nil {
		log.Printf("append booth out: %v", err)
	}
	if session != nil && session.room != nil {
		if err := session.hub.StoreMessage(a.ctx, session.room, a.hostname, a.displayName(), uuid, text, boothID, parentUUID, now); err != nil {
			log.Printf("backlog store: %v", err)
		}
	}
	wailsruntime.EventsEmit(a.ctx, "message", MessageRecord{
		UUID:        uuid,
		Peer:        a.hostname,
		DisplayName: a.displayName(),
		Direction:   store.DirectionOut,
		Text:        text,
		At:          now.Format(time.RFC3339Nano),
		BoothID:     boothID,
		ParentUUID:  parentUUID,
	})
	a.relayToTwin(peer.TwinSyncMessage{
		OriginalPeer: a.hostname,
		Direction:    store.DirectionOut,
		Text:         text,
		At:           now,
		BoothID:      boothID,
	})
	go a.enrichWithCard(uuid, text, "", boothID)
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

// ---------- pin / status / invite ----------

// PinMessage toggles the pin state on a message and broadcasts the change.
func (a *App) PinMessage(uuid string, pinned bool) error {
	if !a.transportReady() {
		return fmt.Errorf("app not ready")
	}
	if uuid == "" {
		return fmt.Errorf("uuid required")
	}
	orig, err := a.store.FindMessageByUUID(a.ctx, uuid)
	if err != nil {
		return fmt.Errorf("no such message")
	}
	if err := a.store.SetMessagePinned(a.ctx, uuid, pinned); err != nil {
		return err
	}
	pin := peer.MessagePin{MessageUUID: uuid, Pinned: pinned, BoothID: orig.BoothID, At: time.Now().UTC()}
	a.fanoutToConversation(orig.BoothID, orig.Peer, func(h *peer.Hub, target string) {
		_ = h.SendMessagePin(target, pin)
	})
	wailsruntime.EventsEmit(a.ctx, "message-pinned", map[string]any{
		"uuid": uuid, "pinned": pinned, "boothId": orig.BoothID,
	})
	return nil
}

// SetStatus broadcasts the local presence state to every connected peer.
// Status: "active" | "idle" | "away".
func (a *App) SetStatus(status string) error {
	if !a.transportReady() {
		return fmt.Errorf("transport not ready")
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "active" && status != "idle" && status != "away" {
		return fmt.Errorf("invalid status %q", status)
	}
	a.statusMu.Lock()
	if a.myStatus == status {
		a.statusMu.Unlock()
		return nil
	}
	a.myStatus = status
	a.statusMu.Unlock()
	ps := peer.PeerStatus{Status: status, At: time.Now().UTC()}
	a.eachHub(func(h *peer.Hub) { h.BroadcastPeerStatus(ps) })
	return nil
}

// PeerStatuses returns the last-known status for each connected peer (in-memory only).
func (a *App) PeerStatuses() (map[string]string, error) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	out := map[string]string{}
	for k, v := range a.peerStatus {
		out[k] = v
	}
	return out, nil
}

// BurnEverything permanently wipes the local data directory and quits the app.
// The caller must pass the literal phrase "burn everything" or the call is
// rejected. After this returns, all chat history, identity, caught files,
// and settings are gone — there is no undo.
func (a *App) BurnEverything(confirm string) error {
	if confirm != "burn everything" {
		return fmt.Errorf("confirmation phrase mismatch (type exactly: burn everything)")
	}
	// Tear down running services so the data dir's files are unlocked.
	a.closeAllSessions("burning")
	if a.store != nil {
		a.store.Close()
	}
	if err := os.RemoveAll(a.dataDir); err != nil {
		log.Printf("burn: removeAll %s: %v", a.dataDir, err)
	}
	wailsruntime.Quit(a.ctx)
	return nil
}

// ---------- search ----------

// SearchHitRecord is one full-text result surfaced to the frontend.
type SearchHitRecord struct {
	UUID      string `json:"uuid,omitempty"`
	Peer      string `json:"peer"`
	Direction string `json:"direction"`
	Text      string `json:"text"`
	Snippet   string `json:"snippet"` // contains <mark>...</mark> highlights
	At        string `json:"at"`
	BoothID   string `json:"boothId,omitempty"`
}

// SearchMessages returns FTS5-ranked matches for the query string.
// Pass a limit <= 0 for the default cap.
func (a *App) SearchMessages(query string, limit int) ([]SearchHitRecord, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	hits, err := a.store.SearchMessages(a.ctx, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHitRecord, 0, len(hits))
	for _, h := range hits {
		out = append(out, SearchHitRecord{
			UUID:      h.Message.UUID,
			Peer:      h.Message.Peer,
			Direction: h.Message.Direction,
			Text:      h.Message.Text,
			Snippet:   h.Snippet,
			At:        h.Message.At.UTC().Format(time.RFC3339Nano),
			BoothID:   h.Message.BoothID,
		})
	}
	return out, nil
}

// ---------- generic preferences (used by Backstage / tour / confetti) ----------

// GetPref returns the value for a settings key, or "" if unset.
func (a *App) GetPref(key string) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("store not ready")
	}
	return a.store.GetSetting(a.ctx, key)
}

// SetPref upserts a settings key.
func (a *App) SetPref(key, value string) error {
	if a.store == nil {
		return fmt.Errorf("store not ready")
	}
	return a.store.SetSetting(a.ctx, key, value)
}

// IsPeerNew returns true the first time it's asked about a given peer name,
// and false on every subsequent call. Used by the frontend to trigger a
// confetti moment exactly once per peer.
func (a *App) IsPeerNew(name string) (bool, error) {
	if a.store == nil {
		return false, fmt.Errorf("store not ready")
	}
	cur, err := a.store.GetSetting(a.ctx, store.SettingSeenPeers)
	if err != nil {
		return false, err
	}
	for _, p := range strings.Split(cur, ",") {
		if p == name {
			return false, nil
		}
	}
	seen := []string{}
	if cur != "" {
		seen = strings.Split(cur, ",")
	}
	seen = append(seen, name)
	// Cap the dedup list so it can't grow without bound over the app's lifetime
	// (it only drives a one-time confetti burst per peer).
	const maxSeenPeers = 1000
	if len(seen) > maxSeenPeers {
		seen = seen[len(seen)-maxSeenPeers:]
	}
	if err := a.store.SetSetting(a.ctx, store.SettingSeenPeers, strings.Join(seen, ",")); err != nil {
		return false, err
	}
	return true, nil
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

// Disconnect closes the active connection to a peer (sending BYE if possible).
func (a *App) Disconnect(peerName string) error {
	hub := a.hubForPeer(peerName)
	if hub == nil {
		return fmt.Errorf("not connected to %s", peerName)
	}
	c := hub.Get(peerName)
	if c == nil {
		return fmt.Errorf("not connected to %s", peerName)
	}
	_ = c.WriteFrame(peer.TypeBye, peer.Bye{Reason: "user disconnected"})
	c.Close()
	return nil
}

// SendFlip starts a file transfer to the named peer. Returns the new flip's id.
func (a *App) SendFlip(peerName, localPath string) (string, error) {
	if peerName == "" || localPath == "" {
		return "", fmt.Errorf("peer and path required")
	}
	hub := a.hubForPeer(peerName)
	if hub == nil {
		return "", fmt.Errorf("not connected to %s", peerName)
	}
	return hub.SendFlip(peerName, localPath)
}

// SendBoothFlip sends the same local file to every connected member of a
// booth (other than us). All members receive the file under the SAME flip id
// so the room has one consistent identity for it across every receiver.
// Returns the shared flip id.
func (a *App) SendBoothFlip(boothID, localPath string) (string, error) {
	if !a.transportReady() {
		return "", fmt.Errorf("app not ready")
	}
	members, err := a.store.BoothMembers(a.ctx, boothID)
	if err != nil {
		return "", err
	}
	hub := a.roomHub(boothID)
	log.Printf("flip: booth=%s path=%q members=%d live=%v", boothID, localPath, len(members), hub != nil)
	id, err := newBoothID() // UUIDv4 helper, reused
	if err != nil {
		return "", err
	}
	delivered := 0
	var firstErr error
	if hub != nil {
		for _, m := range members {
			if m.PeerName == a.hostname {
				continue
			}
			if hub.Get(m.PeerName) == nil {
				continue
			}
			if err := hub.SendFlipWithID(m.PeerName, localPath, id, boothID); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				delivered++
			}
		}
	}
	if delivered == 0 {
		if firstErr != nil {
			return id, firstErr
		}
		// No one's here yet. Park the file LOCALLY (keyed by room id) so it
		// survives a restart, shows the sender a preview, and sends to the
		// next person who joins. Nothing leaves this machine until then.
		name := filepath.Base(localPath)
		mime := mimeByPath(localPath)
		size := fileSize(localPath)
		now := time.Now().UTC()
		if err := a.store.AppendFlip(a.ctx, store.FlipRecord{
			ID: id, Peer: boothID, Direction: store.DirectionOut, Filename: name,
			Size: size, Mime: mime, Path: localPath, Status: store.FlipStatusQueued, StartedAt: now, BoothID: boothID,
		}); err != nil {
			return id, err
		}
		wailsruntime.EventsEmit(a.ctx, "flip", FlipRecord{
			ID: id, Peer: boothID, Direction: store.DirectionOut, Filename: name,
			Size: size, Mime: mime, Path: localPath, Status: store.FlipStatusQueued,
			StartedAt: now.Format(time.RFC3339Nano), BoothID: boothID, CatchURL: "/catch/" + id,
		})
		wailsruntime.EventsEmit(a.ctx, "notice", "Added "+name+" to the room — it'll send when someone joins.")
		return id, nil
	}
	return id, nil
}

// mimeByPath best-efforts a content type from extension, then sniffing.
func mimeByPath(path string) string {
	mt := mimepkg.TypeByExtension(filepath.Ext(path))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if mt == "" {
		if f, err := os.Open(path); err == nil {
			head := make([]byte, 512)
			if n, _ := f.Read(head); n > 0 {
				mt = http.DetectContentType(head[:n])
				if i := strings.IndexByte(mt, ';'); i >= 0 {
					mt = strings.TrimSpace(mt[:i])
				}
			}
			f.Close()
		}
	}
	return mt
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

// deliverQueuedFlips sends a room's locally-parked files (added while no one
// was present) to a peer that just joined. The transfer marks each one
// complete, so it's delivered to the first person who shows up.
func (a *App) deliverQueuedFlips(hub *peer.Hub, boothID, peerName string) {
	rows, err := a.store.FlipsByPeer(a.ctx, boothID) // queued flips are keyed by room id
	if err != nil {
		return
	}
	for _, r := range rows {
		if r.Status != store.FlipStatusQueued {
			continue
		}
		if err := hub.SendFlipWithID(peerName, r.Path, r.ID, boothID); err != nil {
			log.Printf("deliver queued flip %q to %s: %v", r.Path, peerName, err)
			continue
		}
		// It's on its way; it's no longer just-parked.
		_ = a.store.UpdateFlipStatus(a.ctx, r.ID, store.FlipStatusComplete, "", time.Now().UTC())
	}
}

// PickAndSendFlip pops the OS file picker (multi-select), then flips each
// chosen file to the peer. Returns the count sent, or "" if cancelled.
func (a *App) PickAndSendFlip(peerName string) (string, error) {
	if !a.transportReady() {
		return "", fmt.Errorf("transport not ready")
	}
	if peerName == "" {
		return "", fmt.Errorf("peer required")
	}
	paths, err := wailsruntime.OpenMultipleFilesDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Pick file(s) to flip",
	})
	if err != nil {
		return "", err
	}
	return a.sendMany(peerName, paths, false)
}

// PickAndSendBoothFlip pops the OS file picker (multi-select), then flips each
// chosen file to everyone in the room. The reliable way to send files in a
// room (works even where native drag-and-drop doesn't).
func (a *App) PickAndSendBoothFlip(boothID string) (string, error) {
	if !a.transportReady() {
		return "", fmt.Errorf("app not ready")
	}
	if boothID == "" {
		return "", fmt.Errorf("room required")
	}
	paths, err := wailsruntime.OpenMultipleFilesDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Pick file(s) to send to the room",
	})
	if err != nil {
		return "", err
	}
	return a.sendMany(boothID, paths, true)
}

// sendMany flips a batch of files, either to a single peer or fanned out to a
// room (booth). Reports how many started; surfaces the first error if all
// failed.
func (a *App) sendMany(target string, paths []string, booth bool) (string, error) {
	if len(paths) == 0 {
		return "", nil // cancelled
	}
	sent := 0
	var firstErr error
	for _, p := range paths {
		var err error
		if booth {
			_, err = a.SendBoothFlip(target, p)
		} else {
			_, err = a.SendFlip(target, p)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sent++
	}
	if sent == 0 && firstErr != nil {
		return "", firstErr
	}
	return fmt.Sprintf("%d", sent), nil
}

// SendPastedImage writes a clipboard image into the app's own data dir, then
// flips it like any other file — fanning out to a room (booth=true, target is
// the room id) or sending 1:1 (booth=false, target is a peer name). If no one
// else is present it parks like a drag-dropped file. dataB64 is the raw image
// bytes, base64 (StdEncoding). Returns the new flip id.
func (a *App) SendPastedImage(target string, booth bool, dataB64, mime string) (string, error) {
	if !a.transportReady() {
		return "", fmt.Errorf("transport not ready")
	}
	if strings.TrimSpace(target) == "" {
		return "", fmt.Errorf("open a room or peer first")
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return "", fmt.Errorf("bad image data: %w", err)
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("empty image")
	}
	dir := filepath.Join(a.dataDir, "pasted")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var rb [4]byte
	_, _ = cryptoRand.Read(rb[:])
	name := fmt.Sprintf("pasted-%s-%s%s", time.Now().Format("20060102-150405"),
		base64.RawURLEncoding.EncodeToString(rb[:]), extForMime(mime))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", err
	}
	if booth {
		return a.SendBoothFlip(target, path)
	}
	return a.SendFlip(target, path)
}

// extForMime maps a clipboard image MIME type to a file extension.
func extForMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ".png"
	}
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
		if r.BoothID != "" {
			continue // booth flips belong to their room, not this 1:1 view
		}
		out = append(out, flipRowToRecord(r))
	}
	return out, nil
}

func flipRowToRecord(r store.FlipRecord) FlipRecord {
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
		BoothID:   r.BoothID,
	}
	if !r.CompletedAt.IsZero() {
		rec.CompletedAt = r.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	if r.Status == store.FlipStatusComplete || r.Status == store.FlipStatusQueued {
		rec.CatchURL = "/catch/" + r.ID
	}
	return rec
}

// ListBoothFlips returns the flip history scoped to a room (its booth id),
// which keeps files from one room out of others that share a member.
func (a *App) ListBoothFlips(boothID string) ([]FlipRecord, error) {
	if a.store == nil {
		return nil, fmt.Errorf("store not ready")
	}
	rows, err := a.store.FlipsByBooth(a.ctx, boothID)
	if err != nil {
		return nil, err
	}
	out := make([]FlipRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, flipRowToRecord(r))
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
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", f.Path)
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
	if f.Status != store.FlipStatusComplete && f.Status != store.FlipStatusQueued {
		http.Error(w, "flip not ready", http.StatusNotFound)
		return
	}
	// Serves inbound (caught files), outbound (the sender's own originals), and
	// queued room files (also the sender's own originals). This endpoint is
	// only reachable from this app's own webview, never the network, so showing
	// the local user their own file is safe.
	if f.Mime != "" {
		w.Header().Set("Content-Type", f.Mime)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", f.Filename))
	http.ServeFile(w, r, f.Path)
}
