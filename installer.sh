#!/usr/bin/env bash
# =============================================================================
# Printo Print Client — First-Time Installation Script
# =============================================================================
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/hxn999/printo-print-client/main/install.sh \
#     | sudo GITHUB_TOKEN=ghp_xxx bash
#
# Or download and run:
#   sudo GITHUB_TOKEN=ghp_xxx bash install.sh
#
# Options (env vars):
#   GITHUB_TOKEN   required — GitHub PAT with read:packages or public_repo scope
#   INSTALL_DIR    optional — default: /opt/printo
#   GITHUB_REPO    optional — default: hxn999/printo-print-client
#   SERVICE_USER   optional — default: current user or 'pi' if running as root
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
  echo "  export GITHUB_TOKEN=ghp_yourtoken"
  echo "  sudo -E bash install.sh"
  exit 1
fi

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: This script must be run as root (use sudo)."
  exit 1
fi

# Determine the real user to run the service as (not root).
if [[ -n "${SUDO_USER:-}" ]]; then
  SERVICE_USER="${SERVICE_USER:-$SUDO_USER}"
else
  # Fallback for headless/CI environments.
  SERVICE_USER="${SERVICE_USER:-pi}"
fi

# ── Detect architecture ───────────────────────────────────────────────────────

detect_arch() {
  local machine
  machine="$(uname -m)"
  case "$machine" in
    x86_64)  echo "linux-amd64" ;;
    i386|i686) echo "linux-386" ;;
    aarch64|arm64) echo "linux-arm64" ;;
    armv7*|armv6*)
      # Check for ARMv7 specifically
      if grep -q "ARMv7" /proc/cpuinfo 2>/dev/null; then
        echo "linux-armv7"
      else
        echo "linux-armv7"   # safe default for 32-bit ARM on Pi
      fi
      ;;
    *)
      echo "ERROR: Unsupported architecture: $machine" >&2
      exit 1
      ;;
  esac
}

ARCH="$(detect_arch)"
echo "► Detected architecture: ${ARCH}"

# ── Fetch latest release version from GitHub ─────────────────────────────────

echo "► Fetching latest release from github.com/${GITHUB_REPO}..."

RELEASE_JSON=$(curl -fsSL \
  -H "Authorization: Bearer ${GITHUB_TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "https://api.github.com/repos/${GITHUB_REPO}/releases/latest")

VERSION=$(echo "$RELEASE_JSON" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')

if [[ -z "$VERSION" ]]; then
  echo "ERROR: Could not determine latest release version."
  echo "       Check your GITHUB_TOKEN and repo name."
  exit 1
fi

echo "► Latest version: ${VERSION}"

BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
CLIENT_ASSET="printo-client-${VERSION}-${ARCH}"
UPDATER_ASSET="printo-updater-${VERSION}-${ARCH}"

# ── Create directory structure ────────────────────────────────────────────────

echo "► Creating install directories..."

VERSION_DIR="${VERSIONS_DIR}/${VERSION}"
mkdir -p "${VERSION_DIR}"
mkdir -p "${INSTALL_DIR}"

# Ownership: directories owned by SERVICE_USER so the updater can write to them.
chown -R "${SERVICE_USER}:${SERVICE_USER}" "${INSTALL_DIR}"

# ── Download checksums ────────────────────────────────────────────────────────

echo "► Downloading checksums..."

CHECKSUM_FILE="${VERSION_DIR}/checksums.txt"
curl -fsSL \
  -H "Authorization: Bearer ${GITHUB_TOKEN}" \
  -H "Accept: application/octet-stream" \
  -L "${BASE_URL}/checksums.txt" \
  -o "${CHECKSUM_FILE}"

# ── Download & verify binaries ────────────────────────────────────────────────

download_and_verify() {
  local asset="$1"
  local dest_name="$2"
  local dest="${VERSION_DIR}/${dest_name}"

  echo "► Downloading ${asset}..."
  curl -fsSL \
    -H "Authorization: Bearer ${GITHUB_TOKEN}" \
    -H "Accept: application/octet-stream" \
    -L "${BASE_URL}/${asset}" \
    -o "${dest}"

  echo "► Verifying checksum: ${asset}..."
  local expected
  expected=$(grep "${asset}" "${CHECKSUM_FILE}" | awk '{print $1}')

  if [[ -z "$expected" ]]; then
    echo "ERROR: No checksum entry found for ${asset}"
    rm -rf "${VERSION_DIR}"
    exit 1
  fi

  local actual
  actual=$(sha256sum "${dest}" | awk '{print $1}')

  if [[ "$actual" != "$expected" ]]; then
    echo "ERROR: Checksum mismatch for ${asset}!"
    echo "  expected: ${expected}"
    echo "  actual:   ${actual}"
    rm -rf "${VERSION_DIR}"
    exit 1
  fi

  echo "  ✓ checksum OK"
  chmod 755 "${dest}"
}

download_and_verify "${CLIENT_ASSET}"  "printo-client"
download_and_verify "${UPDATER_ASSET}" "printo-updater"

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

# Prevent the unit file world-readable env leaking
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
echo "    sudo systemctl restart ${SERVICE_NAME}"
echo "════════════════════════════════════════════════════"