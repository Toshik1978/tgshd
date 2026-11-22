package config

import "strings"

// MatchHost reports whether host matches the auth_redirect pattern. Matching is case-insensitive:
// "*.example.org" matches "a.example.org" and "a.b.example.org" but not "example.org"; any other
// pattern must equal the host. A port (and the brackets of an IPv6 literal) is stripped from host
// first, so it can be passed req.URL.Host as is. An invalid pattern never matches.
func MatchHost(pattern, host string) bool {
	if !validHostPattern(pattern) {
		return false
	}

	host = strings.TrimSuffix(stripPort(strings.ToLower(host)), ".")
	pattern = strings.ToLower(pattern)

	if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
		// suffix is ".example.org": require at least one non-empty label before it.
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}

	return host == pattern
}

// stripPort removes a trailing ":port" from host, and the brackets of an IPv6 literal.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if end := strings.IndexByte(host, ']'); end > 0 {
			return host[1:end]
		}

		return host
	}

	// A single colon means host:port; more than one is a bare IPv6 literal, left untouched.
	if strings.Count(host, ":") == 1 {
		host, _, _ = strings.Cut(host, ":")
	}

	return host
}

// validHostPattern reports whether pattern is a host name of letters, digits, hyphens and dots,
// optionally prefixed by "*.", with no scheme, port, path, other "*", empty label or bare "*".
func validHostPattern(pattern string) bool {
	name := strings.TrimPrefix(pattern, "*.")
	if name == "" {
		return false
	}

	for label := range strings.SplitSeq(name, ".") {
		if !validHostLabel(label) {
			return false
		}
	}

	return true
}

func validHostLabel(label string) bool {
	if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}

	for i := range len(label) {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}

	return true
}
