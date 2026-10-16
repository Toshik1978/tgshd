package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/notify"
)

// testZone returns a fixed non-UTC location so the tests do not depend on the host TZ.
func testZone() *time.Location { return time.FixedZone("TEST", 5*3600+1800) }

func nowT() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, testZone()) }

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, s *Store, st State, alerts []Alert, samples []SampleRow) {
	t.Helper()
	if err := s.Checkpoint(context.Background(), st, alerts, samples); err != nil {
		t.Fatal(err)
	}
}

func TestAlertsFilterOrderLimit(t *testing.T) {
	s := openTemp(t)
	now := nowT()
	seed(t, s, State{}, []Alert{
		{At: now.Add(-48 * time.Hour), Kind: KindProblem, Check: "disks", Key: "disks/TEST0000000001/wear", Text: "a"},
		{At: now.Add(-2 * time.Hour), Kind: KindResolved, Check: "disks", Key: "disks/TEST0000000001/wear", Text: "b"},
		{At: now.Add(-1 * time.Hour), Kind: KindEvent, Check: "temp", Key: "temp/cpu", Text: "c"},
		{At: now.Add(-40 * 24 * time.Hour), Kind: KindProblem, Check: "disks", Key: "disks/x", Text: "old"},
	}, nil)
	ctx := context.Background()
	texts := func(as []Alert) string {
		var b strings.Builder
		for _, a := range as {
			b.WriteString(a.Text)
		}
		return b.String()
	}
	week := now.Add(-7 * 24 * time.Hour)
	tests := []struct {
		name, filter string
		since        time.Time
		limit        int
		want         string
	}{
		{"all 7d newest first", "", week, 0, "cba"},
		{"24h", "", now.Add(-24 * time.Hour), 0, "cb"},
		{"by check", "disks", week, 0, "ba"},
		{"by key prefix", "disks/TEST0000000001", week, 0, "ba"},
		{"wildcard chars are literal", "disks/%", week, 0, ""},
		{"60d includes old", "disks", now.Add(-60 * 24 * time.Hour), 0, "baold"},
		{"limit", "", week, 2, "cb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Alerts(ctx, tt.filter, tt.since, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if texts(got) != tt.want {
				t.Fatalf("got %q want %q", texts(got), tt.want)
			}
		})
	}
	got, _ := s.Alerts(ctx, "temp", now.Add(-24*time.Hour), 0)
	if len(got) != 1 || got[0].Kind != KindEvent || got[0].Key != "temp/cpu" || !got[0].At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("fields: %+v", got)
	}
}

func TestMetricsCurrentAndStats(t *testing.T) {
	s := openTemp(t)
	now := nowT()
	seed(t, s, State{}, nil, []SampleRow{
		{At: now.Add(-10 * 24 * time.Hour), Metric: "temp.cpu", Value: 99}, // outside window
		{At: now.Add(-6 * 24 * time.Hour), Metric: "temp.cpu", Value: 40},
		{At: now.Add(-3 * time.Hour), Metric: "temp.cpu", Value: 60},
		{At: now.Add(-1 * time.Hour), Metric: "temp.cpu", Value: 50},
		{At: now.Add(-1 * time.Hour), Metric: "drive.1.temp", Value: 30},
	})
	// A rolled-up day inside the window.
	mustExec(
		t,
		s,
		`INSERT INTO samples_daily VALUES (?, 'temp.cpu', 20, 30, 70, 2)`,
		dayStart(now.Add(-5*24*time.Hour)),
	)
	got, err := s.Metrics(context.Background(), "temp.", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	m := got[0]
	// raw 40,60,50 (sum 150) + daily avg 30 x2 (60) -> 210/5 = 42.
	if m.Metric != "temp.cpu" || m.Current != 50 || !m.HasCurrent || m.Min != 20 || m.Max != 70 ||
		m.N != 5 || math.Abs(m.Avg-42) > 1e-9 {
		t.Fatalf("stats: %+v", m)
	}
	all, err := s.Metrics(context.Background(), "", now)
	if err != nil || len(all) != 2 || all[0].Metric != "drive.1.temp" {
		t.Fatalf("all: %+v %v", all, err)
	}
	none, err := s.Metrics(context.Background(), "fan.", now)
	if err != nil || len(none) != 0 {
		t.Fatalf("none: %+v %v", none, err)
	}
}

// TestPrefixFiltersMatchNonASCII: a prefix is matched by characters, so a
// key or metric with multi-byte characters (a container or sensor name) is
// found by its prefix.
func TestPrefixFiltersMatchNonASCII(t *testing.T) {
	s := openTemp(t)
	now := nowT()
	seed(t, s, State{}, []Alert{
		{At: now.Add(-time.Hour), Kind: KindEvent, Check: "temps", Key: "temps/датчик/high", Text: "a"},
	}, []SampleRow{{At: now.Add(-time.Hour), Metric: "temp.датчик", Value: 40}})
	ctx := context.Background()

	got, err := s.Alerts(ctx, "temps/датчик", now.Add(-24*time.Hour), 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("alerts by a non-ASCII key prefix = %+v, %v, want the one record", got, err)
	}
	stats, err := s.Metrics(ctx, "temp.да", now)
	if err != nil || len(stats) != 1 || stats[0].Metric != "temp.датчик" {
		t.Fatalf("metrics by a non-ASCII prefix = %+v, %v, want temp.датчик", stats, err)
	}
}

func TestMetricsOnlyDaily(t *testing.T) {
	s := openTemp(t)
	now := nowT()
	mustExec(
		t,
		s,
		`INSERT INTO samples_daily VALUES (?, 'temp.cpu', 10, 15, 20, 4)`,
		dayStart(now.Add(-2*24*time.Hour)),
	)
	got, err := s.Metrics(context.Background(), "temp.", now)
	if err != nil || len(got) != 1 || got[0].HasCurrent || got[0].Avg != 15 || got[0].N != 4 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestReadQueriesOnReadOnlyStore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	w, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	now := nowT()
	seed(t, w, State{}, []Alert{{At: now.Add(-time.Hour), Kind: KindEvent, Check: "c", Key: "c/k", Text: "x"}},
		[]SampleRow{{At: now.Add(-time.Hour), Metric: "temp.a", Value: 1}})
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if as, err := r.Alerts(ctx, "c", now.Add(-24*time.Hour), 10); err != nil || len(as) != 1 {
		t.Fatalf("alerts %v %v", as, err)
	}
	if ms, err := r.Metrics(ctx, "temp.", now); err != nil || len(ms) != 1 {
		t.Fatalf("metrics %v %v", ms, err)
	}
	if _, err := r.Retention(ctx, now); err == nil {
		t.Fatal("Retention on a read-only store must fail")
	}
}

type dailyRow struct {
	Min, Avg, Max float64
	N             int
}

func dailyRows(t *testing.T, s *Store, metric string) map[int64]dailyRow {
	t.Helper()
	rows, err := s.db.QueryContext(
		context.Background(),
		`SELECT day, min, avg, max, n FROM samples_daily WHERE metric = ?`,
		metric,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]dailyRow{}
	for rows.Next() {
		var day int64
		var r dailyRow
		if err := rows.Scan(&day, &r.Min, &r.Avg, &r.Max, &r.N); err != nil {
			t.Fatal(err)
		}
		out[day] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

func TestRetention(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := nowT()
	zone := now.Location()
	cutoff := now.Add(-90 * 24 * time.Hour)
	day := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day()-10, 0, 0, 0, 0, zone) // local midnight
	prevDay := day.AddDate(0, 0, -1)
	nextDay := day.AddDate(0, 0, 1)

	seed(t, s, State{Queue: []notify.QueuedMessage{
		{ID: 1, CreatedAt: now.Add(-8 * 24 * time.Hour), Channel: "sms", Text: "old", Kind: "event"},
		{ID: 2, CreatedAt: now.Add(-6 * 24 * time.Hour), Channel: "tlg", Text: "fresh", Kind: "event"},
	}}, []Alert{
		{At: now.Add(-366 * 24 * time.Hour), Kind: KindEvent, Check: "c", Key: "c/a", Text: "gone"},
		{At: now.Add(-364 * 24 * time.Hour), Kind: KindEvent, Check: "c", Key: "c/a", Text: "kept"},
	}, []SampleRow{
		// Local-day boundary: 23:59:59 and 00:00:00 land on different days.
		{At: day.Add(-time.Second), Metric: "m", Value: 100},
		{At: day, Metric: "m", Value: 10},
		{At: day.Add(2 * time.Hour), Metric: "m", Value: 30},
		{At: day.Add(5 * time.Hour), Metric: "m", Value: 20},
		{At: nextDay.Add(time.Hour), Metric: "m", Value: 7},
		{At: day, Metric: "other", Value: 5},
		{At: cutoff.Add(time.Hour), Metric: "m", Value: 1}, // younger than 90 d: stays raw
	})
	// Existing rolled-up row for `day` (e.g. from an earlier partial run).
	mustExec(t, s, `INSERT INTO samples_daily VALUES (?, 'm', 5, 50, 60, 1)`, day.Unix())

	res, err := s.Retention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlertsDeleted != 1 || res.SamplesRolledUp != 6 || res.DaysWritten != 4 ||
		len(res.QueueDropped) != 1 || res.QueueDropped[0].ID != 1 || res.QueueDropped[0].Text != "old" {
		t.Fatalf("result: %+v", res)
	}
	for table, want := range map[string]int{"samples": 1, "alerts": 1, "queue": 1} {
		if n := count(t, s, table); n != want {
			t.Fatalf("%s left: %d, want %d", table, n, want)
		}
	}
	rows := dailyRows(t, s, "m")
	if p := rows[prevDay.Unix()]; p.N != 1 || p.Min != 100 || p.Max != 100 {
		t.Fatalf("previous day: %+v", p)
	}
	// Merge: existing (5,50,60,n1) + new (10,20,30,n3, avg 20) -> avg (50+60)/4 = 27.5.
	if g := rows[day.Unix()]; g.N != 4 || g.Min != 5 || g.Max != 60 || math.Abs(g.Avg-27.5) > 1e-9 {
		t.Fatalf("merged day: %+v", g)
	}
	if g := rows[nextDay.Unix()]; g.N != 1 || g.Avg != 7 {
		t.Fatalf("next day: %+v", g)
	}

	// Idempotent: a second run changes nothing.
	res, err = s.Retention(ctx, now)
	if err != nil || res.AlertsDeleted != 0 || res.SamplesRolledUp != 0 || len(res.QueueDropped) != 0 {
		t.Fatalf("second run: %+v %v", res, err)
	}
	if g := dailyRows(t, s, "m")[day.Unix()]; g.N != 4 {
		t.Fatalf("second run changed daily: %+v", g)
	}
}

func TestRetentionRollsBack(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := nowT()
	seed(t, s, State{}, []Alert{{At: now.Add(-400 * 24 * time.Hour), Kind: KindEvent, Check: "c", Key: "k", Text: "t"}},
		[]SampleRow{{At: now.Add(-100 * 24 * time.Hour), Metric: "m", Value: 1}})
	mustExec(t, s, `DROP TABLE queue`)
	if _, err := s.Retention(ctx, now); err == nil {
		t.Fatal("want error")
	}
	if count(t, s, "alerts") != 1 || count(t, s, "samples") != 1 || count(t, s, "samples_daily") != 0 {
		t.Fatal("retention must be atomic")
	}
}
