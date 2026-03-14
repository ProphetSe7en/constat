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
	Time           time.Time `json:"time"`
	DryRun         bool      `json:"dryRun"`
	Mode           string    `json:"mode"`
	ImagesFound    int       `json:"imagesFound"`
	ImagesDeleted  int       `json:"imagesDeleted"`
	SpaceReclaimed int64     `json:"spaceReclaimed"`
	Error          string    `json:"error,omitempty"`
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
		mode := cfg.ImageCleanupMode
		if mode != "dangling" && mode != "all" {
			mode = "dangling"
		}
		ic.runCleanup(ctx, mode, dryRun)
	}
}

func (ic *ImageCleaner) runCleanup(ctx context.Context, mode string, dryRun bool) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result := &ImageCleanupResult{
		Time:   time.Now(),
		DryRun: dryRun,
		Mode:   mode,
	}

	if dryRun {
		images, err := ic.app.listImages(cleanupCtx)
		if err != nil {
			result.Error = err.Error()
			log.Printf("ImageCleanup [dry-run]: error listing images: %v", err)
			ic.setLastResult(result)
			return
		}

		var count int
		var totalSize int64
		for _, img := range images {
			if mode == "dangling" && img.Status != "dangling" {
				continue
			}
			if mode == "all" && img.Status != "dangling" && img.Status != "unused" {
				continue
			}
			count++
			totalSize += img.Size
			log.Printf("ImageCleanup [dry-run]: would remove %s (%v, %s)", img.ID, img.RepoTags, formatBytesGo(img.Size))
		}

		result.ImagesFound = count
		result.SpaceReclaimed = totalSize
		log.Printf("ImageCleanup [dry-run]: %d images would be removed, %s reclaimable", count, formatBytesGo(totalSize))
	} else {
		pruneFilters := filters.NewArgs()
		if mode == "dangling" {
			pruneFilters.Add("dangling", "true")
		}

		report, err := ic.docker.ImagesPrune(cleanupCtx, pruneFilters)
		if err != nil {
			result.Error = err.Error()
			log.Printf("ImageCleanup: error pruning images: %v", err)
			ic.setLastResult(result)
			return
		}

		result.ImagesDeleted = len(report.ImagesDeleted)
		result.SpaceReclaimed = int64(report.SpaceReclaimed)
		log.Printf("ImageCleanup: pruned %d images, reclaimed %s (mode=%s)", result.ImagesDeleted, formatBytesGo(result.SpaceReclaimed), mode)
	}

	ic.setLastResult(result)
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

func formatBytesGo(b int64) string {
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KiB", float64(b)/1024)
	}
	if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MiB", float64(b)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GiB", float64(b)/(1024*1024*1024))
}
