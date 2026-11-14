package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type GitHubClient struct {
	token           string
	patterns        []string
	labels          []string
	httpClient      *http.Client
	discoveredRepos []string
	lastDiscovery   time.Time
}

type UserRepoItem struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	Disabled bool   `json:"disabled"`
}

type WorkflowRunsResponse struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []WorkflowRun `json:"workflow_runs"`
}

type WorkflowRun struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type WorkflowJobsResponse struct {
	TotalCount int           `json:"total_count"`
	Jobs       []WorkflowJob `json:"jobs"`
}

type WorkflowJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

type QueuedJobMatch struct {
	Job   *WorkflowJob
	Owner string
	Repo  string
}

type GenerateJITConfigRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int      `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder"`
}

type GenerateJITConfigResponse struct {
	Runner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

func NewGitHubClient(token string, repos []string, labels []string) *GitHubClient {
	return &GitHubClient{
		token:    token,
		patterns: repos,
		labels:   labels,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (gh *GitHubClient) newRequest(method, url string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+gh.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// DiscoverActiveRepos refreshes the active candidate repositories based on configured patterns.
func (gh *GitHubClient) DiscoverActiveRepos() ([]string, error) {
	if time.Since(gh.lastDiscovery) < 2*time.Minute && len(gh.discoveredRepos) > 0 {
		return gh.discoveredRepos, nil
	}

	hasWildcard := false
	var exactRepos []string
	for _, p := range gh.patterns {
		if strings.Contains(p, "*") {
			hasWildcard = true
		} else if strings.Contains(p, "/") {
			exactRepos = append(exactRepos, strings.TrimSpace(p))
		}
	}

	repoMap := make(map[string]bool)
	for _, r := range exactRepos {
		repoMap[r] = true
	}

	if hasWildcard {
		// Fetch 50 most recently pushed repositories accessible by this token
		url := "https://api.github.com/user/repos?sort=pushed&direction=desc&per_page=50"
		req, err := gh.newRequest("GET", url, nil)
		if err == nil {
			resp, err := gh.httpClient.Do(req)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					var items []UserRepoItem
					if err := json.NewDecoder(resp.Body).Decode(&items); err == nil {
						for _, item := range items {
							if item.Archived || item.Disabled {
								continue
							}
							if gh.matchesPattern(item.FullName) {
								repoMap[item.FullName] = true
							}
						}
					}
				}
			}
		}
	}

	var results []string
	for r := range repoMap {
		results = append(results, r)
	}

	gh.discoveredRepos = results
	gh.lastDiscovery = time.Now()
	log.Printf("[INFO] Discovered %d active repositories matching criteria: %v", len(results), results)
	return results, nil
}

func (gh *GitHubClient) matchesPattern(fullName string) bool {
	cleanName := strings.ToLower(strings.TrimSpace(fullName))
	for _, p := range gh.patterns {
		pattern := strings.ToLower(strings.TrimSpace(p))
		if strings.HasSuffix(pattern, "/*") {
			prefix := strings.TrimSuffix(pattern, "/*")
			if strings.HasPrefix(cleanName, prefix+"/") {
				return true
			}
		} else if pattern == cleanName {
			return true
		}
	}
	return false
}

// FindMatchingQueuedJob scans active repositories for queued workflow jobs requiring runner labels.
func (gh *GitHubClient) FindMatchingQueuedJob() (*QueuedJobMatch, error) {
	repos, err := gh.DiscoverActiveRepos()
	if err != nil {
		return nil, err
	}

	statuses := []string{"queued", "in_progress"}

	for _, fullName := range repos {
		parts := strings.Split(fullName, "/")
		if len(parts) != 2 {
			continue
		}
		owner, repo := parts[0], parts[1]

		for _, status := range statuses {
			url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs?status=%s&per_page=5",
				owner, repo, status)

			req, err := gh.newRequest("GET", url, nil)
			if err != nil {
				continue
			}

			resp, err := gh.httpClient.Do(req)
			if err != nil {
				continue
			}

			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				continue
			}

			var runsResp WorkflowRunsResponse
			err = json.NewDecoder(resp.Body).Decode(&runsResp)
			resp.Body.Close()
			if err != nil || runsResp.TotalCount == 0 {
				continue
			}

			for _, run := range runsResp.WorkflowRuns {
				jobsURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d/jobs?filter=queued",
					owner, repo, run.ID)

				jReq, err := gh.newRequest("GET", jobsURL, nil)
				if err != nil {
					continue
				}

				jResp, err := gh.httpClient.Do(jReq)
				if err != nil {
					continue
				}

				var jobsResp WorkflowJobsResponse
				err = json.NewDecoder(jResp.Body).Decode(&jobsResp)
				jResp.Body.Close()
				if err != nil || len(jobsResp.Jobs) == 0 {
					continue
				}

				for _, job := range jobsResp.Jobs {
					if job.Status == "queued" && gh.matchesLabels(job.Labels) {
						return &QueuedJobMatch{
							Job:   &job,
							Owner: owner,
							Repo:  repo,
						}, nil
					}
				}
			}
		}
	}

	return nil, nil
}

func (gh *GitHubClient) matchesLabels(jobLabels []string) bool {
	if len(jobLabels) == 0 {
		return false
	}

	runnerLabelSet := make(map[string]bool)
	for _, l := range gh.labels {
		runnerLabelSet[strings.ToLower(strings.TrimSpace(l))] = true
	}

	for _, reqLabel := range jobLabels {
		clean := strings.ToLower(strings.TrimSpace(reqLabel))
		if !runnerLabelSet[clean] {
			return false
		}
	}
	return true
}

// GenerateJITConfig creates a single-use JIT configuration for the targeted repository.
func (gh *GitHubClient) GenerateJITConfig(owner, repo, runnerName string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners/generate-jitconfig", owner, repo)

	payload := GenerateJITConfigRequest{
		Name:          runnerName,
		RunnerGroupID: 1, // Default group
		Labels:        gh.labels,
		WorkFolder:    "_work",
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := gh.newRequest("POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := gh.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("generate-jitconfig failed on %s/%s (%d): %s", owner, repo, resp.StatusCode, string(respBody))
	}

	var jitResp GenerateJITConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&jitResp); err != nil {
		return "", err
	}

	return jitResp.EncodedJITConfig, nil
}
