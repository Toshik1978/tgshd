package report

import (
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
)

func TestStatus(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	cases := []struct {
		name string
		snap runner.Snapshot
		want []string
		not  []string
	}{
		{
			"all ok",
			runner.Snapshot{Checks: 7, LastCycle: at},
			[]string{"All checks OK (7 checks, last cycle 2026-01-02 03:04)"},
			[]string{"Note"},
		},
		{
			"before first cycle",
			runner.Snapshot{Checks: 2},
			[]string{"last cycle never"},
			nil,
		},
		{
			"open escaped",
			runner.Snapshot{Open: []runner.OpenCondition{{Key: "disk/a", Text: "<b>&hot", Since: at}}},
			[]string{"disk/a: &lt;b&gt;&amp;hot (since 2026-01-02 03:04)"},
			[]string{"All checks OK"},
		},
		{
			"notes",
			runner.Snapshot{StoreDegraded: true, Quiet: "maint <1>", Checks: 1},
			[]string{"state store degraded", "quiet window active (maint &lt;1&gt;)"},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Status(c.snap)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			for _, n := range c.not {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in %q", n, got)
				}
			}
		})
	}
}

func TestStatusTruncatesByLines(t *testing.T) {
	var s runner.Snapshot
	for range 200 {
		s.Open = append(s.Open, runner.OpenCondition{Key: "k", Text: strings.Repeat("я", 40)})
	}
	got := Status(s)
	if units(got) > MaxMessageUnits || !strings.HasSuffix(got, "…") {
		t.Fatalf("units=%d", units(got))
	}
	body := strings.TrimSuffix(got, "…")
	if !strings.HasSuffix(body, "(since 0001-01-01 00:00)") {
		t.Errorf("expected whole lines kept, got tail %q", body[len(body)-20:])
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short"); got != "short" {
		t.Errorf("got %q", got)
	}
	// 2 UTF-16 units per emoji: 2100 emoji = 4200 units.
	got := Truncate(strings.Repeat("😀", 2100))
	if units(got) > MaxMessageUnits || !strings.HasSuffix(got, "…") {
		t.Errorf("units=%d", units(got))
	}
	if units(got) < MaxMessageUnits-1 {
		t.Errorf("expected full budget use, units=%d", units(got))
	}
	got = Truncate(strings.Repeat("&amp;", 1000))
	if units(got) > MaxMessageUnits {
		t.Errorf("units=%d", units(got))
	}
	if body := strings.TrimSuffix(got, "…"); !strings.HasSuffix(body, "&amp;") {
		t.Errorf("cut inside entity: %q", body[len(body)-6:])
	}
}

func TestStatusInvalidUTF8(t *testing.T) {
	s := runner.Snapshot{Open: []runner.OpenCondition{{Key: "k", Text: strings.Repeat("a\xff", 5000)}}}
	got := Status(s)
	if units(got) > MaxMessageUnits || !strings.HasSuffix(got, "…") {
		t.Errorf("units=%d", units(got))
	}
	// Truncate itself must not panic on raw invalid input either.
	if got := Truncate(strings.Repeat("\xff", 5000)); units(got) > MaxMessageUnits {
		t.Errorf("units=%d", units(got))
	}
}

func TestStatusNotesSurviveTruncation(t *testing.T) {
	s := runner.Snapshot{StoreDegraded: true, Quiet: "maint"}
	for range 300 {
		s.Open = append(s.Open, runner.OpenCondition{Key: "k", Text: strings.Repeat("x", 60)})
	}
	got := Status(s)
	if !strings.Contains(got, "state store degraded") || !strings.Contains(got, "quiet window active (maint)") {
		t.Errorf("notes dropped: %q", got[:120])
	}
	if units(got) > MaxMessageUnits {
		t.Errorf("units=%d", units(got))
	}
}

func TestOpenKeys(t *testing.T) {
	if got := OpenKeys("Unknown", nil); !strings.Contains(got, "No open conditions") {
		t.Errorf("got %q", got)
	}
	if got := OpenKeys("Open:", []string{"a/<b>", "c"}); got != "Open:\na/&lt;b&gt;\nc" {
		t.Errorf("got %q", got)
	}
}
