# Fliporium — Project Brief

> A portable Windows desktop app where friends meet, chat, share anything,
> watch and listen together, and make things side by side — over their own
> private Headscale network. No central server, no accounts, no middleman.
> Just a place that belongs to you and the people in it.

---

## 1. The name

**Fliporium.** Part *flip*, part *-orium* (as in emporium, auditorium,
planetarium) — "the place where you flip things to people." A small,
playful, slightly carnival-flavored name for a small, personal, hand-built
space.

The name gives the app a vocabulary we lean into deliberately rather than
generic chat-app verbs:

- **Flip** — the act of sending. You don't *send* a file, you *flip* it.
- **Catch** — the act of receiving. Received files land in your **Catch
  folder**.
- **Booth** — a room or group conversation. Booths can be public to the
  tailnet or invite-only.
- **The Floor** — the main view, where you see who's around.
- **Backstage** — settings and admin.
- **The Marquee** — a small banner at the top showing what's happening
  right now (who joined, who's sharing their screen, who started a watch
  party).
- **Confetti** — a celebratory animation that fires on certain events
  (birthdays, milestones, when someone catches a flipped file the first
  time). Subtle, optional, off by default for users who hate that kind of
  thing.

Standard verbs ("send," "message," "share") still appear in the UI where
they're clearer. The custom vocabulary is flavor, not enforcement. A user
who never notices it is fine; a user who picks up on it gets a coherent
little world.

The domain is **fliporium.com**, owned by the user. Headscale lives at
**headscale.fliporium.com**. The fliporium.com root can host a small
landing page where invitees can download the app.

---

## 2. The idea in one paragraph

Fliporium is a portable Windows desktop app that turns a small group of
friends, family, or collaborators into a private digital space. Everyone
runs the same `.exe`. The app joins a Headscale-coordinated private
network and, once you're in, every other person running Fliporium appears
as a peer you can chat with, flip files to, watch videos with, listen to
music with, or build things alongside. There is no server in the middle
holding your messages or files — everything flows directly between peers
over an encrypted Tailscale tunnel. Your data stays in a folder next to
the app on your own machine. The app is meant to be the *best* version of
a private hangout space: chat is just the entry point.

---

## 3. Design philosophy

These principles guide every decision and override feature requests when
they conflict:

**Private by architecture.** No third-party service learns who is in your
Fliporium, what you say, or what you share. The only infrastructure is
your own VPS, and that VPS only helps peers find each other — it never
sees content.

**Yours to keep.** Every user's data — messages, files, settings — lives
in a folder beside the exe on their own machine. They can back it up,
move it, encrypt it, delete it. Nothing is held hostage.

**Portable in the literal sense.** Copy the app and its folder to a USB
stick, plug into another Windows machine, run, and you are the same
person with the same history. The app does not depend on the host
machine for anything.

**Delightful in small ways.** A million tiny moments of "oh, that's
nice." Animations that feel right. Sounds that aren't annoying. Defaults
that flatter the user. Easter eggs for the people who go looking.

**No surveillance, ever.** No telemetry. No crash reporting to a server.
No "anonymous usage statistics." If something goes wrong, logs stay on
the user's machine. If they want to send a log file to get help, they do
that consciously.

**Small and trusted, not big and public.** Fliporium is for groups who
know each other. It is not trying to be the next public chat platform.
The trust model assumes everyone in your tailnet is invited and welcome.

---

## 4. What Fliporium does — feature scope

This is the full, ambitious feature list. Phases later determine order;
this list is the whole vision.

### 4.1 Identity and presence

- One-time auth on first launch via Headscale (browser link OR pasted
  pre-auth key — user's choice).
- Display name, avatar (any image the user picks), and short status
  message ("📚 reading," "🎧 listening to records," "AFK," whatever).
- Status auto-detects: **active**, **idle** (no input for a few minutes),
  **away** (locked screen or longer idle), **invisible** (manual).
- Live presence on **The Floor**: see who's around, what they're listening
  to, what they're up to, all at a glance.
- **Right now** badges — small contextual hints: "in a watch party,"
  "sharing screen with Alex," "writing in the Notepad." Optional, per-user
  toggle.

### 4.2 Chat — text

- 1:1 conversations and **Booth** (group) conversations.
- Markdown rendering: bold, italic, links, code blocks, blockquotes, lists.
- LaTeX rendering for math, off by default per conversation, toggleable.
- Syntax-highlighted code blocks with language auto-detection.
- Typing indicators ("Alex is typing…").
- Read receipts, per-conversation toggle.
- Reply / quote any message inline.
- Threaded replies (optional per Booth — some Booths are flat, others
  threaded).
- Emoji reactions on any message (👍 ❤️ 😂 etc.) with a custom emoji
  picker.
- Custom emoji per Booth — drop a PNG, name it `:partyparrot:`, everyone
  in the Booth sees it.
- Edit and delete your own messages (peers honor the edit/delete if
  online; offline peers will sync on next connect).
- **Reactions-only mode** for messages you don't want to clutter with
  replies.
- Message pinning per conversation (pinned messages live in the Booth's
  header).
- Full-text search across all conversations and Booths, including inside
  file names and message attachments' text content where applicable.
- Slash commands: `/me`, `/shrug`, `/roll 2d6`, `/poll`, `/giphy`,
  `/clear`, `/topic`, `/remind`. Easy to extend.
- Polls inline in chat with live vote tally.
- Reminders: `/remind me in 2h to check the laundry` — local reminders
  that fire as system notifications.
- Link previews generated locally on the sender's side (so the URL is
  never leaked to a third party from the recipients).
- Image paste from clipboard, screenshot paste, draw-and-paste (sketch
  inside the message composer with a small drawing tool).
- Voice messages (record, send, peer plays inline). Pure audio first;
  this is light. Real voice *calls* are in §4.6.

### 4.3 Chat — flair

These are the "moments of delight" features that lift the app beyond a
standard chat experience.

- **Confetti** when someone says "happy birthday" to the birthday person,
  or hits a milestone (1000th message in a Booth, etc.).
- **Whisper mode** — temporarily lower the visual prominence of a
  conversation; messages appear smaller, quieter, like notes passed under
  the table.
- **Shout mode** — for an important message, send it as a Shout: it
  appears larger, with a distinct color, and pings everyone in the Booth
  even if they have notifications off (within reason — Shouts have a
  daily cap to prevent abuse).
- **Slow mode** per Booth — message frequency limit to keep conversations
  from going feral.
- **Quiet hours** per user — no notifications during configured hours,
  globally.
- **Re-flip** — forward a message or file to another conversation with
  attribution ("Alex re-flipped from Booth: Movie Night").
- **Time travel** — scrubber at the top of any conversation that lets you
  jump to any date instantly, with a calendar overlay showing days that
  had activity.
- **Memory lane** — once a year, on the anniversary of a Booth's creation
  or a friendship, Fliporium offers a "what happened a year ago today"
  view. Off by default, opt-in.

### 4.4 File sharing — Flipping and Catching

- **Drag and drop** any file or folder into a conversation to flip it.
- **Click-to-attach** with a file picker.
- **Paste-to-flip** — copy a file in Explorer, paste in Fliporium.
- **Multi-file flips** — drop 50 files at once, they go as a single
  flip with a folder-like card the recipient can expand.
- **Folder flips** — drop a folder, the structure is preserved on
  catching.
- **Chunked, resumable transfers** — pause, disconnect, resume from
  where left off, including across sessions.
- **No size cap** from the app. The limits are bandwidth and patience.
- **Catch folder** — default `Downloads/Fliporium/<peer name>/`,
  configurable per peer or per Booth.
- **Auto-catch** toggle per peer ("always accept files from Mom").
- **Quarantine for unknown peers** — if someone you've just met on the
  tailnet flips you something, it sits in a quarantine list until you
  explicitly accept.
- **Flip history** — searchable log of every file ever flipped or
  caught, with the conversation it came from.
- **Re-catch** — if you deleted a file and want it back, ask the peer's
  app for it; if they still have it, it re-flips.
- **Bundles** — pre-package a set of files as a named "Bundle"
  (`Holiday Photos 2025`) you can flip as a unit. Recipients see it as a
  single named bundle, not a wall of files.

### 4.5 Media viewing — inline, in-app, no leaving the conversation

Everything common renders right where it was flipped. No "download to
view." For uncommon formats, a clean "Save and open externally" fallback.

- **Images** — jpg, jpeg, png, gif, webp, bmp, svg, heic, avif. Zoom,
  pan, slideshow mode through all images in a conversation.
- **Animated gifs and webp** — play inline, hover-to-pause.
- **Video** — mp4, webm, mkv (where WebView2 supports the codec), with
  scrubber, picture-in-picture, playback speed.
- **Audio** — mp3, wav, ogg, m4a, flac, with waveform display.
- **PDF** — full PDF.js viewer with page navigation, search within the
  PDF, text selection.
- **Text and code** — any plain-text file, syntax-highlighted by
  extension, line numbers, copy-to-clipboard.
- **Markdown** — rendered.
- **Office documents** — docx, xlsx, pptx — converted to read-only PDF
  on the fly using a small bundled converter, viewable inline. Full
  editing happens externally.
- **Archives** — zip, tar, 7z, rar — show contents inline, extract
  selected files on demand.
- **3D models** — stl, obj, glb — rotate-and-zoom viewer.
- **Geolocation** — gpx, kml — show route on an embedded map (offline
  tile cache if user enables).
- **EPUB / ebooks** — read inline with a basic reader.
- **Subtitles** — if a video is flipped alongside a `.srt`, the player
  picks it up.
- **Fallback** — anything else gets a "Save and Open" button that drops
  to the OS default handler.

### 4.6 Watching and listening together — Showtime

Fliporium isn't just async chat. It has a synchronous-together side.

- **Watch Parties** — start a watch party with a flipped video or a
  shared link (YouTube, Vimeo, etc.). Everyone in the Booth gets a
  synchronized player. Pause, play, scrub — it syncs for everyone.
  Side-chat overlay.
- **Listening Rooms** — same idea for audio. Drop an album or playlist,
  everyone listens synchronously. Visual waveform and "now playing"
  card.
- **Karaoke mode** — if a video has timed lyrics or a separate `.lrc`
  is flipped along with the audio, lyrics scroll in sync.
- **Voice rooms** — drop into a voice channel in a Booth (think Discord
  voice). Peer-to-peer audio mesh up to a comfortable number of
  participants; hub mode for more.
- **Video calls** — 1:1 first; small group calls (up to ~6) later.
- **Screen sharing** — share a window or full screen with a peer or
  Booth. Optional remote pointer ("Alex is pointing here").
- **Watch-along reactions** — emoji reactions appear floating over the
  video as people react in real time, like Twitch chat but tasteful.

### 4.7 Making things together — The Workshop

The collaboration side. Fliporium isn't only for hangouts; it's also for
small creative work between friends.

- **Shared Notepad** per Booth — a persistent collaborative text document
  that everyone in the Booth can edit. Real-time cursors. Markdown.
  Saves automatically. Versioned (Ctrl+Z works across users).
- **Shared Whiteboard** per Booth — an infinite canvas with pens,
  shapes, sticky notes, image drops. Real-time collaborative.
- **Shared Code Pad** — a syntax-highlighted code editor with live
  collaboration. Save to a file, flip the file when done. Good for
  pair-programming-lite.
- **Shared Playlist** — a queue of audio/video that anyone in the Booth
  can add to, used by Listening Rooms and Watch Parties.
- **Shared Photo Album** — drop photos into a Booth's album; they're
  organized by date with optional captions everyone can add.
- **Polls and decisions** — `/poll` or a dedicated "Decide" tool for
  group decisions, with optional anonymous voting.
- **Lists** — shared to-do lists, shopping lists, grocery lists per
  Booth. Tick items off, see who ticked them.
- **Calendar** — a Booth-shared calendar for events, with the Booth
  members invited. Each event has its own thread.
- **Mini-games** — built-in tic-tac-toe, connect-four, chess, checkers,
  a shared sketch-guessing game, a trivia game. Light, fun, optional.

### 4.8 Booths — rooms with character

Booths are more than just group chats. Each Booth has its own personality.

- **Booth themes** — color scheme, background pattern, accent. Set by
  the Booth founder; can be overridden personally.
- **Booth roles** — Owner, Regular, Guest. Permissions are coarse:
  Owners can change settings, Regulars can do everything except admin,
  Guests are read-mostly. Roles are conventions, not security — the
  trust model is "everyone here was invited."
- **Booth-specific features toggle** — each Booth's owner decides which
  collaboration tools are enabled. A "Movie Night" Booth probably only
  needs chat and Watch Parties. A "Project X" Booth might enable the
  whole Workshop.
- **Booth memory** — every Booth has a small "About" page with its
  history, who founded it, key dates, pinned files. Like a wiki page
  for the Booth itself.
- **Founder's keepsake** — when a Booth is created, the founder picks a
  small motto or quote that appears under the Booth name. Pure flair.

### 4.9 The Floor — your main view

The Floor is what you see when Fliporium is open and you're not deep
in a conversation. It's a dashboard that feels alive.

- **People** — every peer on your tailnet, with their status, what
  they're listening to (if shared), and a tiny activity indicator.
  Hover to see more; click to start a 1:1.
- **Booths** — every Booth you're in, with a preview of the latest
  activity.
- **The Marquee** — a small ticker at the top: who joined, who started
  a watch party, who flipped you something, who reacted to your message.
- **Pinned conversations** — your favorites, always at the top.
- **Quick flip** — a search bar that doubles as a peer-finder; type a
  name, hit Enter, you're in their chat. Type a file path, Enter, it
  flips.

### 4.10 Backstage — settings, with personality

Settings live in **Backstage**. Grouped by purpose:

- **You** — display name, avatar, status, accent color, sounds.
- **The Tailnet** — Headscale server, identity, pre-auth flow.
- **Notifications** — per Booth, per peer, quiet hours, do-not-disturb
  toggle.
- **Storage** — Catch folder location, history retention rules,
  database encryption (SQLCipher, off by default; turning it on
  prompts for a passphrase).
- **Privacy** — read receipts, typing indicators, "Right now" badges,
  presence visibility.
- **Themes** — light, dark, a few hand-designed alternates (e.g.
  "Boardwalk," "Lounge," "Library").
- **Sounds** — chimes for messages, files, joins. Each one tasteful
  and short. All toggleable.
- **Easter eggs** — there are some. Backstage lists how many you've
  found out of the total.

### 4.11 Cross-device for one user — Twin Mode

If you run Fliporium on more than one of your own machines (laptop +
desktop, say), you can pair them as Twins. Twins:

- Share a synchronized message history (each device keeps its own
  copy; they reconcile when both are online).
- Hand off conversations — start typing on laptop, finish on desktop.
- See unread state in sync.
- Send files to your own other device with one click (the "send to
  myself" use case).

Twin Mode is opt-in and per-pair. The simple alternative for users who
don't want this complexity: each machine is its own independent install.

### 4.12 Onboarding — getting a friend in

- **QR code invite** — generate a one-time QR containing a Headscale
  pre-auth key and a download link. Friend scans, downloads, runs,
  joins.
- **Invite link** — short URL on `fliporium.com` that lands the friend
  on a page with the exe download and the auth flow.
- **In-app invite generator** — Backstage → Invite a Friend produces
  the QR and the link, with optional expiration and use-count limits.
- **First-launch tour** — three friendly screens explaining the
  vocabulary (Flip, Catch, Booth) and the privacy model. Skippable.

### 4.13 Export, backup, and exit

- **Export a conversation** to Markdown, JSON, or HTML.
- **Export everything** — full data folder as a single zip.
- **Backup to USB** — one-click "back up to drive" that writes the
  whole portable bundle to a chosen location.
- **Leave a Booth** — clean exit, your messages remain in others'
  history (their copies), your local copy of the Booth is archived
  rather than deleted unless you explicitly purge.
- **Burn everything** — a destructive option that wipes your local
  data. Confirms three times. Useful for ending a chapter.

---

## 5. Architecture

### Network layer

- **Headscale** on the user's GoDaddy VPS at `headscale.fliporium.com`.
  Coordination only. No content passes through it.
- **Tailscale** embedded in the app via `tsnet`. Each Fliporium instance
  joins the tailnet directly — no external Tailscale install required.
- After first-launch auth, identity is stored locally and reused.

### Application layer

- **Go backend** using `tsnet` for networking, SQLite (via `mattn/go-sqlite3`
  or `modernc.org/sqlite`) for persistence, and a local WebSocket bridge
  to the UI.
- **Wails v2** to wrap the Go backend with a native Windows window
  backed by WebView2 (installed by default on modern Windows).
- **Web UI** (HTML/CSS/JS, framework TBD — likely Svelte or plain
  modern JS for speed; framework choice is a Phase 4 decision).
- **Single-file build**: all UI assets, fonts, sounds, and the SQLite
  driver are embedded in the exe via Go's `embed`. The exe + a data
  folder is the entire footprint.

### Peer protocol

- TLS-wrapped TCP (or QUIC, decided in Phase 3) over the Tailscale
  network, using each peer's Tailscale IP and a fixed port.
- Message framing: length-prefixed JSON for control messages, binary
  framed chunks for file transfers.
- Sub-protocols: `chat`, `presence`, `file`, `media-sync` (Watch
  Parties), `voice`, `screen`, `notepad`, `whiteboard`. Each has its
  own little spec, and the framing layer multiplexes them on one
  connection.
- All authenticated by Tailscale identity — every connection's peer is
  known by its tailnet node identity, which is cryptographically
  verified by WireGuard underneath.

### Data layer

- One SQLite database per Fliporium install, in
  `./fliporium-data/store.db` (relative to the exe).
- Catch folder defaults to `./fliporium-data/catch/<peer>/` but is
  user-configurable.
- All paths relative to the exe location, never absolute, so the whole
  thing is portable.
- Optional SQLCipher encryption with a user passphrase, off by default.
- Full-text search indexes built with SQLite FTS5.

---

## 6. Server-side setup

One-time setup on the GoDaddy VPS:

1. DNS: A-record `headscale.fliporium.com` → VPS public IP.
2. SSH to VPS. Install Caddy and Headscale.
3. Configure Headscale: `server_url: https://headscale.fliporium.com`,
   tailnet name, SQLite or PostgreSQL backend.
4. Configure Caddy as a reverse proxy on port 443; it handles
   Let's Encrypt automatically.
5. Create the initial Headscale user (the namespace).
6. Generate pre-auth keys, used by Fliporium for first-launch
   registration of each new device.
7. Run Headscale as a systemd service.

Optional second phase:

- Run a private DERP relay on the same VPS for the rare cases when
  direct connections can't be established. Improves resilience without
  adding any company-controlled relay to the path.

---

## 7. Build phases

Each phase ends with something runnable and testable. The user works
through phases with Claude Code, confirming each fits the vision before
moving on. Complexity, time, and difficulty are not constraints — what
matters is everything fits together and works.

### Phase 1 — Headscale server up

- Headscale at `headscale.fliporium.com`, HTTPS via Caddy.
- A stock Tailscale Windows client can join the tailnet with a pre-auth
  key.
- Two machines on the tailnet can ping each other and resolve each
  other's MagicDNS name.

### Phase 2 — Minimal Go binary joins the tailnet

- `tsnet`-based Go program that registers with Headscale, prints its
  Tailscale IP, and lists peers.
- Persists node identity in `./fliporium-data/`.

### Phase 3 — Terminal-mode peer-to-peer chat

- Two binaries can exchange text messages over Tailscale-routed TCP
  with TLS.
- Simple sub-protocol with `HELLO`, `MESSAGE`, `BYE`.

### Phase 4 — Wails wrapper and core UI

- Wails app shell.
- The Floor view, peer list, 1:1 chat with message persistence in
  SQLite, basic Markdown rendering.

### Phase 5 — File flipping and catching, with viewers

- Chunked resumable transfers, drag-and-drop, multi-file flips,
  Catch folder.
- Inline viewers for images, video, audio, PDF, text/code.
- "Save and open externally" fallback for everything else.

### Phase 6 — Booths and group features

- Group conversations with mesh networking for small groups, hub mode
  for larger ones.
- Booth creation, roles, themes, custom emoji.
- Reactions, replies, threading, pinning, search.

### Phase 7 — Showtime (watching/listening together)

- Watch Parties with synchronized playback.
- Listening Rooms.
- Voice rooms (audio first, video later).

### Phase 8 — The Workshop (collaboration tools)

- Shared Notepad with real-time editing (CRDT-based, likely Y.js or
  Automerge).
- Shared Whiteboard.
- Shared Playlists, Photo Albums, Lists, Calendar.
- Mini-games.

### Phase 9 — Twin Mode and cross-device polish

- Multi-device sync for a single user's history across their own
  machines.

### Phase 10 — Polish, portability, and the moments of delight

- Themes, sounds, animations, confetti, easter eggs.
- QR-code invites and the fliporium.com landing page.
- First-launch tour.
- Final pass on the portable build: exe + data folder, runs from USB,
  no traces left elsewhere on the host machine.

---

## 8. What this is, in one sentence

Fliporium is the chat app that turns a small private network into a
place — somewhere you and your people meet, talk, share, watch, listen,
and make things together, with nothing in the middle and nothing held
hostage.
