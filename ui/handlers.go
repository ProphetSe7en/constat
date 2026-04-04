package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

func networkListOptions() network.ListOptions {
	return network.ListOptions{}
}

var validContainerID = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// Summary holds container count statistics
type Summary struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
	NoCheck   int `json:"noCheck"`
	Stopped   int `json:"stopped"`
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (app *App) handleSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Lightweight: only list + inspect, no stats
	raw, err := app.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		writeError(w, 500, "Failed to list containers")
		return
	}

	summary := Summary{Total: len(raw)}
	for _, c := range raw {
		switch c.State {
		case "running":
			summary.Running++
			inspect, err := app.docker.ContainerInspect(ctx, c.ID)
			if err != nil {
				summary.NoCheck++
				continue
			}
			if inspect.State.Health != nil {
				switch inspect.State.Health.Status {
				case "healthy":
					summary.Healthy++
				case "unhealthy":
					summary.Unhealthy++
				default:
					summary.NoCheck++
				}
			} else {
				summary.NoCheck++
			}
		default:
			summary.Stopped++
		}
	}

	writeJSON(w, summary)
}

func (app *App) handleListContainers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	containers, err := app.ListContainers(ctx)
	if err != nil {
		log.Printf("Error listing containers: %v", err)
		writeError(w, 500, "Failed to list containers")
		return
	}

	writeJSON(w, containers)
}

func (app *App) handleContainerStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	if app.stats == nil {
		writeError(w, 503, "Stats collector not available")
		return
	}

	name, ok := app.stats.NameForID(id)
	if !ok {
		writeError(w, 404, "No stats available for container")
		return
	}

	live, ok := app.stats.GetLatest(name)
	if !ok {
		writeError(w, 404, "No stats available for container")
		return
	}

	writeJSON(w, live)
}

func (app *App) handleStartContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := app.docker.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		log.Printf("Error starting container %s: %v", id, err)
		writeError(w, 500, fmt.Sprintf("Failed to start container: %v", err))
		return
	}

	writeJSON(w, map[string]string{"status": "started"})
}

func (app *App) handleStopContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	timeout := 10
	options := containerStopOptions(timeout)
	if err := app.docker.ContainerStop(ctx, id, options); err != nil {
		log.Printf("Error stopping container %s: %v", id, err)
		writeError(w, 500, fmt.Sprintf("Failed to stop container: %v", err))
		return
	}

	writeJSON(w, map[string]string{"status": "stopped"})
}

func (app *App) handleRestartContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	timeout := 10
	options := containerStopOptions(timeout)
	if err := app.docker.ContainerRestart(ctx, id, options); err != nil {
		log.Printf("Error restarting container %s: %v", id, err)
		writeError(w, 500, fmt.Sprintf("Failed to restart container: %v", err))
		return
	}

	writeJSON(w, map[string]string{"status": "restarted"})
}

func (app *App) handlePauseContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := app.docker.ContainerPause(ctx, id); err != nil {
		log.Printf("Error pausing container %s: %v", id, err)
		writeError(w, 500, fmt.Sprintf("Failed to pause container: %v", err))
		return
	}

	writeJSON(w, map[string]string{"status": "paused"})
}

func (app *App) handleUnpauseContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := app.docker.ContainerUnpause(ctx, id); err != nil {
		log.Printf("Error unpausing container %s: %v", id, err)
		writeError(w, 500, fmt.Sprintf("Failed to unpause container: %v", err))
		return
	}

	writeJSON(w, map[string]string{"status": "unpaused"})
}

func (app *App) handleKillContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := app.docker.ContainerKill(ctx, id, "SIGKILL"); err != nil {
		log.Printf("Error killing container %s: %v", id, err)
		writeError(w, 500, fmt.Sprintf("Failed to force stop container: %v", err))
		return
	}

	writeJSON(w, map[string]string{"status": "killed"})
}

func (app *App) handleLogsTail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	logReader, err := app.docker.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "10",
	})
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to get logs: %v", err))
		return
	}
	defer logReader.Close()

	// Check TTY mode for stream format
	inspect, err := app.docker.ContainerInspect(ctx, id)
	if err != nil {
		writeError(w, 500, "Container not found")
		return
	}

	var lines []string
	scanner := bufio.NewScanner(logReader)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !inspect.Config.Tty && len(line) >= 8 {
			line = line[8:] // strip Docker stream header
		}
		// Strip ANSI escape sequences
		cleaned := stripANSI(string(line))
		if cleaned = strings.TrimSpace(cleaned); cleaned != "" {
			lines = append(lines, cleaned)
		}
	}

	writeJSON(w, map[string]interface{}{"lines": lines})
}

func (app *App) handleHealthSuggestions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	containers, err := app.ListContainers(ctx)
	if err != nil {
		writeError(w, 500, "Failed to list containers")
		return
	}

	suggestions := GetSuggestions(containers)
	if suggestions == nil {
		suggestions = []Suggestion{}
	}

	writeJSON(w, suggestions)
}

func (app *App) handleListEvents(w http.ResponseWriter, r *http.Request) {
	eventType := r.URL.Query().Get("type")
	container := r.URL.Query().Get("container")
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 50
	if limitStr != "" {
		if v, err := strconv.Atoi(limitStr); err == nil && v > 0 {
			limit = v
		}
	}

	offset := 0
	if offsetStr != "" {
		if v, err := strconv.Atoi(offsetStr); err == nil && v >= 0 {
			offset = v
		}
	}

	events := app.events.List(eventType, container, limit, offset)
	if events == nil {
		events = []Event{}
	}

	writeJSON(w, map[string]any{
		"events": events,
		"total":  app.events.Total(),
	})
}

func (app *App) handlePostEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Container string `json:"container"`
		Type      string `json:"type"`
		Action    string `json:"action"`
		Detail    string `json:"detail,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "Invalid JSON")
		return
	}
	if req.Container == "" || req.Type == "" || req.Action == "" {
		writeError(w, 400, "container, type, and action are required")
		return
	}

	event := Event{
		Timestamp: time.Now(),
		Container: req.Container,
		Type:      req.Type,
		Action:    req.Action,
		Detail:    req.Detail,
	}
	app.events.Add(event)
	log.Printf("External event: %s %s %s", req.Type, req.Action, req.Container)

	// Track escalation stops for health column badge
	if app.stats != nil {
		if req.Type == "health" && req.Action == "stopped" {
			app.stats.SetContainerStatus(req.Container, "stopped-health")
		} else if req.Type == "memory" && req.Action == "stopped" {
			app.stats.SetContainerStatus(req.Container, "stopped-mem")
		}
	}

	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (app *App) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := app.events.Subscribe()
	defer app.events.Unsubscribe(ch)

	// Send initial keepalive
	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (app *App) handleStatsSSE(w http.ResponseWriter, r *http.Request) {
	if app.stats == nil {
		writeError(w, 503, "Stats collector not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := app.stats.SubscribeStats()
	defer app.stats.UnsubscribeStats(ch)

	// Send initial keepalive
	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case batch := <-ch:
			data, err := json.Marshal(batch)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// containerStopOptions creates container stop options with a timeout
func containerStopOptions(timeoutSeconds int) container.StopOptions {
	t := timeoutSeconds
	return container.StopOptions{Timeout: &t}
}

// sendDiscordMaintenance sends a Discord embed to the maintenance webhook (falls back to health webhook)
func sendDiscordMaintenance(title, description string, color int) {
	cfg, err := ReadConfig(configPath)
	if err != nil || cfg.EnableDiscord != "true" {
		return
	}
	webhook := cfg.WebhookMaintenance
	if webhook == "" {
		webhook = cfg.WebhookHealth // fallback for backwards compatibility
	}
	if webhook == "" {
		return
	}
	botName := cfg.BotName
	if botName == "" {
		botName = "Constat"
	}
	serverLabel := ""
	if cfg.ServerLabel != "" {
		serverLabel = " (" + cfg.ServerLabel + ")"
	}
	payload := map[string]any{
		"username": botName,
		"embeds": []map[string]any{{
			"author":      map[string]string{"name": fmt.Sprintf("🧹 %s: %s", botName, title)},
			"description": description + serverLabel,
			"color":       color,
			"footer":      map[string]string{"text": fmt.Sprintf("Constat v%s by ProphetSe7en", constatVersion)},
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}},
	}
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("Discord maintenance: send failed: %v", err)
		return
	}
	resp.Body.Close()
}

func (app *App) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	if app.updateChecker == nil {
		writeJSON(w, map[string]any{"results": map[string]any{}, "checking": false})
		return
	}
	writeJSON(w, map[string]any{
		"results":   app.updateChecker.GetResults(),
		"checking":  app.updateChecker.IsChecking(),
		"lastCheck": app.updateChecker.LastCheck(),
	})
}

func (app *App) handleTriggerUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if app.updateChecker == nil {
		writeError(w, 400, "Update checker not initialized")
		return
	}
	app.updateChecker.TriggerCheck()
	writeJSON(w, map[string]string{"status": "check triggered"})
}

const configPath = "/config/constat.conf"
const configSamplePath = "/config/constat.conf.sample"

var (
	validWebhookURL    = regexp.MustCompile(`^https://(discord\.com|discordapp\.com)/api/webhooks/`)
	validMemoryLimit   = regexp.MustCompile(`(?i)^\d+(\.\d+)?[mg]$`)
	validHexColor      = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)
	validContainerName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
)

// ensureConfig copies .sample to config if config doesn't exist yet
func ensureConfig() {
	if _, err := os.Stat(configPath); err == nil {
		return // config exists
	}
	data, err := os.ReadFile(configSamplePath)
	if err != nil {
		log.Printf("No config sample found at %s: %v", configSamplePath, err)
		return
	}
	if err := os.WriteFile(configPath, data, 0664); err != nil {
		log.Printf("Failed to create config from sample: %v", err)
		return
	}
	// Match Unraid ownership: nobody:users (99:100)
	os.Chown(configPath, 99, 100)
	log.Printf("Created %s from sample", configPath)
}

func (app *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	ensureConfig()
	config, err := ReadConfig(configPath)
	if err != nil {
		log.Printf("Error reading config: %v", err)
		writeError(w, 500, "Failed to read config")
		return
	}
	// Attach version to response (read-only, not saved to config file)
	resp := struct {
		ConfigData
		Version string `json:"version"`
	}{*config, constatVersion}
	writeJSON(w, resp)
}

func (app *App) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var config ConfigData
	if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
		writeError(w, 400, "Invalid JSON body")
		return
	}

	// Validate webhook URLs
	for _, url := range []string{config.WebhookState, config.WebhookHealth, config.WebhookMaintenance} {
		if url != "" && !validWebhookURL.MatchString(url) {
			writeError(w, 400, "Invalid webhook URL: must start with https://discord.com/api/webhooks/ or https://discordapp.com/api/webhooks/")
			return
		}
	}

	// Validate boolean fields
	if config.EnableDiscord != "" && config.EnableDiscord != "true" && config.EnableDiscord != "false" {
		writeError(w, 400, "enableDiscord must be 'true' or 'false'")
		return
	}
	if config.MemoryPaused != "" && config.MemoryPaused != "true" && config.MemoryPaused != "false" {
		writeError(w, 400, "memoryPaused must be 'true' or 'false'")
		return
	}
	if config.ShowStats != "" && config.ShowStats != "true" && config.ShowStats != "false" {
		writeError(w, 400, "showStats must be 'true' or 'false'")
		return
	}
	if config.ShowCharts != "" && config.ShowCharts != "true" && config.ShowCharts != "false" {
		writeError(w, 400, "showCharts must be 'true' or 'false'")
		return
	}

	// Validate image cleanup fields
	if config.ImageCleanupEnabled != "" && config.ImageCleanupEnabled != "true" && config.ImageCleanupEnabled != "false" {
		writeError(w, 400, "imageCleanupEnabled must be 'true' or 'false'")
		return
	}
	if config.ImageCleanupDryRun != "" && config.ImageCleanupDryRun != "true" && config.ImageCleanupDryRun != "false" {
		writeError(w, 400, "imageCleanupDryRun must be 'true' or 'false'")
		return
	}
	if config.ImageCleanupMode != "" && config.ImageCleanupMode != "dangling" && config.ImageCleanupMode != "all" {
		writeError(w, 400, "imageCleanupMode must be 'dangling' or 'all'")
		return
	}
	if config.ImageCleanupTime != "" {
		if _, err := parseCleanupTime(config.ImageCleanupTime); err != nil {
			writeError(w, 400, "imageCleanupTime must be a valid time (HH:MM or HH:MM AM/PM)")
			return
		}
	}

	if config.CleanupOrphanImages != "" && config.CleanupOrphanImages != "true" && config.CleanupOrphanImages != "false" {
		writeError(w, 400, "cleanupOrphanImages must be 'true' or 'false'")
		return
	}
	if config.CleanupUnusedImages != "" && config.CleanupUnusedImages != "true" && config.CleanupUnusedImages != "false" {
		writeError(w, 400, "cleanupUnusedImages must be 'true' or 'false'")
		return
	}
	if config.CleanupVolumes != "" && config.CleanupVolumes != "true" && config.CleanupVolumes != "false" {
		writeError(w, 400, "cleanupVolumes must be 'true' or 'false'")
		return
	}

	// Validate time format
	if config.TimeFormat != "" && config.TimeFormat != "24h" && config.TimeFormat != "12h" {
		writeError(w, 400, "timeFormat must be '24h' or '12h'")
		return
	}

	// Validate date format
	validDateFormats := map[string]bool{"": true, "YYYY-MM-DD": true, "DD.MM.YYYY": true, "DD/MM/YYYY": true, "MM/DD/YYYY": true}
	if !validDateFormats[config.DateFormat] {
		writeError(w, 400, "dateFormat must be one of: YYYY-MM-DD, DD.MM.YYYY, DD/MM/YYYY, MM/DD/YYYY")
		return
	}

	// Validate numeric fields
	numericFields := map[string]string{
		"batchWindow":          config.BatchWindow,
		"summaryInterval":      config.SummaryInterval,
		"restartCooldown":      config.RestartCooldown,
		"maxRestarts":          config.MaxRestarts,
		"memoryPollInterval":   config.MemoryPollInterval,
		"memoryDefaultDuration": config.MemoryDefaultDuration,
	}
	for name, val := range numericFields {
		if val == "" {
			continue
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 0 {
			writeError(w, 400, fmt.Sprintf("%s must be a non-negative integer", name))
			return
		}
	}

	// Validate color fields
	colorFields := map[string]string{
		"colorStarted":    config.ColorStarted,
		"colorStopped":    config.ColorStopped,
		"colorDied":       config.ColorDied,
		"colorUnhealthy":  config.ColorUnhealthy,
		"colorRecovered":  config.ColorRecovered,
		"colorRestarting": config.ColorRestarting,
		"colorMemoryWarn": config.ColorMemoryWarn,
		"colorMemoryCrit": config.ColorMemoryCrit,
	}
	for name, val := range colorFields {
		if val != "" && !validHexColor.MatchString(val) {
			writeError(w, 400, fmt.Sprintf("%s must be a 6-digit hex color (without #)", name))
			return
		}
	}

	// Validate timezone
	if config.Timezone != "" {
		if _, err := time.LoadLocation(config.Timezone); err != nil {
			writeError(w, 400, fmt.Sprintf("Invalid timezone '%s': use TZ database names like Europe/Oslo", config.Timezone))
			return
		}
	}

	// Validate memory watch entries
	for i, entry := range config.MemoryWatch {
		if strings.TrimSpace(entry.Name) == "" {
			writeError(w, 400, "Memory watch entry name cannot be empty")
			return
		}
		if !validContainerName.MatchString(entry.Name) {
			writeError(w, 400, fmt.Sprintf("Invalid memory watch name '%s': only letters, numbers, dots, hyphens, and underscores allowed", entry.Name))
			return
		}
		if !validMemoryLimit.MatchString(entry.Limit) {
			writeError(w, 400, fmt.Sprintf("Invalid memory limit '%s': must match format like 512m, 1.5g, 20g", entry.Limit))
			return
		}
		// Normalize legacy "notify" to "warn"
		if entry.Action == "notify" {
			entry.Action = "warn"
			config.MemoryWatch[i] = entry
		}
		if entry.Action != "warn" && entry.Action != "restart" {
			writeError(w, 400, fmt.Sprintf("Invalid memory watch action '%s': must be 'warn' or 'restart'", entry.Action))
			return
		}
		if entry.Duration != "" {
			n, err := strconv.Atoi(entry.Duration)
			if err != nil || n <= 0 {
				writeError(w, 400, fmt.Sprintf("Invalid memory watch duration '%s': must be a positive integer", entry.Duration))
				return
			}
		}
		if entry.MaxTriggers != "" {
			n, err := strconv.Atoi(entry.MaxTriggers)
			if err != nil || n < 0 {
				writeError(w, 400, fmt.Sprintf("Invalid memory watch maxTriggers '%s': must be a non-negative integer", entry.MaxTriggers))
				return
			}
		}
		if entry.MaxWindow != "" {
			if _, err := time.ParseDuration(entry.MaxWindow); err != nil {
				writeError(w, 400, fmt.Sprintf("Invalid memory watch maxWindow '%s': must be a Go duration (e.g. 24h, 12h, 1h)", entry.MaxWindow))
				return
			}
		}
	}

	if err := WriteConfig(configPath, &config); err != nil {
		log.Printf("Error writing config: %v", err)
		writeError(w, 500, "Failed to write config")
		return
	}

	writeJSON(w, map[string]string{"status": "saved"})
}

func (app *App) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "Invalid JSON body")
		return
	}
	if !validWebhookURL.MatchString(req.URL) {
		writeError(w, 400, "Invalid webhook URL")
		return
	}

	// Build Discord embed payload
	botName := "Constat"
	serverLabel := ""
	if cfg, err := ReadConfig(configPath); err == nil {
		if cfg.BotName != "" {
			botName = cfg.BotName
		}
		if cfg.ServerLabel != "" {
			serverLabel = " (" + cfg.ServerLabel + ")"
		}
	}

	payload := map[string]any{
		"username": botName,
		"embeds": []map[string]any{{
			"title":       "Webhook Test",
			"description": fmt.Sprintf("This webhook is working correctly.%s", serverLabel),
			"color":       0x3fb950,
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}},
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(req.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		writeError(w, 502, fmt.Sprintf("Webhook request failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		writeError(w, 502, fmt.Sprintf("Discord returned status %d", resp.StatusCode))
		return
	}

	writeJSON(w, map[string]string{"status": "sent"})
}

func (app *App) handleRestartOverride(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, 400, "Container name required")
		return
	}

	app.restartMu.Lock()
	if app.restartDisabled[name] {
		delete(app.restartDisabled, name)
	} else {
		app.restartDisabled[name] = true
	}
	disabled := app.restartDisabled[name]
	app.restartMu.Unlock()

	if err := app.saveRestartDisabled(); err != nil {
		log.Printf("Error saving restart_disabled.json: %v", err)
		writeError(w, 500, "Failed to save override")
		return
	}

	writeJSON(w, map[string]bool{"disabled": disabled})
}

func (app *App) handleLogsSSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Streaming not supported")
		return
	}

	// Validate tail param
	tail := "100"
	if t := r.URL.Query().Get("tail"); t != "" {
		switch t {
		case "100", "500", "1000":
			tail = t
		default:
			writeError(w, 400, "tail must be 100, 500, or 1000")
			return
		}
	}

	ctx := r.Context()

	// Inspect container to check TTY mode
	inspect, err := app.docker.ContainerInspect(ctx, id)
	if err != nil {
		writeError(w, 404, "Container not found")
		return
	}
	isTTY := inspect.Config.Tty

	// Open log stream
	logReader, err := app.docker.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       tail,
		Timestamps: true,
	})
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to open logs: %v", err))
		return
	}
	defer logReader.Close()

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprintf(w, ": keepalive\n\n")
	flusher.Flush()

	type logLine struct {
		Line   string `json:"line"`
		Stream string `json:"stream"`
		Ts     string `json:"ts,omitempty"`
		Level  string `json:"level,omitempty"`
	}

	detectLevel := func(s string) string {
		// Check first 200 chars to avoid scanning huge JSON lines
		if len(s) > 200 {
			s = s[:200]
		}
		low := strings.ToLower(s)
		for _, kw := range []string{"error", "fatal", "critical", "panic"} {
			if strings.Contains(low, kw) {
				return "error"
			}
		}
		for _, kw := range []string{"warn", "warning"} {
			if strings.Contains(low, kw) {
				return "warn"
			}
		}
		for _, kw := range []string{"debug", "trace"} {
			if strings.Contains(low, kw) {
				return "debug"
			}
		}
		if strings.Contains(low, "info") {
			return "info"
		}
		return ""
	}

	sendLine := func(line, stream string) {
		var ts string
		// Docker timestamps: 2006-01-02T15:04:05.999999999Z <content>
		if idx := strings.IndexByte(line, ' '); idx > 0 && idx < 40 {
			candidate := line[:idx]
			if _, err := time.Parse(time.RFC3339Nano, candidate); err == nil {
				ts = candidate
				line = line[idx+1:]
			}
		}
		// Strip ANSI escape codes and detect level from clean text
		clean := ansiEscape.ReplaceAllString(line, "")
		level := detectLevel(clean)
		if stream == "stderr" && level == "" {
			level = "stderr"
		}
		data, err := json.Marshal(logLine{Line: clean, Stream: stream, Ts: ts, Level: level})
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	if isTTY {
		// TTY mode: plain text lines (no multiplexing header)
		scanner := bufio.NewScanner(logReader)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}
			text := scanner.Text()
			// Split bare \r (progress bars) into separate lines
			if strings.Contains(text, "\r") {
				for _, part := range strings.Split(text, "\r") {
					if part != "" {
						sendLine(part, "stdout")
					}
				}
			} else {
				sendLine(text, "stdout")
			}
		}
	} else {
		// Multiplexed stream: 8-byte header per frame
		// Byte 0: stream type (1=stdout, 2=stderr)
		// Bytes 1-3: padding
		// Bytes 4-7: uint32 big-endian payload size
		header := make([]byte, 8)
		var lineBuf string
		var lineBufStream string
		flushBuf := func() {
			if lineBuf != "" {
				sendLine(lineBuf, lineBufStream)
				lineBuf = ""
			}
		}
		for {
			select {
			case <-ctx.Done():
				flushBuf()
				return
			default:
			}

			_, err := io.ReadFull(logReader, header)
			if err != nil {
				flushBuf()
				return
			}

			streamType := header[0]
			size := binary.BigEndian.Uint32(header[4:8])

			if size == 0 {
				continue
			}
			if size > 128*1024 {
				// Skip unreasonably large frames
				if _, err := io.CopyN(io.Discard, logReader, int64(size)); err != nil {
					return
				}
				lineBuf = ""
				continue
			}

			payloadBytes := make([]byte, size)
			_, err = io.ReadFull(logReader, payloadBytes)
			if err != nil {
				flushBuf()
				return
			}

			stream := "stdout"
			if streamType == 2 {
				stream = "stderr"
			}

			payload := string(payloadBytes)

			// Normalize line endings: \r\n → \n, then bare \r → \n
			// (bare \r is used by progress bars to overwrite the same line)
			payload = strings.ReplaceAll(payload, "\r\n", "\n")
			payload = strings.ReplaceAll(payload, "\r", "\n")

			// Prepend buffered partial line from previous frame
			if lineBuf != "" && stream == lineBufStream {
				payload = lineBuf + payload
				lineBuf = ""
			} else if lineBuf != "" {
				// Stream type changed — flush old buffer as-is
				sendLine(lineBuf, lineBufStream)
				lineBuf = ""
			}

			// If payload doesn't end with newline, last segment is incomplete
			if !strings.HasSuffix(payload, "\n") {
				idx := strings.LastIndex(payload, "\n")
				if idx >= 0 {
					lineBuf = payload[idx+1:]
					lineBufStream = stream
					payload = payload[:idx+1]
				} else {
					// No newline at all — entire payload is partial
					lineBuf = payload
					lineBufStream = stream
					continue
				}
			}

			lines := strings.Split(strings.TrimRight(payload, "\n"), "\n")
			for _, line := range lines {
				if line != "" {
					sendLine(line, stream)
				}
			}
		}
	}
}

func (app *App) handleContainerHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	sinceStr := r.URL.Query().Get("since")
	since := int64(0)
	if sinceStr != "" {
		if v, err := strconv.ParseInt(sinceStr, 10, 64); err == nil {
			since = v
		}
	}

	if app.stats == nil {
		writeJSON(w, []StatPoint{})
		return
	}

	name, ok := app.stats.NameForID(id)
	if !ok {
		writeJSON(w, []StatPoint{})
		return
	}

	points := app.stats.GetHistory(name, since)
	if points == nil {
		points = []StatPoint{}
	}
	writeJSON(w, points)
}

// --- Container Config Handler ---

// secretEnvPatterns matches env var names that should be masked
var secretEnvPatterns = []string{"KEY", "TOKEN", "PASSWORD", "SECRET", "PASS", "CREDENTIAL"}



// ContainerConfig is the structured inspect response for the config panel
type ContainerConfig struct {
	Ports       []string          `json:"ports"`
	Volumes     []string          `json:"volumes"`
	Networks    []NetworkInfo     `json:"networks"`
	Env         []EnvVar          `json:"env"`
	Labels      map[string]string `json:"labels"`
	Settings    ContainerSettings `json:"settings"`
	Healthcheck *HealthcheckConfig `json:"healthcheck,omitempty"`
}

type NetworkInfo struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	Gateway    string `json:"gateway"`
	MacAddress string `json:"mac,omitempty"`
}

// Network topology types

type NetworkTopology struct {
	Networks []NetworkGroup `json:"networks"`
}

type NetworkGroup struct {
	Name       string             `json:"name"`
	Driver     string             `json:"driver"`
	Subnet     string             `json:"subnet"`
	Gateway    string             `json:"gateway"`
	Type       string             `json:"type"` // "custom", "bridge", "macvlan", "host", "shared"
	Containers []NetworkContainer `json:"containers"`
}

type NetworkContainer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Health     string `json:"health"`
	IP         string `json:"ip"`
	MacAddress string `json:"mac"`
	Gateway    string `json:"gateway"`
	Ports      string `json:"ports"`
	SharedVia  string `json:"sharedVia,omitempty"` // parent container name for container:X mode
}

type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
}

type ContainerSettings struct {
	RestartPolicy   string            `json:"restartPolicy"`
	Privileged      bool              `json:"privileged"`
	NetworkMode     string            `json:"networkMode"`
	Capabilities    []string          `json:"capabilities,omitempty"`
	Sysctls         map[string]string `json:"sysctls,omitempty"`
	ExtraParameters []string          `json:"extraParameters,omitempty"`
}

type HealthcheckConfig struct {
	Test     string `json:"test"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`
	Retries  int    `json:"retries"`
}

func (app *App) handleContainerConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid container ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	inspect, err := app.docker.ContainerInspect(ctx, id)
	if err != nil {
		writeError(w, 404, "Container not found")
		return
	}

	// Get image defaults to show only user-configured env vars and labels
	imageEnv := make(map[string]string)
	imageLabels := make(map[string]string)
	if imgInspect, _, err := app.docker.ImageInspectWithRaw(ctx, inspect.Image); err == nil {
		for _, e := range imgInspect.Config.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				imageEnv[parts[0]] = parts[1]
			}
		}
		imageLabels = imgInspect.Config.Labels
	} else {
		log.Printf("Warning: could not inspect image %s, showing all env vars: %v", inspect.Image[:19], err)
	}

	cfg := ContainerConfig{}

	// Ports
	for port, bindings := range inspect.HostConfig.PortBindings {
		for _, b := range bindings {
			hostPort := b.HostPort
			if hostPort == "" {
				hostPort = "auto"
			}
			cfg.Ports = append(cfg.Ports, fmt.Sprintf("%s:%s", hostPort, port))
		}
	}

	// Volumes: bind mounts from HostConfig + named volumes from Mounts
	cfg.Volumes = inspect.HostConfig.Binds
	for _, m := range inspect.Mounts {
		if m.Type == "volume" {
			cfg.Volumes = append(cfg.Volumes, fmt.Sprintf("%s:%s", m.Name, m.Destination))
		}
	}

	// Networks
	for name, net := range inspect.NetworkSettings.Networks {
		cfg.Networks = append(cfg.Networks, NetworkInfo{
			Name:       name,
			IP:         net.IPAddress,
			Gateway:    net.Gateway,
			MacAddress: net.MacAddress,
		})
	}

	// Environment variables — only user-configured (not in image, or value changed)
	for _, e := range inspect.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		key := parts[0]
		val := ""
		if len(parts) == 2 {
			val = parts[1]
		}
		if imgVal, inImage := imageEnv[key]; inImage && val == imgVal {
			continue
		}
		ev := EnvVar{Key: key, Value: val}
		upperKey := strings.ToUpper(key)
		for _, pat := range secretEnvPatterns {
			if strings.Contains(upperKey, pat) {
				ev.Secret = true
				break
			}
		}
		cfg.Env = append(cfg.Env, ev)
	}

	// Labels — only user-configured (not in image, or value changed)
	cfg.Labels = make(map[string]string)
	for k, v := range inspect.Config.Labels {
		if imgVal, inImage := imageLabels[k]; inImage && v == imgVal {
			continue
		}
		cfg.Labels[k] = v
	}

	// Settings
	restartPolicy := string(inspect.HostConfig.RestartPolicy.Name)
	if restartPolicy == "" {
		restartPolicy = "no"
	}
	cfg.Settings = ContainerSettings{
		RestartPolicy: restartPolicy,
		Privileged:    inspect.HostConfig.Privileged,
		NetworkMode:   string(inspect.HostConfig.NetworkMode),
		Capabilities:  inspect.HostConfig.CapAdd,
		Sysctls:       inspect.HostConfig.Sysctls,
	}

	// Extra Parameters — Docker flags not covered by standard Unraid UI fields
	var extras []string
	if inspect.Config.Healthcheck != nil && len(inspect.Config.Healthcheck.Test) > 1 {
		test := strings.Join(inspect.Config.Healthcheck.Test[1:], " ")
		extras = append(extras, fmt.Sprintf("--health-cmd='%s'", test))
		if d := inspect.Config.Healthcheck.Interval; d > 0 {
			extras = append(extras, fmt.Sprintf("--health-interval=%s", d))
		}
		if d := inspect.Config.Healthcheck.Timeout; d > 0 {
			extras = append(extras, fmt.Sprintf("--health-timeout=%s", d))
		}
		if r := inspect.Config.Healthcheck.Retries; r > 0 {
			extras = append(extras, fmt.Sprintf("--health-retries=%d", r))
		}
		if d := inspect.Config.Healthcheck.StartPeriod; d > 0 {
			extras = append(extras, fmt.Sprintf("--health-start-period=%s", d))
		}
	}
	if len(inspect.HostConfig.ExtraHosts) > 0 {
		for _, h := range inspect.HostConfig.ExtraHosts {
			extras = append(extras, fmt.Sprintf("--add-host=%s", h))
		}
	}
	if len(inspect.HostConfig.DNS) > 0 {
		for _, d := range inspect.HostConfig.DNS {
			extras = append(extras, fmt.Sprintf("--dns=%s", d))
		}
	}
	if len(inspect.HostConfig.Tmpfs) > 0 {
		for path, opts := range inspect.HostConfig.Tmpfs {
			if opts != "" {
				extras = append(extras, fmt.Sprintf("--tmpfs %s:%s", path, opts))
			} else {
				extras = append(extras, fmt.Sprintf("--tmpfs %s", path))
			}
		}
	}
	if inspect.HostConfig.ShmSize > 0 && inspect.HostConfig.ShmSize != 67108864 { // 64MB is default
		extras = append(extras, fmt.Sprintf("--shm-size=%d", inspect.HostConfig.ShmSize))
	}
	if inspect.HostConfig.PidMode != "" && inspect.HostConfig.PidMode != "private" {
		extras = append(extras, fmt.Sprintf("--pid=%s", inspect.HostConfig.PidMode))
	}
	cfg.Settings.ExtraParameters = extras

	// Healthcheck
	if inspect.Config.Healthcheck != nil && len(inspect.Config.Healthcheck.Test) > 0 {
		test := strings.Join(inspect.Config.Healthcheck.Test, " ")
		// Strip "CMD-SHELL " prefix for readability
		test = strings.TrimPrefix(test, "CMD-SHELL ")
		cfg.Healthcheck = &HealthcheckConfig{
			Test:     test,
			Interval: inspect.Config.Healthcheck.Interval.String(),
			Timeout:  inspect.Config.Healthcheck.Timeout.String(),
			Retries:  inspect.Config.Healthcheck.Retries,
		}
	}

	writeJSON(w, cfg)
}

// --- Network Topology Handler ---

func (app *App) handleNetworks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// List all containers
	raw, err := app.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		writeError(w, 500, "Failed to list containers")
		return
	}

	// Build ID→name lookup
	idToName := make(map[string]string, len(raw))
	for _, c := range raw {
		if len(c.Names) > 0 {
			idToName[c.ID] = strings.TrimPrefix(c.Names[0], "/")
			if len(c.ID) > 12 {
				idToName[c.ID[:12]] = strings.TrimPrefix(c.Names[0], "/")
			}
		}
	}

	// Collect network→containers mapping
	type netMeta struct {
		driver  string
		subnet  string
		gateway string
	}
	networkMeta := make(map[string]netMeta)
	networkContainers := make(map[string][]NetworkContainer)
	var sharedContainers []NetworkContainer

	for _, c := range raw {
		inspect, err := app.docker.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}

		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		health := "none"
		if inspect.State.Health != nil {
			health = string(inspect.State.Health.Status)
		}

		// Format ports
		var portParts []string
		for port, bindings := range inspect.HostConfig.PortBindings {
			for _, b := range bindings {
				if b.HostPort != "" {
					portParts = append(portParts, fmt.Sprintf("%s:%s", b.HostPort, port))
				}
			}
		}
		sort.Strings(portParts)
		ports := strings.Join(portParts, ", ")

		hasNetworks := false
		if inspect.NetworkSettings != nil {
			for netName, net := range inspect.NetworkSettings.Networks {
				hasNetworks = true
				networkContainers[netName] = append(networkContainers[netName], NetworkContainer{
					ID:         c.ID[:12],
					Name:       name,
					State:      c.State,
					Health:     health,
					IP:         net.IPAddress,
					MacAddress: net.MacAddress,
					Gateway:    net.Gateway,
					Ports:      ports,
				})
			}
		}

		// Containers using container:X network mode
		if !hasNetworks && inspect.HostConfig != nil {
			mode := string(inspect.HostConfig.NetworkMode)
			if strings.HasPrefix(mode, "container:") {
				ref := strings.TrimPrefix(mode, "container:")
				if resolved, ok := idToName[ref]; ok {
					ref = resolved
				}
				sharedContainers = append(sharedContainers, NetworkContainer{
					ID:        c.ID[:12],
					Name:      name,
					State:     c.State,
					Health:    health,
					Ports:     ports,
					SharedVia: ref,
				})
			}
		}
	}

	// Fetch Docker network metadata (driver, subnet)
	dockerNetworks, err := app.docker.NetworkList(ctx, networkListOptions())
	if err != nil {
		log.Printf("Warning: failed to list Docker networks: %v", err)
	} else {
		for _, dn := range dockerNetworks {
			meta := netMeta{driver: dn.Driver}
			if len(dn.IPAM.Config) > 0 {
				meta.subnet = dn.IPAM.Config[0].Subnet
				meta.gateway = dn.IPAM.Config[0].Gateway
			}
			networkMeta[dn.Name] = meta
		}
	}

	// Build result — order: custom networks first, then bridge, then macvlan
	var groups []NetworkGroup
	// Custom networks first (not "bridge", "host", "none")
	for netName, containers := range networkContainers {
		meta := networkMeta[netName]
		if netName == "bridge" || netName == "host" || netName == "none" {
			continue
		}
		netType := "custom"
		if meta.driver == "macvlan" || meta.driver == "ipvlan" {
			netType = "macvlan"
		}
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
		groups = append(groups, NetworkGroup{
			Name:       netName,
			Driver:     meta.driver,
			Subnet:     meta.subnet,
			Gateway:    meta.gateway,
			Type:       netType,
			Containers: containers,
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })

	// Shared namespace groups (group by parent)
	parentGroups := make(map[string][]NetworkContainer)
	for _, sc := range sharedContainers {
		parentGroups[sc.SharedVia] = append(parentGroups[sc.SharedVia], sc)
	}
	for parent, containers := range parentGroups {
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
		// Find the parent container and add it as first entry
		var withParent []NetworkContainer
		for _, netContainers := range networkContainers {
			for _, nc := range netContainers {
				if nc.Name == parent {
					parentEntry := nc
					parentEntry.SharedVia = "" // It IS the parent
					withParent = append(withParent, parentEntry)
					break
				}
			}
			if len(withParent) > 0 {
				break
			}
		}
		withParent = append(withParent, containers...)
		groups = append(groups, NetworkGroup{
			Name:       "container:" + parent,
			Driver:     "container",
			Type:       "shared",
			Containers: withParent,
		})
	}

	// Host network
	if containers, ok := networkContainers["host"]; ok {
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
		groups = append(groups, NetworkGroup{
			Name:       "host",
			Driver:     "host",
			Type:       "host",
			Containers: containers,
		})
	}

	// Default bridge last
	if containers, ok := networkContainers["bridge"]; ok {
		meta := networkMeta["bridge"]
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
		groups = append(groups, NetworkGroup{
			Name:       "bridge",
			Driver:     meta.driver,
			Subnet:     meta.subnet,
			Gateway:    meta.gateway,
			Type:       "bridge",
			Containers: containers,
		})
	}

	writeJSON(w, NetworkTopology{Networks: groups})
}

// --- Sequence Handlers ---

var validSequenceID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

func (app *App) handleListSequences(w http.ResponseWriter, r *http.Request) {
	sequences := app.sequences.List()
	if sequences == nil {
		sequences = []Sequence{}
	}
	writeJSON(w, sequences)
}

func (app *App) handleCreateSequence(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var seq Sequence
	if err := json.NewDecoder(r.Body).Decode(&seq); err != nil {
		writeError(w, 400, "Invalid JSON body")
		return
	}
	created, err := app.sequences.Create(seq)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, created)
}

func (app *App) handleUpdateSequence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSequenceID.MatchString(id) {
		writeError(w, 400, "Invalid sequence ID")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var seq Sequence
	if err := json.NewDecoder(r.Body).Decode(&seq); err != nil {
		writeError(w, 400, "Invalid JSON body")
		return
	}
	updated, err := app.sequences.Update(id, seq)
	if err != nil {
		if errors.Is(err, ErrSeqNotFound) {
			writeError(w, 404, err.Error())
		} else {
			writeError(w, 400, err.Error())
		}
		return
	}
	writeJSON(w, updated)
}

func (app *App) handleDeleteSequence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSequenceID.MatchString(id) {
		writeError(w, 400, "Invalid sequence ID")
		return
	}
	if err := app.sequences.Delete(id); err != nil {
		if errors.Is(err, ErrSeqNotFound) {
			writeError(w, 404, err.Error())
		} else {
			writeError(w, 500, err.Error())
		}
		return
	}
	writeJSON(w, map[string]string{"status": "deleted"})
}

func writeSeqExecError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAlreadyRunning):
		writeError(w, 409, err.Error())
	case errors.Is(err, ErrSeqNotFound):
		writeError(w, 404, err.Error())
	case errors.Is(err, ErrNotRunning):
		writeError(w, 409, err.Error())
	default:
		writeError(w, 500, err.Error())
	}
}

func (app *App) handleStartSequence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSequenceID.MatchString(id) {
		writeError(w, 400, "Invalid sequence ID")
		return
	}
	if err := app.sequences.StartSequence(id); err != nil {
		writeSeqExecError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "started"})
}

func (app *App) handleStopSequence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSequenceID.MatchString(id) {
		writeError(w, 400, "Invalid sequence ID")
		return
	}
	if err := app.sequences.StopSequence(id); err != nil {
		writeSeqExecError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "stopping"})
}

func (app *App) handleRestartSequence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSequenceID.MatchString(id) {
		writeError(w, 400, "Invalid sequence ID")
		return
	}
	if err := app.sequences.RestartSequence(id); err != nil {
		writeSeqExecError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "restarting"})
}

func (app *App) handleAbortSequence(w http.ResponseWriter, r *http.Request) {
	if err := app.sequences.AbortExecution(); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "aborting"})
}

func (app *App) handleSequenceStatus(w http.ResponseWriter, r *http.Request) {
	exec := app.sequences.GetExecution()
	if exec == nil {
		writeJSON(w, map[string]any{"status": "idle"})
		return
	}
	writeJSON(w, exec)
}

func (app *App) handleSequencesSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := app.sequences.SubscribeSeq()
	defer app.sequences.UnsubscribeSeq(ch)

	// Send current execution state on connect (reconnection recovery)
	// Include terminal states so reconnecting clients see the final result
	exec := app.sequences.GetExecution()
	if exec != nil {
		eventType := "seq-update"
		switch exec.Status {
		case "complete":
			eventType = "seq-complete"
		case "failed":
			eventType = "seq-failed"
		case "aborted":
			eventType = "seq-aborted"
		}
		data, err := json.Marshal(SeqEvent{Type: eventType, Data: *exec})
		if err == nil {
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	} else {
		if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
			return
		}
		flusher.Flush()
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
