#!/usr/bin/env bash
#
# her-go VPS bootstrap script
#
# Sets up a fresh Linux server (tested on Hetzner CAX11, Ubuntu/Debian ARM64)
# with everything needed to run her-go as a systemd service.
#
# Usage:
#   curl -sL https://raw.githubusercontent.com/AutumnsGrove/her-go/main/scripts/setup-server.sh | bash
#
# Or clone first and run locally:
#   git clone https://github.com/AutumnsGrove/her-go.git /opt/her-go
#   bash /opt/her-go/scripts/setup-server.sh
#
# What this installs:
#   - Swap (2GB) + UFW firewall
#   - Caddy (reverse proxy with auto-TLS)
#   - Go (latest stable, ARM64 or AMD64)
#   - Python 3 + uv (for TTS sidecar)
#   - Ollama + nomic-embed-text (embedding model)
#   - Piper TTS voice model
#   - her-go binary + systemd service
#   - Auto-update timer (checks main every 5 minutes)
#
# Optional env vars:
#   HER_DOMAIN  — domain name for Caddy TLS (e.g. her.yourdomain.com)
#
# Memory budget (4GB VPS):
#   OS ~300MB + Go binary ~50MB + Piper ~80MB + Ollama ~500MB + Python ~100MB
#   Total ~1.2GB — comfortable headroom on a 4GB plan. 2GB swap as safety net.

set -euo pipefail

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; exit 1; }

# --- Preflight checks ---

if [[ $EUID -ne 0 ]]; then
    fail "This script must be run as root (use sudo)"
fi

OS=$(uname -s)
ARCH=$(uname -m)

if [[ "$OS" != "Linux" ]]; then
    fail "This script is for Linux servers. Got: $OS"
fi

case "$ARCH" in
    aarch64|arm64) GO_ARCH="arm64" ;;
    x86_64)        GO_ARCH="amd64" ;;
    *)             fail "Unsupported architecture: $ARCH" ;;
esac

info "Platform: Linux $ARCH (Go arch: $GO_ARCH)"

# --- Configuration ---

REPO_URL="https://github.com/AutumnsGrove/her-go.git"
INSTALL_DIR="/opt/her-go"
SERVICE_USER="hergo"
GO_VERSION="1.25.4"
PIPER_VOICE="en_GB-southern_english_female-low"
PIPER_QUALITY="low"

# --- Step 0a: Swap file ---

info "[0a] Configuring swap..."
if swapon --show | grep -q '/swapfile'; then
    ok "Swap already active"
else
    fallocate -l 2G /swapfile
    chmod 600 /swapfile
    mkswap /swapfile > /dev/null 2>&1
    swapon /swapfile
    if ! grep -q '/swapfile' /etc/fstab; then
        echo '/swapfile none swap sw 0 0' >> /etc/fstab
    fi
    # Low swappiness — only use swap under real pressure.
    sysctl -q vm.swappiness=10
    if ! grep -q 'vm.swappiness' /etc/sysctl.d/99-hergo.conf 2>/dev/null; then
        echo 'vm.swappiness=10' > /etc/sysctl.d/99-hergo.conf
    fi
    ok "2GB swap created (swappiness=10)"
fi

# --- Step 0b: Firewall ---

info "[0b] Configuring firewall..."
apt-get update -qq
apt-get install -y -qq ufw > /dev/null 2>&1
ufw --force reset > /dev/null 2>&1
ufw default deny incoming > /dev/null 2>&1
ufw default allow outgoing > /dev/null 2>&1
ufw allow 22/tcp > /dev/null 2>&1   # SSH
ufw allow 80/tcp > /dev/null 2>&1   # Caddy HTTP (ACME challenges)
ufw allow 443/tcp > /dev/null 2>&1  # Caddy HTTPS (Telegram webhook)
ufw --force enable > /dev/null 2>&1
ok "UFW enabled: SSH(22) + HTTP(80) + HTTPS(443)"

# --- Step 1: System dependencies ---

info "[1/11] Installing system dependencies..."
apt-get install -y -qq git sqlite3 libsqlite3-dev build-essential curl ffmpeg > /dev/null 2>&1
ok "System dependencies installed"

# --- Step 2: Caddy reverse proxy ---

info "[2/11] Installing Caddy..."
if command -v caddy &>/dev/null; then
    ok "Caddy already installed: $(caddy version)"
else
    apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https > /dev/null 2>&1
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>/dev/null
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list > /dev/null
    apt-get update -qq
    apt-get install -y -qq caddy > /dev/null 2>&1
    ok "Caddy installed"
fi

# --- Step 3: Create service user ---

info "[3/11] Creating service user '$SERVICE_USER'..."
if id "$SERVICE_USER" &>/dev/null; then
    ok "User '$SERVICE_USER' already exists"
else
    useradd --system --create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    ok "User '$SERVICE_USER' created"
fi

# --- Step 4: Install Go ---

info "[4/11] Installing Go $GO_VERSION ($GO_ARCH)..."
if command -v go &>/dev/null && go version | grep -q "$GO_VERSION"; then
    ok "Go $GO_VERSION already installed"
else
    GO_TAR="go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
    curl -sSL "https://go.dev/dl/$GO_TAR" -o "/tmp/$GO_TAR"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/$GO_TAR"
    rm "/tmp/$GO_TAR"

    # Add to system PATH for all users.
    if ! grep -q '/usr/local/go/bin' /etc/profile.d/go.sh 2>/dev/null; then
        echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
    fi
    export PATH=$PATH:/usr/local/go/bin
    ok "Go $GO_VERSION installed: $(go version)"
fi

# --- Step 5: Install Python + uv ---

info "[5/11] Installing Python and uv..."
apt-get install -y -qq python3 python3-venv > /dev/null 2>&1

if command -v uv &>/dev/null; then
    ok "uv already installed: $(uv --version)"
else
    curl -LsSf https://astral.sh/uv/install.sh | sh > /dev/null 2>&1
    export PATH="$HOME/.local/bin:$PATH"

    # Also install for the service user.
    su -s /bin/bash -c 'curl -LsSf https://astral.sh/uv/install.sh | sh > /dev/null 2>&1' "$SERVICE_USER"
    ok "uv installed"
fi

# --- Step 6: Install Ollama ---

info "[6/11] Installing Ollama..."
if command -v ollama &>/dev/null; then
    ok "Ollama already installed"
else
    curl -fsSL https://ollama.com/install.sh | sh > /dev/null 2>&1
    ok "Ollama installed"
fi

# Start Ollama service so we can pull models.
systemctl enable ollama > /dev/null 2>&1 || true
systemctl start ollama > /dev/null 2>&1 || true

# Configure Ollama for low memory usage.
mkdir -p /etc/systemd/system/ollama.service.d
cat > /etc/systemd/system/ollama.service.d/memory.conf << 'OLLAMA_CONF'
[Service]
Environment="OLLAMA_NUM_PARALLEL=1"
Environment="OLLAMA_MAX_LOADED_MODELS=1"
OLLAMA_CONF
systemctl daemon-reload
systemctl restart ollama > /dev/null 2>&1 || true

# Wait for Ollama to be ready.
for i in $(seq 1 15); do
    if ollama list &>/dev/null; then break; fi
    sleep 1
done

# --- Step 7: Pull embedding model ---

info "[7/11] Pulling nomic-embed-text model..."
if ollama list 2>/dev/null | grep -q "nomic-embed-text"; then
    ok "nomic-embed-text already pulled"
else
    ollama pull nomic-embed-text
    ok "nomic-embed-text pulled"
fi

# --- Step 8: Clone repo + build ---

info "[8/11] Cloning repo and building binary..."
if [[ -d "$INSTALL_DIR/.git" ]]; then
    info "Repo exists, pulling latest..."
    git -C "$INSTALL_DIR" pull origin main --quiet
else
    git clone --quiet "$REPO_URL" "$INSTALL_DIR"
fi

cd "$INSTALL_DIR"
/usr/local/go/bin/go build -o "$INSTALL_DIR/her-go" .
ok "Binary built: $INSTALL_DIR/her-go"

# --- Step 9: Download Piper voice model ---

info "[9/11] Downloading Piper TTS voice model..."
VOICES_DIR="$INSTALL_DIR/scripts/piper-voices"
mkdir -p "$VOICES_DIR"

ONNX_FILE="$VOICES_DIR/${PIPER_VOICE}.onnx"
JSON_FILE="$VOICES_DIR/${PIPER_VOICE}.onnx.json"
HF_BASE="https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_GB/southern_english_female/$PIPER_QUALITY"

if [[ -f "$ONNX_FILE" && -s "$ONNX_FILE" ]]; then
    ok "Voice model already downloaded"
else
    curl -sSL -o "$ONNX_FILE" "$HF_BASE/${PIPER_VOICE}.onnx"
    curl -sSL -o "$JSON_FILE" "$HF_BASE/${PIPER_VOICE}.onnx.json"
    ok "Voice model downloaded to $VOICES_DIR"
fi

# --- Step 10: Config + service setup ---

info "[10/11] Setting up config and service..."

# Create config.yaml from example if it doesn't exist.
if [[ ! -f "$INSTALL_DIR/config.yaml" ]]; then
    cp "$INSTALL_DIR/config.yaml.example" "$INSTALL_DIR/config.yaml"

    # Apply VPS-specific defaults: remote STT, local embed via Ollama.
    # sed is crude but avoids requiring yq or python for a one-time setup.
    sed -i 's/engine: "parakeet"/engine: "whisper"/' "$INSTALL_DIR/config.yaml"

    warn "config.yaml created from example — edit it with your API keys:"
    warn "  nano $INSTALL_DIR/config.yaml"
    warn ""
    warn "Required keys:"
    warn "  openrouter.api_key    — get from https://openrouter.ai/keys"
    warn "  telegram.token        — get from @BotFather on Telegram"
fi

# Create logs directory.
mkdir -p "$INSTALL_DIR/logs"

# Set ownership.
chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"

# Build PATH for the service — includes Go, uv, Ollama, and system bins.
SERVICE_PATH="/usr/local/go/bin:/home/$SERVICE_USER/.local/bin:/usr/local/bin:/usr/bin:/bin"

# Run setup as the service user to generate the systemd unit.
su -s /bin/bash -c "
    export PATH='$SERVICE_PATH'
    cd '$INSTALL_DIR'
    '$INSTALL_DIR/her-go' setup --config '$INSTALL_DIR/config.yaml' 2>&1 || true
" "$SERVICE_USER"

ok "Setup complete"

# --- Step 11/11: Caddy reverse proxy config ---

info "[11/11] Configuring Caddy reverse proxy..."
CADDY_DOMAIN="${HER_DOMAIN:-}"

if [[ -n "$CADDY_DOMAIN" ]]; then
    cat > /etc/caddy/Caddyfile << CADDYEOF
$CADDY_DOMAIN {
    reverse_proxy localhost:8443
}
CADDYEOF
    systemctl reload caddy > /dev/null 2>&1 || systemctl restart caddy > /dev/null 2>&1
    ok "Caddy configured for $CADDY_DOMAIN → localhost:8443"
else
    warn "No domain set — skipping Caddy config."
    warn "  Set HER_DOMAIN before running, or edit /etc/caddy/Caddyfile manually:"
    warn "    export HER_DOMAIN=her.yourdomain.com"
    warn "    Then re-run this script, or:"
    warn "    echo 'her.yourdomain.com { reverse_proxy localhost:8443 }' > /etc/caddy/Caddyfile"
    warn "    systemctl reload caddy"
fi

# --- Auto-update timer ---

info "Setting up auto-update timer..."
cat > /etc/systemd/system/her-go-update.service << 'UPDATEEOF'
[Unit]
Description=her-go auto-update check
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
WorkingDirectory=/opt/her-go
ExecStart=/bin/bash -c '\
    cd /opt/her-go && \
    git fetch --quiet origin main && \
    LOCAL=$(git rev-parse HEAD) && \
    REMOTE=$(git rev-parse origin/main) && \
    if [ "$LOCAL" != "$REMOTE" ]; then \
        git pull --quiet origin main && \
        chown -R hergo:hergo /opt/her-go && \
        sudo -u hergo /usr/local/go/bin/go build -o /opt/her-go/her-go.next . && \
        cp /opt/her-go/her-go /opt/her-go/her-go.backup && \
        mv /opt/her-go/her-go.next /opt/her-go/her-go && \
        echo "UPDATE_APPLIED: $(git log --oneline -1)" && \
        systemctl restart her-go; \
    else \
        echo "Already up to date"; \
    fi'
Environment="PATH=/usr/local/go/bin:/home/hergo/.local/bin:/usr/local/bin:/usr/bin:/bin"
Environment="HOME=/root"
Environment="GOPATH=/root/go"
UPDATEEOF

cat > /etc/systemd/system/her-go-update.timer << 'TIMEREOF'
[Unit]
Description=Check for her-go updates every 5 minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=5min
RandomizedDelaySec=30

[Install]
WantedBy=timers.target
TIMEREOF

systemctl daemon-reload
systemctl enable --now her-go-update.timer > /dev/null 2>&1
ok "Auto-update timer enabled (every 5 minutes)"

# --- Summary ---

echo ""
echo -e "${GREEN}════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  her-go is installed at $INSTALL_DIR${NC}"
echo -e "${GREEN}════════════════════════════════════════════════${NC}"
echo ""
echo "Next steps:"
echo "  1. Point your domain's DNS A record at this server's IP"
echo ""
echo "  2. Configure Caddy (if you didn't set HER_DOMAIN):"
echo "     echo 'her.yourdomain.com { reverse_proxy localhost:8443 }' > /etc/caddy/Caddyfile"
echo "     systemctl reload caddy"
echo ""
echo "  3. Edit config with your API keys:"
echo "     nano $INSTALL_DIR/config.yaml"
echo "     # Required: openrouter.api_key, telegram.token"
echo "     # Set telegram.mode to 'webhook'"
echo "     # Set telegram.webhook_url to 'https://yourdomain.com'"
echo ""
echo "  4. Start the service:"
echo "     systemctl start her-go"
echo ""
echo "  5. Check status:"
echo "     systemctl status her-go"
echo "     journalctl -u her-go -f"
echo ""
echo "Auto-update: pushes to main deploy within 5 minutes."
echo "  Check timer: systemctl list-timers her-go-update.timer"
echo "  Manual update: systemctl start her-go-update.service"
echo "  Or from Telegram: /update"
echo ""
echo "Firewall: SSH(22) + HTTP(80) + HTTPS(443) only."
echo "Swap: 2GB (swappiness=10)."
echo "Ollama: OLLAMA_NUM_PARALLEL=1, OLLAMA_MAX_LOADED_MODELS=1."
echo ""
