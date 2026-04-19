package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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

// UpdateChecker runs periodic image update checks using the Docker daemon's
// /distribution/{ref}/json endpoint (DistributionInspect). Credentials for
// private registries come from the RegistryStore.
type UpdateChecker struct {
	mu             sync.RWMutex
	docker         *client.Client
	registry       *RegistryStore
	results        map[string]*UpdateStatus
	lastFullRun    time.Time
	checking       bool
	wasEnabled     bool
	manualTrigger  chan struct{}
	checkProgress  int    // containers processed in current run
	checkTotal     int    // total containers queued for current run
	checkCurrent   string // container currently being checked
}

// NewUpdateChecker creates a new update checker
func NewUpdateChecker(docker *client.Client, registry *RegistryStore) *UpdateChecker {
	uc := &UpdateChecker{
		docker:        docker,
		registry:      registry,
		results:       make(map[string]*UpdateStatus),
		manualTrigger: make(chan struct{}, 1),
	}
	uc.loadFromDisk()
	return uc
}

// Run starts the periodic update check loop
func (uc *UpdateChecker) Run(ctx context.Context) {
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
		uc.mu.Lock()
		uc.wasEnabled = false
		uc.mu.Unlock()
		return false
	}
	uc.mu.Lock()
	defer uc.mu.Unlock()
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
	uc.checkProgress = 0
	uc.checkTotal = 0
	uc.checkCurrent = ""
	uc.mu.Unlock()

	log.Println("Updates: starting update check...")

	defer func() {
		uc.mu.Lock()
		uc.checking = false
		uc.checkProgress = 0
		uc.checkTotal = 0
		uc.checkCurrent = ""
		uc.lastFullRun = time.Now()
		uc.mu.Unlock()
		uc.saveToDisk()
	}()

	cfg, err := ReadConfig(configPath)
	if err != nil {
		log.Printf("Updates: failed to read config: %v", err)
		return
	}

	// Snapshot registry auths once up front. All per-container auth lookups
	// during this run read from this snapshot instead of re-opening
	// config.json, which would otherwise happen dozens of times per check.
	var authSnap AuthSnapshot
	if uc.registry != nil {
		authSnap = uc.registry.Snapshot()
	} else {
		authSnap = AuthSnapshot{}
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

	// Single pass: collect names, build the "currently exists" set for
	// stale-result cleanup, and compute the progress total (excluding any
	// containers that match the exclude set).
	currentNames := make(map[string]bool, len(containers))
	total := 0
	for _, c := range containers {
		n := c.ID[:12]
		if len(c.Names) > 0 {
			n = strings.TrimPrefix(c.Names[0], "/")
		}
		currentNames[n] = true
		if !excludeSet[strings.ToLower(n)] {
			total++
		}
	}

	uc.mu.Lock()
	uc.checkTotal = total
	for k := range uc.results {
		if !currentNames[k] || excludeSet[strings.ToLower(k)] {
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

		// Progress is tracked per-iteration (one tick for every container
		// we actually attempt — excluded ones were already subtracted from
		// checkTotal above). The checkCurrent field is only used for the
		// UI's "Checking name..." label.
		uc.mu.Lock()
		uc.checkCurrent = name
		uc.checkProgress++
		uc.mu.Unlock()

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

		// When c.Image is a valid tag (the normal case), respect it — never
		// override with RepoTags[0] because Docker may order multiple tags
		// unpredictably and we'd report the wrong tag for multi-tagged images.
		// Only fall back when c.Image is a bare "sha256:..." (tag displaced
		// locally because the container template references a tag that no
		// longer matches any local tag — common when an upstream repo gets
		// renamed and the user updated their template but never re-pulled).
		if strings.HasPrefix(imageRef, "sha256:") {
			// 1. Prefer Config.Image from the container itself — that's the
			//    user's intent at create time and is the most authoritative
			//    source for "what should this be checked against".
			if inspect, err := uc.docker.ContainerInspect(ctx, c.ID); err == nil &&
				inspect.Config != nil && inspect.Config.Image != "" &&
				!strings.HasPrefix(inspect.Config.Image, "sha256:") {
				imageRef = inspect.Config.Image
				status.Image = imageRef
				log.Printf("Updates: %s — c.Image was sha256, recovered from container Config.Image: %s", name, imageRef)
			} else if len(imageInspect.RepoTags) > 0 && imageInspect.RepoTags[0] != "<none>:<none>" {
				// 2. Container inspect failed or also returned sha256 — fall
				//    back to whatever tag the local image actually carries.
				imageRef = imageInspect.RepoTags[0]
				status.Image = imageRef
				log.Printf("Updates: %s — c.Image was sha256, recovered from RepoTags: %s", name, imageRef)
			}
		}
		if strings.HasPrefix(imageRef, "sha256:") {
			// Tagless image — try to recover the tag from the OCI title label.
			// Publishers like hotio set org.opencontainers.image.title to "name:tag"
			// which lets us rebuild the full ref when combined with RepoDigests.
			recovered := ""
			repoName := ""
			for _, rd := range imageInspect.RepoDigests {
				if idx := strings.LastIndex(rd, "@"); idx >= 0 {
					repoName = rd[:idx]
					break
				}
			}
			if repoName != "" && imageInspect.Config != nil {
				if title := imageInspect.Config.Labels["org.opencontainers.image.title"]; title != "" {
					if colonIdx := strings.LastIndex(title, ":"); colonIdx >= 0 && colonIdx < len(title)-1 {
						tag := title[colonIdx+1:]
						recovered = repoName + ":" + tag
					}
				}
			}
			if recovered != "" {
				log.Printf("Updates: %s — tag recovered from image.title label: %s", name, recovered)
				imageRef = recovered
				status.Image = recovered
			} else {
				log.Printf("Updates: skipping %s (tagless image — original tag displaced locally)", name)
				if repoName != "" {
					status.Error = fmt.Sprintf("tagless local image — repull %s with your desired tag to enable update checks", repoName)
					status.Image = repoName + " (tag unknown)"
				} else {
					status.Error = "tagless local image — repull the original tag to enable update checks"
				}
				uc.setResult(name, status)
				continue
			}
		}

		// Normalize image ref — ensure tag
		if !strings.Contains(imageRef, ":") {
			imageRef += ":latest"
		}

		// Ask the Docker daemon for the current remote digest. The daemon
		// contacts the registry directly and handles bearer-token auth,
		// multi-arch manifest lists, and redirects.
		remoteDigest, err := uc.getRemoteDigest(ctx, imageRef, authSnap)
		if err != nil {
			// Fallback: if the registry says the image doesn't exist, the
			// container reference is probably stale (e.g. user renamed the
			// repo on GHCR but the container still points at the old name).
			// Retry using whatever tag the local image is actually labeled
			// with — that reflects the last successful pull.
			errStr := strings.ToLower(err.Error())
			isMissing := strings.Contains(errStr, "unauthorized") ||
				strings.Contains(errStr, "manifest unknown") ||
				strings.Contains(errStr, "not found") ||
				strings.Contains(errStr, "repository name not known")
			if isMissing && len(imageInspect.RepoTags) > 0 &&
				imageInspect.RepoTags[0] != "<none>:<none>" &&
				imageInspect.RepoTags[0] != imageRef {
				altRef := imageInspect.RepoTags[0]
				log.Printf("Updates: %s — %q failed (%s), retrying with local tag %q", name, imageRef, err, altRef)
				if altDigest, altErr := uc.getRemoteDigest(ctx, altRef, authSnap); altErr == nil {
					imageRef = altRef
					status.Image = altRef
					remoteDigest = altDigest
					err = nil
				}
			}
			if err != nil {
				log.Printf("Updates: %s — registry error: %s", name, err)
				status.Error = err.Error()
				uc.setResult(name, status)
				continue
			}
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
		// No explicit rate limit here: the Docker daemon queues and
		// serializes registry calls internally, and the registries we
		// care about (GHCR, Docker Hub, GitLab, Quay) all tolerate
		// back-to-back manifest HEAD requests at this cadence. The
		// previous 1s sleep existed to throttle a regctl subprocess
		// that has since been removed.
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
				lines = append(lines, fmt.Sprintf("• **%s** — `%s`  ", name, r.Image))
			}
		}
		uc.mu.RUnlock()
		description := fmt.Sprintf("%d new update(s) available:\n%s", len(newUpdateNames), strings.Join(lines, "\n"))
		go sendDiscordMaintenance("Image Updates Available", description, 0xd29922)
		go sendGotifyMaintenance("Image Updates Available", fmt.Sprintf("%d new update(s) available:\n\n%s", len(newUpdateNames), strings.Join(lines, "\n")))
	}
}

// getRemoteDigest asks the Docker daemon for the current registry digest of
// an image reference. The daemon contacts the registry itself and handles
// multi-arch manifest lists, redirects, and bearer-token auth internally.
//
// Auth handling:
//
//  1. If Constat has stored credentials for the image's registry host, try
//     with auth first — this is needed for private packages.
//  2. If the authenticated request is rejected with "denied"/"unauthorized",
//     retry anonymously. GHCR specifically returns "denied: denied" when
//     a PAT is sent for a package the token owner doesn't have explicit
//     access to, EVEN IF the package is publicly pullable. Anonymous works
//     in that case, so we fall through.
//  3. If there are no stored credentials, go straight to an anonymous call.
//
// The authSnap argument is an in-memory snapshot taken at the start of the
// check run so we don't re-read config.json once per container.
func (uc *UpdateChecker) getRemoteDigest(ctx context.Context, imageRef string, authSnap AuthSnapshot) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	encodedAuth := BuildRegistryAuthFrom(authSnap, imageRef)

	resp, err := uc.docker.DistributionInspect(checkCtx, imageRef, encodedAuth)
	if err != nil && encodedAuth != "" && isAuthRejection(err) {
		// Retry anonymously — our creds don't grant access to this specific
		// package, but it may be public.
		log.Printf("Updates: %s — auth rejected (%v), retrying anonymously", imageRef, err)
		resp, err = uc.docker.DistributionInspect(checkCtx, imageRef, "")
	}
	if err != nil {
		msg := err.Error()
		// Clean up the common prefix the daemon adds to HEAD errors so
		// users see "unauthorized" rather than a 200-character wrapper.
		if idx := strings.Index(msg, ": "); idx >= 0 && idx < 60 {
			msg = strings.TrimSpace(msg[idx+2:])
		}
		return "", fmt.Errorf("%s", msg)
	}
	digest := string(resp.Descriptor.Digest)
	if digest == "" {
		return "", fmt.Errorf("registry returned empty digest")
	}
	return digest, nil
}

// isAuthRejection reports whether an error from DistributionInspect looks
// like the registry denying our credentials (vs. network/transport errors
// or legitimate "image not found"). Matches the common phrases GHCR, Docker
// Hub, and standard OCI registries return.
func isAuthRejection(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "denied: denied") ||
		strings.Contains(s, "denied: requested access to the resource is denied") ||
		strings.Contains(s, "403 forbidden")
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

// Progress returns the current check's progress: containers processed,
// total queued, and the name of the container being checked right now.
// When no check is running all three return zero values.
func (uc *UpdateChecker) Progress() (done, total int, current string) {
	uc.mu.RLock()
	defer uc.mu.RUnlock()
	return uc.checkProgress, uc.checkTotal, uc.checkCurrent
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
	if err := atomicWriteFile(updatesPersistPath, raw, 0664); err != nil {
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
	uc.mu.Lock()
	defer uc.mu.Unlock()
	if data.Results != nil {
		uc.results = data.Results
	}
	uc.lastFullRun = data.LastFullRun
}
