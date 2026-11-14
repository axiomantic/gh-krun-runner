#!/usr/bin/env bash
# ==============================================================================
# guest/entrypoint.sh — Ephemeral Runner VM Entrypoint & GPU/Network Initializer
# ==============================================================================
set -euo pipefail

echo "================================================================="
echo "=== Booting Ephemeral Linux ARM64 GPU Runner MicroVM ==="
echo "================================================================="

# 1. Essential mounts if not already mounted by init
for mnt in /proc /sys /dev /dev/pts /dev/shm; do
    if ! mountpoint -q "$mnt"; then
        mkdir -p "$mnt"
        case "$mnt" in
            /proc) mount -t proc proc /proc ;;
            /sys) mount -t sysfs sysfs /sys ;;
            /dev) mount -t devtmpfs devtmpfs /dev ;;
            /dev/pts) mount -t devpts devpts /dev/pts ;;
            /dev/shm) mount -t tmpfs tmpfs /dev/shm ;;
        esac
    fi
done

# 2. Network Activation & Gateway Discovery
echo "[INFO] Initializing network interface eth0..."
ip link set eth0 up || true
if command -v dhclient >/dev/null 2>&1; then
    dhclient eth0 2>/dev/null || true
elif command -v udhcpc >/dev/null 2>&1; then
    udhcpc -i eth0 -n -q 2>/dev/null || true
elif command -v systemctl >/dev/null 2>&1; then
    systemctl restart systemd-networkd 2>/dev/null || true
fi

# Detect Gateway IP from route table (usually 192.168.127.1 for gvproxy)
GATEWAY_IP=$(ip route show default 2>/dev/null | awk '/default/ {print $3}' || echo "192.168.127.1")
echo "[INFO] Network default gateway: ${GATEWAY_IP}"

# 3. Strict Firewall & Egress Isolation (Requirement 2)
# Blocks local loopback spoofing, RFC1918 subnets (except the gvproxy gateway for DNS), and link-local.
if command -v iptables >/dev/null 2>&1; then
    echo "[INFO] Applying defense-in-depth egress firewall rules..."
    iptables -F OUTPUT || true
    iptables -A OUTPUT -o lo -j ACCEPT
    iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

    # Allow DNS & DHCP queries to the gvproxy gateway
    iptables -A OUTPUT -d "${GATEWAY_IP}" -p udp --dport 53 -j ACCEPT
    iptables -A OUTPUT -d "${GATEWAY_IP}" -p tcp --dport 53 -j ACCEPT
    iptables -A OUTPUT -p udp --dport 67:68 --sport 67:68 -j ACCEPT

    # Block access to host loopback, LAN subnets, and link-local metadata
    iptables -A OUTPUT -d 127.0.0.0/8 -j REJECT
    iptables -A OUTPUT -d 10.0.0.0/8 -j REJECT
    iptables -A OUTPUT -d 172.16.0.0/12 -j REJECT
    iptables -A OUTPUT -d 169.254.0.0/16 -j REJECT
    iptables -A OUTPUT -d 192.168.0.0/16 ! -d "${GATEWAY_IP}" -j REJECT

    # Allow outbound HTTPS/HTTP for GitHub Actions, package registries, git over SSH
    iptables -A OUTPUT -p tcp --dport 443 -j ACCEPT
    iptables -A OUTPUT -p tcp --dport 80 -j ACCEPT
    iptables -A OUTPUT -p tcp --dport 22 -j ACCEPT

    # Default policy: drop all other traffic
    iptables -P OUTPUT DROP
fi

# 4. Virtio-GPU Venus Hardware Node Verification
echo "[INFO] Checking Virtio-GPU Venus render node..."
if [ -e /dev/dri/renderD128 ]; then
    chmod 0666 /dev/dri/renderD128 || true
    echo "[INFO] Found /dev/dri/renderD128 (Virtio-GPU Venus active)"
else
    echo "[WARN] /dev/dri/renderD128 not found! Loading virtio-gpu kernel module..."
    modprobe virtio-gpu 2>/dev/null || true
    if [ -e /dev/dri/renderD128 ]; then
        chmod 0666 /dev/dri/renderD128 || true
        echo "[INFO] Successfully initialized /dev/dri/renderD128"
    else
        echo "[ERROR] Virtio-GPU device could not be located"
    fi
fi

# Configure Mesa Vulkan Venus ICD environment
export VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/virtio_icd.aarch64.json
export VN_PERF=direct_resource
export MESA_VK_DEVICE_SELECT=10005:0000

# Smoke-test Vulkan enumeration
if command -v vulkaninfo >/dev/null 2>&1; then
    echo "[INFO] Vulkan Device Summary:"
    vulkaninfo --summary 2>&1 | head -n 25 || true
fi

# 5. Mount Shared Host Models Directory as Strictly Read-Only (virtio-fs)
mkdir -p /models
echo "[INFO] Mounting /models (virtio-fs: models) strictly read-only..."
if mount -t virtiofs -o ro,nodev,nosuid models /models 2>/dev/null; then
    echo "[INFO] /models mounted successfully (read-only):"
    ls -lah /models || true
else
    echo "[WARN] Could not mount virtio-fs 'models' tag. Proceeding without host /models..."
fi

# 6. Read Transient JIT Runner Config & Scrub Immediately
RUNNER_DIR="/home/runner/actions-runner"
mkdir -p /mnt/runner-config
JIT_CONFIG=""

if mount -t virtiofs -o ro,nodev,nosuid runner-config /mnt/runner-config 2>/dev/null; then
    if [ -f /mnt/runner-config/jitconfig.txt ]; then
        echo "[INFO] Reading single-use JIT runner configuration..."
        JIT_CONFIG=$(cat /mnt/runner-config/jitconfig.txt)
        # Shred the transient secret immediately
        if command -v shred >/dev/null 2>&1; then
            shred -u /mnt/runner-config/jitconfig.txt 2>/dev/null || rm -f /mnt/runner-config/jitconfig.txt
        else
            rm -f /mnt/runner-config/jitconfig.txt
        fi
    fi
    umount /mnt/runner-config 2>/dev/null || true
fi

# Fallback: check /etc/runner-jitconfig if passed via disk injection
if [ -z "${JIT_CONFIG}" ] && [ -f /etc/runner-jitconfig ]; then
    echo "[INFO] Reading JIT config from /etc/runner-jitconfig..."
    JIT_CONFIG=$(cat /etc/runner-jitconfig)
    rm -f /etc/runner-jitconfig
fi

# 7. Execute Runner or Standalone Smoke Test
if [ -n "${JIT_CONFIG}" ]; then
    echo "[INFO] Launching GitHub Actions Runner with ephemeral JIT config..."
    chown -R runner:runner "${RUNNER_DIR}" /models 2>/dev/null || true
    
    # Export GPU and Vulkan variables for the runner job processes
    cat << 'EOF' > /home/runner/.bashrc
export VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/virtio_icd.aarch64.json
export VN_PERF=direct_resource
export MESA_VK_DEVICE_SELECT=10005:0000
export PATH="/usr/local/bin:/usr/bin:/bin:$PATH"
EOF
    chown runner:runner /home/runner/.bashrc

    cd "${RUNNER_DIR}"
    # Run the single ephemeral job. The runner automatically unregisters and exits when done.
    su runner -c "./run.sh --jitconfig '${JIT_CONFIG}'" || true
    echo "[INFO] Ephemeral job execution finished."
else
    echo "[WARN] No JIT_CONFIG provided! Running standalone verification mode..."
    if command -v llama-cli >/dev/null 2>&1; then
        echo "[INFO] llama.cpp version info:"
        llama-cli --version 2>&1 || true
    fi
    sleep 3
fi

# 8. Clean Shutdown & Power Off MicroVM
echo "[INFO] Flushing disk writes and powering off microVM..."
sync
sleep 1
poweroff -f || shutdown -h now || true
