package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

var validVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// VolumeInfo represents a Docker volume with usage classification
type VolumeInfo struct {
	Name           string   `json:"name"`
	Driver         string   `json:"driver"`
	Mountpoint     string   `json:"mountpoint"`
	Created        string   `json:"created"`
	Status         string   `json:"status"` // "in-use", "unused"
	Containers     int      `json:"containers"`
	ContainerNames []string `json:"containerNames"`
}

// listVolumes returns all local volumes classified by usage status
func (app *App) listVolumes(ctx context.Context) ([]VolumeInfo, error) {
	// Get all volumes
	vols, err := app.docker.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("volume list: %w", err)
	}

	// Get all containers (running + stopped) to determine which volumes are in use
	containers, err := app.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}

	// Build set of volume names referenced by containers
	usedVolumes := make(map[string][]string) // volume name -> container names
	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		cName := strings.TrimPrefix(c.Names[0], "/")
		for _, m := range c.Mounts {
			if m.Type == "volume" {
				usedVolumes[m.Name] = append(usedVolumes[m.Name], cName)
			}
		}
	}

	var result []VolumeInfo
	for _, v := range vols.Volumes {
		name := v.Name
		driver := v.Driver
		mountpoint := v.Mountpoint
		created := v.CreatedAt

		status := "unused"
		containerCount := 0
		var containerNames []string
		if names, ok := usedVolumes[name]; ok {
			status = "in-use"
			containerCount = len(names)
			containerNames = names
		}

		result = append(result, VolumeInfo{
			Name:           name,
			Driver:         driver,
			Mountpoint:     mountpoint,
			Created:        created,
			Status:         status,
			Containers:     containerCount,
			ContainerNames: containerNames,
		})
	}

	// Sort: unused first, then in-use. Within each group, by name.
	statusOrder := map[string]int{"unused": 0, "in-use": 1}
	sort.Slice(result, func(i, j int) bool {
		si, sj := statusOrder[result[i].Status], statusOrder[result[j].Status]
		if si != sj {
			return si < sj
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})

	return result, nil
}

// handleListVolumes returns all local volumes with classification
func (app *App) handleListVolumes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	volumes, err := app.listVolumes(ctx)
	if err != nil {
		writeError(w, 500, "Failed to list volumes")
		log.Printf("Error listing volumes: %v", err)
		return
	}

	var inUse, unused int
	for _, v := range volumes {
		switch v.Status {
		case "in-use":
			inUse++
		case "unused":
			unused++
		}
	}

	writeJSON(w, map[string]interface{}{
		"volumes": volumes,
		"inUse":   inUse,
		"unused":  unused,
	})
}

// handleRemoveVolume removes a single volume by name
func (app *App) handleRemoveVolume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validVolumeName.MatchString(name) {
		writeError(w, 400, "Invalid volume name")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	err := app.docker.VolumeRemove(ctx, name, false)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to remove volume: %v", err))
		return
	}

	log.Printf("Removed volume: %s", name)
	writeJSON(w, map[string]string{"status": "removed"})
}

// handlePruneVolumes removes all unused volumes
func (app *App) handlePruneVolumes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	report, err := app.docker.VolumesPrune(ctx, filters.NewArgs())
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to prune volumes: %v", err))
		return
	}

	count := len(report.VolumesDeleted)
	log.Printf("Pruned %d volumes, reclaimed %d bytes", count, report.SpaceReclaimed)

	writeJSON(w, map[string]interface{}{
		"deleted":        count,
		"spaceReclaimed": report.SpaceReclaimed,
	})
}
