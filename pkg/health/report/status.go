package report

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
)

// MaxMessageUnits is the Telegram message limit in UTF-16 code units.
const MaxMessageUnits = 4096

const (
	ellipsis   = "…"
	timeLayout = "2006-01-02 15:04"
)

// Status renders the /status reply: open problems one per line, or the
// all-OK line, followed by notes about a degraded store or a quiet window.
func Status(s runner.Snapshot) string {
	var lines []string

	// Notes come first so truncation never drops them.
	if s.StoreDegraded {
		lines = append(lines, "Note: state store degraded, history may be incomplete")
	}
	if s.Quiet != "" {
		lines = append(lines, quietNote(s))
	}
	if len(s.Open) == 0 {
		last := "never"
		if !s.LastCycle.IsZero() {
			last = s.LastCycle.Format(timeLayout)
		}
		lines = append(lines, fmt.Sprintf("All checks OK (%d checks, last cycle %s)", s.Checks, last))
	}
	for _, c := range s.Open {
		lines = append(lines, fmt.Sprintf("%s: %s (since %s)",
			escape(c.Key), escape(c.Text), c.Since.Format(timeLayout)))
	}

	return Truncate(strings.Join(lines, "\n"))
}

// quietNote renders the quiet-window note: the reason, since when the
// maintenance lock is held, and the checks the window pauses (FR-026,
// FR-027). Every check not listed keeps running.
func quietNote(s runner.Snapshot) string {
	reason := escape(s.Quiet)
	if !s.HeldSince.IsZero() {
		reason += ", held since " + s.HeldSince.Format(timeLayout)
	}
	paused := "none"
	if len(s.Paused) > 0 {
		names := make([]string, 0, len(s.Paused))
		for _, p := range s.Paused {
			names = append(names, escape(p))
		}
		paused = strings.Join(names, ", ")
	}

	return "Note: quiet window active (" + reason + "); paused: " + paused
}

// OpenKeys renders a heading followed by the open condition keys.
func OpenKeys(heading string, keys []string) string {
	var b strings.Builder
	b.WriteString(escape(heading))
	if len(keys) == 0 {
		b.WriteString("\nNo open conditions")
	}
	for _, k := range keys {
		b.WriteString("\n" + escape(k))
	}

	return Truncate(b.String())
}

// Truncate fits text into one Telegram message (UTF-16 code units),
// dropping trailing lines and appending an ellipsis when it is too long.
func Truncate(text string) string {
	return truncateLines(strings.Split(text, "\n"), MaxMessageUnits)
}

// truncateLines joins lines into a text of at most limit UTF-16 units,
// keeping whole lines and appending an ellipsis when some are dropped; a first
// line that alone is too long is cut (never inside an HTML entity).
func truncateLines(lines []string, limit int) string {
	text := strings.Join(lines, "\n")
	if units(text) <= limit {
		return text
	}

	budget := limit - units(ellipsis)
	used := 0
	var b strings.Builder
	for i, l := range lines {
		add := l
		if i > 0 {
			add = "\n" + l
		}
		if used+units(add) > budget {
			if i == 0 {
				b.WriteString(cut(l, budget))
			}

			break
		}
		used += units(add)
		b.WriteString(add)
	}

	return b.String() + ellipsis
}

// cut returns the longest prefix of s within budget UTF-16 units that does
// not end inside an HTML entity.
func cut(s string, budget int) string {
	n := 0
	end := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > budget {
			break
		}
		n += w
		_, size := utf8.DecodeRuneInString(s[i:])
		end = i + size
	}
	s = s[:end]
	if amp := strings.LastIndexByte(s, '&'); amp >= 0 && !strings.Contains(s[amp:], ";") {
		s = s[:amp]
	}

	return s
}

// escape makes dynamic text valid UTF-8 and HTML-safe.
func escape(s string) string {
	return html.EscapeString(strings.ToValidUTF8(s, "�"))
}

func units(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}

	return n
}
