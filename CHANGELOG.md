# Changelog

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
