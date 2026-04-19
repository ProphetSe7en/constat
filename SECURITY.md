# Security Policy

## Supported versions

| Version | Security updates |
|---------|------------------|
| `v0.9.17` and later | ✅ Yes |
| Earlier `v0.9.x` releases | ❌ No — please upgrade |

## Reporting a vulnerability

**Please do NOT open a public GitHub issue for security bugs.** Even describing an attack path in a public forum before a fix ships puts other users at risk.

### Preferred channel

Email: **eirik.svortevik@gmail.com** with subject line `[Constat Security] <brief summary>`.

### Fallback

If email fails or you need pseudonymous submission, use GitHub's private **Report a vulnerability** link on the [repository security tab](https://github.com/ProphetSe7en/constat/security/advisories/new).

### What to include

- Constat version (from the About section, or `GET /api/version`)
- Clear reproduction steps (command + request body + expected vs actual response is ideal)
- Impact assessment — what data/access can the attacker obtain? (Constat has Docker-socket access, so the blast radius for auth-bypass is higher than a typical API-only tool — please be specific.)
- Your disclosure timeline preference

### What to expect

- **Acknowledgement within 72 hours** of receipt (usually faster — solo maintainer, best-effort).
- **Triage and severity assessment within 7 days.** I'll confirm whether I accept the finding, classify severity, and propose a fix + disclosure timeline.
- **Fix within 14 days** for Critical/High findings. Medium/Low may take a release cycle.
- **Coordinated disclosure** — I'll ship a patched release first, then credit you in the CHANGELOG and this document (unless you prefer anonymity). Please do not publish details before the patch ships.

### How I handle reports

- Reporter credit in CHANGELOG + this document by default (anonymous on request).
- Honest acknowledgement when a report is valid — including in the CHANGELOG.
- Open to public discussion of a finding after the patch ships.

## Security model

Constat is a **local Docker management tool** — it reads the Docker socket, lists containers, runs container-level commands (restart, update). This is significantly more privileged than a typical web app:

- **Host assumption:** You control the machine where Constat runs. Constat has effective root via the Docker socket.
- **Network assumption:** Port 7890 is not exposed directly to the internet. Use a reverse proxy with TLS if any external access is needed.
- **File-system assumption:** `/config/` is writable only by the container UID (PUID:PGID). Other users on the same host should not have read access — Constat does not attempt to protect against a hostile local root.

### What Constat does

- **Authentication required by default** (Forms mode, bcrypt cost 12). First-run setup wizard forces admin account creation — no default credentials.
- **CSRF protection** on all state-mutating endpoints (double-submit cookie pattern).
- **Security headers**: `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`.
- **SSRF guards** on outbound HTTP (registry queries, Docker Hub / GHCR, webhook notifications) — blocklisted internal IP ranges + per-request DNS revalidation.
- **Secret masking** in all API responses — Discord webhooks, registry credentials, Gotify/Pushover tokens. Empty-on-unchanged-edit preserves stored secrets.
- **Session persistence** to disk (atomic write, survives container restart).
- **File permissions**: `/config/auth.json` mode `0600` in dir `0700`. `/config/constat.conf` mode `0600` — contains Discord webhook URLs, Gotify token, Pushover user/app keys, and bot-name/server-label config. `/config/.docker/config.json` (registry credentials) mode `0600` in dir `0700`.
- **X-Forwarded-For hardening**: only trusted when the direct peer matches a configured Trusted Proxy. Rightmost-non-trusted algorithm.
- **Env-var override** for trust-boundary config (`TRUSTED_NETWORKS`, `TRUSTED_PROXIES`) — pin at host level to defend against UI-takeover attackers.
- **Password re-confirmation** on catastrophic ops (disabling auth). Local-bypass peers can't silently disable auth just by reaching the UI.

### What Constat does NOT do (by design)

- **Registry credentials plaintext on disk.** Stored in Docker config format at `/config/.docker/config.json` (mode 0600 in dir 0700). Same trust model as running `docker login` on the host — if an attacker has filesystem access, they have the credentials.
- **Rate limiting on `/login`.** Delegated to the reverse proxy.
- **Account lockout.** Same reasoning.
- **Audit log of admin actions.** The Docker event stream + reverse-proxy access logs cover this.
- **TLS termination.** Runs plain HTTP on port 7890. Use a reverse proxy for TLS.

## Security audit trail

Constat's security implementation is backed by an internal trap catalogue (T1–T66) — every finding from past code reviews is preserved with the mitigation pattern and the reason it was flagged. This is a living internal document (not published to this repo) covering the full hardening arc: auth primitives, middleware wiring, sensitive-data redaction, CSRF, security headers, race conditions, info leakage, log injection, supply-chain, and Docker-socket privilege-boundary concerns. Four full review rounds on Phase 1+2 + a delta review on env-var lock additions + a 5-agent parallel audit produced the current trap set. Requests for access to specific trap rationale can be made via the disclosure email above.

Current CI: `go test -race ./...` + `govulncheck ./...` run on every push and PR against `main`.

## Changelog of security-relevant changes

See `CHANGELOG.md` — entries flagged **Security** or explicitly reference trap IDs (T1–T66).
