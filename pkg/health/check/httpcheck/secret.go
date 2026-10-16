package httpcheck

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrSecret reports a header secret that is missing, empty or still a
// placeholder. Its messages name the environment variable, never the value.
var ErrSecret = errors.New("header secret unusable")

// headerValue returns the configured header value (prefix + secret), or an
// error wrapping ErrSecret when the secret cannot be used.
func (c *Check) headerValue() (string, error) {
	if c.header == nil {
		return "", nil
	}

	env := c.header.ValueEnv

	v, ok := c.lookupEnv(env)

	var reason string

	switch {
	case !ok:
		reason = "is not set"
	case strings.TrimSpace(v) == "":
		reason = "is empty"
	case hasControl(v):
		reason = "contains a control character"
	case looksLikePlaceholder(v):
		reason = "looks like a placeholder"
	default:
		return c.header.Prefix + v, nil
	}

	return "", fmt.Errorf("%w: environment variable %s %s", ErrSecret, env, reason)
}

// hasControl reports whether s holds a character not allowed in a header
// value (anything below space except tab, and DEL).
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r != '\t' && unicode.IsControl(r)
	})
}

// looksLikePlaceholder reports whether v is a typical stand-in left in an
// environment file instead of a real secret.
func looksLikePlaceholder(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))

	return placeholderWord(s) || placeholderShape(s) || placeholderMarker(s)
}

func placeholderWord(s string) bool {
	switch s {
	case "todo", "tbd", "fixme", "secret", "token", "password", "dummy", "example",
		"none", "null", "nil", "undefined", "changeit":
		return true
	default:
		return false
	}
}

// placeholderShape matches template syntax and runs of a filler character.
func placeholderShape(s string) bool {
	if enclosed(s, "<", ">") || enclosed(s, "${", "}") || enclosed(s, "{{", "}}") || enclosed(s, "%", "%") {
		return true
	}

	for _, filler := range []string{"x", "*", ".", "-", "_", "#"} {
		if strings.Trim(s, filler) == "" {
			return true
		}
	}

	return false
}

// placeholderMarker matches stand-in phrases: anywhere for the unambiguous
// ones, only at the start or the end for "your-…" and "…-here".
func placeholderMarker(s string) bool {
	for _, marker := range []string{
		"changeme", "change-me", "change_me", "replaceme", "replace-me", "replace_me", "placeholder",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}

	return strings.HasPrefix(s, "your-") || strings.HasPrefix(s, "your_") ||
		strings.HasSuffix(s, "-here") || strings.HasSuffix(s, "_here")
}

func enclosed(s, open, closing string) bool {
	return len(s) >= len(open)+len(closing) && strings.HasPrefix(s, open) && strings.HasSuffix(s, closing)
}
