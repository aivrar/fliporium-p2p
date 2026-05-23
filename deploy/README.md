# Fliporium deployment

This directory holds everything needed to deploy `fliporium.com` (the website
+ download/contact backend) to the VPS that already hosts `headscale.fliporium.com`.

## What it deploys

- **site/** (built upstream in the repo) → `/var/www/fliporium/`
- **fliporium.exe** (built by `.\build.ps1 -Gui`) → `/var/www/fliporium/dl/fliporium.exe`
- **cmd/flipstats** → cross-compiled to linux/amd64 → `/usr/local/bin/flipstats`
- **deploy/Caddyfile** → `/etc/caddy/Caddyfile`
- **deploy/flipstats.service** → `/etc/systemd/system/flipstats.service`

## Layout on the VPS

```
/var/www/fliporium/
├── index.html, docs.html, privacy.html, stats.html
├── style.css, logo.svg, favicon.ico, icon-256.png, og.png
└── dl/
    └── fliporium.exe

/usr/local/bin/flipstats         # downloads + contact server binary
/var/lib/flipstats/downloads.count  # persistent download counter
/etc/systemd/system/flipstats.service
/etc/caddy/Caddyfile
```

## How to deploy

From the repo root:

```bash
./deploy/deploy.sh
```

That:
1. Verifies `fliporium.exe` is present (build it first with `.\build.ps1 -Gui` from PowerShell).
2. Cross-compiles `flipstats` for linux/amd64.
3. scp's everything to `/tmp/fliporium-deploy/` on the VPS.
4. Runs `install.sh` over there with `sudo -S`.
5. Reloads Caddy and restarts flipstats.

To redeploy only the site files (skip rebuilding binaries):

```bash
./deploy/deploy.sh --site-only
```

The deploy script reads `${repo}/.vps` (gitignored) for `VPS_HOST`, `VPS_USER`,
`VPS_PASS`.

## After DNS is set up

When `fliporium.com` and `www.fliporium.com` A-records point at the VPS,
Caddy will auto-acquire a real Let's Encrypt cert within ~30 seconds. No
config change needed — this `Caddyfile` already targets ACME by default.

If the VPS currently has `tls internal` in the Caddyfile (added during
pre-DNS testing), remove that one line and reload Caddy:

```bash
ssh aivrar@<vps> "sudo sed -i '/tls internal/d' /etc/caddy/Caddyfile && sudo systemctl reload caddy"
```
