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
			got := Status(c.snap, nil)
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
	got := Status(s, nil)
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
	got := Status(s, nil)
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
	got := Status(s, nil)
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

// TestQuietNoteListsPaused: the quiet note names since when the maintenance
// lock is held and lists the checks the window pauses, or none (FR-026,
// FR-027, contracts/telegram-commands.md /status).
func TestQuietNoteListsPaused(t *testing.T) {
	held := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	cases := []struct {
		name string
		snap runner.Snapshot
		want string
	}{
		{
			"maintenance",
			runner.Snapshot{Quiet: "maintenance", HeldSince: held, Paused: []string{"network", "http/<a>"}},
			"Note: quiet window active (maintenance, held since 2026-01-02 03:04); paused: network, http/&lt;a&gt;",
		},
		{
			"boot grace",
			runner.Snapshot{Quiet: "boot grace", Paused: []string{"network"}},
			"Note: quiet window active (boot grace); paused: network",
		},
		{
			"nothing paused",
			runner.Snapshot{Quiet: "maintenance", HeldSince: held},
			"Note: quiet window active (maintenance, held since 2026-01-02 03:04); paused: none",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := strings.Split(Status(c.snap, nil), "\n")
			if lines[0] != c.want {
				t.Fatalf("note = %q, want %q", lines[0], c.want)
			}
		})
	}
	if got := Status(runner.Snapshot{Paused: []string{"network"}}, nil); strings.Contains(got, "paused") {
		t.Fatalf("no quiet window, got %q", got)
	}
}

const gib = uint64(1) << 30

// contractMemory is the /status example of contracts/messages.md.
func contractMemory() *Memory {
	return &Memory{
		Total: 62 * gib, Available: 28 * gib, SwapTotal: 8 * gib, SwapUsed: 8 * gib / 5,
		Stalled: 0, HasPressure: true,
		Top: []Consumer{
			{
				Name: "web-app",
				Size: 13 * gib,
			},
			{Name: "db", Size: 9*gib + gib/10},
			{Name: "worker", Size: 2*gib + 4*gib/10},
		},
		Guard: &Guard{Mode: "report", Last: &GuardAction{
			Action: "restart", Service: "web-app", Report: true, At: time.Date(2026, 10, 2, 1, 12, 0, 0, time.UTC),
		}},
	}
}

func TestStatusMemoryBlock(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 2, 1, 10, 0, 0, time.UTC)
	snap := runner.Snapshot{Open: []runner.OpenCondition{{Key: "http/api", Text: "HTTP 500", Since: at}}}

	got := Status(snap, contractMemory())
	want := "http/api: HTTP 500 (since 2026-10-02 01:10)\n" +
		"Memory: 45% available (28 of 62 GB), swap 20% used, stalled 0%\n" +
		"Top: web-app 13 GB, db 9.1 GB, worker 2.4 GB\n" +
		"Guard: report — last: would restart web-app 2 Oct 01:12"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	if got := Status(runner.Snapshot{Checks: 1}, nil); strings.Contains(got, "Memory") {
		t.Fatalf("no memory section: got %q", got)
	}
}

func TestStatusMemoryLines(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 10, 2, 1, 12, 0, 0, time.UTC)
	cases := []struct {
		name string
		mem  func(*Memory)
		want string
	}{
		{
			"no swap",
			func(m *Memory) { m.SwapTotal, m.SwapUsed = 0, 0 },
			"Memory: 45% available (28 of 62 GB), stalled 0%",
		},
		{
			"no pressure",
			func(m *Memory) { m.HasPressure = false },
			"Memory: 45% available (28 of 62 GB), swap 20% used\n",
		},
		{"stalled", func(m *Memory) { m.Stalled = 35.4 }, ", stalled 35%"},
		{"unreadable", func(m *Memory) { *m = Memory{Unreadable: true, Guard: m.Guard} }, "Memory: unreadable\nGuard:"},
		{"no top", func(m *Memory) { m.Top = nil }, "stalled 0%\nGuard:"},
		{"escaped", func(m *Memory) { m.Top = []Consumer{{Name: "a<b>", Size: 31 * gib}} }, "Top: a&lt;b&gt; 31 GB"},
		{"no guard", func(m *Memory) { m.Guard = nil }, "worker 2.4 GB"},
		{"guard off", func(m *Memory) { m.Guard = &Guard{Mode: "off"} }, "Guard: off"},
		{"no action yet", func(m *Memory) { m.Guard.Last = nil }, "Guard: report — last: none"},
		{"restarted", func(m *Memory) {
			m.Guard = &Guard{Mode: "act", Last: &GuardAction{Action: "restart", Service: "web-app", At: at}}
		}, "Guard: act — last: restarted web-app 2 Oct 01:12"},
		{"stopped", func(m *Memory) {
			m.Guard = &Guard{Mode: "act", Last: &GuardAction{Action: "stop", Service: "web-app", At: at}}
		}, "Guard: act — last: stopped web-app 2 Oct 01:12"},
		{"killed", func(m *Memory) {
			m.Guard = &Guard{Mode: "act", Last: &GuardAction{Action: "kill", Service: "backup.service", At: at}}
		}, "Guard: act — last: killed backup.service 2 Oct 01:12"},
		{"would stop", func(m *Memory) { m.Guard.Last.Action = "stop" }, "last: would stop web-app"},
		{"not running", func(m *Memory) { m.Guard = &Guard{Mode: "not running"} }, "Guard: not running — last: none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := contractMemory()
			tc.mem(m)
			if got := Status(runner.Snapshot{Checks: 1}, m); !strings.Contains(got, tc.want) {
				t.Fatalf("got\n%s\nwant it to contain %q", got, tc.want)
			}
		})
	}
}

func TestStatusMemorySizes(t *testing.T) {
	t.Parallel()

	for size, want := range map[uint64]string{
		5*gib + 6*gib/10: "5.6 GB", 31 * gib: "31 GB", 8 * gib: "8 GB", 9*gib + 99*gib/100: "10 GB", gib / 2: "0.5 GB",
	} {
		if got := gigabytes(size); got != want {
			t.Errorf("gigabytes(%d) = %q, want %q", size, got, want)
		}
	}
}
