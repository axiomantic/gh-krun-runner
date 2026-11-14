package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.toml", "Path to config.toml")
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	log.Println("==================================================================")
	log.Println("=== GitHub Actions Linux ARM64 Ephemeral GPU Runner Orchestrator ===")
	log.Println("==================================================================")

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("[FATAL] Failed to load configuration: %v", err)
	}

	log.Printf("[INFO] Active Configuration: %s", cfg.String())
	log.Printf("[INFO] Base Disk Image: %s", cfg.VM.BaseImagePath)
	if cfg.VM.ModelsDir != "" {
		log.Printf("[INFO] Models Mount: %s", cfg.VM.ModelsDir)
	} else {
		log.Printf("[INFO] Models Mount: <disabled> (100%% sealed sandbox, zero host fs mounts)")
	}
	log.Printf("[INFO] Single-Concurrency GPU Lock: %s", cfg.Lock.LockFilePath)

	// Ensure work directory exists
	if err := os.MkdirAll(cfg.VM.WorkDir, 0755); err != nil {
		log.Fatalf("[FATAL] Failed to create work directory: %v", err)
	}

	gpuLock := NewGPULock(cfg.Lock.LockFilePath)
	ghClient := NewGitHubClient(cfg.GitHub.Token, cfg.GitHub.Repos, cfg.GitHub.Labels)

	// Initial discovery of active repositories
	if active, err := ghClient.DiscoverActiveRepos(); err == nil {
		log.Printf("[INFO] Initial active repositories watched (%d): %v", len(active), active)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful termination
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("[INFO] Received signal %v. Initiating graceful shutdown...", sig)
		cancel()
	}()

	ticker := time.NewTicker(time.Duration(cfg.GitHub.PollIntervalSec) * time.Second)
	defer ticker.Stop()

	log.Println("[INFO] Orchestrator daemon is actively polling for queued GPU jobs across all watched repos...")

	for {
		select {
		case <-ctx.Done():
			log.Println("[INFO] Daemon shut down cleanly.")
			return
		case <-ticker.C:
			// 1. Enforce single-concurrency GPU lock
			if !gpuLock.TryLock() {
				// Lock is held by an active VM or another process
				continue
			}

			// 2. Poll for queued matching job across watched repositories
			match, err := ghClient.FindMatchingQueuedJob()
			if err != nil {
				log.Printf("[WARN] Error polling GitHub Actions API: %v", err)
				gpuLock.Unlock()
				continue
			}

			if match == nil {
				// No matching job queued
				gpuLock.Unlock()
				continue
			}

			job := match.Job
			log.Printf("[INFO] Detected queued job #%d (%q) matching labels %v in %s/%s (run #%d)",
				job.ID, job.Name, job.Labels, match.Owner, match.Repo, job.RunID)

			// 3. Generate single-use JIT configuration for the target repo
			runnerName := fmt.Sprintf("krun-%s-%d", match.Repo, job.ID)
			jitConfig, err := ghClient.GenerateJITConfig(match.Owner, match.Repo, runnerName)
			if err != nil {
				log.Printf("[ERROR] Failed to generate JIT config for %s/%s job #%d: %v",
					match.Owner, match.Repo, job.ID, err)
				gpuLock.Unlock()
				continue
			}

			// 4. Create and execute ephemeral microVM
			vmRunner, err := NewVMRunner(cfg)
			if err != nil {
				log.Printf("[ERROR] Failed to initialize VM runner: %v", err)
				gpuLock.Unlock()
				continue
			}

			// Run job under hard timeout context
			jobTimeout := time.Duration(cfg.VM.TimeoutMinutes) * time.Minute
			jobCtx, jobCancel := context.WithTimeout(ctx, jobTimeout)

			log.Printf("[INFO] Starting execution for %s/%s job #%d with %v timeout...",
				match.Owner, match.Repo, job.ID, jobTimeout)
			runErr := vmRunner.RunEphemeralJob(jobCtx, jitConfig)
			jobCancel()

			if runErr != nil {
				log.Printf("[ERROR] Job #%d execution failed: %v", job.ID, runErr)
			} else {
				log.Printf("[SUCCESS] Job #%d finished cleanly", job.ID)
			}

			// 5. Release GPU lock for next job
			gpuLock.Unlock()
		}
	}
}
