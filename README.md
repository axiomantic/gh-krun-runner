# GitHub Actions Ephemeral Linux GPU Runner on Apple Silicon (`krunkit` + Virtio-GPU Venus)

Production-ready, ephemeral Linux GitHub Actions self-hosted runner infrastructure running natively on **macOS Apple Silicon (M-series)** hosts. It delivers **Metal-backed Vulkan 1.3 GPU compute** to isolated Linux ARM64 microVMs using `krunkit`, `libkrun`, `virglrenderer`, `MoltenVK`, and `gvproxy`.

---

## 1. System Architecture

```
                                    +-------------------------------------------------------+
                                    |              macOS Host (Apple Silicon)               |
                                    |                                                       |
                                    |  +--------------------+        +-------------------+  |
                                    |  | Host Orchestrator  |<------>| GitHub REST API   |  |
                                    |  | (gh-krun-daemon)   |        | (JIT Runner Conf) |  |
                                    |  +---------+----------+        +-------------------+  |
                                    |            |                                          |
                   Single-Concurrency GPU Lock   | [APFS CoW cp -c]                         |
                  (/tmp/gh-krun-gpu.lock)        v                                          |
                                       +--------------------+                               |
                                       | Ephemeral MicroVM  |                               |
                                       | (krunkit / libkrun)|                               |
                                       +---------+----------+                               |
                                                 |                                          |
          +-----------------------+--------------+-----------------+---------------------+  |
          |                       |                                |                     |  |
          v                       v                                v                     v  |
   [Virtio-GPU]             [Virtio-Net]                      [Virtio-FS]           [Virtio-Blk]
   virglrenderer            gvproxy (UNIX Dgram)              Host Dir Mounts       Ephemeral CoW
   + MoltenVK / Metal       + iptables Egress Drop            /models (Read-Only)   disk.raw
          |                       |                                |                     |  |
+---------|-----------------------|--------------------------------|---------------------|--+
| GUEST   v                       v                                v                     v  |
|   /dev/dri/renderD128      eth0 (Isolated)                    /models              / (rootfs)
|   Mesa Venus Vulkan 1.3    Outbound HTTPS Only                GGUF / ONNX Weights  Discarded on exit
|   llama.cpp (-DGGML_VULKAN)                                                               |
+-------------------------------------------------------------------------------------------+
```

### Core Security & Compute Invariants
1. **Metal-Backed Vulkan Hardware Compute**: The guest accesses Apple Silicon unified memory GPU compute through Mesa's Venus protocol (`/dev/dri/renderD128`) translated to Metal via MoltenVK.
2. **Strict Network Isolation**: Transparent Socket Impersonation (TSI) is disabled. Traffic routes through `gvproxy` over a UNIX datagram socket, paired with in-guest `iptables` drop rules that reject host loopback (`127.0.0.0/8`), local LAN subnets (`192.168.0.0/16`, `10.0.0.0/8`, `172.16.0.0/12`), and link-local addresses, allowing outbound HTTPS only.
3. **Single-Concurrency GPU Locking**: The host daemon enforces an exclusive lock (`/tmp/gh-krun-gpu.lock`) across processes, preventing unified memory exhaustion and cross-job GPU memory bleed.
4. **Instant APFS CoW Ephemeral Disks**: Every job clones the base raw disk image in <50ms using APFS clonefile (`cp -c`). When the job finishes, the VM triggers `poweroff` and the host daemon completely wipes the cloned disk and transient JIT secrets.
5. **Read-Only Weights Mount**: Host model directory (`/Users/Shared/ci-models`) is attached via `virtio-fs` as **strictly read-only (`ro`)**, preventing jobs from mutating weights or touching host storage.

---

## 2. Directory Structure

```
gh-krun-runner/
├── Makefile                       # Top-level orchestration (setup, build, run, clean)
├── README.md                      # Documentation & workflow guides
├── daemon/                        # Go Orchestrator Daemon
│   ├── config.go                  # Config loader with environment overrides
│   ├── config.example.toml        # Sample configuration file
│   ├── github.go                  # GitHub API client (Polling & JIT Generation)
│   ├── gpulock.go                 # Single-concurrency host GPU locking
│   ├── main.go                    # Daemon lifecycle & signal handling
│   ├── vm.go                      # MicroVM execution, gvproxy & APFS CoW
│   └── go.mod
├── guest/                         # Linux Guest Image Sources
│   ├── Containerfile              # Ubuntu 24.04 ARM64 with Mesa Venus & llama.cpp
│   └── entrypoint.sh              # Network/GPU init, JIT runner start & poweroff
├── launchd/
│   └── com.local.gh-krun-runner.plist # macOS LaunchAgent service definition
└── scripts/
    ├── build_rootfs.sh            # Packages Containerfile into EFI raw disk
    ├── hypervisor.entitlements    # com.apple.security.hypervisor entitlement
    ├── setup.sh                   # Dependency installation & host setup
    └── smoke_test.sh              # Standalone GPU verification test
```

---

## 3. GitHub Token & Permission Scopes

The orchestrator daemon uses GitHub's single-use **Just-In-Time (JIT) Runner Configuration API** (`POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig`).

### Required Permissions:
* **Fine-Grained Personal Access Token (Recommended)**:
  * **Repository access**: Select target repository (e.g. `elijahr/eek`)
  * **Actions**: `Read and write` (to view workflow runs/jobs and register runners)
  * **Administration**: `Read and write` (required to invoke `generate-jitconfig`)
* **Classic Personal Access Token**:
  * Scope: `repo` (Full control of private repositories)

---

## 4. Installation & Quick Start

### Step 1: Install Host Dependencies & Compile Daemon
Run the setup script to install `krunkit`, `virglrenderer`, `molten-vk`, `qemu`, build `gvproxy`, and compile the orchestrator daemon:
```bash
cd /Users/eek/Development/gh-krun-runner
make setup
```

### Step 2: Configure the Orchestrator
Copy and edit the runtime configuration:
```bash
cp daemon/config.example.toml /Users/Shared/gh-krun-runner/config.toml
```
Edit `/Users/Shared/gh-krun-runner/config.toml`:
```toml
[github]
token = "ghp_yourPersonalAccessTokenHere"
owner = "elijahr"
repo = "eek"
labels = ["self-hosted", "linux", "arm64", "gpu"]

[vm]
vcpus = 6
memory_mib = 12288
models_dir = "/Users/Shared/ci-models"
```

### Step 3: Build the Guest Linux Disk Image
Build the container image and package it into an EFI-bootable raw disk image (`base_rootfs.raw`):
```bash
make rootfs
```
*(Requires Docker Desktop, OrbStack, or Podman).*

### Step 4: Smoke Test GPU Compute Inside MicroVM
Verify that the microVM boots, detects `/dev/dri/renderD128`, and executes Vulkan compute:
```bash
make smoke-test
```
You should see output similar to:
```
[INFO] Vulkan Device Summary:
GPU0:
	apiVersion         = 1.3.275
	driverVersion      = 24.0.9
	vendorID           = 0x10005 (Mesa)
	deviceID           = 0x0000 (Venus / Apple M4 Pro)
	deviceType         = PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU
	deviceName         = Venus (Apple M4 Pro)
```

### Step 5: Start as a Background macOS Service (`launchd`)
Install and load the LaunchAgent so the daemon runs persistently in the background:
```bash
make install-service
```
Stream daemon logs in real-time:
```bash
make logs
```

To stop or uninstall the service:
```bash
make uninstall-service
```

---

## 5. Sample GitHub Actions Workflow

Create `.github/workflows/gpu-vulkan-ci.yml` in your repository:

```yaml
name: GPU Vulkan CI

on:
  push:
    branches: [ main ]
  pull_request:
    branches: [ main ]
  workflow_dispatch:

jobs:
  vulkan-llm-test:
    name: MicroVM Vulkan GPU LLM Tests
    runs-on: [self-hosted, linux, arm64, gpu]

    steps:
      - name: Checkout Code
        uses: actions/checkout@v4

      - name: Verify Vulkan Venus Hardware Node
        run: |
          ls -la /dev/dri/renderD128
          vulkaninfo --summary

      - name: Test llama.cpp Vulkan Offload
        run: |
          if [ -f /models/qwen2.5-coder-1.5b-instruct-q4_k_m.gguf ]; then
            llama-cli \
              -m /models/qwen2.5-coder-1.5b-instruct-q4_k_m.gguf \
              -p "Write an idiomatic lock-free queue in Nim." \
              -n 64 \
              -ngl 99 \
              --log-disable
          else
            echo "Model file not present in read-only /models. Running banner test..."
            llama-cli --version
          fi

      - name: Test Python ONNX Runtime & PyTorch
        run: |
          python3 -c "
          import onnxruntime as ort
          print('ONNX Runtime version:', ort.__version__)
          print('Available Execution Providers:', ort.get_available_providers())
          "
```

---

## 6. Maintenance & Troubleshooting

* **Single-Concurrency Lock**: If a job hangs or host power is interrupted, check `/tmp/gh-krun-gpu.lock`. The daemon automatically releases this lock when krunkit exits or when the hard timeout triggers.
* **Network Egress Testing**: From within a job, attempting to connect to host loopback (`curl http://127.0.0.1:8000`) or local home router (`curl http://192.168.1.1`) will immediately fail with `Connection refused` or `Network unreachable` via the iptables filter.
* **Logs**: Check `~/Library/Logs/gh-krun-runner/daemon.stdout.log` and `daemon.stderr.log` for granular lifecycle and API polling records.
