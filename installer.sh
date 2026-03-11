#!/usr/bin/env bash
# =============================================================================
# Printo Print Client — First-Time Installation Script
# =============================================================================
# Usage:
#   sudo GITHUB_TOKEN=ghp_xxx bash install.sh
#
# Options (env vars):
#   GITHUB_TOKEN   required — GitHub PAT with repo or public_repo scope
#   INSTALL_DIR    optional — default: /opt/printo
#   GITHUB_REPO    optional — default: hxn999/printo-print-client
#   SERVICE_USER   optional — default: $SUDO_USER, or 'pi' fallback
# =============================================================================

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────

GITHUB_REPO="${GITHUB_REPO:-hxn999/printo-print-client}"
INSTALL_DIR="${INSTALL_DIR:-/opt/printo}"
VERSIONS_DIR="${INSTALL_DIR}/versions"
CURRENT_LINK="${INSTALL_DIR}/current"
ENV_FILE="${INSTALL_DIR}/.env"
SERVICE_NAME="printo-updater"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# ── Validate ──────────────────────────────────────────────────────────────────

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  echo "ERROR: GITHUB_TOKEN env var is required."
  echo "  export GITHUB_TOKEN=ghp_yourtoken && sudo -E bash install.sh"
  exit 1
fi

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: This script must be run as root (use sudo)."
  exit 1
fi

if [[ -n "${SUDO_USER:-}" ]]; then
  SERVICE_USER="${SERVICE_USER:-$SUDO_USER}"
else
  SERVICE_USER="${SERVICE_USER:-pi}"
fi

# ── Require jq ───────────────────────────────────────────────────────────────

if ! command -v jq &>/dev/null; then
  echo "► jq not found — installing..."
  apt-get install -y -qq jq
fi

# ── Detect architecture ───────────────────────────────────────────────────────

detect_arch() {
  local machine
  machine="$(uname -m)"
  case "$machine" in
    x86_64)        echo "linux-amd64" ;;
    i386|i686)     echo "linux-386" ;;
    aarch64|arm64) echo "linux-arm64" ;;
    armv7*|armv6*) echo "linux-armv7" ;;
    *)
      echo "ERROR: Unsupported architecture: $machine" >&2
      exit 1
      ;;
  esac
}

ARCH="$(detect_arch)"
echo "► Detected architecture: ${ARCH}"

# ── Fetch release JSON once, extract everything from it ───────────────────────

echo "► Fetching latest release from github.com/${GITHUB_REPO}..."

RELEASE_JSON=$(curl -fsSL \
  -H "Authorization: Bearer ${GITHUB_TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "https://api.github.com/repos/${GITHUB_REPO}/releases/latest")

VERSION=$(echo "$RELEASE_JSON" | jq -r '.tag_name')

if [[ -z "$VERSION" || "$VERSION" == "null" ]]; then
  echo "ERROR: Could not determine latest release version."
  echo "       Check your GITHUB_TOKEN and repo name."
  exit 1
fi

echo "► Latest version: ${VERSION}"
echo ""
echo "► Available assets in this release:"
echo "$RELEASE_JSON" | jq -r '.assets[].name' | sed 's/^/    /'
echo ""

# Resolve download URLs directly from the API response — no name guessing.
# Finds the first asset whose name contains both the arch suffix and "client" or "updater".

get_asset_url() {
  local keyword="$1"   # "client" or "updater"
  local url
  url=$(echo "$RELEASE_JSON" | jq -r \
    --arg kw "$keyword" \
    --arg arch "$ARCH" \
    '.assets[] | select(.name | contains($kw) and contains($arch)) | .browser_download_url' \
    | head -1)
  echo "$url"
}

get_asset_name() {
  local keyword="$1"
  local name
  name=$(echo "$RELEASE_JSON" | jq -r \
    --arg kw "$keyword" \
    --arg arch "$ARCH" \
    '.assets[] | select(.name | contains($kw) and contains($arch)) | .name' \
    | head -1)
  echo "$name"
}

get_checksum_url() {
  local url
  url=$(echo "$RELEASE_JSON" | jq -r \
    '.assets[] | select(.name == "checksums.txt") | .browser_download_url' \
    | head -1)
  echo "$url"
}

CLIENT_URL="$(get_asset_url "client")"
CLIENT_NAME="$(get_asset_name "client")"
UPDATER_URL="$(get_asset_url "updater")"
UPDATER_NAME="$(get_asset_name "updater")"
CHECKSUM_URL="$(get_checksum_url)"

# Validate all three were found before touching disk.
MISSING=0
if [[ -z "$CLIENT_URL" ]]; then
  echo "ERROR: Could not find a 'client' asset for arch '${ARCH}' in this release."
  MISSING=1
fi
if [[ -z "$UPDATER_URL" ]]; then
  echo "ERROR: Could not find an 'updater' asset for arch '${ARCH}' in this release."
  MISSING=1
fi
if [[ -z "$CHECKSUM_URL" ]]; then
  echo "ERROR: Could not find checksums.txt in this release."
  MISSING=1
fi
if [[ $MISSING -eq 1 ]]; then
  echo ""
  echo "Assets found in release:"
  echo "$RELEASE_JSON" | jq -r '.assets[].name' | sed 's/^/  /'
  exit 1
fi

echo "► Resolved assets:"
echo "    client:   ${CLIENT_NAME}"
echo "    updater:  ${UPDATER_NAME}"
echo "    checksum: checksums.txt"
echo ""

# ── Create directory structure ────────────────────────────────────────────────

echo "► Creating install directories..."

VERSION_DIR="${VERSIONS_DIR}/${VERSION}"
mkdir -p "${VERSION_DIR}"
mkdir -p "${INSTALL_DIR}"
chown -R "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}"

# ── Download checksums ────────────────────────────────────────────────────────

echo "► Downloading checksums..."

CHECKSUM_FILE="${VERSION_DIR}/checksums.txt"
curl -fsSL \
  -H "Authorization: Bearer ${GITHUB_TOKEN}" \
  -H "Accept: application/octet-stream" \
  -L "${CHECKSUM_URL}" \
  -o "${CHECKSUM_FILE}"

# ── Download & verify helper ──────────────────────────────────────────────────

download_and_verify() {
  local url="$1"
  local asset_name="$2"   # original release asset filename — used for checksum lookup
  local dest="$3"         # where to save on disk

  echo "► Downloading ${asset_name}..."
  curl -fsSL \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -H "Accept: application/octet-stream" \
    -L "${url}" \
    -o "${dest}"

  echo "► Verifying checksum: ${asset_name}..."
  local expected
  expected=$(grep "${asset_name}" "${CHECKSUM_FILE}" | awk '{print $1}')

  if [[ -z "$expected" ]]; then
    echo "ERROR: No checksum entry found for '${asset_name}' in checksums.txt"
    echo "       Entries present:"
    cat "${CHECKSUM_FILE}" | sed 's/^/    /'
    rm -rf "${VERSION_DIR}"
    exit 1
  fi

  local actual
  actual=$(sha256sum "${dest}" | awk '{print $1}')

  if [[ "$actual" != "$expected" ]]; then
    echo "ERROR: Checksum mismatch for ${asset_name}!"
    echo "  expected: ${expected}"
    echo "  actual:   ${actual}"
    rm -rf "${VERSION_DIR}"
    exit 1
  fi

  echo "  ✓ checksum OK"
  chmod 755 "${dest}"
}

download_and_verify "${CLIENT_URL}"  "${CLIENT_NAME}"  "${VERSION_DIR}/printo-client"
download_and_verify "${UPDATER_URL}" "${UPDATER_NAME}" "${VERSION_DIR}/printo-updater"

# ── Atomic symlink swap ───────────────────────────────────────────────────────

echo "► Pointing current symlink to ${VERSION}..."

TMP_LINK="${CURRENT_LINK}.tmp"
ln -sfn "${VERSION_DIR}" "${TMP_LINK}"
mv -T "${TMP_LINK}" "${CURRENT_LINK}"

echo "  ✓ ${CURRENT_LINK} → ${VERSION_DIR}"

# ── Write env file (token stored securely, not in unit file) ─────────────────

echo "► Writing env file to ${ENV_FILE}..."

cat > "${ENV_FILE}" <<EOF
GITHUB_TOKEN=${GITHUB_TOKEN}
EOF

chmod 600 "${ENV_FILE}"
chown "${SERVICE_USER}:${SERVICE_USER}" "${ENV_FILE}"

echo "  ✓ ${ENV_FILE} (mode 600, owner ${SERVICE_USER})"

# ── Systemd service ───────────────────────────────────────────────────────────

echo "► Writing systemd service to ${SERVICE_FILE}..."

cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Printo Auto-Updater and Print Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
WorkingDirectory=${INSTALL_DIR}
EnvironmentFile=${ENV_FILE}
ExecStart=${CURRENT_LINK}/printo-updater
Restart=on-failure
RestartSec=10s
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF

# ── Enable & start ────────────────────────────────────────────────────────────

echo "► Enabling and starting ${SERVICE_NAME}..."

systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl restart "${SERVICE_NAME}"

sleep 2
systemctl status "${SERVICE_NAME}" --no-pager || true

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "════════════════════════════════════════════════════"
echo "  ✓ Printo installed successfully"
echo ""
echo "  Version:      ${VERSION}"
echo "  Arch:         ${ARCH}"
echo "  Install dir:  ${INSTALL_DIR}"
echo "  Current:      ${CURRENT_LINK} → ${VERSION_DIR}"
echo "  Env file:     ${ENV_FILE}"
echo "  Service:      ${SERVICE_NAME}"
echo ""
echo "  Useful commands:"
echo "    sudo systemctl status ${SERVICE_NAME}"
echo "    sudo journalctl -u ${SERVICE_NAME} -f"
echo "    sudo journalctl -u ${SERVICE_NAME} --since today"
echo "    sudo systemctl restart ${SERVICE_NAME}"
echo "    tail -f /var/log/printo/client.log"
echo "    tail -f /var/log/printo/updater.log"
echo "════════════════════════════════════════════════════"