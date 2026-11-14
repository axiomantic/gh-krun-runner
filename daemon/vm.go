package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type VMRunner struct {
	cfg        *Config
	instanceID string
	jobDir     string
	diskPath   string
	sockPath   string
	configDir  string
	gvCmd      *exec.Cmd
	krunCmd    *exec.Cmd
}

func NewVMRunner(cfg *Config) (*VMRunner, error) {
	randBytes := make([]byte, 4)
	if _, err := rand.Read(randBytes); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("krun-%d-%s", time.Now().Unix(), hex.EncodeToString(randBytes))
	jobDir := filepath.Join(cfg.VM.WorkDir, id)

	return &VMRunner{
		cfg:        cfg,
		instanceID: id,
		jobDir:     jobDir,
		diskPath:   filepath.Join(jobDir, "disk.raw"),
		sockPath:   filepath.Join(jobDir, "net.sock"),
		configDir:  filepath.Join(jobDir, "config"),
	}, nil
}

// RunEphemeralJob bootstraps, executes, and tears down the microVM.
func (v *VMRunner) RunEphemeralJob(ctx context.Context, jitConfig string) error {
	defer v.Cleanup()

	log.Printf("[%s] Initializing ephemeral workspace in %s", v.instanceID, v.jobDir)
	if err := os.MkdirAll(v.configDir, 0755); err != nil {
		return fmt.Errorf("failed to create job directories: %w", err)
	}

	// 1. APFS Copy-on-Write Clone of base rootfs
	log.Printf("[%s] Performing APFS clonefile: %s -> %s", v.instanceID, v.cfg.VM.BaseImagePath, v.diskPath)
	if err := v.cloneRootfs(); err != nil {
		return fmt.Errorf("failed to clone base rootfs: %w", err)
	}

	// 2. Stage single-use JIT configuration
	jitFile := filepath.Join(v.configDir, "jitconfig.txt")
	if err := os.WriteFile(jitFile, []byte(jitConfig), 0600); err != nil {
		return fmt.Errorf("failed to write transient JIT config: %w", err)
	}

	// 3. Launch isolated network proxy (gvproxy)
	log.Printf("[%s] Starting network isolation proxy via gvproxy on %s", v.instanceID, v.sockPath)
	if err := v.startGvproxy(ctx); err != nil {
		return fmt.Errorf("failed to start gvproxy: %w", err)
	}

	// 4. Launch krunkit microVM
	log.Printf("[%s] Spawning GPU-accelerated microVM (vCPUs: %d, RAM: %d MiB, Virtio-GPU Venus)",
		v.instanceID, v.cfg.VM.VCPUs, v.cfg.VM.MemoryMiB)
	if err := v.startKrunkit(ctx); err != nil {
		return fmt.Errorf("krunkit execution failed: %w", err)
	}

	log.Printf("[%s] Job completed successfully and microVM powered off", v.instanceID)
	return nil
}

// cloneRootfs uses macOS 'cp -c' to perform an instantaneous APFS copy-on-write clone.
func (v *VMRunner) cloneRootfs() error {
	if _, err := os.Stat(v.cfg.VM.BaseImagePath); err != nil {
		return fmt.Errorf("base image not found at %s: %w", v.cfg.VM.BaseImagePath, err)
	}

	cmd := exec.Command("cp", "-c", v.cfg.VM.BaseImagePath, v.diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Fallback to standard copy if APFS clonefile fails (e.g. cross-volume)
		log.Printf("[%s] APFS clonefile failed (%v: %s), falling back to standard copy...", v.instanceID, err, string(out))
		fallbackCmd := exec.Command("cp", v.cfg.VM.BaseImagePath, v.diskPath)
		if fOut, fErr := fallbackCmd.CombinedOutput(); fErr != nil {
			return fmt.Errorf("copy fallback failed: %w (%s)", fErr, string(fOut))
		}
	}
	return nil
}

// startGvproxy starts gvproxy on a Unix datagram socket.
func (v *VMRunner) startGvproxy(ctx context.Context) error {
	os.Remove(v.sockPath)

	listenArg := fmt.Sprintf("unixgram://%s", v.sockPath)
	v.gvCmd = exec.CommandContext(ctx, v.cfg.VM.GvproxyPath,
		"-listen", listenArg,
	)
	v.gvCmd.Stdout = os.Stdout
	v.gvCmd.Stderr = os.Stderr

	if err := v.gvCmd.Start(); err != nil {
		return fmt.Errorf("failed to start gvproxy process: %w", err)
	}

	// Wait up to 3 seconds for socket to appear
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(v.sockPath); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("gvproxy socket %s did not initialize within timeout", v.sockPath)
}

// startKrunkit executes the microVM and blocks until the single job finishes.
func (v *VMRunner) startKrunkit(ctx context.Context) error {
	macAddr := "52:54:00:12:34:56"
	netDevice := fmt.Sprintf("virtio-net,type=unixgram,path=%s,mac=%s", v.sockPath, macAddr)
	configDevice := fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=runner-config", v.configDir)
	blkDevice := fmt.Sprintf("virtio-blk,path=%s,format=raw", v.diskPath)

	args := []string{
		"--cpus", strconv.Itoa(v.cfg.VM.VCPUs),
		"--memory", strconv.Itoa(v.cfg.VM.MemoryMiB),
		"--device", "virtio-gpu",
		"--device", netDevice,
		"--device", configDevice,
		"--device", blkDevice,
	}

	// Strictly opt-in: only mount models if explicitly configured and directory exists
	if v.cfg.VM.ModelsDir != "" {
		if fi, err := os.Stat(v.cfg.VM.ModelsDir); err == nil && fi.IsDir() {
			modelsDevice := fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=models", v.cfg.VM.ModelsDir)
			args = append(args, "--device", modelsDevice)
		}
	}

	v.krunCmd = exec.CommandContext(ctx, v.cfg.VM.KrunkitPath, args...)
	v.krunCmd.Stdout = os.Stdout
	v.krunCmd.Stderr = os.Stderr

	if err := v.krunCmd.Start(); err != nil {
		return fmt.Errorf("failed to start krunkit: %w", err)
	}

	// Wait for process exit (triggered by guest 'poweroff' upon single job completion)
	err := v.krunCmd.Wait()
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("microVM timed out or was cancelled: %w", ctx.Err())
	}
	return err
}

// Cleanup tears down processes and wipes all ephemeral files.
func (v *VMRunner) Cleanup() {
	log.Printf("[%s] Cleaning up ephemeral resources...", v.instanceID)

	// Terminate krunkit if still running
	if v.krunCmd != nil && v.krunCmd.Process != nil {
		v.krunCmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		v.krunCmd.Process.Kill()
	}

	// Terminate gvproxy
	if v.gvCmd != nil && v.gvCmd.Process != nil {
		v.gvCmd.Process.Signal(syscall.SIGTERM)
		time.Sleep(100 * time.Millisecond)
		v.gvCmd.Process.Kill()
	}

	// Wipe job directory and ephemeral disk clone
	if v.jobDir != "" {
		os.RemoveAll(v.jobDir)
	}
}
