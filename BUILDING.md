# Building Fliporium from source

Fliporium is a single Go binary with its HTML/CSS/JS frontend baked in via
`//go:embed`. There's **no Node.js, no JavaScript build step, no `wails` CLI, and
no C compiler** (the SQLite driver is pure Go). If you have Go and Git, you can
build the same `fliporium.exe` we publish on the
[Releases](https://github.com/aivrar/fliporium-p2p/releases) page.

## What you need

| Tool | Notes |
| --- | --- |
| **Windows 10 or 11** | The GUI build target. |
| **Go 1.26+** | <https://go.dev/dl/> — confirm with `go version`. |
| **Git** | <https://git-scm.com/> — to clone the repo. |
| **WebView2 runtime** | Already present on Windows 10/11 (Edge ships it). Needed at *run* time, not build time. |

That's the whole list — no Node, no `wails`, no MSVC.

## 1. Get the code

```powershell
git clone https://github.com/aivrar/fliporium-p2p.git
cd fliporium-p2p
```

## 2. Build the GUI

The easy way — the helper script sets the toolchain path and the required flags:

```powershell
.\build.ps1 -Gui
```

It writes `fliporium.exe` to the repo root and prints the size (~20 MB).

Prefer to build by hand? **The build tags and linker flags are mandatory:**

```powershell
go build -tags 'desktop,production' -ldflags '-H windowsgui -s -w' -o fliporium.exe ./cmd/fliporium
```

- `-tags 'desktop,production'` — selects the Wails v2 production runtime.
- `-ldflags '-H windowsgui'` — builds a GUI app with no console window.
- `-ldflags '-s -w'` — strips debug info to shrink the binary.

> ⚠️ **Don't run a bare `go build`.** Without the tags the app launches and
> immediately pops a Win32 **"Error"** dialog, then exits. Always use `build.ps1`
> or the full command above.

## 3. Run it

```powershell
.\run.ps1
```

…or just double-click `fliporium.exe`. On first launch it generates an Ed25519
identity and creates a `fliporium-data` folder next to the exe. There's no signup
— **create a room** or **paste an invite link** from inside the app.

Two peers on one machine for testing? Each data dir is an independent install
with its own identity:

```powershell
.\run.ps1 -Name alice   # uses fliporium-data-alice
.\run.ps1 -Name bob     # uses fliporium-data-bob
```

## 4. Verify your build (optional)

Every [Release](https://github.com/aivrar/fliporium-p2p/releases) lists the
SHA-256 of the official `fliporium.exe`. Compute yours and compare:

```powershell
Get-FileHash -Algorithm SHA256 .\fliporium.exe
```

Builds aren't guaranteed byte-identical across machines (Go version, file paths,
and timestamps can differ), so a mismatch isn't automatically a problem — but
**matching** hashes prove the bytes are identical to ours.

## Run your own server

By default the app uses the coordination server at `wss://fliporium.com/ws`.
There's no lock-in — point clients at your own:

```powershell
$env:FLIPORIUM_SIGNAL = "wss://your-host/ws"
.\fliporium.exe
```

For the full walkthrough — building `flipsignal`, a systemd unit, TLS with Caddy,
and optional TURN — see the
[Self-Hosting guide](https://github.com/aivrar/fliporium-p2p/wiki/Self-Hosting).

## Tests

```powershell
go test ./...
```

Covers the wire protocol and handshake (impersonation / auth-relay resistance),
offline-backlog authentication, the end-to-end encryption round-trip, the SQLite
store, and the link-unfurl guards.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| **"Error" dialog on launch** | You built without the Wails tags — rebuild with `build.ps1` or the tagged command in step 2. |
| **`go.exe not found`** | Install Go and reopen your terminal (or check `~/go-sdk/bin`). |
| **Blank / white window** | Install the [WebView2 runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (default on Win10/11). |
| **`go: updates to go.mod needed`** | Run `go mod download`, then build again. |

---

Project: [fliporium.com](https://fliporium.com) ·
Source: [github.com/aivrar/fliporium-p2p](https://github.com/aivrar/fliporium-p2p) ·
[Security policy](SECURITY.md)
