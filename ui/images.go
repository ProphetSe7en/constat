package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

// ImageInfo represents a Docker image with usage classification
type ImageInfo struct {
	ID         string   `json:"id"`
	RepoTags   []string `json:"repoTags"`
	Size       int64    `json:"size"`
	Created    int64    `json:"created"`
	Containers int      `json:"containers"` // number of containers using this image
	Status     string   `json:"status"`     // "in-use", "unused", "dangling"
}

// listImages returns all local images classified by usage status
func (app *App) listImages(ctx context.Context) ([]ImageInfo, error) {
	// Get all images
	imgs, err := app.docker.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		return nil, fmt.Errorf("image list: %w", err)
	}

	// Get all containers (running + stopped) to determine which images are in use
	containers, err := app.docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("container list: %w", err)
	}

	// Build set of image IDs referenced by containers
	usedImageIDs := make(map[string]int) // imageID -> container count
	for _, c := range containers {
		usedImageIDs[c.ImageID]++
	}

	var result []ImageInfo
	for _, img := range imgs {
		id := img.ID
		if strings.HasPrefix(id, "sha256:") {
			id = id[7:]
		}
		if len(id) > 12 {
			id = id[:12]
		}

		tags := img.RepoTags
		if tags == nil {
			tags = []string{}
		}

		// Classify
		status := "unused"
		containerCount := 0
		if count, ok := usedImageIDs[img.ID]; ok {
			status = "in-use"
			containerCount = count
		}
		// Dangling: no tags (or only <none>:<none>)
		if len(img.RepoTags) == 0 || (len(img.RepoTags) == 1 && img.RepoTags[0] == "<none>:<none>") {
			if status != "in-use" {
				status = "dangling"
			}
			tags = []string{"<none>"}
		}

		result = append(result, ImageInfo{
			ID:         id,
			RepoTags:   tags,
			Size:       img.Size,
			Created:    img.Created,
			Containers: containerCount,
			Status:     status,
		})
	}

	// Sort: dangling first, then unused, then in-use. Within each group, by created desc.
	statusOrder := map[string]int{"dangling": 0, "unused": 1, "in-use": 2}
	sort.Slice(result, func(i, j int) bool {
		si, sj := statusOrder[result[i].Status], statusOrder[result[j].Status]
		if si != sj {
			return si < sj
		}
		return result[i].Created > result[j].Created
	})

	return result, nil
}

// handleListImages returns all local images with classification
func (app *App) handleListImages(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	images, err := app.listImages(ctx)
	if err != nil {
		writeError(w, 500, "Failed to list images")
		log.Printf("Error listing images: %v", err)
		return
	}

	// Summary
	var totalSize, reclaimableSize int64
	var inUse, unused, dangling int
	for _, img := range images {
		totalSize += img.Size
		switch img.Status {
		case "in-use":
			inUse++
		case "unused":
			unused++
			reclaimableSize += img.Size
		case "dangling":
			dangling++
			reclaimableSize += img.Size
		}
	}

	writeJSON(w, map[string]interface{}{
		"images":          images,
		"totalSize":       totalSize,
		"reclaimableSize": reclaimableSize,
		"inUse":           inUse,
		"unused":          unused,
		"dangling":        dangling,
	})
}

// handleRemoveImage removes a single image by ID
func (app *App) handleRemoveImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validContainerID.MatchString(id) {
		writeError(w, 400, "Invalid image ID")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	_, err := app.docker.ImageRemove(ctx, id, image.RemoveOptions{
		Force:         false,
		PruneChildren: true,
	})
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to remove image: %v", err))
		return
	}

	log.Printf("Removed image: %s", id)
	writeJSON(w, map[string]string{"status": "removed"})
}

// handlePruneImages removes dangling-only or all unused images
func (app *App) handlePruneImages(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode != "dangling" {
		mode = "all"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	pruneFilters := filters.NewArgs()
	if mode == "dangling" {
		pruneFilters.Add("dangling", "true")
	} else {
		// dangling=false prunes all unused images (orphan + tagged unused)
		pruneFilters.Add("dangling", "false")
	}

	report, err := app.docker.ImagesPrune(ctx, pruneFilters)
	if err != nil {
		writeError(w, 500, fmt.Sprintf("Failed to prune images: %v", err))
		return
	}

	count := len(report.ImagesDeleted)
	reclaimed := int64(report.SpaceReclaimed)
	label := "orphan"
	if mode == "all" {
		label = "unused"
	}
	log.Printf("Pruned %d %s images, reclaimed %d bytes", count, label, reclaimed)

	if count > 0 {
		go sendDiscordMaintenance("Image Cleanup", fmt.Sprintf("Removed %d %s images, reclaimed %s", count, label, formatBytesGo(reclaimed)), 0x3fb950)
		go sendGotifyMaintenance("Image Cleanup", fmt.Sprintf("Removed %d %s images, reclaimed %s", count, label, formatBytesGo(reclaimed)))
	}

	writeJSON(w, map[string]interface{}{
		"deleted":        count,
		"spaceReclaimed": reclaimed,
	})
}

// handleImageCleanupStatus returns the last scheduled cleanup result
func (app *App) handleImageCleanupStatus(w http.ResponseWriter, r *http.Request) {
	if app.imageCleaner == nil {
		writeJSON(w, map[string]interface{}{"lastRun": nil})
		return
	}
	result := app.imageCleaner.GetLastResult()
	writeJSON(w, map[string]interface{}{"lastRun": result})
}
