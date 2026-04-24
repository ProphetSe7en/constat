# Constat — Container Stats Web UI

**Last Updated:** 2026-04-22 (CLAUDE.md restructure Phase 3 pilot)

Root `CLAUDE.md` rules, paths, and shared docs still apply — this file is project-specific orientation only.

---

## Identity

- **GitHub:** `ProphetSe7en/constat` (public)
- **Docker image:** `ghcr.io/prophetse7en/constat:latest` + Docker Hub mirror
- **Local build tag:** `constat:dev`
- **Tech:** Go 1.25.9 + Alpine.js single-page app on Alpine 3.21
- **Data:** `/config/constat.conf`, `/config/auth.json` (0700 dir + 0600 files), `/config/.docker/config.json` (private-registry creds)
- **Replaced:** Sentinel + Monocker + Autoheal (absorbed all three)

---

## Status (2026-04-22)

| Channel | Version | Notes |
|---------|---------|-------|
| GHCR `:latest` + Docker Hub | **v0.9.17** | Security release (2026-04-19) — T1–T74 applied |
| Local `:dev` | ad-hoc | pprof re-enabled for memory investigation |

v0.9.17 is live in production. Breaking change: first boot redirects to `/setup` wizard. Default auth mode is "disabled for trusted networks" (LAN users skip login with default CIDRs).

---

## Current focus

**No active feature work in flight** — v0.9.17 shipped and is stable. Next tasks when session picks up:

1. **Settings sidebar design-language refresh** (TODO.md P1) — match vpn-gateway + PurgeBot reference pattern (left sidebar for sections, right panel for content). Current Constat Settings is flat scrolling.
2. **Container update via Docker SDK** — replace any remaining update-check paths with SDK calls (partially done in v0.9.15).
3. **Phase 6 ideas (deferred)** — compact expanded view, container categories, mass start/stop, start sequences with dependency chains, action button placement, orphan image cleanup, update availability badges (all tracked in TODO.md P3 under "Constat").

---

## Project rules

- **Go layout:** uses canonical `internal/{api,arr,auth,core,netsec,utils,health}/` per baseline (flat `ui/` layout is legacy — confirm during next touch).
- **Shared Go code (IDENTICAL COPIES):** `ui/netsec/` and `ui/auth/` are byte-for-byte identical with vpn-gateway / clonarr / tagarr. Port every fix to all 4.
- **Workflow `ci.yml` has intentional Moby advisory** — `continue-on-error: true` on govulncheck with documented GO-2026-4887 (Moby AuthZ bypass). This divergence stays — Constat is a Docker *client*, not daemon, so the vuln's call paths aren't exploitable. Baseline §18 codifies this as example of "intentional drift with explanation."
- **Atomic writes on all state files** (T59 applied) — power-loss no longer truncates.
- **Private-registry auth:** `DOCKER_CONFIG=/config/.docker` env var, entrypoint creates dir 0700 + chown nobody:users. Verify hits `/v2/` endpoint with bearer-token challenge handling (GHCR + Docker Hub).
- **Docker SDK over regctl** — `updates.go:getRemoteDigest()` uses `client.DistributionInspect(ctx, ref, encodedAuth)`. No more `signal: killed`.

---

## Open project-specific TODOs

- Settings sidebar redesign (design-language match to vpn-gateway/PurgeBot)
- Homepage double-notification on UI Apply (cosmetic, inherited from Sentinel)
- Phase 6 idea-pool (expanded view, categories, mass start/stop, sequences, orphan image cleanup, update badges) — see TODO.md P3

---

## Pointers

- **Deep handoff + phase history:** `dev/PROJECT.md` (238 lines — phase history 1→5b, session logs, architecture diagram)
- **Security posture:** `docs/security-implementation-baseline.md` (T1–T74 applied — Constat was the first hardened container and is the baseline reference)
- **UI mockups:** `docs/ideas/constat-*.html` (Phase 6 alternatives)
- **CHANGELOG:** `CHANGELOG.md`
