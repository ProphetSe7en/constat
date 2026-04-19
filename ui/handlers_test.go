package main

import "testing"

// Semantic-compare helpers backing T63 env-lock save validation. These
// must be order-, whitespace-, and (for IPs) case-insensitive — the
// env-var raw string and the UI's echo of it can diverge trivially
// through reformatting or copy-paste without changing the trust
// boundary. A byte-wise compare would 403 on equivalent input; a
// semantic compare matches the real invariant.

func TestTrustedProxiesEqual(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"both empty", "", "", true},
		{"identical single", "10.0.0.1", "10.0.0.1", true},
		{"identical multi", "10.0.0.1, 10.0.0.2", "10.0.0.1, 10.0.0.2", true},
		{"whitespace differs", "10.0.0.1, 10.0.0.2", "10.0.0.1,10.0.0.2", true},
		{"outer whitespace differs", "  10.0.0.1  ", "10.0.0.1", true},
		{"order differs", "10.0.0.1, 10.0.0.2", "10.0.0.2, 10.0.0.1", true},
		{"ipv6 case differs", "fe80::1", "FE80::1", true},
		{"ipv6 mixed with ipv4", "10.0.0.1, fe80::1", "FE80::1, 10.0.0.1", true},
		{"boundary widened", "10.0.0.1", "10.0.0.1, 10.0.0.2", false},
		{"different single ip", "10.0.0.1", "10.0.0.2", false},
		{"empty vs populated", "", "10.0.0.1", false},
		{"populated vs empty", "10.0.0.1", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trustedProxiesEqual(c.a, c.b); got != c.want {
				t.Errorf("trustedProxiesEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestTrustedNetworksEqual(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"both empty", "", "", true},
		{"identical single cidr", "192.168.0.0/24", "192.168.0.0/24", true},
		{"identical multi", "192.168.0.0/24, 10.0.0.0/8", "192.168.0.0/24, 10.0.0.0/8", true},
		{"whitespace differs", "192.168.0.0/24, 10.0.0.0/8", "192.168.0.0/24,10.0.0.0/8", true},
		{"order differs", "192.168.0.0/24, 10.0.0.0/8", "10.0.0.0/8, 192.168.0.0/24", true},
		{"boundary narrowed", "192.168.0.0/24", "192.168.0.0/25", false},
		{"boundary widened with extra", "192.168.0.0/24", "192.168.0.0/24, 10.0.0.0/8", false},
		{"different cidr", "192.168.0.0/24", "10.0.0.0/8", false},
		{"ipv6 cidr case", "fe80::/10", "FE80::/10", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trustedNetworksEqual(c.a, c.b); got != c.want {
				t.Errorf("trustedNetworksEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}
