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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"constat-ui/auth"
	"constat-ui/netsec"

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

	// Registry credential store for private image update checks.
	// Must be created before the UpdateChecker so it can pass auth headers
	// to the Docker daemon's /distribution endpoint.
	registryStore := NewRegistryStore()

	// Create app context
	app := &App{
		docker:          cli,
		events:          events,
		restartLabel:    restartLabel,
		stats:           statsCollector,
		sequences:       seqExecutor,
		registryStore:   registryStore,
		restartDisabled: make(map[string]bool),
	}
	app.loadRestartDisabled()

	// Start Docker event watcher
	go app.WatchEvents(ctx)

	// Stats-history cleanup: phantom containers (transient one-shots with
	// auto-generated names like `admiring_hofstadter`, left behind when
	// their stats leaked into history maps forever) are purged reactively
	// via the `destroy` event handler in WatchEvents. This goroutine is
	// the safety net: one immediate pass 30 s after startup to flush the
	// backlog from prior constat versions + pre-destroy-handler uptime,
	// then hourly passes to catch any destroys the event stream might
	// have missed (reconnect gaps, daemon restarts).
	safeGo("stats-cleanup", func() {
		// Initial scan — wait for syncStreams to have run at least once so
		// we don't race against it and drop entries that are about to be
		// populated for currently-running containers.
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		if n := statsCollector.PruneStale(ctx); n > 0 {
			log.Printf("StatsCleanup: pruned %d stale container(s) from stats history on startup", n)
		}

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := statsCollector.PruneStale(ctx); n > 0 {
					log.Printf("StatsCleanup: pruned %d stale container(s) from stats history", n)
				}
			}
		}
	})

	// Start image cleanup scheduler
	imageCleaner := &ImageCleaner{docker: cli, app: app}
	app.imageCleaner = imageCleaner
	go imageCleaner.Run(ctx)

	// Start update checker
	updateChecker := NewUpdateChecker(cli, registryStore)
	app.updateChecker = updateChecker
	go updateChecker.Run(ctx)

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
	mux.HandleFunc("POST /api/config/test-gotify", app.handleTestGotify)
	mux.HandleFunc("GET /api/containers/{id}/history", app.handleContainerHistory)
	mux.HandleFunc("GET /api/containers/{id}/config", app.handleContainerConfig)
	mux.HandleFunc("GET /api/containers/{id}/logs/stream", app.handleLogsSSE)
	mux.HandleFunc("GET /api/containers/{id}/logs/tail", app.handleLogsTail)
	mux.HandleFunc("POST /api/containers/{id}/kill", app.handleKillContainer)
	mux.HandleFunc("POST /api/containers/{id}/pause", app.handlePauseContainer)
	mux.HandleFunc("POST /api/containers/{id}/unpause", app.handleUnpauseContainer)
	mux.HandleFunc("GET /api/stats/stream", app.handleStatsSSE)

	// Network topology
	mux.HandleFunc("GET /api/networks", app.handleNetworks)

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

	// Image management
	mux.HandleFunc("GET /api/images", app.handleListImages)
	mux.HandleFunc("DELETE /api/images/{id}", app.handleRemoveImage)
	mux.HandleFunc("POST /api/images/prune", app.handlePruneImages)
	mux.HandleFunc("GET /api/image-cleanup/status", app.handleImageCleanupStatus)

	// Update checking
	mux.HandleFunc("GET /api/updates", app.handleGetUpdates)
	mux.HandleFunc("POST /api/updates/check", app.handleTriggerUpdateCheck)

	// Private registry login (Tools tab). The logout endpoint takes the
	// host as a query parameter instead of a path wildcard because
	// Docker Hub's canonical key contains slashes that can't fit a
	// single-segment ServeMux pattern.
	mux.HandleFunc("GET /api/registry", app.handleListRegistry)
	mux.HandleFunc("POST /api/registry/login", app.handleRegistryLogin)
	mux.HandleFunc("DELETE /api/registry", app.handleRegistryLogout)

	// Categories
	cats := newCategoryStore()
	app.categories = cats
	mux.HandleFunc("GET /api/categories", cats.handleListCategories)
	mux.HandleFunc("PUT /api/categories", cats.handleUpdateCategories)

	// Volume management
	mux.HandleFunc("GET /api/volumes", app.handleListVolumes)
	mux.HandleFunc("DELETE /api/volumes/{name}", app.handleRemoveVolume)
	mux.HandleFunc("POST /api/volumes/prune", app.handlePruneVolumes)

	// ==== Authentication =====================================================
	// Loads auth config from constat.conf (falls back to safe defaults if the
	// file is missing — fresh install). Middleware enforces auth on every
	// request; handlers below provide the setup wizard + login page + status.
	authStore, authHandlers := initAuth(ctx)
	app.authStore = authStore

	mux.HandleFunc("GET /setup", authHandlers.handleSetupPage)
	mux.HandleFunc("POST /setup", authHandlers.handleSetupSubmit)
	mux.HandleFunc("GET /login", authHandlers.handleLoginPage)
	mux.HandleFunc("POST /login", authHandlers.handleLoginSubmit)
	mux.HandleFunc("POST /logout", authHandlers.handleLogout)
	mux.HandleFunc("GET /api/auth/status", authHandlers.handleAuthStatus)
	mux.HandleFunc("GET /api/auth/api-key", authHandlers.handleGetAPIKey)
	mux.HandleFunc("POST /api/auth/regenerate-api-key", authHandlers.handleRegenAPIKey)
	mux.HandleFunc("POST /api/auth/change-password", authHandlers.handleChangePassword)

	// Public liveness endpoint. Returns 200 regardless of auth config so
	// Docker HEALTHCHECK / Uptime Kuma / Kubernetes probes work even when
	// AUTHENTICATION_REQUIRED=enabled blocks the rest of the API.
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	// Static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("Failed to create static file system: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// Background: reap expired sessions every 5 minutes (bounds memory).
	safeGo("session-cleanup", func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				authStore.CleanupExpiredSessions()
			}
		}
	})

	// Wrap the mux with the middleware stack. Order (outermost first):
	//   SecurityHeaders → CSRF → Auth → mux
	//
	// SecurityHeaders is outermost so the headers appear on every response,
	// including errors returned by inner middleware.
	// CSRF runs before Auth so the CSRF cookie is set on any GET (including
	// public paths like /login) and mutations are rejected before auth even
	// evaluates — cheaper and consistent with industry practice.
	// Auth runs last before the mux so all route handlers are protected.
	var handler http.Handler = authStore.Middleware(mux)
	handler = authStore.CSRFMiddleware(handler)
	handler = auth.SecurityHeadersMiddleware(handler)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
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

// initAuth loads auth settings from constat.conf (safe defaults if missing),
// validates the combination, loads existing credentials if configured, and
// returns the store + handlers ready to wire into the mux.
//
// Refuses to start (log.Fatal) on any unsafe combination (ModeNone without
// explicit I_UNDERSTAND_AUTH_IS_DISABLED=yes) or on malformed auth.json.
func initAuth(ctx context.Context) (*auth.Store, *AuthHandlers) {
	cfg := auth.DefaultConfig()

	// Read settings from constat.conf if it exists. Missing file is fine —
	// fresh install will show the setup wizard.
	if data, err := ReadConfig(configPath); err == nil {
		if data.Authentication != "" {
			cfg.Mode = auth.AuthMode(data.Authentication)
		}
		if data.AuthenticationRequired != "" {
			cfg.Requirement = auth.Requirement(data.AuthenticationRequired)
		}
		if data.SessionTTLDays != "" {
			if days, perr := strconv.Atoi(data.SessionTTLDays); perr == nil && days > 0 {
				cfg.SessionTTL = time.Duration(days) * 24 * time.Hour
			}
		}
		if data.TrustedProxies != "" {
			ips, perr := netsec.ParseTrustedProxies(data.TrustedProxies)
			if perr != nil {
				log.Fatalf("auth: invalid TRUSTED_PROXIES config: %v", perr)
			}
			cfg.TrustedProxies = ips
		}
		if data.TrustedNetworks != "" {
			nets, perr := netsec.ParseTrustedNetworks(data.TrustedNetworks)
			if perr != nil {
				log.Fatalf("auth: invalid TRUSTED_NETWORKS config: %v", perr)
			}
			cfg.TrustedNetworks = nets
		}
	} else if !os.IsNotExist(err) {
		log.Printf("auth: could not read %s for auth settings: %v (using defaults)", configPath, err)
	}

	// Env-var override for trust-boundary config. If the env var is set at
	// process start, that value wins over the config-file value AND the UI
	// cannot change it. Use this in Unraid templates / docker-compose to
	// lock down the trust boundary against UI-takeover attacks (session
	// hijack, local-bypass peer adding themselves to the trust list).
	//
	// Note: these env vars are read from the process environment (docker
	// run --env), NOT from constat.conf — that file is parsed separately
	// and never exported to the process.
	if envNets := strings.TrimSpace(os.Getenv("TRUSTED_NETWORKS")); envNets != "" {
		nets, err := netsec.ParseTrustedNetworks(envNets)
		if err != nil {
			log.Fatalf("auth: invalid TRUSTED_NETWORKS env var: %v", err)
		}
		cfg.TrustedNetworks = nets
		cfg.TrustedNetworksLocked = true
		cfg.TrustedNetworksRaw = envNets
		log.Printf("auth: trusted_networks locked by TRUSTED_NETWORKS env var (%d entries)", len(nets))
	}
	if envProxies := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); envProxies != "" {
		ips, err := netsec.ParseTrustedProxies(envProxies)
		if err != nil {
			log.Fatalf("auth: invalid TRUSTED_PROXIES env var: %v", err)
		}
		cfg.TrustedProxies = ips
		cfg.TrustedProxiesLocked = true
		cfg.TrustedProxiesRaw = envProxies
		log.Printf("auth: trusted_proxies locked by TRUSTED_PROXIES env var (%d entries)", len(ips))
	}

	if err := auth.ValidateConfig(cfg); err != nil {
		log.Fatalf("auth config refuses to start: %v", err)
	}

	store := auth.NewStore(cfg)
	if _, err := store.Load(); err != nil {
		log.Fatalf("auth: load credentials: %v", err)
	}

	if store.IsConfigured() {
		log.Printf("auth: mode=%s required=%s user=%s", cfg.Mode, cfg.Requirement, store.Username())
	} else {
		log.Printf("auth: no credentials yet — first run, /setup wizard will prompt for admin user")
	}

	if cfg.Mode == auth.ModeNone {
		log.Printf("auth: WARNING — authentication is DISABLED via AUTHENTICATION=none. Do not expose this container to untrusted networks.")
	}

	// While in none mode, emit a loud warning periodically so it shows up
	// in Gotify/Discord/log-aggregators (not just the startup log that
	// scrolls out of view). Re-checks current mode every tick so a live-
	// reload TO auth=none picks up the alarm without restart, and a
	// live-reload FROM none stops it.
	//
	// Wired to caller-supplied context so graceful shutdown stops the
	// ticker instead of leaking it (significant if initAuth is ever
	// called in a test harness or future hot-reload flow).
	safeGo("auth-none-warning", func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if store.Config().Mode == auth.ModeNone {
					log.Printf("auth: WARNING — authentication is still DISABLED. Every request is admin. Re-enable auth or bind to 127.0.0.1.")
				}
			}
		}
	})

	return store, &AuthHandlers{Store: store}
}

// Version is set at build time via -ldflags="-X main.Version=..."
var Version = "dev"

// sharedNotifyClient is a package-level HTTP client for Discord/Gotify
// notifications. Reused across all notification calls to avoid leaking
// idle connection pools from per-call http.Client creation.
//
// Wrapped with netsec SSRF protection: outbound calls are blocked to
// loopback, RFC1918, link-local, IPv6 ULA, cloud metadata, and CGN.
// Discord/Gotify are public endpoints so this never rejects legitimate
// traffic. Allowlist is nil — users who want to send webhooks to a LAN
// Gotify instance must configure it on a non-blocked address or we will
// need per-deployment allowlisting (deferred until asked).
var sharedNotifyClient = netsec.NewSafeHTTPClient(10*time.Second, nil)
const restartDisabledPath = "/config/restart_disabled.json"

// App holds shared application state
type App struct {
	docker          *client.Client
	events          *EventBuffer
	restartLabel    string
	stats           *StatsCollector
	sequences       *SequenceExecutor
	imageCleaner    *ImageCleaner
	updateChecker   *UpdateChecker
	registryStore   *RegistryStore
	categories      *categoryStore
	authStore       *auth.Store // exposed so handleUpdateConfig can live-reload auth settings
	restartDisabled map[string]bool
	restartMu       sync.RWMutex
	// configMu serialises PUT /api/config (handleUpdateConfig). H4 fleet-drift
	// fix: without this, two concurrent saves can both ReadConfig → mutate →
	// WriteConfig and the second write clobbers the first. One save at a time
	// closes the race.
	configMu sync.Mutex
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
	if err := atomicWriteFile(restartDisabledPath, data, 0664); err != nil {
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
