#!/usr/bin/env bash
# ==============================================================================
# scripts/build_rootfs.sh — Build EFI-Bootable ARM64 Raw Disk from Containerfile
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUTPUT_DIR="${ROOT_DIR}/images"
RAW_IMAGE="${OUTPUT_DIR}/base_rootfs.raw"
IMAGE_TAG="gh-krun-guest:latest"
DISK_SIZE_MB="${DISK_SIZE_MB:-12288}" # 12 GB default

mkdir -p "${OUTPUT_DIR}"

echo "================================================================="
echo "=== Building Ephemeral Runner Linux ARM64 Disk Image ==="
echo "================================================================="

# Detect container engine (Docker or Podman)
CONTAINER_CMD=""
if command -v docker >/dev/null 2>&1; then
    CONTAINER_CMD="docker"
elif command -v podman >/dev/null 2>&1; then
    CONTAINER_CMD="podman"
else
    echo "[ERROR] Neither Docker nor Podman was found on the host."
    echo "Please install Docker Desktop / OrbStack or Podman to build the guest disk."
    exit 1
fi

echo "[INFO] Using container engine: ${CONTAINER_CMD}"
echo "[INFO] Building base container image from guest/Containerfile..."

cd "${ROOT_DIR}"
${CONTAINER_CMD} build --platform linux/arm64 -t "${IMAGE_TAG}" -f guest/Containerfile .

echo "[INFO] Container build complete. Now packaging into EFI bootable raw disk: ${RAW_IMAGE}..."

# We execute an in-container disk creation script with loop device privileges
${CONTAINER_CMD} run --rm --privileged \
    --platform linux/arm64 \
    -v "${OUTPUT_DIR}:/output" \
    "${IMAGE_TAG}" bash -c "
        set -euo pipefail
        apt-get update -q && apt-get install -y --no-install-recommends \
            dosfstools e2fsprogs parted grub-efi-arm64-bin grub-efi-arm64 grub-common udev rsync

        echo '[GUEST-BUILD] Creating blank raw disk image (${DISK_SIZE_MB} MB)...'
        truncate -s ${DISK_SIZE_MB}M /output/base_rootfs.raw

        echo '[GUEST-BUILD] Partitioning disk with GPT table...'
        parted -s /output/base_rootfs.raw mklabel gpt
        parted -s /output/base_rootfs.raw mkpart ESP fat32 1MiB 513MiB
        parted -s /output/base_rootfs.raw set 1 esp on
        parted -s /output/base_rootfs.raw mkpart root ext4 513MiB 100%

        echo '[GUEST-BUILD] Setting up loop device...'
        LOOP=\$(losetup -Pf --show /output/base_rootfs.raw)
        echo \"[GUEST-BUILD] Attached to \${LOOP}\"

        echo '[GUEST-BUILD] Formatting filesystems...'
        mkfs.vfat -F32 -n EFI \"\${LOOP}p1\"
        mkfs.ext4 -F -L rootfs \"\${LOOP}p2\"

        mkdir -p /mnt/target
        mount \"\${LOOP}p2\" /mnt/target
        mkdir -p /mnt/target/boot/efi
        mount \"\${LOOP}p1\" /mnt/target/boot/efi

        echo '[GUEST-BUILD] Syncing rootfs from container...'
        rsync -aHAX --exclude='/output' --exclude='/mnt' --exclude='/proc' --exclude='/sys' --exclude='/dev' / /mnt/target/

        mkdir -p /mnt/target/proc /mnt/target/sys /mnt/target/dev /mnt/target/models /mnt/target/mnt/runner-config

        ROOT_UUID=\$(blkid -s UUID -o value \"\${LOOP}p2\")
        EFI_UUID=\$(blkid -s UUID -o value \"\${LOOP}p1\")

        echo '[GUEST-BUILD] Generating /etc/fstab...'
        cat << FSTAB > /mnt/target/etc/fstab
UUID=\${ROOT_UUID} / ext4 errors=remount-ro,noatime 0 1
UUID=\${EFI_UUID} /boot/efi vfat umask=0077 0 1
FSTAB

        echo '[GUEST-BUILD] Installing GRUB EFI ARM64 bootloader...'
        grub-install --target=arm64-efi --efi-directory=/mnt/target/boot/efi --boot-directory=/mnt/target/boot --removable --no-nvram

        cat << GRUBCFG > /mnt/target/boot/grub/grub.cfg
set default=0
set timeout=1

menuentry 'Ubuntu Ephemeral Runner' {
    search --no-floppy --fs-uuid --set=root \${ROOT_UUID}
    linux /boot/vmlinuz root=UUID=\${ROOT_UUID} ro console=ttyAMA0,115200 rootwait quiet
    initrd /boot/initrd.img
}
GRUBCFG

        echo '[GUEST-BUILD] Cleaning up mounts...'
        umount /mnt/target/boot/efi
        umount /mnt/target
        losetup -d \"\${LOOP}\"
        echo '[GUEST-BUILD] Base raw disk image created successfully!'
    "

echo "================================================================="
echo "[SUCCESS] Base rootfs image ready at:"
ls -lh "${RAW_IMAGE}"
echo "================================================================="
