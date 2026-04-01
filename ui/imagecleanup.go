package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// ImageCleanupResult holds the result of the last scheduled cleanup
type ImageCleanupResult struct {
	Time            time.Time `json:"time"`
	DryRun          bool      `json:"dryRun"`
	Mode            string    `json:"mode"`
	ImagesFound     int       `json:"imagesFound"`
	ImagesDeleted   int       `json:"imagesDeleted"`
	VolumesDeleted  int       `json:"volumesDeleted"`
	SpaceReclaimed  int64     `json:"spaceReclaimed"`
	Error           string    `json:"error,omitempty"`
}

// ImageCleaner runs scheduled image cleanup
type ImageCleaner struct {
	docker      *client.Client
	app         *App
	lastRunYear int
	lastRunDay  int // day of year when last run happened
	mu          sync.Mutex
	lastResult  *ImageCleanupResult
}

// GetLastResult returns the last cleanup result (thread-safe)
func (ic *ImageCleaner) GetLastResult() *ImageCleanupResult {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	return ic.lastResult
}

func (ic *ImageCleaner) setLastResult(r *ImageCleanupResult) {
	ic.mu.Lock()
	defer ic.mu.Unlock()
	ic.lastResult = r
}

// Run starts the scheduler loop, checking every 60 seconds
func (ic *ImageCleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	log.Println("ImageCleaner: scheduler started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ic.check(ctx)
		}
	}
}

func (ic *ImageCleaner) check(ctx context.Context) {
	cfg, err := ReadConfig(configPath)
	if err != nil {
		log.Printf("ImageCleaner: failed to read config: %v", err)
		return
	}

	if cfg.ImageCleanupEnabled != "true" {
		return
	}

	// Parse scheduled time (supports 24h and 12h AM/PM)
	parsed, err := parseCleanupTime(cfg.ImageCleanupTime)
	if err != nil {
		log.Printf("ImageCleaner: invalid cleanup time %q: %v", cfg.ImageCleanupTime, err)
		return
	}
	hour, minute := parsed[0], parsed[1]

	// Get current time in configured timezone
	now := time.Now()
	if cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			now = now.In(loc)
		}
	}

	// Check if current time matches and we haven't run today
	year, dayOfYear := now.Year(), now.YearDay()
	if now.Hour() == hour && now.Minute() == minute && (ic.lastRunYear != year || ic.lastRunDay != dayOfYear) {
		ic.lastRunYear = year
		ic.lastRunDay = dayOfYear
		dryRun := cfg.ImageCleanupDryRun == "true"
		orphans := cfg.CleanupOrphanImages == "true"
		unused := cfg.CleanupUnusedImages == "true"
		volumes := cfg.CleanupVolumes == "true"
		if !orphans && !unused && !volumes {
			return // nothing enabled
		}
		ic.runCleanup(ctx, orphans, unused, volumes, dryRun)
	}
}

func (ic *ImageCleaner) runCleanup(ctx context.Context, orphans, unused, volumes, dryRun bool) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Build mode string for logging/display
	var parts []string
	if orphans { parts = append(parts, "orphans") }
	if unused { parts = append(parts, "unused") }
	if volumes { parts = append(parts, "volumes") }
	mode := strings.Join(parts, "+")

	result := &ImageCleanupResult{
		Time:   time.Now(),
		DryRun: dryRun,
		Mode:   mode,
	}

	if dryRun {
		// Dry-run images
		if orphans || unused {
			images, err := ic.app.listImages(cleanupCtx)
			if err != nil {
				result.Error = err.Error()
				log.Printf("Cleanup [dry-run]: error listing images: %v", err)
				ic.setLastResult(result)
				return
			}
			for _, img := range images {
				if orphans && img.Status == "dangling" {
					result.ImagesFound++
					result.SpaceReclaimed += img.Size
				} else if unused && img.Status == "unused" {
					result.ImagesFound++
					result.SpaceReclaimed += img.Size
				}
			}
		}
		// Dry-run volumes
		if volumes {
			vols, err := ic.app.listVolumes(cleanupCtx)
			if err != nil {
				result.Error = err.Error()
				log.Printf("Cleanup [dry-run]: error listing volumes: %v", err)
				ic.setLastResult(result)
				return
			}
			for _, v := range vols {
				if v.Status == "unused" {
					result.VolumesDeleted++
				}
			}
		}
		log.Printf("Cleanup [dry-run]: %d images, %d volumes would be removed, %s reclaimable (%s)",
			result.ImagesFound, result.VolumesDeleted, formatBytesGo(result.SpaceReclaimed), mode)
		ic.setLastResult(result)
		ic.sendCleanupDiscord(result)
		return
	}

	// Prune orphan images (dangling)
	if orphans {
		pruneFilters := filters.NewArgs()
		pruneFilters.Add("dangling", "true")
		report, err := ic.docker.ImagesPrune(cleanupCtx, pruneFilters)
		if err != nil {
			result.Error = fmt.Sprintf("orphan prune: %v", err)
			log.Printf("Cleanup: error pruning orphan images: %v", err)
			ic.setLastResult(result)
			return
		}
		result.ImagesDeleted += len(report.ImagesDeleted)
		result.SpaceReclaimed += int64(report.SpaceReclaimed)
	}
	// Prune unused images (tagged but unreferenced)
	if unused {
		pruneFilters := filters.NewArgs()
		pruneFilters.Add("dangling", "false")
		report, err := ic.docker.ImagesPrune(cleanupCtx, pruneFilters)
		if err != nil {
			result.Error = fmt.Sprintf("unused prune: %v", err)
			log.Printf("Cleanup: error pruning unused images: %v", err)
			ic.setLastResult(result)
			return
		}
		result.ImagesDeleted += len(report.ImagesDeleted)
		result.SpaceReclaimed += int64(report.SpaceReclaimed)
	}
	// Prune unused volumes
	if volumes {
		report, err := ic.docker.VolumesPrune(cleanupCtx, filters.NewArgs())
		if err != nil {
			result.Error = fmt.Sprintf("volume prune: %v", err)
			log.Printf("Cleanup: error pruning volumes: %v", err)
			ic.setLastResult(result)
			return
		}
		result.VolumesDeleted = len(report.VolumesDeleted)
		result.SpaceReclaimed += int64(report.SpaceReclaimed)
	}
	log.Printf("Cleanup: %d images, %d volumes removed, %s reclaimed (%s)",
		result.ImagesDeleted, result.VolumesDeleted, formatBytesGo(result.SpaceReclaimed), mode)

	ic.setLastResult(result)
	ic.sendCleanupDiscord(result)
}

var timePattern24 = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
var timePattern12 = regexp.MustCompile(`(?i)^(\d{1,2}):(\d{2})\s*(AM|PM)$`)

// parseCleanupTime parses "HH:MM" (24h) or "HH:MM AM/PM" (12h) and returns [hour24, minute]
func parseCleanupTime(s string) ([2]int, error) {
	s = strings.TrimSpace(s)

	// Try 12h format
	if m := timePattern12.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		ampm := strings.ToUpper(m[3])
		if h < 1 || h > 12 || min > 59 {
			return [2]int{}, fmt.Errorf("invalid time: %s", s)
		}
		if ampm == "AM" && h == 12 {
			h = 0
		} else if ampm == "PM" && h != 12 {
			h += 12
		}
		return [2]int{h, min}, nil
	}

	// Try 24h format
	if m := timePattern24.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		min, _ := strconv.Atoi(m[2])
		if h > 23 || min > 59 {
			return [2]int{}, fmt.Errorf("invalid time: %s", s)
		}
		return [2]int{h, min}, nil
	}

	return [2]int{}, fmt.Errorf("invalid time format: %s (expected HH:MM or HH:MM AM/PM)", s)
}

func (ic *ImageCleaner) sendCleanupDiscord(result *ImageCleanupResult) {
	var title, description string
	var color int

	if result.DryRun {
		title = "Scheduled Cleanup — Dry Run"
		color = 0x58a6ff // blue
		var parts []string
		if result.ImagesFound > 0 {
			parts = append(parts, fmt.Sprintf("%d images (%s)", result.ImagesFound, formatBytesGo(result.SpaceReclaimed)))
		}
		if result.VolumesDeleted > 0 {
			parts = append(parts, fmt.Sprintf("%d volumes", result.VolumesDeleted))
		}
		if len(parts) == 0 {
			description = "Nothing to clean up"
		} else {
			description = "Would remove: " + strings.Join(parts, ", ")
		}
	} else if result.Error != "" {
		title = "Scheduled Cleanup Failed"
		color = 0xed4245 // red
		description = result.Error
	} else {
		title = "Scheduled Cleanup"
		color = 0x3fb950 // green
		var parts []string
		if result.ImagesDeleted > 0 {
			parts = append(parts, fmt.Sprintf("%d images", result.ImagesDeleted))
		}
		if result.VolumesDeleted > 0 {
			parts = append(parts, fmt.Sprintf("%d volumes", result.VolumesDeleted))
		}
		if len(parts) == 0 {
			return // nothing was cleaned, don't notify
		}
		description = fmt.Sprintf("Removed %s, reclaimed %s", strings.Join(parts, " + "), formatBytesGo(result.SpaceReclaimed))
	}

	sendDiscordEmbed(title, description, color)
}

func formatBytesGo(b int64) string {
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MiB", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GiB", float64(b)/(1024*1024*1024))
}
