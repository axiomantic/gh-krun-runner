#!/usr/bin/env bash
# ==============================================================================
# scripts/setup.sh — Automated Host Environment Setup & Dependency Installation
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

echo "================================================================="
echo "=== Setting Up GitHub Actions krunkit GPU Runner Environment ==="
echo "================================================================="

# 1. Architecture & Platform Verification
ARCH=$(uname -m)
if [ "${ARCH}" != "arm64" ]; then
    echo "[ERROR] This virtualization stack requires Apple Silicon (ARM64). Detected: ${ARCH}"
    exit 1
fi
echo "[INFO] Verified Apple Silicon host (arm64)"

# 2. Check Homebrew
if ! command -v brew >/dev/null 2>&1; then
    echo "[ERROR] Homebrew is required to install host dependencies."
    exit 1
fi

echo "[INFO] Installing required Homebrew packages (krunkit, virglrenderer, molten-vk, qemu)..."
brew install krunkit virglrenderer molten-vk qemu || true

# 3. Install or Build gvproxy
if ! command -v gvproxy >/dev/null 2>&1; then
    echo "[INFO] gvproxy not found in PATH. Building gvproxy using host Go compiler..."
    if command -v go >/dev/null 2>&1; then
        go install github.com/containers/gvisor-tap-vsock/cmd/gvproxy@latest
        GOPATH_BIN="$(go env GOPATH)/bin"
        if [ -f "${GOPATH_BIN}/gvproxy" ]; then
            sudo cp "${GOPATH_BIN}/gvproxy" /usr/local/bin/gvproxy 2>/dev/null || cp "${GOPATH_BIN}/gvproxy" /opt/homebrew/bin/gvproxy 2>/dev/null || true
            echo "[INFO] gvproxy successfully installed to $(which gvproxy || echo ${GOPATH_BIN}/gvproxy)"
        fi
    else
        echo "[ERROR] Go compiler not found to build gvproxy."
        echo "Run: brew install go"
        exit 1
    fi
else
    echo "[INFO] Found gvproxy at $(which gvproxy)"
fi

# 4. Codesign krunkit with Hypervisor Entitlements if needed
KRUNKIT_PATH="$(which krunkit 2>/dev/null || echo '/opt/homebrew/bin/krunkit')"
if [ -f "${KRUNKIT_PATH}" ]; then
    echo "[INFO] Verifying codesign entitlements for ${KRUNKIT_PATH}..."
    if ! codesign -d --entitlements :- "${KRUNKIT_PATH}" 2>&1 | grep -q "com.apple.security.hypervisor"; then
        echo "[INFO] Applying com.apple.security.hypervisor entitlement to ${KRUNKIT_PATH}..."
        codesign -s - --force --entitlements "${SCRIPT_DIR}/hypervisor.entitlements" "${KRUNKIT_PATH}"
    else
        echo "[INFO] ${KRUNKIT_PATH} already holds hypervisor entitlements."
    fi
fi

# 5. Compile Host Orchestrator Daemon
echo "[INFO] Compiling host orchestrator daemon..."
cd "${ROOT_DIR}/daemon"
go mod tidy
go build -v -o "${ROOT_DIR}/daemon/gh-krun-daemon" .

# 6. Initialize Global Directory Structure
echo "[INFO] Preparing /Users/Shared runtime directories..."
sudo mkdir -p /Users/Shared/gh-krun-runner/{bin,images} /Users/Shared/ci-models
sudo chown -R "$(id -un):$(id -gn)" /Users/Shared/gh-krun-runner /Users/Shared/ci-models

mkdir -p "${HOME}/Library/Logs/gh-krun-runner"
cp "${ROOT_DIR}/daemon/gh-krun-daemon" /Users/Shared/gh-krun-runner/bin/gh-krun-daemon

if [ ! -f /Users/Shared/gh-krun-runner/config.toml ]; then
    echo "[INFO] Creating template configuration at /Users/Shared/gh-krun-runner/config.toml..."
    cp "${ROOT_DIR}/daemon/config.example.toml" /Users/Shared/gh-krun-runner/config.toml
fi

echo "================================================================="
echo "[SUCCESS] Host setup complete!"
echo "Next Steps:"
echo "1. Edit /Users/Shared/gh-krun-runner/config.toml with your GitHub token."
echo "2. Run 'make rootfs' (or ./scripts/build_rootfs.sh) to build the base Linux disk."
echo "3. Run 'make smoke-test' (or ./scripts/smoke_test.sh) to verify GPU Vulkan compute."
echo "4. Run 'make install-service' to launch the background service."
echo "================================================================="
