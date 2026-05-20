#!/usr/bin/env bash
# Run by deploy.sh on the VPS as root. Expects /tmp/fliporium-deploy/ to be
# populated. Installs site files, binary, systemd unit, Caddyfile, then
# reloads everything.

set -euo pipefail

STAGE="/tmp/fliporium-deploy"
WEBROOT="/var/www/fliporium"
DL_DIR="${WEBROOT}/dl"
BIN_PATH="/usr/local/bin/flipstats"
DATA_DIR="/var/lib/flipstats"
UNIT_PATH="/etc/systemd/system/flipstats.service"
CADDYFILE="/etc/caddy/Caddyfile"

echo "==> verifying staging files"
for f in "${STAGE}/Caddyfile.new" "${STAGE}/flipstats.service" "${STAGE}/site/index.html"; do
  if [[ ! -f "$f" ]]; then
    echo "MISSING: $f" >&2
    exit 1
  fi
done

mkdir -p "${WEBROOT}" "${DL_DIR}" "${DATA_DIR}"

echo "==> syncing site/"
rsync -a --delete --exclude='dl/' "${STAGE}/site/" "${WEBROOT}/"

if [[ -f "${STAGE}/fliporium.exe" ]]; then
  echo "==> installing fliporium.exe"
  install -m 0644 "${STAGE}/fliporium.exe" "${DL_DIR}/fliporium.exe"
fi

if [[ -f "${STAGE}/flipstats" ]]; then
  echo "==> installing flipstats binary"
  install -m 0755 "${STAGE}/flipstats" "${BIN_PATH}"
fi

echo "==> installing systemd unit"
install -m 0644 "${STAGE}/flipstats.service" "${UNIT_PATH}"

echo "==> installing Caddyfile"
if [[ ! -f "${CADDYFILE}.before-fliporium" ]]; then
  cp "${CADDYFILE}" "${CADDYFILE}.before-fliporium"
fi
install -m 0644 "${STAGE}/Caddyfile.new" "${CADDYFILE}"

echo "==> validating Caddyfile"
if ! caddy validate --config "${CADDYFILE}" --adapter caddyfile 2>&1 | tee /tmp/caddy-validate.log | grep -q "Valid configuration"; then
  echo "Caddy config invalid -- restoring previous and aborting" >&2
  cp "${CADDYFILE}.before-fliporium" "${CADDYFILE}"
  cat /tmp/caddy-validate.log >&2
  exit 2
fi

echo "==> reloading services"
systemctl daemon-reload
systemctl enable flipstats >/dev/null
systemctl restart flipstats
sleep 1
systemctl reload caddy

echo "==> health checks"
sleep 1
if ! curl -fsS http://127.0.0.1:8088/healthz >/dev/null; then
  echo "flipstats /healthz failed" >&2
  systemctl status flipstats --no-pager | tail -20
  exit 3
fi

echo "==> stats now:"
curl -fsS http://127.0.0.1:8088/api/stats
echo
echo "==> deployment complete"
