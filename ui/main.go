package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/client"
)

//go:embed static
var staticFiles embed.FS

func main() {
	port := "7890"

	restartLabel := os.Getenv("RESTART_LABEL")
	if restartLabel == "" {
		restartLabel = "constat.restart"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
	defer cli.Close()

	// Verify Docker connection
	_, err = cli.Ping(ctx)
	if err != nil {
		log.Fatalf("Cannot connect to Docker: %v", err)
	}

	// Fetch host info for CPU/RAM totals
	hostCPUs := runtime.NumCPU()
	var hostMemory uint64
	info, err := cli.Info(ctx)
	if err != nil {
		log.Printf("Warning: failed to get Docker host info, using runtime fallback: %v", err)
	} else {
		hostCPUs = info.NCPU
		hostMemory = uint64(info.MemTotal)
	}

	// Initialize event buffer
	events := NewEventBuffer(1000)

	// Initialize stats collector with host info
	statsCollector := NewStatsCollector(cli, hostCPUs, hostMemory)
	go statsCollector.Run(ctx)

	// Initialize sequence executor
	seqExecutor := NewSequenceExecutor(cli)

	// Create app context
	app := &App{
		docker:          cli,
		events:          events,
		restartLabel:    restartLabel,
		stats:           statsCollector,
		sequences:       seqExecutor,
		restartDisabled: make(map[string]bool),
	}
	app.loadRestartDisabled()

	// Start Docker event watcher
	go app.WatchEvents(ctx)

	// Set up HTTP routes
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/summary", app.handleSummary)
	mux.HandleFunc("GET /api/containers", app.handleListContainers)
	mux.HandleFunc("GET /api/containers/{id}/stats", app.handleContainerStats)
	mux.HandleFunc("POST /api/containers/{id}/start", app.handleStartContainer)
	mux.HandleFunc("POST /api/containers/{id}/stop", app.handleStopContainer)
	mux.HandleFunc("POST /api/containers/{id}/restart", app.handleRestartContainer)
	mux.HandleFunc("GET /api/health-suggestions", app.handleHealthSuggestions)
	mux.HandleFunc("GET /api/events", app.handleListEvents)
	mux.HandleFunc("POST /api/events", app.handlePostEvent)
	mux.HandleFunc("GET /api/events/stream", app.handleEventsSSE)
	mux.HandleFunc("GET /api/config", app.handleGetConfig)
	mux.HandleFunc("PUT /api/config", app.handleUpdateConfig)
	mux.HandleFunc("POST /api/config/test-webhook", app.handleTestWebhook)
	mux.HandleFunc("GET /api/containers/{id}/history", app.handleContainerHistory)
	mux.HandleFunc("GET /api/containers/{id}/config", app.handleContainerConfig)
	mux.HandleFunc("GET /api/containers/{id}/logs/stream", app.handleLogsSSE)
	mux.HandleFunc("GET /api/stats/stream", app.handleStatsSSE)

	// Restart override route
	mux.HandleFunc("POST /api/restart-override/{name}", app.handleRestartOverride)

	// Sequence routes
	mux.HandleFunc("GET /api/sequences", app.handleListSequences)
	mux.HandleFunc("POST /api/sequences", app.handleCreateSequence)
	mux.HandleFunc("PUT /api/sequences/{id}", app.handleUpdateSequence)
	mux.HandleFunc("DELETE /api/sequences/{id}", app.handleDeleteSequence)
	mux.HandleFunc("POST /api/sequences/{id}/start", app.handleStartSequence)
	mux.HandleFunc("POST /api/sequences/{id}/stop", app.handleStopSequence)
	mux.HandleFunc("POST /api/sequences/{id}/restart", app.handleRestartSequence)
	mux.HandleFunc("POST /api/sequences/abort", app.handleAbortSequence)
	mux.HandleFunc("GET /api/sequences/status", app.handleSequenceStatus)
	mux.HandleFunc("GET /api/sequences/stream", app.handleSequencesSSE)

	// Static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("Failed to create static file system: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE needs unlimited write timeout
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		log.Println("Shutting down web UI...")
		cancel()
		seqExecutor.Close()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("Constat Web UI starting on port %s", port)
	fmt.Printf("[%s] Web UI available at http://localhost:%s\n", time.Now().Format("2006-01-02 15:04:05"), port)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}
}

const restartDisabledPath = "/config/restart_disabled.json"

// App holds shared application state
type App struct {
	docker          *client.Client
	events          *EventBuffer
	restartLabel    string
	stats           *StatsCollector
	sequences       *SequenceExecutor
	restartDisabled map[string]bool
	restartMu       sync.RWMutex
}

func (app *App) loadRestartDisabled() {
	data, err := os.ReadFile(restartDisabledPath)
	if err != nil {
		return // file doesn't exist yet — no overrides
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		log.Printf("Warning: invalid restart_disabled.json: %v", err)
		return
	}
	app.restartMu.Lock()
	defer app.restartMu.Unlock()
	for _, n := range names {
		app.restartDisabled[n] = true
	}
}

func (app *App) saveRestartDisabled() error {
	app.restartMu.RLock()
	names := make([]string, 0, len(app.restartDisabled))
	for n := range app.restartDisabled {
		names = append(names, n)
	}
	app.restartMu.RUnlock()
	data, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if err := os.WriteFile(restartDisabledPath, data, 0664); err != nil {
		return err
	}
	os.Chown(restartDisabledPath, 99, 100)
	return nil
}

func (app *App) isRestartDisabled(name string) bool {
	app.restartMu.RLock()
	defer app.restartMu.RUnlock()
	return app.restartDisabled[name]
}
