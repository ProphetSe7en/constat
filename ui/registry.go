package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"constat-ui/netsec"
)

// registryHTTPClient is a package-level SSRF-safe client used for all
// registry probe/auth calls. Registries are public endpoints, so blocking
// private/loopback IPs is correct: an attacker cannot configure a fake
// "registry" on LAN to steal credentials via our /api/registry/login
// handler.
var registryHTTPClient = netsec.NewSafeHTTPClient(15*time.Second, nil)

// Registry auth store for private container registries.
//
// Credentials are persisted to /config/.docker/config.json in the standard
// Docker config format:
//
//	{ "auths": { "ghcr.io": { "auth": "base64(user:pass)" } } }
//
// This matches what `docker login` writes, which means docker-cli in the
// container (used by constat.sh) can also reuse these credentials if needed.
//
// Update checks no longer shell out to regctl — Constat calls the Docker
// daemon's /distribution/{ref}/json endpoint directly (via client.DistributionInspect)
// and passes the stored creds in the X-Registry-Auth header when the image
// host matches a logged-in registry.

const registryConfigPath = "/config/.docker/config.json"

// dockerHubAuthKey is the historical key docker-cli uses for Docker Hub.
// When the user types "docker.io" or leaves the default for a Docker Hub
// image, we normalize to this key so the config.json matches what
// `docker login` would write on the host.
const dockerHubAuthKey = "https://index.docker.io/v1/"

// validRegistryHost keeps the Login endpoint from being used as an SSRF
// vector. Matches plain hostnames and "host:port" (e.g. localhost:5000).
var validRegistryHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*(:[0-9]{1,5})?$`)

// DockerConfigFile mirrors the subset of Docker's config.json we care about.
// We preserve unknown top-level keys on save by round-tripping through a
// generic map so a user's existing docker-cli settings (credsStore, etc.)
// are not wiped out if they happen to have mounted their own config.
type dockerAuthEntry struct {
	// Auth is base64(username + ":" + password). Docker's standard encoding.
	Auth string `json:"auth,omitempty"`
	// Some configs split user/pass instead — we don't write these, but we
	// tolerate reading them for compatibility.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// RegistryStore manages the Docker config.json credential file.
type RegistryStore struct {
	mu   sync.RWMutex
	path string
}

// NewRegistryStore returns a store backed by /config/.docker/config.json.
func NewRegistryStore() *RegistryStore {
	return &RegistryStore{path: registryConfigPath}
}

// loadRaw reads the full config.json as a generic map so unknown keys are
// preserved on write. Returns an empty map if the file does not exist.
func (rs *RegistryStore) loadRaw() (map[string]any, error) {
	data, err := os.ReadFile(rs.path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("config.json is malformed: %w", err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

// loadAuths extracts the auths map from raw config, or an empty map.
func (rs *RegistryStore) loadAuths(raw map[string]any) map[string]dockerAuthEntry {
	out := map[string]dockerAuthEntry{}
	authsAny, ok := raw["auths"]
	if !ok {
		return out
	}
	authsMap, ok := authsAny.(map[string]any)
	if !ok {
		return out
	}
	for host, entryAny := range authsMap {
		entryMap, ok := entryAny.(map[string]any)
		if !ok {
			continue
		}
		var entry dockerAuthEntry
		if v, ok := entryMap["auth"].(string); ok {
			entry.Auth = v
		}
		if v, ok := entryMap["username"].(string); ok {
			entry.Username = v
		}
		if v, ok := entryMap["password"].(string); ok {
			entry.Password = v
		}
		out[host] = entry
	}
	return out
}

// saveRaw writes config.json atomically with restrictive permissions.
func (rs *RegistryStore) saveRaw(raw map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(rs.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := rs.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Chown(tmp, 99, 100); err != nil {
		// Best-effort — if we're not root we can't chown, but the write
		// still succeeded and the file is readable by its owner.
		_ = err
	}
	return os.Rename(tmp, rs.path)
}

// normalizeHost maps user-friendly registry inputs to the canonical key
// used in config.json. "docker.io" and "hub.docker.com" both become
// "https://index.docker.io/v1/" so docker-cli can reuse the entry.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimSuffix(h, "/")
	if h == "docker.io" || h == "index.docker.io" || h == "hub.docker.com" || h == "registry-1.docker.io" {
		return dockerHubAuthKey
	}
	return h
}

// MaskedAuth is what the API returns — never the raw token.
type MaskedAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
}

// List returns all logged-in registries with usernames. Tokens are never
// returned — the UI shows only a masked placeholder.
func (rs *RegistryStore) List() ([]MaskedAuth, error) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	raw, err := rs.loadRaw()
	if err != nil {
		return nil, err
	}
	auths := rs.loadAuths(raw)
	out := make([]MaskedAuth, 0, len(auths))
	for host, entry := range auths {
		user := entry.Username
		if user == "" && entry.Auth != "" {
			if decoded, err := base64.StdEncoding.DecodeString(entry.Auth); err == nil {
				parts := strings.SplitN(string(decoded), ":", 2)
				if len(parts) > 0 {
					user = parts[0]
				}
			}
		}
		out = append(out, MaskedAuth{Host: host, Username: user})
	}
	return out, nil
}

// Save stores credentials for a registry. Host is normalized before storing.
// Callers should Verify() first so we never persist bad creds — Save itself
// only enforces the bare minimum so callers can't accidentally write garbage
// keys that would break the config.json schema.
func (rs *RegistryStore) Save(host, username, password string) error {
	host = normalizeHost(host)
	if host == "" {
		return errors.New("registry host is required")
	}
	// The Docker Hub sentinel URL is a valid stored key but obviously
	// doesn't match the hostname regex; allow it explicitly.
	if host != dockerHubAuthKey && !validRegistryHost.MatchString(host) {
		return fmt.Errorf("invalid registry host: %q", host)
	}
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	raw, err := rs.loadRaw()
	if err != nil {
		return err
	}
	authsAny, _ := raw["auths"].(map[string]any)
	if authsAny == nil {
		authsAny = map[string]any{}
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	authsAny[host] = map[string]any{"auth": encoded}
	raw["auths"] = authsAny
	return rs.saveRaw(raw)
}

// Remove deletes credentials for a registry. Host is normalized first so
// "docker.io" removes "https://index.docker.io/v1/".
func (rs *RegistryStore) Remove(host string) error {
	host = normalizeHost(host)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	raw, err := rs.loadRaw()
	if err != nil {
		return err
	}
	authsAny, ok := raw["auths"].(map[string]any)
	if !ok {
		return nil // nothing to delete
	}
	delete(authsAny, host)
	raw["auths"] = authsAny
	return rs.saveRaw(raw)
}

// AuthSnapshot is an in-memory snapshot of all configured registry logins,
// keyed by the normalized host string. Used by the update checker to avoid
// re-reading config.json for every container in a check run.
type AuthSnapshot map[string]struct{ Username, Password string }

// Snapshot loads all configured auths once and returns them as a plain map.
// Callers can then use BuildRegistryAuthFrom without any further disk I/O.
// Returns an empty (non-nil) map on error so callers can safely index into it.
func (rs *RegistryStore) Snapshot() AuthSnapshot {
	out := AuthSnapshot{}
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	raw, err := rs.loadRaw()
	if err != nil {
		return out
	}
	for host, entry := range rs.loadAuths(raw) {
		if entry.Username != "" && entry.Password != "" {
			out[host] = struct{ Username, Password string }{entry.Username, entry.Password}
			continue
		}
		if entry.Auth == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			continue
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			continue
		}
		out[host] = struct{ Username, Password string }{parts[0], parts[1]}
	}
	return out
}

// BuildRegistryAuthFrom returns the X-Registry-Auth header value for an image
// reference, looking up credentials in an in-memory snapshot. Returns "" when
// no auth is configured for the image's registry — the caller should then
// fall through to an anonymous call.
func BuildRegistryAuthFrom(snap AuthSnapshot, imageRef string) string {
	host := normalizeHost(extractRegistryHost(imageRef))
	entry, ok := snap[host]
	if !ok || entry.Username == "" || entry.Password == "" {
		return ""
	}
	auth := struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{entry.Username, entry.Password}
	b, err := json.Marshal(auth)
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(b)
}

// extractRegistryHost pulls the registry host out of an image reference.
// Examples:
//
//	alpine:latest                           -> docker.io
//	library/alpine                          -> docker.io
//	ghcr.io/prophetse7en/cnaf:v1.1.0        -> ghcr.io
//	localhost:5000/foo                      -> localhost:5000
//	registry.example.com:8443/team/img:tag  -> registry.example.com:8443
func extractRegistryHost(imageRef string) string {
	// Strip digest suffix if present.
	if at := strings.Index(imageRef, "@"); at >= 0 {
		imageRef = imageRef[:at]
	}
	slash := strings.Index(imageRef, "/")
	if slash < 0 {
		return "docker.io"
	}
	first := imageRef[:slash]
	// A Docker Hub ref like "library/alpine" has no "." or ":" in the first
	// segment and isn't "localhost". Everything else is treated as a host.
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io"
	}
	return first
}

// Verify tests credentials against the registry directly. It does not use
// the Docker daemon — we hit the registry's /v2/ endpoint with Basic Auth
// and treat HTTP 200/401 as definitive answers.
//
//	200        -> creds accepted (or registry allows anonymous /v2/)
//	401        -> creds rejected (or none sent and /v2/ requires auth)
//	other      -> transport/registry error, reported to the user as-is
//
// We deliberately don't try to pull a manifest — some registries restrict
// catalog access even to valid users, so /v2/ with Basic Auth is the most
// reliable universal check.
func (rs *RegistryStore) Verify(ctx context.Context, host, username, password string) error {
	host = normalizeHost(host)
	// Strip the Docker Hub sentinel URL back to a real host for the probe.
	probeHost := host
	if host == dockerHubAuthKey {
		probeHost = "registry-1.docker.io"
	}
	if !validRegistryHost.MatchString(probeHost) {
		return fmt.Errorf("invalid registry host: %q", probeHost)
	}
	url := "https://" + probeHost + "/v2/"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(username, password)
	req.Header.Set("User-Agent", "constat/"+Version)
	// netsec-wrapped client: registry endpoints are public internet, so the
	// SSRF blocklist correctly rejects any attempt to point Constat at an
	// internal "registry" host (e.g. attacker-configured http://192.168.x.x).
	// TLS ≥1.2 is enforced via the netsec transport's defaults.
	resp, err := registryHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		// Some registries (GHCR, Docker Hub) use bearer-token challenges
		// rather than Basic Auth on /v2/. A 401 with a WWW-Authenticate
		// header containing "Bearer" means the registry is alive and is
		// asking us to get a token — it is NOT proof that our creds are
		// wrong. In that case we follow the challenge to get a token.
		challenge := resp.Header.Get("WWW-Authenticate")
		if strings.HasPrefix(strings.ToLower(challenge), "bearer") {
			return verifyBearer(ctx, challenge, username, password)
		}
		return errors.New("credentials rejected (401)")
	default:
		return fmt.Errorf("registry returned HTTP %d", resp.StatusCode)
	}
}

// verifyBearer handles the token-auth flow used by GHCR, Docker Hub and most
// OCI registries. The challenge header looks like:
//
//	Bearer realm="https://auth.ipv6.docker.com/token",service="registry.docker.io"
//
// We hit the realm with Basic Auth and expect a JSON {"token":"..."} response
// on success. A 401 from the token endpoint means creds are wrong.
func verifyBearer(ctx context.Context, challenge, username, password string) error {
	params := parseBearerChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return errors.New("registry sent bearer challenge without realm")
	}
	tokenURL := realm
	if service, ok := params["service"]; ok && service != "" {
		sep := "?"
		if strings.Contains(tokenURL, "?") {
			sep = "&"
		}
		tokenURL += sep + "service=" + url.QueryEscape(service)
	}
	// No scope — we're only probing creds, not asking for repo access.
	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(username, password)
	req.Header.Set("User-Agent", "constat/"+Version)
	resp, err := registryHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("credentials rejected by registry")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("token endpoint returned unreadable body: %w", err)
	}
	if body.Token == "" && body.AccessToken == "" {
		return errors.New("token endpoint returned empty token")
	}
	return nil
}

// parseBearerChallenge parses a WWW-Authenticate: Bearer header into its
// key=value parameters. It only handles the keys Constat needs (realm,
// service) and is not a full RFC 7235 parser.
func parseBearerChallenge(header string) map[string]string {
	out := map[string]string{}
	// Drop the leading "Bearer " prefix.
	if i := strings.IndexByte(header, ' '); i >= 0 {
		header = header[i+1:]
	}
	// Split on commas at the top level (values are quoted, no nested commas).
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		out[key] = val
	}
	return out
}
