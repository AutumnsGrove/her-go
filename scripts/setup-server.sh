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
#   - Go (latest stable, ARM64 or AMD64)
#   - Python 3 + uv (for TTS sidecar)
#   - Ollama + nomic-embed-text (embedding model)
#   - Piper TTS voice model
#   - her-go binary + systemd service
#
# Memory budget (4GB VPS):
#   OS ~300MB + Go binary ~50MB + Piper ~80MB + Ollama ~500MB + Python ~100MB
#   Total ~1.2GB — comfortable headroom on a 4GB plan.

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
GO_VERSION="1.24.4"
PIPER_VOICE="en_GB-southern_english_female-low"
PIPER_QUALITY="low"

# --- Step 1: System dependencies ---

info "[1/9] Installing system dependencies..."
apt-get update -qq
apt-get install -y -qq git sqlite3 build-essential curl ffmpeg > /dev/null 2>&1
ok "System dependencies installed"

# --- Step 2: Create service user ---

info "[2/9] Creating service user '$SERVICE_USER'..."
if id "$SERVICE_USER" &>/dev/null; then
    ok "User '$SERVICE_USER' already exists"
else
    useradd --system --create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    ok "User '$SERVICE_USER' created"
fi

# --- Step 3: Install Go ---

info "[3/9] Installing Go $GO_VERSION ($GO_ARCH)..."
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

# --- Step 4: Install Python + uv ---

info "[4/9] Installing Python and uv..."
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

# --- Step 5: Install Ollama ---

info "[5/9] Installing Ollama..."
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

# --- Step 6: Pull embedding model ---

info "[6/9] Pulling nomic-embed-text model..."
if ollama list 2>/dev/null | grep -q "nomic-embed-text"; then
    ok "nomic-embed-text already pulled"
else
    ollama pull nomic-embed-text
    ok "nomic-embed-text pulled"
fi

# --- Step 7: Clone repo + build ---

info "[7/9] Cloning repo and building binary..."
if [[ -d "$INSTALL_DIR/.git" ]]; then
    info "Repo exists, pulling latest..."
    git -C "$INSTALL_DIR" pull origin main --quiet
else
    git clone --quiet "$REPO_URL" "$INSTALL_DIR"
fi

cd "$INSTALL_DIR"
/usr/local/go/bin/go build -o "$INSTALL_DIR/her-go" .
ok "Binary built: $INSTALL_DIR/her-go"

# --- Step 8: Download Piper voice model ---

info "[8/9] Downloading Piper TTS voice model..."
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

# --- Step 9: Config + service setup ---

info "[9/9] Setting up config and service..."

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

# --- Summary ---

echo ""
echo -e "${GREEN}════════════════════════════════════════════════${NC}"
echo -e "${GREEN}  her-go is installed at $INSTALL_DIR${NC}"
echo -e "${GREEN}════════════════════════════════════════════════${NC}"
echo ""
echo "Next steps:"
echo "  1. Edit config with your API keys:"
echo "     nano $INSTALL_DIR/config.yaml"
echo ""
echo "  2. Start the service:"
echo "     systemctl start her-go"
echo ""
echo "  3. Check status:"
echo "     systemctl status her-go"
echo "     journalctl -u her-go -f"
echo ""
echo "  4. Self-update from Telegram:"
echo "     Send /update to your bot"
echo ""
echo "Memory-saving Ollama config applied:"
echo "  OLLAMA_NUM_PARALLEL=1, OLLAMA_MAX_LOADED_MODELS=1"
echo ""
