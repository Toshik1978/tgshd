package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/store"
)

type fakeHistory struct {
	alerts []store.Alert
	err    error
	calls  int
	filter string
	since  time.Time
}

func (f *fakeHistory) Alerts(_ context.Context, filter string, since time.Time, _ int) ([]store.Alert, error) {
	f.calls++
	f.filter, f.since = filter, since

	return f.alerts, f.err
}

func TestHistoryCommand(t *testing.T) {
	week := 7 * 24 * time.Hour
	row := []store.Alert{{At: time.Unix(0, 0), Kind: store.KindProblem, Key: "k", Text: "t"}}
	cases := []struct {
		name       string
		cmd        string
		alerts     []store.Alert
		wantCalls  int
		wantFilter string
		wantAge    time.Duration
		wantMsg    string
	}{
		{"default", "/history", nil, 1, "", week, "No alerts in 7d"},
		{"period only", "/history 24h", nil, 1, "", 24 * time.Hour, "No alerts in 24h"},
		{"check only", "/history disk", nil, 1, "disk", week, "No alerts in 7d"},
		{"both", "/history disk/a 30d", nil, 1, "disk/a", 30 * 24 * time.Hour, "No alerts in 30d"},
		{"rows", "/history", row, 1, "", week, " PROBLEM k: t"},
		{"period-like", "/history 5d", nil, 0, "", 0, "Usage: /history"},
		{"bad period", "/history disk 5d", nil, 0, "", 0, "Usage: /history"},
		{"too many", "/history a 7d b", nil, 0, "", 0, "Usage: /history"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pub := &fakePublisher{}
			src := &fakeHistory{alerts: c.alerts}
			cmd := NewHistoryCommand(zap.NewNop(), pub, src)
			if cmd.Name() != "history" || !cmd.Enabled() {
				t.Fatalf("Name=%q Enabled=%v", cmd.Name(), cmd.Enabled())
			}
			if err := cmd.Handle(context.Background(), 5, c.cmd); err != nil {
				t.Fatal(err)
			}
			if src.calls != c.wantCalls || src.filter != c.wantFilter {
				t.Errorf("calls=%d filter=%q", src.calls, src.filter)
			}
			if c.wantCalls > 0 {
				if age := time.Since(src.since); age < c.wantAge || age > c.wantAge+time.Minute {
					t.Errorf("age = %v", age)
				}
			}
			if pub.lastRecipient != 5 || !strings.Contains(pub.lastMsg, c.wantMsg) {
				t.Errorf("recipient=%d msg=%q", pub.lastRecipient, pub.lastMsg)
			}
		})
	}
}

func TestHistoryCommandErrors(t *testing.T) {
	boom := errors.New("boom")
	cmd := NewHistoryCommand(zap.NewNop(), &fakePublisher{}, &fakeHistory{err: boom})
	if err := cmd.Handle(context.Background(), 1, "/history"); !errors.Is(err, boom) {
		t.Errorf("source err = %v", err)
	}
	pub := &errPublisher{err: boom}
	cmd = NewHistoryCommand(zap.NewNop(), pub, &fakeHistory{})
	if err := cmd.Handle(context.Background(), 1, "/history"); !errors.Is(err, boom) {
		t.Errorf("publish err = %v", err)
	}
}
