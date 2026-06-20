#!/usr/bin/env bash
# Install the sddb-agent binary, create the system user, and register the
# systemd service. Run as root or with sudo on the agent host.
#
# Usage:
#   sudo bash install-agent.sh
#
# After installation, copy your cert files from the dashboard host:
#   /etc/sddb/agent.crt
#   /etc/sddb/agent.key
#   /etc/sddb/ca.crt
# Then: sudo systemctl enable --now sddb-agent

set -euo pipefail

REPO="smokinstack/sddb"
INSTALL_BIN="/usr/local/bin/sddb-agent"
CERT_DIR="/etc/sddb"
SERVICE_FILE="/etc/systemd/system/sddb-agent.service"
SERVICE_URL="https://raw.githubusercontent.com/${REPO}/main/deploy/sddb-agent.service"

# ── helpers ────────────────────────────────────────────────────────────────────

info()  { printf '\e[32m[+]\e[0m %s\n' "$*"; }
warn()  { printf '\e[33m[!]\e[0m %s\n' "$*"; }
die()   { printf '\e[31m[✗]\e[0m %s\n' "$*" >&2; exit 1; }

require() { command -v "$1" &>/dev/null || die "Required command not found: $1"; }

# ── preflight ──────────────────────────────────────────────────────────────────

[[ $EUID -eq 0 ]] || die "This script must be run as root (try: sudo bash $0)"

require curl
require tar
require systemctl

# ── detect platform ────────────────────────────────────────────────────────────

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "Unsupported architecture: $ARCH" ;;
esac

[[ "$OS" == "linux" ]] || die "This installer only supports Linux (detected: $OS)"

# ── resolve latest version ─────────────────────────────────────────────────────

info "Resolving latest release from GitHub..."
VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep '"tag_name"' | cut -d'"' -f4)
[[ -n "$VERSION" ]] || die "Could not determine latest version from GitHub API"
VERSION_NO_V="${VERSION#v}"
info "Latest version: ${VERSION}"

# ── download and install binary ────────────────────────────────────────────────

TARBALL="sddb-agent_${VERSION_NO_V}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${VERSION}/${TARBALL}"

info "Downloading ${TARBALL}..."
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

curl -fsSL "$DOWNLOAD_URL" | tar xz -C "$TMP_DIR"

[[ -f "$TMP_DIR/sddb-agent" ]] || die "Binary not found in archive — check release asset names"

install -m755 "$TMP_DIR/sddb-agent" "$INSTALL_BIN"
info "Installed binary: ${INSTALL_BIN}"

# ── create system user ─────────────────────────────────────────────────────────

if ! id -u sddb &>/dev/null; then
  useradd -r -s /sbin/nologin sddb
  info "Created system user: sddb"
else
  info "System user already exists: sddb"
fi

if getent group docker &>/dev/null; then
  usermod -aG docker sddb
  info "Added sddb to the docker group"
else
  warn "docker group not found — add sddb to the docker group manually before starting the agent"
fi

# ── create cert directory ──────────────────────────────────────────────────────

mkdir -p "$CERT_DIR"
chown sddb:sddb "$CERT_DIR"
chmod 750 "$CERT_DIR"
info "Cert directory ready: ${CERT_DIR}"

# ── install systemd unit ───────────────────────────────────────────────────────

info "Installing systemd service unit..."
curl -fsSL "$SERVICE_URL" | tee "$SERVICE_FILE" > /dev/null
systemctl daemon-reload
info "Installed service: ${SERVICE_FILE}"

# ── done ───────────────────────────────────────────────────────────────────────

printf '\n'
info "Installation complete."
printf '\n'
printf '  Next steps:\n'
printf '  1. Copy cert files from the dashboard host to this machine:\n'
printf '       /etc/sddb/agent.crt\n'
printf '       /etc/sddb/agent.key\n'
printf '       /etc/sddb/ca.crt\n'
printf '\n'
printf '     Set ownership and permissions:\n'
printf '       sudo chown sddb:sddb /etc/sddb/*\n'
printf '       sudo chmod 640 /etc/sddb/agent.crt /etc/sddb/ca.crt\n'
printf '       sudo chmod 600 /etc/sddb/agent.key\n'
printf '\n'
printf '  2. Enable and start the service:\n'
printf '       sudo systemctl enable --now sddb-agent\n'
printf '\n'
printf '  3. Add this agent in the dashboard UI (port 8484).\n'
printf '\n'
