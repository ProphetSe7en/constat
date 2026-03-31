package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ConfigData represents the parsed constat.conf
type ConfigData struct {
	// Discord
	EnableDiscord string `json:"enableDiscord"`
	WebhookState  string `json:"webhookState"`
	WebhookHealth string `json:"webhookHealth"`
	// Identity
	BotName     string `json:"botName"`
	ServerLabel string `json:"serverLabel"`
	// Behavior
	BatchWindow       string `json:"batchWindow"`
	ExcludeContainers string `json:"excludeContainers"`
	SummaryInterval   string `json:"summaryInterval"`
	// Auto-restart
	RestartLabel    string `json:"restartLabel"`
	RestartCooldown string `json:"restartCooldown"`
	MaxRestarts     string `json:"maxRestarts"`
	// Memory monitoring
	MemoryPaused         string             `json:"memoryPaused"`
	MemoryPollInterval    string             `json:"memoryPollInterval"`
	MemoryDefaultDuration string             `json:"memoryDefaultDuration"`
	MemoryWatch           []MemoryWatchEntry `json:"memoryWatch"`
	// Scheduled cleanup
	ImageCleanupEnabled  string `json:"imageCleanupEnabled"`  // "true" or "false"
	ImageCleanupTime     string `json:"imageCleanupTime"`     // "HH:MM"
	ImageCleanupMode     string `json:"imageCleanupMode"`     // legacy: "dangling" or "all" (migrated to individual toggles)
	ImageCleanupDryRun   string `json:"imageCleanupDryRun"`   // "true" or "false"
	CleanupOrphanImages  string `json:"cleanupOrphanImages"`  // "true" or "false"
	CleanupUnusedImages  string `json:"cleanupUnusedImages"`  // "true" or "false"
	CleanupVolumes       string `json:"cleanupVolumes"`       // "true" or "false"
	// Display
	Timezone       string `json:"timezone"`
	TimeFormat     string `json:"timeFormat"`     // "24h" or "12h"
	DateFormat     string `json:"dateFormat"`     // "YYYY-MM-DD", "DD.MM.YYYY", "DD/MM/YYYY", "MM/DD/YYYY"
	ShowStats      string `json:"showStats"`      // "true" or "false"
	ShowCharts     string `json:"showCharts"`     // "true" or "false"
	// Colors
	ColorStarted    string `json:"colorStarted"`
	ColorStopped    string `json:"colorStopped"`
	ColorDied       string `json:"colorDied"`
	ColorUnhealthy  string `json:"colorUnhealthy"`
	ColorRecovered  string `json:"colorRecovered"`
	ColorRestarting string `json:"colorRestarting"`
	ColorMemoryWarn string `json:"colorMemoryWarn"`
	ColorMemoryCrit string `json:"colorMemoryCrit"`
}

// MemoryWatchEntry represents one entry in the MEMORY_WATCH array
type MemoryWatchEntry struct {
	Name     string `json:"name"`
	Limit    string `json:"limit"`
	Action   string `json:"action"`
	Duration string `json:"duration,omitempty"`
}

// keyToField maps bash variable names to ConfigData JSON field names
var keyToField = map[string]string{
	"ENABLE_DISCORD":          "enableDiscord",
	"DISCORD_WEBHOOK_STATE":   "webhookState",
	"DISCORD_WEBHOOK_HEALTH":  "webhookHealth",
	"BOT_NAME":                "botName",
	"SERVER_LABEL":            "serverLabel",
	"BATCH_WINDOW":            "batchWindow",
	"EXCLUDE_CONTAINERS":      "excludeContainers",
	"SUMMARY_INTERVAL":        "summaryInterval",
	"RESTART_LABEL":           "restartLabel",
	"RESTART_COOLDOWN":        "restartCooldown",
	"MAX_RESTARTS":            "maxRestarts",
	"MEMORY_PAUSED":          "memoryPaused",
	"MEMORY_POLL_INTERVAL":    "memoryPollInterval",
	"MEMORY_DEFAULT_DURATION": "memoryDefaultDuration",
	"COLOR_STARTED":           "colorStarted",
	"COLOR_STOPPED":           "colorStopped",
	"COLOR_DIED":              "colorDied",
	"COLOR_UNHEALTHY":         "colorUnhealthy",
	"COLOR_RECOVERED":         "colorRecovered",
	"COLOR_RESTARTING":        "colorRestarting",
	"COLOR_MEMORY_WARN":       "colorMemoryWarn",
	"COLOR_MEMORY_CRIT":       "colorMemoryCrit",
	"IMAGE_CLEANUP_ENABLED":   "imageCleanupEnabled",
	"IMAGE_CLEANUP_TIME":      "imageCleanupTime",
	"IMAGE_CLEANUP_MODE":      "imageCleanupMode",
	"IMAGE_CLEANUP_DRY_RUN":   "imageCleanupDryRun",
	"CLEANUP_ORPHAN_IMAGES":   "cleanupOrphanImages",
	"CLEANUP_UNUSED_IMAGES":   "cleanupUnusedImages",
	"CLEANUP_VOLUMES":         "cleanupVolumes",
	"TIMEZONE":                "timezone",
	"TIME_FORMAT":             "timeFormat",
	"DATE_FORMAT":             "dateFormat",
	"SHOW_STATS":              "showStats",
	"SHOW_CHARTS":             "showCharts",
}

// fieldToKey is the reverse mapping
var fieldToKey map[string]string

func init() {
	fieldToKey = make(map[string]string, len(keyToField))
	for k, v := range keyToField {
		fieldToKey[v] = k
	}
}

var kvPattern = regexp.MustCompile(`^([A-Z_]+)="(.*)"$`)

// shellSanitizer escapes characters that are dangerous inside double-quoted bash strings
var shellSanitizer = strings.NewReplacer(
	`"`, `\"`,
	`$`, `\$`,
	"`", "\\`",
	`\`, `\\`,
)

// ReadConfig parses a constat.conf file into ConfigData
func ReadConfig(path string) (*ConfigData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	data := &ConfigData{}
	values := make(map[string]string)

	scanner := bufio.NewScanner(f)
	inMemoryWatch := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Handle MEMORY_WATCH array
		if strings.HasPrefix(line, "MEMORY_WATCH=(") {
			inMemoryWatch = true
			// Check if single-line: MEMORY_WATCH=()
			if strings.HasSuffix(line, ")") {
				inner := line[len("MEMORY_WATCH=(") : len(line)-1]
				inner = strings.TrimSpace(inner)
				if inner != "" {
					data.MemoryWatch = parseMemoryWatchEntries(inner)
				}
				inMemoryWatch = false
			}
			continue
		}
		if inMemoryWatch {
			if strings.TrimSpace(line) == ")" {
				inMemoryWatch = false
				continue
			}
			// Parse entry: "name:limit:action[:duration]"
			entry := parseMemoryWatchEntry(line)
			if entry != nil {
				data.MemoryWatch = append(data.MemoryWatch, *entry)
			}
			continue
		}

		// Parse KEY="VALUE" lines
		m := kvPattern.FindStringSubmatch(line)
		if m != nil {
			values[m[1]] = m[2]
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Map values to struct fields
	data.EnableDiscord = values["ENABLE_DISCORD"]
	data.WebhookState = values["DISCORD_WEBHOOK_STATE"]
	data.WebhookHealth = values["DISCORD_WEBHOOK_HEALTH"]
	data.BotName = values["BOT_NAME"]
	data.ServerLabel = values["SERVER_LABEL"]
	data.BatchWindow = values["BATCH_WINDOW"]
	data.ExcludeContainers = values["EXCLUDE_CONTAINERS"]
	data.SummaryInterval = values["SUMMARY_INTERVAL"]
	data.RestartLabel = values["RESTART_LABEL"]
	data.RestartCooldown = values["RESTART_COOLDOWN"]
	data.MaxRestarts = values["MAX_RESTARTS"]
	data.MemoryPaused = values["MEMORY_PAUSED"]
	if data.MemoryPaused == "" {
		data.MemoryPaused = values["MEMORY_ENABLED"] // migration from old key
	}
	data.MemoryPollInterval = values["MEMORY_POLL_INTERVAL"]
	data.MemoryDefaultDuration = values["MEMORY_DEFAULT_DURATION"]
	data.ColorStarted = values["COLOR_STARTED"]
	data.ColorStopped = values["COLOR_STOPPED"]
	data.ColorDied = values["COLOR_DIED"]
	data.ColorUnhealthy = values["COLOR_UNHEALTHY"]
	data.ColorRecovered = values["COLOR_RECOVERED"]
	data.ColorRestarting = values["COLOR_RESTARTING"]
	data.ColorMemoryWarn = values["COLOR_MEMORY_WARN"]
	data.ColorMemoryCrit = values["COLOR_MEMORY_CRIT"]
	data.ImageCleanupEnabled = values["IMAGE_CLEANUP_ENABLED"]
	if data.ImageCleanupEnabled == "" {
		data.ImageCleanupEnabled = "false"
	}
	data.ImageCleanupTime = values["IMAGE_CLEANUP_TIME"]
	if data.ImageCleanupTime == "" {
		data.ImageCleanupTime = "03:00"
	}
	data.ImageCleanupMode = values["IMAGE_CLEANUP_MODE"]
	if data.ImageCleanupMode == "" {
		data.ImageCleanupMode = "dangling"
	}
	data.ImageCleanupDryRun = values["IMAGE_CLEANUP_DRY_RUN"]
	if data.ImageCleanupDryRun == "" {
		data.ImageCleanupDryRun = "true"
	}
	data.CleanupOrphanImages = values["CLEANUP_ORPHAN_IMAGES"]
	data.CleanupUnusedImages = values["CLEANUP_UNUSED_IMAGES"]
	data.CleanupVolumes = values["CLEANUP_VOLUMES"]
	// Migrate from legacy IMAGE_CLEANUP_MODE if new toggles not set
	if data.CleanupOrphanImages == "" && data.CleanupUnusedImages == "" {
		if data.ImageCleanupMode == "all" {
			data.CleanupOrphanImages = "true"
			data.CleanupUnusedImages = "true"
		} else if data.ImageCleanupMode == "dangling" {
			data.CleanupOrphanImages = "true"
			data.CleanupUnusedImages = "false"
		} else {
			data.CleanupOrphanImages = "false"
			data.CleanupUnusedImages = "false"
		}
	}
	if data.CleanupVolumes == "" {
		data.CleanupVolumes = "false"
	}
	data.Timezone = values["TIMEZONE"]
	if data.Timezone == "" {
		data.Timezone = os.Getenv("TZ")
	}
	data.TimeFormat = values["TIME_FORMAT"]
	if data.TimeFormat == "" {
		data.TimeFormat = "24h"
	}
	data.DateFormat = values["DATE_FORMAT"]
	if data.DateFormat == "" {
		data.DateFormat = "YYYY-MM-DD"
	}
	data.ShowStats = values["SHOW_STATS"]
	if data.ShowStats == "" {
		data.ShowStats = "true"
	}
	data.ShowCharts = values["SHOW_CHARTS"]
	if data.ShowCharts == "" {
		data.ShowCharts = "true"
	}

	if data.MemoryWatch == nil {
		data.MemoryWatch = []MemoryWatchEntry{}
	}

	return data, nil
}

// WriteConfig updates a constat.conf file in-place, preserving comments and structure
func WriteConfig(path string, data *ConfigData) error {
	// Build a map of KEY -> new value from the struct
	newValues := map[string]string{
		"ENABLE_DISCORD":          data.EnableDiscord,
		"DISCORD_WEBHOOK_STATE":   data.WebhookState,
		"DISCORD_WEBHOOK_HEALTH":  data.WebhookHealth,
		"BOT_NAME":                data.BotName,
		"SERVER_LABEL":            data.ServerLabel,
		"BATCH_WINDOW":            data.BatchWindow,
		"EXCLUDE_CONTAINERS":      data.ExcludeContainers,
		"SUMMARY_INTERVAL":        data.SummaryInterval,
		"RESTART_LABEL":           data.RestartLabel,
		"RESTART_COOLDOWN":        data.RestartCooldown,
		"MAX_RESTARTS":            data.MaxRestarts,
		"MEMORY_PAUSED":          data.MemoryPaused,
		"MEMORY_POLL_INTERVAL":    data.MemoryPollInterval,
		"MEMORY_DEFAULT_DURATION": data.MemoryDefaultDuration,
		"COLOR_STARTED":           data.ColorStarted,
		"COLOR_STOPPED":           data.ColorStopped,
		"COLOR_DIED":              data.ColorDied,
		"COLOR_UNHEALTHY":         data.ColorUnhealthy,
		"COLOR_RECOVERED":         data.ColorRecovered,
		"COLOR_RESTARTING":        data.ColorRestarting,
		"COLOR_MEMORY_WARN":       data.ColorMemoryWarn,
		"COLOR_MEMORY_CRIT":       data.ColorMemoryCrit,
		"IMAGE_CLEANUP_ENABLED":   data.ImageCleanupEnabled,
		"IMAGE_CLEANUP_TIME":      data.ImageCleanupTime,
		"IMAGE_CLEANUP_MODE":      data.ImageCleanupMode,
		"IMAGE_CLEANUP_DRY_RUN":   data.ImageCleanupDryRun,
		"TIMEZONE":                data.Timezone,
		"TIME_FORMAT":             data.TimeFormat,
		"DATE_FORMAT":             data.DateFormat,
		"SHOW_STATS":              data.ShowStats,
		"SHOW_CHARTS":             data.ShowCharts,
	}

	// Read existing file
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config for read: %w", err)
	}

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	// Process lines — replace KEY="VALUE" and rebuild MEMORY_WATCH
	var output []string
	inMemoryWatch := false
	memoryWatchWritten := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Handle MEMORY_WATCH array replacement
		if strings.HasPrefix(trimmed, "MEMORY_WATCH=(") {
			inMemoryWatch = true
			// Write replacement
			output = append(output, buildMemoryWatchBlock(data.MemoryWatch)...)
			memoryWatchWritten = true
			// If single-line, we're done
			if strings.HasSuffix(trimmed, ")") {
				inMemoryWatch = false
			}
			continue
		}
		if inMemoryWatch {
			if strings.TrimSpace(trimmed) == ")" {
				inMemoryWatch = false
			}
			continue // skip old array content
		}

		// Replace KEY="VALUE" lines
		m := kvPattern.FindStringSubmatch(trimmed)
		if m != nil {
			key := m[1]
			if val, ok := newValues[key]; ok {
				output = append(output, fmt.Sprintf(`%s="%s"`, key, shellSanitizer.Replace(val)))
				continue
			}
		}

		output = append(output, line)
	}

	// Safety: if MEMORY_WATCH was never in the file, append it
	if !memoryWatchWritten {
		output = append(output, "")
		output = append(output, buildMemoryWatchBlock(data.MemoryWatch)...)
	}

	// Config migration: append any new KEY="VALUE" fields not yet in the file
	writtenKeys := make(map[string]bool)
	for _, line := range output {
		m := kvPattern.FindStringSubmatch(strings.TrimSpace(line))
		if m != nil {
			writtenKeys[m[1]] = true
		}
	}
	var missing []string
	for key, val := range newValues {
		if !writtenKeys[key] {
			missing = append(missing, fmt.Sprintf(`%s="%s"`, key, shellSanitizer.Replace(val)))
		}
	}
	if len(missing) > 0 {
		output = append(output, "")
		output = append(output, "#=== ADDED BY CONFIG MIGRATION ============================================#")
		output = append(output, missing...)
	}

	// Write back — preserve nobody:users (99:100) ownership for Unraid
	content := strings.Join(output, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0664); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	os.Chown(path, 99, 100)

	return nil
}

// buildMemoryWatchBlock creates the MEMORY_WATCH=(...) lines
func buildMemoryWatchBlock(entries []MemoryWatchEntry) []string {
	if len(entries) == 0 {
		return []string{"MEMORY_WATCH=()"}
	}
	lines := []string{"MEMORY_WATCH=("}
	for _, e := range entries {
		entry := fmt.Sprintf("%s:%s:%s", shellSanitizer.Replace(e.Name), shellSanitizer.Replace(e.Limit), shellSanitizer.Replace(e.Action))
		if e.Duration != "" {
			entry += ":" + e.Duration
		}
		lines = append(lines, fmt.Sprintf(`    "%s"`, entry))
	}
	lines = append(lines, ")")
	return lines
}

// parseMemoryWatchEntries parses entries from a single-line MEMORY_WATCH=(...)
func parseMemoryWatchEntries(inner string) []MemoryWatchEntry {
	var entries []MemoryWatchEntry
	// Split on whitespace, each entry is quoted
	for _, part := range strings.Fields(inner) {
		entry := parseMemoryWatchEntry(part)
		if entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// parseMemoryWatchEntry parses a single "name:limit:action[:duration]" entry
func parseMemoryWatchEntry(raw string) *MemoryWatchEntry {
	// Strip quotes and whitespace
	s := strings.TrimSpace(raw)
	s = strings.Trim(s, `"`)
	if s == "" || strings.HasPrefix(s, "#") {
		return nil
	}

	parts := strings.SplitN(s, ":", 4)
	if len(parts) < 3 {
		return nil
	}

	entry := &MemoryWatchEntry{
		Name:   parts[0],
		Limit:  parts[1],
		Action: parts[2],
	}
	if len(parts) == 4 {
		entry.Duration = parts[3]
	}
	return entry
}
