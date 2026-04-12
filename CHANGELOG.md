# Changelog

## v0.9.16

### Improved

- **Mobile view — update indicator** — Containers with available updates now show an orange container name in mobile view, with an "Outdated" badge in the expanded detail. Uses the same update check data as desktop view. No layout shift.

## v0.9.15

### Features
- **Private registry login** — Update checks now work for images on private container registries (sponsor-gated GHCR packages, private organization images, paid Docker Hub accounts). Add credentials under **Tools → Image Updates → Private registry login**. Credentials are verified against the registry's `/v2/` endpoint with bearer-token challenge handling before saving, stored in the standard Docker config format (`/config/.docker/config.json`, permissions 600), and are never returned via the API or written to logs. If an authenticated request is rejected for an image that is actually publicly pullable (GHCR behavior when the token owner lacks explicit access to a public package), Constat automatically retries anonymously so logging in never breaks checks for other images on the same host.
- **Token creation guide in UI** — The Private registry login form includes an inline, always-visible guide with a direct link to `github.com/settings/tokens/new` and a warning that only the `read:packages` scope should be enabled. Docker Hub alternative is also covered.
- **Progress indicator during update checks** — The Check Now button now shows `Checking 42/57` live with the name of the container currently being processed, so you can see what is happening during the 1–2 minute full run instead of a static spinner.
- **Calmer update-check taxonomy** — Replaced the binary "up to date / error" classification with five distinct, non-alarming states: **Outdated** (yellow, actionable), **Up to date** (green), **Local** (locally built, grey), **No access** (registry requires login, grey), and a neutral dash for rare edge cases (pinned digests, tagless locals, transient registry errors). Summary row in the Image Updates panel no longer uses the word "errors" for local builds and hides zero-count categories. Update-column sort order now puts most-actionable states first.

### Breaking changes
- **regctl removed from the image** — Update checks now go through the Docker daemon's `/distribution/{ref}/json` endpoint (`client.DistributionInspect`) instead of spawning `regctl` as a subprocess for each container. The image is ~38 MB smaller, update checks are faster, and there are no more `signal: killed` errors when a registry is slow. No user action required — the new path is strictly better. If you were reading `update-status.json` directly for the `Error` field, a few error strings have changed wording (e.g. `registry check timed out` → the daemon's own `context deadline exceeded`).

### Performance
- **No more 1-second rate limit between containers** — The previous sleep existed to throttle the regctl subprocess. Now that Constat goes through the Docker daemon directly (which queues and serializes registry calls internally), the sleep has been removed. A full update check of 57 containers is roughly 57 seconds faster per run.
- **Registry auths snapshot once per run** — Configured credentials are now loaded once at the start of each check run instead of re-reading `/config/.docker/config.json` for every container. For a 57-container run this drops 57 disk reads down to 1.

### Internal
- **`DELETE /api/registry` now takes `host` as a query parameter** instead of a path wildcard. Go's ServeMux single-segment wildcard cannot match Docker Hub's canonical key (`https://index.docker.io/v1/`) which contains slashes.
- **`RegistryStore.Save()` validates host format** against a regex (with an explicit pass-through for the Docker Hub sentinel key) so callers can't write garbage keys if they bypass the Verify/login path.
- **Frontend race fix:** `/api/updates` poll responses are now ignored if a newer sequence number has already been applied. Previously, out-of-order responses from overlapping in-flight requests could flip `updateChecking` back to `true` after the check completed, leaving the UI stuck on "Checking...".
- **README:** Added sections documenting the Tools tab, Image Update Checks, and Private registry login with a step-by-step token creation guide.

## v0.9.14

### Bug fixes
- **Update checker exclude list ignored** — Containers added to the Tools → Image Cleanup exclude list still appeared under "Updates available" or "Errors" because the cached results from earlier checks were never cleaned up. Now excluded containers are filtered out at the API layer (immediate effect after saving config) and also pruned from the result map on every check run.
- **Exclude takes effect immediately on save** — `saveConfig` now triggers a fresh `/api/updates` fetch right after a successful save, so newly-added exclusions disappear from the UI without waiting for the next 60s poll.
- **"Last check" timestamp frozen** — The "Last check: X ago" label only re-rendered when the underlying value changed, so the relative time appeared stuck even though polling kept running in the background. Now ticks live every second alongside other timer-driven UI.
- **`signal: killed` registry errors** — When a registry check exceeded the 30s timeout, `exec.CommandContext` killed regctl with SIGKILL and the cryptic `signal: killed` string was shown to the user. Now reported as `registry check timed out (>30s)`.
- **Updates list does not refresh during a running check** — While `checking=true`, the frontend now polls `/api/updates` every 2s instead of 60s, so the up-to-date / updates / errors counts and the per-container badges trickle in live as regctl finishes each container. Polling falls back to the normal 60s cadence as soon as the check completes.
- **Stale image references after repo rename** — Containers whose `Config.Image` points at a repo name that no longer matches any local tag (e.g. user renamed the GHCR repo but the local image still carries the old tag) caused Docker's list API to return a bare `sha256:...` for `c.Image`. The previous tag-recovery code then picked the *local* RepoTag (the old, displaced repo name) and asked the registry about it — failing with `unauthorized` because the old repo no longer exists. Now the checker first calls `ContainerInspect` and prefers `Config.Image` (the container template's intent) when recovering a tagless `c.Image`, falling back to `RepoTags[0]` and finally the OCI title-label recovery.
- **Registry retry on missing-image errors** — As a second safety net, when the chosen image ref returns `unauthorized` / `manifest unknown` / `not found`, the checker now retries with whatever tag the local image actually carries. This catches edge cases where neither `Config.Image` nor `RepoTags[0]` initially produces a working ref.

## v0.9.13

### Bug fixes
- **Update checker failing on tagless images** — When a local image tag was displaced by a newer pull (common with rolling tags like hotio's `:release`), the Docker API returned `sha256:...` instead of the tag in the container list. Constat then tried to query `docker.io/library/sha256:...` via regctl and failed with `unauthorized`. Now reconstructs the full image reference from the OCI `org.opencontainers.image.title` label combined with the repo path from `RepoDigests`, so update checks work correctly even for tagless images. Falls back to a helpful error message ("repull `<repo>` with your desired tag") when reconstruction isn't possible.
- **Header toolbar shifts when values change** — CPU/RAM/NET stats in the top header had variable-width fields, so the entire toolbar shifted sideways whenever a number gained or lost digits (e.g. `0 B/s` → `12.3 MB/s`). Fixed with `font-variant-numeric: tabular-nums` globally on stat numbers plus per-type `min-width` + right-align on the header resource pills.

## v0.9.12

### Bug fixes
- **Gotify settings autofill** — Browser password managers no longer autofill the Gotify URL and token fields in Config (added `autocomplete="one-time-code"`). Also applied to the Discord webhook field.

### Improvements
- **Version single source of truth** — Version is now defined once in the Dockerfile `ARG VERSION` and injected into the Go binary at build time (`-ldflags -X main.Version=...`) and into `constat.sh` via the `CONSTAT_VERSION` environment variable. CI overrides it automatically from the git tag on release builds.
- **Healthcheck suggestions** — Added verified suggestion for `drazzilb08/daps`.

## v0.9.11

### Features
- **Gotify push notifications** — Optional Gotify support alongside Discord. One app token with configurable priority values per severity level (critical/warning/info). Includes test button in settings UI and markdown formatting in messages
- **Network parent down detection** — Containers using `container:X` network mode now show a warning triangle when their network parent is down. Red badge in the Networks column, Discord/Gotify notifications, and events in the Events tab. No restart of dependent containers (restarting doesn't help when the network parent is the problem)

### Bug fixes
- **Health tooltip width** — Tooltip no longer follows the narrow column width, now centers over the badge with proper sizing

### Improvements
- **Version sync** — Fixed Go web UI version (`0.9.8` → `0.9.11`) to match bash script and changelog

## v0.9.10

### Bug fixes
- **Stats freeze after container restart** — Fixed: per-container stats stream goroutine exited on restart (EOF) but never cleaned up, preventing syncStreams from starting a new stream. Stats (CPU/RAM/network) would freeze permanently for restarted containers.
- **Group view column misalignment** — Fixed: group header row was missing the Mem Watch column cell, causing all columns after Health to shift left (Update showed in Mem Watch, CPU in Restart, etc.).

## v0.9.9

### Features
- **Memory rule escalation** — Restart rules can now optionally stop a container after X restarts within a configurable time window (e.g. "stop after 3 restarts in 24h"). Configured per rule with `maxTriggers` and `maxWindow` fields.
- **Mem Watch column** — New sortable column in container table showing memory usage as percentage of rule limit with color coding: green (<75%), orange (75-90%), red (>90%). Shows countdown timer and trigger count during active restarts.
- **Real-time health, state, and uptime** — Health status, container state, and uptime now update via SSE every 3-10 seconds without page refresh. Powered by Docker inspect in syncStreams.
- **Health column badges** — New "Checking" badge (yellow) when health check is pending. "Stopped: health" and "Stopped: mem" badges (red) when container was stopped by escalation. Badges persist across UI refresh and Constat restarts.
- **Memory event badges** — Events tab shows "mem restart" (red) and "mem warn" (orange) badges that are distinct from manual restarts. Memory detail visible on group header without expanding.
- **Unhealthy recheck** — Periodic recheck of unhealthy containers that missed their restart due to cooldown. Ensures restart eventually happens when cooldown expires.
- **Memory trigger counter** — Live display of restart count vs max (e.g. "1/3 restarts") in both expanded memory watch view and Mem Watch column.

### Improvements
- **Rename notify → warn** — Memory watch action "notify" renamed to "warn" throughout config, UI, logs, and Discord. Legacy "notify" auto-normalizes to "warn" for backward compatibility.
- **Health auto-restart redesign** — Cooldown is now minimum time between restarts (not a batch window). After max restarts without sustained recovery, container is stopped with Discord notification. Recovery requires staying healthy for at least the cooldown period.
- **Event ordering** — Expanded events sorted chronologically with logical tiebreaker (memory trigger → restarted → healthy). Redundant "started" event filtered when "restarted" exists.
- **Config hints** — Auto-restart settings (cooldown, max restarts, restart label) now have explanatory text in Config tab.
- **Update badge** — "Update" renamed to "Outdated" for clarity.
- **Update check refresh** — Tools → Updates "last check" time now auto-refreshes every 60s without page reload.
- **Group border** — Expanded event group left border brightened for better visibility.
- **No HC tooltip** — Shortened to "expand container for suggestion" instead of showing full healthcheck command in tooltip.
- **Memory watch forms** — Labeled fields (Limit, Action, Duration, Max restarts, Period), contextual descriptions that change when switching warn/restart, auto-formatting of inputs (duration in seconds, period in hours).

### Bug fixes
- **Health restart counter** — Fixed: counter no longer resets on every container start event (was breaking cooldown and max restarts). Counter only resets on sustained healthy recovery or escalation stop.
- **Separate memory cooldown** — Memory restarts use independent counters from health auto-restart. Memory restarts have no artificial limit — the duration threshold provides natural rate-limiting.
- **Docker state suppression removed** — Memory-triggered restarts no longer suppress Docker state events. All events logged normally; UI grouping and badges handle presentation.
- **Status persistence** — Container escalation status (stopped-health, stopped-mem) persisted to /config/container_status.json, survives Constat restarts and UI refreshes.

## v0.9.8

### Features
- **Image update checker** — Detect available Docker image updates via registry digest comparison using regctl. Periodic checks (configurable: 6h/12h/24h), manual "Check Now" button, per-container exclude list. No images are pulled — only manifests are checked.
- **Update badges** — New "Update" column in container table showing Up to date (green), Update (orange), Local, or Pinned status with informative tooltips.
- **Updates filter** — Orange filter badge showing count of containers with available updates. Click to filter the container list.
- **Update notifications** — Discord notifications via maintenance webhook for newly discovered updates (no duplicate alerts).

### Improvements
- **Health badges** — "No Check" renamed to "No Health" in filters, "No HC" in table with hoverable tooltip showing suggested healthcheck command. Stopped containers show empty health cell.
- **Group visual separation** — Spacer rows between groups, colored left border per group (auto-assigned), containers listed before detail panel for clearer visual hierarchy.
- **Group sorting** — Column headers in group view are now sortable, sorting containers within each group independently.
- **Group reordering** — Move groups up/down in Manage Groups modal with arrow buttons.
- **Event detail** — Memory watch "exceeded limit by X" now correctly calculates difference across mixed units (MiB/GiB).
- **Uptime sorting** — Stopped containers sort last regardless of sort direction.
- **Sortable Update column** — Click to sort by update status (updates first).

### Bug fixes
- **Alpine expression errors** — Fixed `sequenceList` → `sequences` rename, `formatUptime` → `c.uptime` in group view.
- **updateSummary collision** — Renamed to `updateCheckSummary` to avoid conflict with existing method.
- **Missing closing tag** — Fixed broken `<td>` in group detail panel.
- **Update checker** — Stale results cleaned up for removed containers, digest-pinned images skipped, platform-specific digest comparison (not manifest list), context cancellation in check loop, Discord notifications only for new updates.

## v0.9.7

### Features
- **Container groups** — Create user-defined groups (e.g. "Media Stack", "Network") via Manage Groups modal. Groups display aggregated CPU, RAM, and network stats with sparklines. Containers can belong to multiple groups.
- **Group expanded view** — Click a group header to expand members and open an aggregated chart. Toggle individual container overlays with color-coded pills (up to 8 members). Same chart controls as container view (CPU/RAM toggle, 1h/6h/24h/3d/7d range, hover tooltip with per-member values).
- **Group actions** — Stop All, Restart All, Start All buttons with custom confirm dialogs. Buttons shown contextually based on group state.
- **Maintenance webhook** — New `DISCORD_WEBHOOK_MAINTENANCE` for cleanup notifications (image/volume prune). Falls back to health webhook if empty. Separates maintenance from health concerns.
- **Event detail inline** — Memory watch events (notify, recovered, blocked) now show context inline: "exceeded limit for 31s (11.88 GiB / 10.00 GiB)". Multi-line details (crash logs) shown as hover tooltip.
- **Version display** — Version number and "Built by ProphetSe7en" shown in header, dynamically fetched from backend.

### Improvements
- **Shared network detection** — Containers using `container:X` network mode show "(shared)" label on network I/O stats. Group and header totals exclude shared containers to prevent double-counting.
- **Consistent network colors** — Upload (green) and download (orange) arrow colors unified across all views.
- **Group header info** — Running count in green, healthy/unhealthy/no check/stopped counts in expanded view pills.
- **Delete group UX** — Delete button (x) directly in group list view, custom confirm dialog instead of browser prompt.
- **Badge alignment** — Fixed-width event badges for consistent column alignment in event log.

### Bug fixes
- **Group sparkline CPU scaling** — Dynamic max for groups where total CPU exceeds 100%.
- **Chart X-axis** — Timestamp-based spacing in group charts (consistent with container charts).
- **File permissions** — Categories config uses 0664 (consistent with other config files).

## v0.9.6

### Features
- **Memory watch hot-reload** — Changing thresholds, actions, or adding/removing rules takes effect immediately without container restart. Timers are preserved for unchanged rules.
- **Cleanup Discord notifications** — Scheduled cleanup, manual image prune, and manual volume prune now send Discord notifications via the health webhook with summary of what was removed.

### Bug fixes
- **Version in Discord footer** — Was hardcoded as v0.6.0, now shows correct version in all Discord notifications.
- **Memory watch unit inconsistency** — Thresholds displayed as "512m"/"1.5g" while actual usage showed "512 MiB"/"1.5 GiB". Both now use consistent MiB/GiB format.

## v0.9.5

### Improvements
- **Unified Cleanup section** — Merged Image Cleanup and Volume Cleanup into single "Cleanup" section under Tools
- **Scheduled volume prune** — Three individual toggles (orphan images, unused images, unused volumes) replace old Mode dropdown — all default off for safe opt-in
- **Consistent layout** — Images and Volumes subsections use identical card styling, font sizes, and spacing
- Migration from legacy `IMAGE_CLEANUP_MODE` to new toggles (existing configs auto-migrate)

## v0.9.4

### Features
- **Tools tab** — New tab between Network and Config for operational tools
- **Volume Cleanup** — List, inspect, and delete unused Docker volumes with mountpoint display and container tracking
- **Compact memory watch** — Rules grouped by container with inline progress bars, collapsible section, color-coded actions (notify=yellow, restart=red)

### Improvements
- **Sticky save bar** — Config/Tools save button stays visible at bottom of viewport
- **Reorganized Config** — Memory Watch and Image Cleanup moved to Tools tab. Config now has 4 pure settings sections (Discord, Behavior, Display, Auto-Restart)
- **Font readability** — Bumped font sizes across cleanup sections (10-11px → 12-13px), replaced #6e7681 with #8b949e globally
- **Consistent delete buttons** — Red "Delete" buttons in first column position across all cleanup lists, renamed from "Remove" for consistency with "Delete all"
- **Section descriptions** — Prominent 13px descriptions for Memory Watch, Image Cleanup, and Volume Cleanup

### Bug fixes
- **Unused image prune** — Fixed "Delete all unused" doing nothing. Docker API needs `dangling=false` filter to prune tagged unused images
- **Dry run label** — Clarified that dry run only applies to scheduled cleanup, not manual buttons
- **CPU avg overflow** — Docker occasionally reports bogus CPU deltas, causing avg values in the millions. Now clamped to sane range and corrupted averages auto-reset on load

### Security
- **Volume name validation** — Regex validation on volume delete endpoint
- **Nil pointer guard** — Bounds check on container Names slice in volume listing

## v0.9.2

### Improvements
- **Memory optimization** — Stats history uses two-tier storage: recent 24h at 30s intervals, older data aggregated to 5min intervals. Reduces RAM usage ~75% (509 MiB → ~130 MiB) and stats file size ~90% with no visible difference in charts
- **Compare bar visibility** — "Compare:" label and "+ Add" button now more visible (brighter text and blue dashed border)

## v0.9.1

### Bug fixes
- **Chart time scale distortion** — Live SSE data points (every 3s) caused chart to compress historical data into a tiny area. Fixed by using timestamp-based x-positioning instead of array-index spacing
- **Compare chart not updating live** — Compare overlay now correctly aligns with time-based main chart
- **Chart hover accuracy** — Tooltip snaps to nearest data point by timestamp (binary search) instead of linear index mapping

## v0.9.0

### Features
- **Mobile view** — Dedicated mobile UI with auto-detect (screen width <=768px) and manual toggle in header
- **Container cards** — Compact cards showing name, status, health badge, CPU%, RAM%, and uptime
- **Tap to expand** — Memory watch rules with progress bars, simplified CPU/RAM charts (1h-7d), action buttons (restart/stop/pause/start)
- **Sort and filter** — Sort by name, CPU, RAM, or health with ascending/descending toggle. Filter by status (all/running/unhealthy/stopped)
- **Resource summary** — Total CPU, RAM, and network I/O displayed in compact bar
- **Mobile events** — Simplified event timeline with grouped events and container filter
- **View persistence** — Mobile/desktop preference saved in localStorage

## v0.8.0

### Features
- **Scheduled image cleanup** — Daily scheduler (ImageCleaner goroutine) with configurable time, mode (dangling/all), and dry-run
- **Search improvements** — Clear button and multi-term OR search (space-separated)

### Bug fixes
- **Security hardening** — Shell injection prevention in config write, data race mutex on LastResult, timezone/name validation, strict time parsing

## v0.7.1

### Features
- **Search clear button** — Quick-clear search with X button
- **Multi-term OR search** — Space-separated terms match as OR conditions

## v0.7.0

### Features
- **Orphan image cleanup** — Config > Maintenance section for removing unused Docker images
- **Sticky table header** — Column headers stay visible when scrolling
- **Sequence action buttons** — Run/delete buttons in sequence cards
- **Image tag badge** — Shows image tag in container details
- **Port-aware healthcheck suggestions** — Auto-detects actual container ports for accurate healthcheck commands
- **Extra parameters panel** — View Docker extra parameters per container

## v0.6.0

### Features
- **Memory notifications** — Discord notifications for memory watch threshold events
- **Container icons** — Automatic icon detection from container labels
- **Pause/unpause** — Full container pause/unpause support with state indicators
- **Restart override toggle** — Clickable Yes/No in table to override auto-restart per container
- **Sparkline click-to-chart** — Click sparkline to jump directly to full chart
- **Event cleanup** — Crash context in events, improved event grouping
- **Network tab** — Swim lane topology with click-to-expand details and balanced grid layout
- **Sequences** — Multi-step container orchestration with dependency chains, emoji picker, searchable dropdown, and step delay

## v0.5.0

### Features
- **Log viewer** — Real-time log streaming with server-side timestamp extraction, ANSI escape stripping, and level detection (error/warn/info/debug)
- **Dozzle-inspired styling** — Color-coded left borders per log level, row striping
- **Memory watch multi-rule** — Multiple memory watch rules per container
- **Display toggles** — Show/hide stats columns and charts via config
- **Docker proxy support** — `DOCKER_HOST=tcp://dockerproxy:2375` support

## v0.4.0

### Features
- **Streaming stats** — Replaced polling with `docker stats` streaming goroutines via SSE (3s updates)
- **Network I/O** — Live upload/download rates per container
- **Webhook test** — Test Discord webhooks from Config tab
- **Sortable columns** — Sort by any column in container table
- **Sparklines** — Mini CPU/RAM graphs in each table row
- **Event grouping** — Rapid events grouped with expand/collapse

## v0.3.0

### Features
- **Charts** — CPU/RAM history graphs with selectable time range
- **Events tab** — Docker event history (start/stop/die/health changes)
- **Config editor** — Edit all settings from the browser
- **Discord notifications** — State changes and health events with colored embeds

## v0.2.0

### Features
- **Health monitoring** — Docker healthcheck status tracking
- **Auto-restart** — Label-gated restart for unhealthy containers with cooldown
- **Memory watch** — Per-container memory thresholds with notify or restart actions

## v0.1.0

### Features
- **Initial release** — Container list with live CPU/RAM stats, health badges, web UI on port 7890
