package main

import (
	"bufio"
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// Event represents a container event
type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	Container   string    `json:"container"`
	Image       string    `json:"image"`
	Type        string    `json:"type"`
	Action      string    `json:"action"`
	ExitCode    string    `json:"exitCode,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	AutoRestart bool      `json:"autoRestart"`
}

// EventBuffer is a thread-safe ring buffer for events
type EventBuffer struct {
	mu          sync.RWMutex
	events      []Event
	maxSize     int
	subscribers map[chan Event]struct{}
}

// NewEventBuffer creates a new event buffer with the given capacity
func NewEventBuffer(size int) *EventBuffer {
	return &EventBuffer{
		events:      make([]Event, 0, size),
		maxSize:     size,
		subscribers: make(map[chan Event]struct{}),
	}
}

// Add appends an event to the buffer and notifies subscribers
func (eb *EventBuffer) Add(event Event) {
	eb.mu.Lock()
	if len(eb.events) >= eb.maxSize {
		// Copy to new slice to avoid memory leak from growing backing array
		newEvents := make([]Event, eb.maxSize-1, eb.maxSize)
		copy(newEvents, eb.events[1:])
		eb.events = newEvents
	}
	eb.events = append(eb.events, event)

	// Copy subscriber list while holding lock
	subs := make([]chan Event, 0, len(eb.subscribers))
	for ch := range eb.subscribers {
		subs = append(subs, ch)
	}
	eb.mu.Unlock()

	// Notify outside of lock
	for _, ch := range subs {
		select {
		case ch <- event:
		default:
			// Drop event if subscriber is slow
		}
	}
}

// List returns events matching the given filters
func (eb *EventBuffer) List(eventType, containerName string, limit, offset int) []Event {
	eb.mu.RLock()
	defer eb.mu.RUnlock()

	// Filter events (iterate in reverse for newest first)
	var filtered []Event
	for i := len(eb.events) - 1; i >= 0; i-- {
		e := eb.events[i]
		if eventType != "" && e.Type != eventType {
			continue
		}
		if containerName != "" && !strings.Contains(strings.ToLower(e.Container), strings.ToLower(containerName)) {
			continue
		}
		filtered = append(filtered, e)
	}

	// Apply offset and limit
	if offset > 0 {
		if offset >= len(filtered) {
			return nil
		}
		filtered = filtered[offset:]
	}
	if limit > 0 && limit < len(filtered) {
		filtered = filtered[:limit]
	}

	return filtered
}

// Total returns the total number of events in the buffer
func (eb *EventBuffer) Total() int {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	return len(eb.events)
}

// Subscribe creates a new channel for receiving live events
func (eb *EventBuffer) Subscribe() chan Event {
	ch := make(chan Event, 64)
	eb.mu.Lock()
	eb.subscribers[ch] = struct{}{}
	eb.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel
func (eb *EventBuffer) Unsubscribe(ch chan Event) {
	eb.mu.Lock()
	delete(eb.subscribers, ch)
	eb.mu.Unlock()
	// Don't close the channel — Add() may still be iterating a copied subscriber list.
	// Let GC handle cleanup.
}

// WatchEvents listens to Docker events and populates the buffer
func (app *App) WatchEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Println("Connecting to Docker event stream...")

		f := filters.NewArgs()
		f.Add("type", string(events.ContainerEventType))

		eventCh, errCh := app.docker.Events(ctx, events.ListOptions{Filters: f})

		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errCh:
				if err != nil {
					log.Printf("Docker event stream error: %v", err)
				}
				goto reconnect
			case msg := <-eventCh:
				app.processDockerEvent(msg)
			}
		}

	reconnect:
		log.Println("Docker event stream disconnected. Reconnecting in 5s...")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// fetchLastLogLines returns the last N log lines from a container, cleaned of ANSI codes.
func (app *App) fetchLastLogLines(containerID string, n int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	logReader, err := app.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "50",
	})
	if err != nil {
		return ""
	}
	defer logReader.Close()

	// Check TTY mode
	inspect, err := app.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return ""
	}

	var lines []string
	scanner := bufio.NewScanner(logReader)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !inspect.Config.Tty && len(line) >= 8 {
			line = line[8:]
		}
		cleaned := strings.TrimSpace(stripANSI(string(line)))
		if cleaned != "" {
			lines = append(lines, cleaned)
		}
	}

	// Filter out generic shutdown noise, keep the actual context
	var filtered []string
	for _, l := range lines {
		lower := strings.ToLower(l)
		if strings.Contains(lower, "stopping the container") ||
			strings.Contains(lower, "shutting down") ||
			strings.Contains(lower, "graceful shutdown") ||
			strings.Contains(lower, "signal received") ||
			strings.Contains(lower, "sigterm") ||
			strings.Contains(lower, "sigkill") ||
			strings.Contains(lower, "s6-rc: info:") ||
			strings.Contains(lower, "cont-finish") {
			continue
		}
		filtered = append(filtered, l)
	}
	if len(filtered) == 0 {
		filtered = lines
	}
	// Return last N lines after filtering
	if len(filtered) > n {
		filtered = filtered[len(filtered)-n:]
	}
	return strings.Join(filtered, "\n")
}

func (app *App) processDockerEvent(msg events.Message) {
	name := msg.Actor.Attributes["name"]
	image := msg.Actor.Attributes["image"]
	if name == "" {
		return
	}

	var event *Event

	switch msg.Action {
	case "start":
		event = &Event{
			Type:   "state",
			Action: "started",
		}
		// Clear escalation status when container starts
		if app.stats != nil {
			app.stats.SetContainerStatus(name, "")
		}
	case "stop":
		event = &Event{
			Type:   "state",
			Action: "stopped",
		}
	case "die":
		exitCode := msg.Actor.Attributes["exitCode"]
		if exitCode != "" && exitCode != "0" {
			// Fetch crash logs asynchronously to avoid blocking the event stream
			ts := time.Unix(msg.Time, msg.TimeNano%1e9)
			containerID := msg.Actor.ID
			go func() {
				detail := app.fetchLastLogLines(containerID, 5)
				e := Event{
					Timestamp: ts,
					Container: name,
					Image:     image,
					Type:      "state",
					Action:    "died",
					ExitCode:  exitCode,
					Detail:    detail,
				}
				app.events.Add(e)
			}()
			return // event added by goroutine
		}
	case "restart":
		event = &Event{
			Type:   "state",
			Action: "restarted",
		}
	case "pause":
		event = &Event{
			Type:   "state",
			Action: "paused",
		}
	case "unpause":
		event = &Event{
			Type:   "state",
			Action: "unpaused",
		}
	case "destroy":
		// Container fully removed (explicit `docker rm` or `--rm` auto-
		// cleanup for one-shots). Purge its in-memory stats so we don't
		// leak ring-buffer + map entries. See StatsCollector.RemoveContainer
		// for the rationale and the phantom-accumulation backstory.
		//
		// No event surfaced to the UI — destroys happen constantly for
		// transient one-shots and would flood the event feed. Real-name
		// containers the user cares about emit `die` + `stop` first,
		// which ARE surfaced.
		if app.stats != nil {
			app.stats.RemoveContainer(name)
		}
		return
	case events.ActionHealthStatusUnhealthy:
		hasLabel := false
		inspect, err := app.docker.ContainerInspect(context.Background(), msg.Actor.ID)
		if err == nil {
			if v, ok := inspect.Config.Labels[app.restartLabel]; ok && v == "true" {
				hasLabel = true
			}
		}
		if app.isRestartDisabled(name) {
			hasLabel = false
		}
		event = &Event{
			Type:        "health",
			Action:      "unhealthy",
			AutoRestart: hasLabel,
		}
	case events.ActionHealthStatusHealthy:
		event = &Event{
			Type:   "health",
			Action: "healthy",
		}
		// Clear stopped-health status on recovery
		if app.stats != nil {
			app.stats.SetContainerStatus(name, "")
		}
	}

	if event != nil {
		event.Timestamp = time.Unix(msg.Time, msg.TimeNano%1e9)
		event.Container = name
		event.Image = image
		app.events.Add(*event)
	}
}
