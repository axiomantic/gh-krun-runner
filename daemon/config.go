package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

type Config struct {
	GitHub GitHubConfig `toml:"github"`
	VM     VMConfig     `toml:"vm"`
	Lock   LockConfig   `toml:"lock"`
}

type GitHubConfig struct {
	Token           string   `toml:"token"`
	Owner           string   `toml:"owner"`
	Repo            string   `toml:"repo"`
	Repos           []string `toml:"repos"`
	Labels          []string `toml:"labels"`
	PollIntervalSec int      `toml:"poll_interval_sec"`
}

type VMConfig struct {
	KrunkitPath    string `toml:"krunkit_path"`
	GvproxyPath    string `toml:"gvproxy_path"`
	BaseImagePath  string `toml:"base_image_path"`
	ModelsDir      string `toml:"models_dir"`
	VCPUs          int    `toml:"vcpus"`
	MemoryMiB      int    `toml:"memory_mib"`
	TimeoutMinutes int    `toml:"timeout_minutes"`
	WorkDir        string `toml:"work_dir"`
}

type LockConfig struct {
	LockFilePath string `toml:"lock_file_path"`
}

func LoadConfig(path string) (*Config, error) {
	cfg := &Config{
		GitHub: GitHubConfig{
			Repos:           []string{"axiomantic/*", "elijahr/*"},
			Labels:          []string{"self-hosted", "linux", "arm64", "gpu"},
			PollIntervalSec: 5,
		},
		VM: VMConfig{
			KrunkitPath:    "/opt/homebrew/bin/krunkit",
			GvproxyPath:    "/opt/homebrew/bin/gvproxy",
			BaseImagePath:  "/Users/Shared/gh-krun-runner/images/base_rootfs.raw",
			ModelsDir:      "",
			VCPUs:          6,
			MemoryMiB:      12288,
			TimeoutMinutes: 30,
			WorkDir:        "/tmp/gh-krun-runner",
		},
		Lock: LockConfig{
			LockFilePath: "/tmp/gh-krun-gpu.lock",
		},
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := toml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse config file: %w", err)
			}
		}
	}

	// Environment variable overrides
	if val := os.Getenv("GH_RUNNER_GITHUB_TOKEN"); val != "" {
		cfg.GitHub.Token = val
	} else if val := os.Getenv("GITHUB_TOKEN"); val != "" && cfg.GitHub.Token == "" {
		cfg.GitHub.Token = val
	}
	if val := os.Getenv("GH_RUNNER_BASE_IMAGE"); val != "" {
		cfg.VM.BaseImagePath = val
	}
	if val := os.Getenv("GH_RUNNER_MODELS_DIR"); val != "" {
		cfg.VM.ModelsDir = val
	}

	// Normalize single owner/repo into Repos slice if provided
	if len(cfg.GitHub.Repos) == 0 {
		if cfg.GitHub.Owner != "" && cfg.GitHub.Repo != "" {
			cfg.GitHub.Repos = []string{fmt.Sprintf("%s/%s", cfg.GitHub.Owner, cfg.GitHub.Repo)}
		} else {
			cfg.GitHub.Repos = []string{"axiomantic/*", "elijahr/*"}
		}
	}

	// Auto-discover GitHub token from gh CLI if not set or set to "auto"
	if cfg.GitHub.Token == "" || cfg.GitHub.Token == "auto" {
		if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
			t := strings.TrimSpace(string(out))
			if t != "" {
				cfg.GitHub.Token = t
			}
		}
	}

	// Fallback path resolution for krunkit and gvproxy
	if _, err := os.Stat(cfg.VM.KrunkitPath); err != nil {
		if p, err := findExecutable("krunkit"); err == nil {
			cfg.VM.KrunkitPath = p
		}
	}
	if _, err := os.Stat(cfg.VM.GvproxyPath); err != nil {
		if p, err := findExecutable("gvproxy"); err == nil {
			cfg.VM.GvproxyPath = p
		}
	}

	// Validation
	if cfg.GitHub.Token == "" {
		return nil, fmt.Errorf("missing GitHub Token: provide in config.toml or via gh auth login")
	}
	if len(cfg.GitHub.Repos) == 0 {
		return nil, fmt.Errorf("missing GitHub Repos: provide repos list in config.toml")
	}

	return cfg, nil
}

func findExecutable(name string) (string, error) {
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", name)
}

func (c *Config) String() string {
	maskedToken := "none"
	if len(c.GitHub.Token) > 6 {
		maskedToken = c.GitHub.Token[:3] + "..." + c.GitHub.Token[len(c.GitHub.Token)-3:]
	}
	return fmt.Sprintf("Repos: [%s] | Labels: [%s] | vCPUs: %d | RAM: %d MiB | Token: %s",
		strings.Join(c.GitHub.Repos, ", "), strings.Join(c.GitHub.Labels, ", "), c.VM.VCPUs, c.VM.MemoryMiB, maskedToken)
}
