package main

import "testing"

func TestSanitizeNext(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"simple_path", "/dashboard", "/dashboard"},
		{"path_with_query", "/dashboard?tab=events", "/dashboard?tab=events"},
		{"deep_path", "/a/b/c?x=1&y=2", "/a/b/c?x=1&y=2"},
		// Absolute / scheme-relative — reject
		{"absolute_http", "http://evil.com/", ""},
		{"absolute_https", "https://evil.com/", ""},
		{"scheme_relative", "//evil.com/", ""},
		// Backslash / mixed normalization
		{"backslash_host", "/\\evil.com", ""},
		{"backslash_path", "/foo\\bar", ""},
		// JavaScript / data URIs
		{"javascript_uri", "javascript:alert(1)", ""},
		{"data_uri", "data:text/html,<script>", ""},
		{"relative_no_leading_slash", "foo/bar", ""},
		// Control chars (CR/LF → header injection)
		{"cr_in_path", "/foo\rbar", ""},
		{"lf_in_path", "/foo\nbar", ""},
		{"tab_in_path", "/foo\tbar", ""},
		{"nul_in_path", "/foo\x00bar", ""},
		{"del_in_path", "/foo\x7fbar", ""},
		// Unicode line separators — log poisoning vector
		{"lsep_2028", "/foo\u2028bar", ""},
		{"psep_2029", "/foo\u2029bar", ""},
		// @ in path is LEGAL (path segment), not a host marker
		{"at_in_path", "/@user/repos", "/@user/repos"},
		// Fragment — allowed
		{"fragment", "/page#section", "/page#section"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeNext(c.input)
			if got != c.want {
				t.Errorf("sanitizeNext(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

func TestIsSecretEnvName(t *testing.T) {
	cases := []struct {
		name   string
		secret bool
		reason string
	}{
		// Positive — should be flagged
		{"API_KEY", true, "API_KEY"},
		{"DATABASE_PASSWORD", true, "PASSWORD"},
		{"JWT_SECRET", true, "SECRET"},
		{"OAUTH_TOKEN", true, "TOKEN"},
		{"ADMIN_TOKEN", true, "TOKEN"},
		{"DB_PASS", true, "PASS (whole token)"},
		{"SESSION_COOKIE", true, "COOKIE"},
		{"BCRYPT_HASH", true, "HASH"},
		{"SIGNING_SIGNATURE", true, "SIGNATURE"},
		{"PASSWORD_SALT", true, "SALT"},
		{"BEARER_AUTH", true, "BEARER + AUTH"},
		{"DISCORD_WEBHOOK", true, "WEBHOOK"},
		{"DATABASE_DSN", true, "DSN"},
		{"PASSWD", true, "PASSWD exact"},
		{"APIKEY", true, "APIKEY whole"},
		{"GITHUB_CREDENTIAL", true, "CREDENTIAL"},
		{"AWS_CREDENTIALS", true, "CREDENTIALS"},
		{"PRIVATE_KEY", true, "PRIVATE + KEY"},
		{"lowercase_token", true, "case insensitive"},
		// Negative — must NOT be flagged (common false-positive traps)
		{"MONKEY_BUSINESS", false, "MONKEY not a token"},
		{"KEEPALIVE_INTERVAL", false, "KEEPALIVE not KEY token"},
		{"KEYBOARD_LAYOUT", false, "KEYBOARD not KEY"},
		{"COMPASS_HEADING", false, "COMPASS not PASS"},
		{"BYPASS_CACHE", false, "BYPASS not PASS (whole token)"},
		{"AUTHOR_NAME", false, "AUTHOR not AUTH"},
		{"SIGNUPS_ALLOWED", false, "SIGNUPS not SIGNATURE"},
		{"HOST_OS", false, "HOST not secret"},
		{"LOG_LEVEL", false, "plain config"},
		{"PUID", false, "user id"},
		{"TZ", false, "timezone"},
		{"INVITATIONS_ALLOWED", false, "feature flag"},
		// Edge: env var with PASS as exact token
		{"PASS", true, "PASS alone"},
		{"KEY", true, "KEY alone"},
		// Empty
		{"", false, "empty name"},
	}
	for _, c := range cases {
		t.Run(c.name+"/"+c.reason, func(t *testing.T) {
			got := isSecretEnvName(c.name)
			if got != c.secret {
				t.Errorf("isSecretEnvName(%q) = %v, want %v (%s)", c.name, got, c.secret, c.reason)
			}
		})
	}
}
