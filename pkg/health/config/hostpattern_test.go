package config

import "testing"

func TestMatchHost(t *testing.T) {
	tests := []struct {
		name, pattern, host string
		want                bool
	}{
		{"exact", "login.example.org", "login.example.org", true},
		{"exact case-insensitive", "Login.Example.ORG", "login.example.org", true},
		{"exact host upper", "login.example.org", "LOGIN.example.org", true},
		{"exact mismatch", "login.example.org", "other.example.org", false},
		{"exact not suffix", "example.org", "a.example.org", false},
		{"wildcard one label", "*.example.org", "a.example.org", true},
		{"wildcard many labels", "*.example.org", "a.b.example.org", true},
		{"wildcard not apex", "*.example.org", "example.org", false},
		{"wildcard not lookalike", "*.example.org", "badexample.org", false},
		{"wildcard not lookalike dot", "*.example.org", ".example.org", false},
		{"wildcard case-insensitive", "*.EXAMPLE.org", "A.Example.Org", true},
		{"port stripped", "login.example.org", "login.example.org:8443", true},
		{"wildcard port stripped", "*.example.org", "a.example.org:443", true},
		{"ipv6 literal with port", "::1", "[::1]:8080", false},
		{"ipv6 literal with port vs name", "*.example.org", "[::1]:8080", false},
		{"ipv6 bracketed no port", "example.org", "[::1]", false},
		{"trailing dot", "login.example.org", "login.example.org.", true},
		{"empty host", "*.example.org", "", false},
		{"empty pattern", "", "", false},
		{"invalid pattern never matches", "*", "a.example.org", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchHost(tt.pattern, tt.host); got != tt.want {
				t.Fatalf("MatchHost(%q, %q) = %v, want %v", tt.pattern, tt.host, got, tt.want)
			}
		})
	}
}

func TestValidHostPattern(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"login.example.org", true},
		{"*.example.org", true},
		{"Login-1.Example.org", true},
		{"localhost", true},
		{"", false},
		{"*", false},
		{"*.", false},
		{"*.*.example.org", false},
		{"a.*.example.org", false},
		{"a*.example.org", false},
		{"https://example.org", false},
		{"example.org:443", false},
		{"example.org/path", false},
		{"example..org", false},
		{".example.org", false},
		{"example.org.", false},
		{"-a.example.org", false},
		{"a-.example.org", false},
		{"exa_mple.org", false},
		{"exa mple.org", false},
		{"[::1]", false},
		{"пример.org", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if got := validHostPattern(tt.pattern); got != tt.want {
				t.Fatalf("validHostPattern(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
		})
	}
}
