package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

const updatesPersistPath = "/config/update-status.json"

// UpdateStatus represents the update check result for a single container
type UpdateStatus struct {
	ContainerName string    `json:"containerName"`
	Image         string    `json:"image"`
	LocalDigest   string    `json:"localDigest"`
	RemoteDigest  string    `json:"remoteDigest"`
	HasUpdate     bool      `json:"hasUpdate"`
	Error         string    `json:"error,omitempty"`
	CheckedAt     time.Time `json:"checkedAt"`
}

// UpdateChecker runs periodic image update checks using regctl
type UpdateChecker struct {
	mu            sync.RWMutex
	docker        *client.Client
	results       map[string]*UpdateStatus
	lastFullRun   time.Time
	checking      bool
	wasEnabled    bool
	manualTrigger chan struct{}
}

// NewUpdateChecker creates a new update checker
func NewUpdateChecker(docker *client.Client) *UpdateChecker {
	uc := &UpdateChecker{
		docker:        docker,
		results:       make(map[string]*UpdateStatus),
		manualTrigger: make(chan struct{}, 1),
	}
	uc.loadFromDisk()
	return uc
}

// Run starts the periodic update check loop
func (uc *UpdateChecker) Run(ctx context.Context) {
	// Check if regctl is available
	if _, err := exec.LookPath("regctl"); err != nil {
		log.Println("Updates: regctl not found, update checking disabled")
		return
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Initial check if stale
	if uc.shouldRun() {
		uc.runCheck(ctx)
	}

	for {
		select {
		case <-ctx.Done():
			uc.saveToDisk()
			return
		case <-ticker.C:
			if uc.shouldRun() {
				uc.runCheck(ctx)
			}
		case <-uc.manualTrigger:
			uc.runCheck(ctx)
		}
	}
}

func (uc *UpdateChecker) shouldRun() bool {
	cfg, err := ReadConfig(configPath)
	if err != nil || cfg.UpdateCheckEnabled != "true" {
		uc.wasEnabled = false
		return false
	}
	// Run immediately when first enabled (or re-enabled)
	if !uc.wasEnabled {
		uc.wasEnabled = true
		return true
	}
	interval := parseInterval(cfg.UpdateCheckInterval)
	return time.Since(uc.lastFullRun) >= interval
}

func parseInterval(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d >= time.Hour {
		return d
	}
	return 12 * time.Hour
}

func (uc *UpdateChecker) runCheck(ctx context.Context) {
	uc.mu.Lock()
	if uc.checking {
		uc.mu.Unlock()
		return
	}
	uc.checking = true
	uc.mu.Unlock()

	log.Println("Updates: starting update check...")

	defer func() {
		uc.mu.Lock()
		uc.checking = false
		uc.lastFullRun = time.Now()
		uc.mu.Unlock()
		uc.saveToDisk()
	}()

	cfg, err := ReadConfig(configPath)
	if err != nil {
		log.Printf("Updates: failed to read config: %v", err)
		return
	}

	// Build exclude set
	excludeSet := make(map[string]bool)
	for _, name := range strings.Split(cfg.UpdateExclude, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			excludeSet[strings.ToLower(name)] = true
		}
	}

	// List all containers
	containers, err := uc.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		log.Printf("Updates: failed to list containers: %v", err)
		return
	}

	// Clean up stale results for removed containers
	currentNames := make(map[string]bool, len(containers))
	for _, c := range containers {
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		currentNames[name] = true
	}
	uc.mu.Lock()
	for k := range uc.results {
		if !currentNames[k] {
			delete(uc.results, k)
		}
	}
	uc.mu.Unlock()

	// Track previously known updates to avoid duplicate Discord notifications
	uc.mu.RLock()
	previousUpdates := make(map[string]bool)
	for k, v := range uc.results {
		if v.HasUpdate {
			previousUpdates[k] = true
		}
	}
	uc.mu.RUnlock()

	var newUpdateNames []string

	for _, c := range containers {
		// Check for shutdown
		select {
		case <-ctx.Done():
			return
		default:
		}

		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		if excludeSet[strings.ToLower(name)] {
			log.Printf("Updates: skipping %s (excluded)", name)
			continue
		}

		imageRef := c.Image
		status := &UpdateStatus{
			ContainerName: name,
			Image:         imageRef,
			CheckedAt:     time.Now(),
		}

		// Skip digest-pinned images
		if strings.Contains(imageRef, "@sha256:") {
			log.Printf("Updates: skipping %s (pinned image)", name)
			status.Error = "pinned image"
			uc.setResult(name, status)
			continue
		}

		// Get local RepoDigests
		imageInspect, _, err := uc.docker.ImageInspectWithRaw(ctx, c.ImageID)
		if err != nil {
			log.Printf("Updates: %s — inspect failed: %v", name, err)
			status.Error = fmt.Sprintf("inspect failed: %v", err)
			uc.setResult(name, status)
			continue
		}

		if len(imageInspect.RepoDigests) == 0 {
			log.Printf("Updates: skipping %s (local image)", name)
			status.Error = "local image, no registry digest"
			uc.setResult(name, status)
			continue
		}
		status.LocalDigest = extractDigestFromRepoDigests(imageInspect.RepoDigests)

		// Normalize image ref — ensure tag
		if !strings.Contains(imageRef, ":") {
			imageRef += ":latest"
		}

		// Get remote digest via regctl
		remoteDigest, err := uc.getRemoteDigest(ctx, imageRef)
		if err != nil {
			log.Printf("Updates: %s — registry error: %s", name, err)
			status.Error = err.Error()
			uc.setResult(name, status)
			time.Sleep(1 * time.Second)
			continue
		}
		status.RemoteDigest = remoteDigest

		// Compare
		localDigests := strings.Join(imageInspect.RepoDigests, " ")
		status.HasUpdate = !strings.Contains(localDigests, remoteDigest)

		if status.HasUpdate {
			log.Printf("Updates: %s — UPDATE AVAILABLE (%s)", name, imageRef)
			if !previousUpdates[name] {
				newUpdateNames = append(newUpdateNames, name)
			}
		} else {
			log.Printf("Updates: %s — up to date", name)
		}

		uc.setResult(name, status)
		time.Sleep(1 * time.Second) // Rate limit between checks
	}

	// Count total updates
	uc.mu.RLock()
	totalUpdates := 0
	for _, v := range uc.results {
		if v.HasUpdate {
			totalUpdates++
		}
	}
	uc.mu.RUnlock()

	log.Printf("Updates: complete — %d checked, %d up to date, %d updates, %d new", len(containers), len(containers)-totalUpdates, totalUpdates, len(newUpdateNames))

	// Discord notification only for NEW updates
	if len(newUpdateNames) > 0 {
		var lines []string
		uc.mu.RLock()
		for _, name := range newUpdateNames {
			if r, ok := uc.results[name]; ok {
				lines = append(lines, fmt.Sprintf("• **%s** — `%s`", name, r.Image))
			}
		}
		uc.mu.RUnlock()
		description := fmt.Sprintf("%d new update(s) available:\n%s", len(newUpdateNames), strings.Join(lines, "\n"))
		go sendDiscordMaintenance("Image Updates Available", description, 0xd29922)
	}
}

func (uc *UpdateChecker) getRemoteDigest(ctx context.Context, imageRef string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, "regctl", "image", "digest", "--list", imageRef)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("%s", errMsg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func extractDigestFromRepoDigests(repoDigests []string) string {
	for _, rd := range repoDigests {
		if idx := strings.LastIndex(rd, "@"); idx >= 0 {
			return rd[idx+1:]
		}
	}
	return ""
}

func (uc *UpdateChecker) setResult(name string, status *UpdateStatus) {
	uc.mu.Lock()
	uc.results[name] = status
	uc.mu.Unlock()
}

// GetResults returns a copy of all update check results
func (uc *UpdateChecker) GetResults() map[string]*UpdateStatus {
	uc.mu.RLock()
	defer uc.mu.RUnlock()
	copy := make(map[string]*UpdateStatus, len(uc.results))
	for k, v := range uc.results {
		copy[k] = v
	}
	return copy
}

// IsChecking returns whether a check is currently running
func (uc *UpdateChecker) IsChecking() bool {
	uc.mu.RLock()
	defer uc.mu.RUnlock()
	return uc.checking
}

// LastCheck returns the time of the last full check run
func (uc *UpdateChecker) LastCheck() time.Time {
	uc.mu.RLock()
	defer uc.mu.RUnlock()
	return uc.lastFullRun
}

// TriggerCheck triggers an immediate update check
func (uc *UpdateChecker) TriggerCheck() {
	select {
	case uc.manualTrigger <- struct{}{}:
	default:
		// Already triggered, ignore
	}
}

func (uc *UpdateChecker) saveToDisk() {
	uc.mu.RLock()
	data := struct {
		Results     map[string]*UpdateStatus `json:"results"`
		LastFullRun time.Time                `json:"lastFullRun"`
	}{uc.results, uc.lastFullRun}
	uc.mu.RUnlock()

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("Updates: failed to marshal: %v", err)
		return
	}
	if err := os.WriteFile(updatesPersistPath, raw, 0664); err != nil {
		log.Printf("Updates: failed to save: %v", err)
		return
	}
	_ = os.Chown(updatesPersistPath, 99, 100)
}

func (uc *UpdateChecker) loadFromDisk() {
	raw, err := os.ReadFile(updatesPersistPath)
	if err != nil {
		return
	}
	var data struct {
		Results     map[string]*UpdateStatus `json:"results"`
		LastFullRun time.Time                `json:"lastFullRun"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Printf("Updates: failed to parse saved data: %v", err)
		return
	}
	if data.Results != nil {
		uc.results = data.Results
	}
	uc.lastFullRun = data.LastFullRun
}
