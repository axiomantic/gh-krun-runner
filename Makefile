# ==============================================================================
# Makefile — GitHub Actions krunkit Ephemeral GPU Runner
# ==============================================================================

SHELL := /bin/bash
ROOT_DIR := $(shell pwd)
DAEMON_DIR := $(ROOT_DIR)/daemon
PLIST_NAME := com.local.gh-krun-runner.plist
PLIST_SRC := $(ROOT_DIR)/launchd/$(PLIST_NAME)
PLIST_DEST := $(HOME)/Library/LaunchAgents/$(PLIST_NAME)

.PHONY: all setup daemon rootfs smoke-test install-service uninstall-service status logs clean

all: setup

## setup: Install host dependencies, build gvproxy & compile orchestrator daemon
setup:
	@chmod +x scripts/*.sh guest/*.sh
	@./scripts/setup.sh

## daemon: Compile the Go orchestrator daemon binary
daemon:
	@echo "==> Compiling daemon in $(DAEMON_DIR)..."
	@cd $(DAEMON_DIR) && go mod tidy && go build -v -o gh-krun-daemon .
	@mkdir -p /Users/Shared/gh-krun-runner/bin
	@cp $(DAEMON_DIR)/gh-krun-daemon /Users/Shared/gh-krun-runner/bin/gh-krun-daemon
	@echo "[OK] Daemon built and copied to /Users/Shared/gh-krun-runner/bin/gh-krun-daemon"

## rootfs: Build base EFI-bootable ARM64 raw disk image with Venus Vulkan & runner
rootfs:
	@echo "==> Building base disk image..."
	@./scripts/build_rootfs.sh
	@mkdir -p /Users/Shared/gh-krun-runner/images
	@cp $(ROOT_DIR)/images/base_rootfs.raw /Users/Shared/gh-krun-runner/images/base_rootfs.raw
	@echo "[OK] Base disk ready at /Users/Shared/gh-krun-runner/images/base_rootfs.raw"

## smoke-test: Test microVM GPU acceleration and Vulkan enumeration
smoke-test:
	@echo "==> Running microVM GPU smoke test..."
	@./scripts/smoke_test.sh

## install-service: Install and start launchd LaunchAgent
install-service: daemon
	@echo "==> Installing launchd service $(PLIST_NAME)..."
	@mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs/gh-krun-runner
	@cp $(PLIST_SRC) $(PLIST_DEST)
	@launchctl bootout gui/$(shell id -u)/$(PLIST_NAME) 2>/dev/null || true
	@launchctl bootstrap gui/$(shell id -u) $(PLIST_DEST)
	@echo "[OK] Service loaded. Inspect logs with: make logs"

## uninstall-service: Stop and remove launchd LaunchAgent
uninstall-service:
	@echo "==> Unloading launchd service $(PLIST_NAME)..."
	@launchctl bootout gui/$(shell id -u)/$(PLIST_NAME) 2>/dev/null || true
	@rm -f $(PLIST_DEST)
	@echo "[OK] Service uninstalled."

## status: Check if the orchestrator daemon is currently running
status:
	@echo "==> Daemon process status:"
	@pgrep -lf gh-krun-daemon || echo "gh-krun-daemon is not currently running"
	@echo "==> Launchd status:"
	@launchctl list | grep gh-krun || true

## logs: Stream daemon execution logs
logs:
	@echo "==> Streaming logs from $(HOME)/Library/Logs/gh-krun-runner/daemon.stdout.log..."
	@tail -f $(HOME)/Library/Logs/gh-krun-runner/daemon.stdout.log $(HOME)/Library/Logs/gh-krun-runner/daemon.stderr.log 2>/dev/null || echo "No logs found yet."

## clean: Remove transient files and local disk images
clean:
	@rm -f $(DAEMON_DIR)/gh-krun-daemon
	@rm -rf $(ROOT_DIR)/images
	@rm -rf /tmp/gh-krun-runner*
	@echo "[OK] Cleaned build artifacts."
