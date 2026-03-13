package main

import (
	"fmt"
	"strconv"
	"strings"
)

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
	cmd         string
	note        string
	defaultPort string // internal container port, empty for non-HTTP checks (pgrep, pg_isready, etc.)
}{
	// Reverse proxy
	"linuxserver/swag": {
		cmd:         `--health-cmd='curl -fsSk https://localhost:443 -o /dev/null || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=90s`,
		defaultPort: "443",
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
		cmd:         `--health-cmd='curl --connect-timeout 15 --silent --show-error --fail http://localhost:32400/identity || exit 1' --health-interval=60s --health-retries=3 --health-timeout=30s --health-start-period=120s`,
		defaultPort: "32400",
	},
	"plexinc/pms-docker": {
		cmd:         `--health-cmd='curl --connect-timeout 15 --silent --show-error --fail http://localhost:32400/identity || exit 1' --health-interval=60s --health-retries=3 --health-timeout=30s --health-start-period=120s`,
		defaultPort: "32400",
	},
	"linuxserver/plex": {
		cmd:         `--health-cmd='curl --connect-timeout 15 --silent --show-error --fail http://localhost:32400/identity || exit 1' --health-interval=60s --health-retries=3 --health-timeout=30s --health-start-period=120s`,
		defaultPort: "32400",
	},
	// Arr apps
	"linuxserver/radarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:7878/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "7878",
	},
	"hotio/radarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:7878/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "7878",
	},
	"linuxserver/sonarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:8989/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "8989",
	},
	"hotio/sonarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:8989/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "8989",
	},
	"linuxserver/prowlarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:9696/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "9696",
	},
	"hotio/prowlarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:9696/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "9696",
	},
	"linuxserver/bazarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:6767/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "6767",
	},
	"hotio/bazarr": {
		cmd:         `--health-cmd='curl -fSs http://localhost:6767/ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "6767",
	},
	// Web apps — seerr variants (same API, different images)
	"overseerr": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:5055/api/v1/status || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "5055",
	},
	"jellyseerr": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:5055/api/v1/status || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "5055",
	},
	"tautulli": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:8181/status || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "8181",
	},
	"autobrr": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:7474/api/healthz/liveness || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		defaultPort: "7474",
	},
	// Utilities
	"tecnativa/docker-socket-proxy": {
		cmd:         `--health-cmd='wget --spider --quiet http://localhost:2375/_ping || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		note:        "Requires PING=1 environment variable to be set",
		defaultPort: "2375",
	},
	"flaresolverr": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:8191/health || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "8191",
	},
	"scrutiny": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:8080/api/health || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		defaultPort: "8080",
	},
	"thelounge": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:9000/ || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		defaultPort: "9000",
	},
	"zipline": {
		cmd:         `--health-cmd='wget -q --spider http://127.0.0.1:3000/ || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		note:        "Uses 127.0.0.1 (not localhost) and wget (no curl available)",
		defaultPort: "3000",
	},
	"vaultwarden/server": {
		cmd:         `--health-cmd='curl -fSs http://localhost:80/alive || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=30s`,
		defaultPort: "80",
	},
	// Torrent clients
	"hotio/qbittorrent": {
		cmd:         `--health-cmd='curl -fSs http://localhost:8080/api/v2/app/version || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "8080",
	},
	"linuxserver/qbittorrent": {
		cmd:         `--health-cmd='curl -fSs http://localhost:8080/api/v2/app/version || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "8080",
	},
	// Schedulers
	"cronicle": {
		cmd:         `--health-cmd='curl -fSs -o /dev/null http://localhost:3012/ || exit 1' --health-interval=60s --health-retries=3 --health-timeout=10s --health-start-period=60s`,
		defaultPort: "3012",
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

	// Strip digest first (before tag, since digest may contain ":")
	if idx := strings.Index(image, "@"); idx != -1 {
		image = image[:idx]
	}
	// Strip tag
	if idx := strings.LastIndex(image, ":"); idx != -1 {
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

// adjustPort checks whether the default port in a healthcheck suggestion matches
// the container's actual internal ports. If the port differs, it substitutes the
// correct port and adds an explanatory note.
func adjustPort(cmd, note, defaultPort string, internalPorts []uint16) (string, string) {
	if len(internalPorts) == 0 {
		// No port info (host/container network mode, etc.) — use default
		return cmd, note
	}

	defPort, _ := strconv.ParseUint(defaultPort, 10, 16)

	// Check if default port exists in container's port bindings
	for _, p := range internalPorts {
		if p == uint16(defPort) {
			return cmd, note // confirmed, use as-is
		}
	}

	// Default port not found — try to find the actual port
	// For single-port containers, auto-substitute
	if len(internalPorts) == 1 {
		actualPort := strconv.Itoa(int(internalPorts[0]))
		cmd = strings.Replace(cmd, "localhost:"+defaultPort, "localhost:"+actualPort, 1)
		cmd = strings.Replace(cmd, "127.0.0.1:"+defaultPort, "127.0.0.1:"+actualPort, 1)
		portNote := fmt.Sprintf("Port adjusted: %s → %s (detected from container)", defaultPort, actualPort)
		if note != "" {
			note = note + ". " + portNote
		} else {
			note = portNote
		}
		return cmd, note
	}

	// Multiple ports, can't determine which is correct — warn user
	portNote := fmt.Sprintf("Default port %s not detected — verify the port matches your configuration", defaultPort)
	if note != "" {
		note = note + ". " + portNote
	} else {
		note = portNote
	}
	return cmd, note
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
				cmd := suggestion.cmd
				note := suggestion.note

				// Adjust port if we have port info and a default port to check
				if suggestion.defaultPort != "" {
					cmd, note = adjustPort(cmd, note, suggestion.defaultPort, c.InternalPorts)
				}

				suggestions = append(suggestions, Suggestion{
					Container:  c.Name,
					Image:      c.Image,
					Suggestion: cmd,
					Note:       note,
				})
				break
			}
		}
	}

	return suggestions
}
