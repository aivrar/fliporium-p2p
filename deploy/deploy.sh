#!/usr/bin/env bash
# Deploy or update the Fliporium VPS install: site files, the .exe download,
# the flipstats backend, the Caddyfile, and the systemd unit.
#
# Run from the repository root with the VPS reachable via the keyed SSH
# config in $HOME/.ssh/config or directly via the user@host below. Reads
# credentials from .vps in the repo root (gitignored).
#
# Usage:
#   ./deploy/deploy.sh                # full deploy
#   ./deploy/deploy.sh --site-only    # skip rebuilding the binary
#
# Idempotent. Safe to re-run.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${REPO_ROOT}"

# shellcheck disable=SC1091
source ".vps"
SSH_TARGET="${VPS_USER}@${VPS_HOST}"
STAGE="/tmp/fliporium-deploy"

SITE_ONLY=0
for arg in "$@"; do
  case "$arg" in
    --site-only) SITE_ONLY=1 ;;
  esac
done

echo "==> building fliporium.exe (Windows GUI)"
# Done in PowerShell on the developer's box; this script assumes the binary
# is already present at ./fliporium.exe. We verify rather than rebuild.
if [[ ! -f fliporium.exe ]]; then
  echo "fliporium.exe is missing -- run .\\build.ps1 -Gui first" >&2
  exit 1
fi

echo "==> cross-building flipstats (linux/amd64)"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o flipstats-linux ./cmd/flipstats

echo "==> staging on VPS at ${STAGE}"
ssh -o StrictHostKeyChecking=no "${SSH_TARGET}" "rm -rf ${STAGE} && mkdir -p ${STAGE}/site"

echo "==> uploading site/"
scp -rq site/* "${SSH_TARGET}:${STAGE}/site/"

if [[ "$SITE_ONLY" -eq 0 ]]; then
  echo "==> uploading fliporium.exe"
  scp -q fliporium.exe "${SSH_TARGET}:${STAGE}/fliporium.exe"

  echo "==> uploading flipstats binary"
  scp -q flipstats-linux "${SSH_TARGET}:${STAGE}/flipstats"
fi

echo "==> uploading caddy + unit"
scp -q deploy/Caddyfile "${SSH_TARGET}:${STAGE}/Caddyfile.new"
scp -q deploy/flipstats.service "${SSH_TARGET}:${STAGE}/flipstats.service"
scp -q deploy/install.sh "${SSH_TARGET}:${STAGE}/install.sh"

echo "==> running install on VPS"
ssh "${SSH_TARGET}" "echo '${VPS_PASS}' | sudo -S bash ${STAGE}/install.sh"

rm -f flipstats-linux
echo "==> done"
