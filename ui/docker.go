package main

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

// Container represents a Docker container with its current state and stats
type Container struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Image           string  `json:"image"`
	State           string  `json:"state"`
	Health          string  `json:"health"`
	HasRestartLabel bool    `json:"hasRestartLabel"`
	HasLabel        bool    `json:"hasLabel"`
	Uptime          string  `json:"uptime"`
	StartedAt       string  `json:"startedAt"`
	CPU             float64 `json:"cpu"`
	Memory          uint64  `json:"memory"`
	MemoryLimit     uint64  `json:"memoryLimit"`
	AvgCPU          float64     `json:"avgCpu"`
	AvgMemory       uint64      `json:"avgMemory"`
	NetRxRate       float64     `json:"netRxRate"`
	NetTxRate       float64     `json:"netTxRate"`
	Networks        []string    `json:"networks"`
	RecentStats     []StatPoint `json:"recentStats,omitempty"`
	Created         string      `json:"created,omitempty"`
	RestartPolicy   string      `json:"restartPolicy"`
	HealthcheckCmd  string      `json:"healthcheckCmd"`
	Icon            string      `json:"icon,omitempty"`
	NetParent       string      `json:"netParent,omitempty"`     // parent container name when using container:X network mode
	NetParentDown   bool        `json:"netParentDown,omitempty"` // true when network parent container is not running
	Status          string      `json:"status,omitempty"`    // transient status: stopped-health, stopped-mem
	InternalPorts   []uint16    `json:"-"`                   // internal container ports, used for healthcheck suggestions
}

// ListContainers returns all containers with their current state and resource usage
func (app *App) ListContainers(ctx context.Context) ([]Container, error) {
	raw, err := app.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}

	// Build ID→name lookup for resolving container:ID network modes
	idToName := make(map[string]string, len(raw))
	for _, c := range raw {
		if len(c.Names) > 0 {
			n := strings.TrimPrefix(c.Names[0], "/")
			idToName[c.ID] = n
			// Also map short ID (12 chars)
			if len(c.ID) > 12 {
				idToName[c.ID[:12]] = n
			}
		}
	}

	containers := make([]Container, len(raw))

	for i, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		// Get detailed inspect for health, labels, restart policy
		inspect, err := app.docker.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}

		health := "none"
		if inspect.State.Health != nil {
			health = string(inspect.State.Health.Status)
		}

		hasLabel := false
		if v, ok := inspect.Config.Labels[app.restartLabel]; ok && v == "true" {
			hasLabel = true
		}
		effectiveRestart := hasLabel && !app.isRestartDisabled(name)

		uptime := ""
		startedAt := ""
		if c.State == "running" && inspect.State.StartedAt != "" {
			t, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt)
			if err == nil {
				uptime = formatUptime(time.Since(t))
				startedAt = inspect.State.StartedAt
			}
		}

		restartPolicy := ""
		if inspect.HostConfig != nil {
			restartPolicy = string(inspect.HostConfig.RestartPolicy.Name)
		}

		icon := inspect.Config.Labels["net.unraid.docker.icon"]

		healthcheckCmd := ""
		if inspect.Config.Healthcheck != nil && len(inspect.Config.Healthcheck.Test) > 1 {
			healthcheckCmd = strings.Join(inspect.Config.Healthcheck.Test[1:], " ")
		}

		// Collect internal (private) TCP ports from port bindings for healthcheck suggestions
		var internalPorts []uint16
		for _, p := range c.Ports {
			if p.Type == "tcp" && p.PrivatePort > 0 {
				// Deduplicate (same private port can appear multiple times with different host bindings)
				found := false
				for _, existing := range internalPorts {
					if existing == p.PrivatePort {
						found = true
						break
					}
				}
				if !found {
					internalPorts = append(internalPorts, p.PrivatePort)
				}
			}
		}

		var networks []string
		if inspect.NetworkSettings != nil {
			for netName := range inspect.NetworkSettings.Networks {
				networks = append(networks, netName)
			}
			sort.Strings(networks)
		}
		// Containers using another container's network (e.g. container:vpn-gateway)
		netParent := ""
		if len(networks) == 0 && inspect.HostConfig != nil {
			mode := string(inspect.HostConfig.NetworkMode)
			if strings.HasPrefix(mode, "container:") {
				ref := strings.TrimPrefix(mode, "container:")
				if resolved, ok := idToName[ref]; ok {
					ref = resolved
				}
				netParent = ref
				networks = []string{"→ " + ref}
			} else if mode == "host" {
				networks = []string{"host"}
			}
		}

		// Get transient status (stopped-health, stopped-mem) if available
		containerStatus := ""
		if app.stats != nil {
			containerStatus = app.stats.GetContainerStatus(name)
		}

		containers[i] = Container{
			ID:              c.ID[:12],
			Name:            name,
			Image:           c.Image,
			State:           c.State,
			Health:          health,
			HasRestartLabel: effectiveRestart,
			HasLabel:        hasLabel,
			Uptime:          uptime,
			StartedAt:       startedAt,
			Created:         inspect.Created,
			RestartPolicy:   restartPolicy,
			HealthcheckCmd:  healthcheckCmd,
			Icon:            icon,
			NetParent:       netParent,
			Status:          containerStatus,
			InternalPorts:   internalPorts,
			Networks:        networks,
		}

		// Read live stats from StatsCollector (no Docker API calls, keyed by name)
		if c.State == "running" && app.stats != nil {
			if live, ok := app.stats.GetLatest(name); ok {
				containers[i].CPU = live.CPU
				containers[i].Memory = live.Memory
				containers[i].MemoryLimit = live.MemoryLimit
				containers[i].NetRxRate = live.NetRxRate
				containers[i].NetTxRate = live.NetTxRate
			}
			if avgCPU, avgMem, ok := app.stats.GetAverage(name); ok {
				containers[i].AvgCPU = avgCPU
				containers[i].AvgMemory = avgMem
			}
			containers[i].RecentStats = app.stats.GetRecentStats(name, 20)
		}
	}

	// Filter out empty entries (from failed inspects)
	result := make([]Container, 0, len(containers))
	for _, c := range containers {
		if c.Name != "" {
			result = append(result, c)
		}
	}

	// Mark containers whose network parent is not running
	nameToState := make(map[string]string, len(result))
	for _, c := range result {
		nameToState[c.Name] = c.State
	}
	for i := range result {
		if result[i].NetParent != "" {
			if parentState, ok := nameToState[result[i].NetParent]; !ok || parentState != "running" {
				result[i].NetParentDown = true
			}
		}
	}

	return result, nil
}

// calculateCPUPercent computes CPU usage percentage (same formula as docker stats)
func calculateCPUPercent(stats *container.StatsResponse) float64 {
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)

	if systemDelta > 0.0 && cpuDelta > 0.0 {
		cpuCount := float64(stats.CPUStats.OnlineCPUs)
		if cpuCount == 0 {
			cpuCount = float64(len(stats.CPUStats.CPUUsage.PercpuUsage))
		}
		if cpuCount == 0 {
			cpuCount = float64(runtime.NumCPU())
		}
		pct := (cpuDelta / systemDelta) * cpuCount * 100.0
		// Clamp to sane range — Docker can report bogus deltas on first read or clock skew
		if pct > cpuCount*100.0 {
			return 0.0
		}
		return pct
	}
	return 0.0
}

// calculateMemUsage returns memory usage minus cache
func calculateMemUsage(stats *container.StatsResponse) uint64 {
	usage := stats.MemoryStats.Usage
	// Subtract inactive_file cache if available
	if cache, ok := stats.MemoryStats.Stats["inactive_file"]; ok {
		if usage > cache {
			usage -= cache
		}
	}
	return usage
}

// formatUptime formats a duration into a human-readable string
func formatUptime(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
