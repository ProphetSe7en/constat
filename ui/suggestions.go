package main

import "strings"

// Suggestion represents a healthcheck suggestion for a container
type Suggestion struct {
	Container  string `json:"container"`
	Image      string `json:"image"`
	Suggestion string `json:"suggestion"`
	Note       string `json:"note,omitempty"`
}

// healthcheckSuggestions maps image patterns to suggested healthcheck commands.
// Patterns with "/" match owner/name (e.g. "hotio/radarr" matches "ghcr.io/hotio/radarr:latest").
// Patterns without "/" match the image name exactly (e.g. "postgres" matches "postgres:16").
var healthcheckSuggestions = map[string]struct {
	cmd  string
	note string
}{
	// Reverse proxy
	"linuxserver/swag": {
		cmd: `--health-cmd='curl -fsSk https://localhost:443 -o /dev/null || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=90s`,
	},
	// Databases
	"postgres": {
		cmd: `--health-cmd='pg_isready -U postgres || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=120s`,
	},
	"mariadb": {
		cmd: `--health-cmd='mariadb-admin ping -h localhost --silent || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=120s`,
	},
	// Media
	"hotio/plex": {
		cmd: `--health-cmd='curl --connect-timeout 15 --silent --show-error --fail http://localhost:32400/identity || exit 1' --health-interval=60s --health-retries=3 --health-timeout=30s --health-start-period=120s`,
	},
	"plexinc/pms-docker": {
		cmd: `--health-cmd='curl --connect-timeout 15 --silent --show-error --fail http://localhost:32400/identity || exit 1' --health-interval=60s --health-retries=3 --health-timeout=30s --health-start-period=120s`,
	},
	"linuxserver/plex": {
		cmd: `--health-cmd='curl --connect-timeout 15 --silent --show-error --fail http://localhost:32400/identity || exit 1' --health-interval=60s --health-retries=3 --health-timeout=30s --health-start-period=120s`,
	},
	// Arr apps
	"linuxserver/radarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:7878/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"hotio/radarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:7878/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"linuxserver/sonarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:8989/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"hotio/sonarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:8989/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"linuxserver/prowlarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:9696/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"hotio/prowlarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:9696/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"linuxserver/bazarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:6767/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"hotio/bazarr": {
		cmd: `--health-cmd='curl -fSs http://localhost:6767/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	// Web apps — seerr variants (same API, different images)
	"overseerr": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:5055/api/v1/status || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"jellyseerr": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:5055/api/v1/status || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"tautulli": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:8181/status || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"autobrr": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:7474/api/healthz/liveness || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
	},
	// Utilities
	"tecnativa/docker-socket-proxy": {
		cmd:  `--health-cmd='wget --spider --quiet http://localhost:2375/_ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		note: "Requires PING=1 environment variable to be set",
	},
	"flaresolverr": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:8191/health || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"scrutiny": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:8080/api/health || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
	},
	"thelounge": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:9000/ || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
	},
	"zipline": {
		cmd:  `--health-cmd='wget -q --spider http://127.0.0.1:3000/ || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		note: "Uses 127.0.0.1 (not localhost) and wget (no curl available)",
	},
	"vaultwarden/server": {
		cmd: `--health-cmd='curl -fSs http://localhost:80/alive || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
	},
	// Torrent clients
	"hotio/qbittorrent": {
		cmd: `--health-cmd='curl -fSs http://localhost:8080/api/v2/app/version || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"linuxserver/qbittorrent": {
		cmd: `--health-cmd='curl -fSs http://localhost:8080/api/v2/app/version || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	// Schedulers
	"cronicle": {
		cmd: `--health-cmd='curl -fSs -o /dev/null http://localhost:3012/ || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
	},
	"kometa": {
		cmd:  `--health-cmd='kill -0 1 || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=120s`,
		note: "Minimal Python container — no curl/wget available, uses process check",
	},
	"seasonpackarr": {
		cmd:  `--health-cmd='pgrep -f seasonpackarr' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		note: "No HTTP health endpoint — uses process check",
	},
	// Cloudflare DDNS (hotio — no curl/wget available)
	"cloudflareddns": {
		cmd:  `--health-cmd='pgrep -f cloudflare || exit 1' --health-interval=300s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		note: "No curl/wget available in hotio images — uses process check",
	},
}

// matchImage checks if a container image matches a suggestion pattern.
// Patterns with "/" match against the last two path segments (owner/name).
// Patterns without "/" match the image name (last segment) exactly.
// Tags and digests are stripped before matching.
func matchImage(image, pattern string) bool {
	image = strings.ToLower(image)
	pattern = strings.ToLower(pattern)

	// Strip tag
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		image = image[:idx]
	}
	// Strip digest
	if idx := strings.Index(image, "@"); idx != -1 {
		image = image[:idx]
	}

	parts := strings.Split(image, "/")
	name := parts[len(parts)-1]

	if strings.Contains(pattern, "/") {
		// Pattern has owner — match owner/name
		if len(parts) >= 2 {
			return parts[len(parts)-2]+"/"+name == pattern
		}
		return false
	}

	// Pattern is just a name — exact match
	return name == pattern
}

// GetSuggestions returns healthcheck suggestions for containers without healthchecks
func GetSuggestions(containers []Container) []Suggestion {
	var suggestions []Suggestion

	for _, c := range containers {
		// Only suggest for running containers without healthcheck
		if c.State != "running" || c.Health != "none" {
			continue
		}

		for pattern, suggestion := range healthcheckSuggestions {
			if matchImage(c.Image, pattern) {
				suggestions = append(suggestions, Suggestion{
					Container:  c.Name,
					Image:      c.Image,
					Suggestion: suggestion.cmd,
					Note:       suggestion.note,
				})
				break
			}
		}
	}

	return suggestions
}
