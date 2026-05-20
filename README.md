# Fliporium

A portable Windows desktop app for small private groups, built on a self-hosted
Headscale tailnet. See `fliporium-brief.md` for the full vision.

## Status

| Phase | What | State |
| ----- | ---- | ----- |
| 1 | Headscale + Caddy on the GoDaddy VPS at `headscale.fliporium.com` | done |
| 2 | tsnet-backed Go binary joins the tailnet, persists identity | done |
| 3 | Terminal P2P chat (HELLO/MESSAGE/BYE over TLS-on-tsnet) | done |
| 4 | Wails desktop UI: The Floor, peer list, 1:1 chat, SQLite history, basic Markdown | done (alpha) |
| 5 | File flipping + Catch folder + inline image/video/audio/pdf/text viewers | done v0.1 (no resumability, no multi-file, no folder flips) |
| 6+ | Booths, Showtime, Workshop, Twin Mode, polish | not started |

## Layout

```
cmd/
  fliporium/        Wails desktop binary (Go + embedded frontend)
    main.go         entry point
    app.go          App struct + bindings exposed to JS
    frontend/dist/  index.html / main.js / style.css
    wails.json      Wails project config (mostly informational)
  fliporium-cli/    headless / REPL peer binary (kept around for scripted tests)
  probestore/       dev tool: dump a store.db's peers and messages
internal/
  peer/             wire protocol (proto.go) + Hub / dial / accept / TLS (peer.go)
  store/            SQLite-backed message + peer persistence
build.ps1           one-shot build of both binaries
run.ps1             one-shot launch of the GUI (or CLI with -Cli)
```

## Build

```powershell
.\build.ps1                # build both binaries
.\build.ps1 -Gui           # GUI only
.\build.ps1 -Cli           # CLI only
```

**Important:** the GUI binary must be built with Wails build tags. `build.ps1`
already does this. If you ever invoke `go build` by hand, use:

```powershell
go build -tags 'desktop,production' -ldflags '-H windowsgui -s -w' -o fliporium.exe ./cmd/fliporium
```

Otherwise the binary launches and immediately pops a Win32 error dialog
("Wails applications will not build without the correct build tags").

## Run

```powershell
.\run.ps1                                          # GUI: hostname=fliporium, data=.\fliporium-data-fliporium
.\run.ps1 -Hostname alice                          # custom tailnet hostname
.\run.ps1 -DataDir D:\flip                         # custom identity/data location
.\run.ps1 -Cli                                     # CLI REPL instead (in this window)
```

First launch needs a Headscale pre-auth key. `run.ps1` loads it from
`.preauth-test` for this dev session. Subsequent launches reuse the saved
tailnet identity in the data dir.

To rotate the pre-auth key:

```bash
# on the VPS
sudo headscale preauthkeys create --user 1 --reusable --expiration 24h
```

Drop the resulting `hskey-auth-...` value into `.preauth-test`.

## Verifying the chat works

In two PowerShell windows:

```powershell
# window 1: GUI
.\run.ps1 -Hostname flip-gui

# window 2: CLI peer that auto-connects and sends a message
$env:FLIPORIUM_AUTOPEER = 'flip-gui'
$env:FLIPORIUM_AUTOSAY  = 'hello **from the CLI**'
.\run.ps1 -Cli -Hostname flip-a
```

The GUI should show `flip-a` in The Floor; clicking on it shows the message
with Markdown rendering. Type a reply in the GUI; the CLI REPL prints it.

To inspect the SQLite store directly:

```powershell
go run .\cmd\probestore .\fliporium-data-flip-gui
```

## Architecture notes (Phase 4 state)

- Each Fliporium instance runs its own `tsnet.Server` in-process - no separate
  Tailscale install required.
- Peer connections are TCP on port 41642, wrapped in TLS (self-signed, peer
  identity is established by the underlying Tailscale/WireGuard layer; the
  TLS layer is defense in depth).
- Messages are length-prefixed JSON Envelopes with one of HELLO / MESSAGE / BYE.
- SQLite (`modernc.org/sqlite`, pure Go) persists peers and messages inside the
  data dir. Identity (`tailscaled.state`) also lives there.
- Frontend is plain HTML + CSS + a single `main.js`; no build chain. Markdown
  rendering is an in-house ~50-line escape-then-transform renderer.

## Flipping files (Phase 5)

Drag any file onto the Fliporium window while a peer is selected and connected,
or click the **flip** button to pick from a dialog. The file streams over the
peer connection (chunked, JSON-wrapped, ~64 KB per chunk), is sha256-verified
on arrival, and lands in `<dataDir>/catch/<peer>/<filename>`.

Inline viewers (auto-selected by MIME):

- images (jpg/png/gif/webp/svg/avif/...)
- video (mp4/webm/...)
- audio (mp3/wav/ogg/flac/...)
- PDF (via WebView2's native viewer)
- text/code (md/txt/json/go/py/...; truncated above 8 KB with "open" button)

Everything else gets a download card with an **open** button that hands the file
to the OS default application.

CLI equivalents (in `.\run.ps1 -Cli`):

```
flip <path>             # send to the only open peer
flip @<peer> <path>     # send to a specific peer
```

## Known caveats (will be tightened in later phases)

- Outbound message path from GUI is implemented but not yet visually verified.
- TLS cert validation is intentionally skipped (`InsecureSkipVerify`); peer
  identity is left to Tailscale. A future phase will pin Tailscale node keys.
- No retry on disconnect; if a peer drops, you have to click `connect` again.
- Single Booth (everyone is just "a peer"); Phase 6 introduces real Booths.
- Flip MVP limitations: no resumability, no multi-file flips, no folder flips,
  no pause/cancel. Big files load fully into memory per chunk because we wrap
  chunks in JSON+base64 — a future binary-framing change fixes that.
