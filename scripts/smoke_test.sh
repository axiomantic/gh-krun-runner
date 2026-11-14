#!/usr/bin/env bash
# ==============================================================================
# scripts/smoke_test.sh — Smoke Test Vulkan & Virtio-GPU Acceleration in MicroVM
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
BASE_IMAGE="${ROOT_DIR}/images/base_rootfs.raw"
KRUNKIT_BIN="${KRUNKIT_BIN:-/opt/homebrew/bin/krunkit}"
SMOKE_WORK_DIR="/tmp/gh-krun-runner-smoke"

echo "================================================================="
echo "=== Smoke Testing MicroVM with Virtio-GPU Venus (Metal Vulkan) ==="
echo "================================================================="

if [ ! -f "${KRUNKIT_BIN}" ]; then
    echo "[ERROR] krunkit binary not found at ${KRUNKIT_BIN}"
    echo "Run: brew install krunkit"
    exit 1
fi

if [ ! -f "${BASE_IMAGE}" ]; then
    echo "[ERROR] Base raw disk image not found at ${BASE_IMAGE}"
    echo "Run: ./scripts/build_rootfs.sh first"
    exit 1
fi

rm -rf "${SMOKE_WORK_DIR}"
mkdir -p "${SMOKE_WORK_DIR}"

TEST_DISK="${SMOKE_WORK_DIR}/smoke_disk.raw"
echo "[INFO] Creating ephemeral APFS clone of base disk..."
cp -c "${BASE_IMAGE}" "${TEST_DISK}" 2>/dev/null || cp "${BASE_IMAGE}" "${TEST_DISK}"

trap 'rm -rf "${SMOKE_WORK_DIR}"' EXIT

echo "[INFO] Launching test microVM..."
echo "[INFO] Running: ${KRUNKIT_BIN} --cpus 4 --memory 4096 --device virtio-gpu --device virtio-blk,path=${TEST_DISK},format=raw"
echo "-----------------------------------------------------------------"

# In standalone mode (no JIT config), entrypoint.sh runs vulkaninfo, llama-cli, and powers off.
"${KRUNKIT_BIN}" \
    --cpus 4 \
    --memory 4096 \
    --device virtio-gpu \
    --device virtio-blk,path="${TEST_DISK}",format=raw

echo "-----------------------------------------------------------------"
echo "[SUCCESS] MicroVM smoke test execution complete!"
