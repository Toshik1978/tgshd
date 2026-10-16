package report

import (
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/store"
)

func TestParsePeriod(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"24h", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"30d", 30 * 24 * time.Hour, true},
		{"1d", 0, false},
		{"", 0, false},
		{"disk", 0, false},
	}
	for _, c := range cases {
		got, ok := ParsePeriod(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParsePeriod(%q) = %v, %v", c.in, got, ok)
		}
	}
}

func TestHistory(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	got := History([]store.Alert{
		{At: at, Kind: store.KindProblem, Check: "disk", Key: "disk/<a>", Text: "hot & bad"},
		{At: at, Kind: store.KindCheckerError, Check: "disk", Key: "disk/b", Text: "oops"},
	}, "7d", loc)
	want := "2026-01-02 04:04 PROBLEM disk/&lt;a&gt;: hot &amp; bad\n2026-01-02 04:04 CHECKER ERROR disk/b: oops"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHistoryEmpty(t *testing.T) {
	if got := History(nil, "24h", time.UTC); got != "No alerts in 24h" {
		t.Errorf("got %q", got)
	}
}

func TestHistoryTruncated(t *testing.T) {
	alerts := make([]store.Alert, 500)
	for i := range alerts {
		alerts[i] = store.Alert{At: time.Unix(0, 0), Kind: store.KindEvent, Key: "k", Text: strings.Repeat("x", 50)}
	}
	got := History(alerts, "7d", time.UTC)
	if units(got) > MaxMessageUnits || !strings.HasSuffix(got, ellipsis) {
		t.Errorf("units=%d", units(got))
	}
}

func TestHistoryUsage(t *testing.T) {
	if !strings.HasPrefix(HistoryUsage(), "Usage: /history") {
		t.Errorf("usage = %q", HistoryUsage())
	}
}
