package health

import (
	"testing"
	"time"
)

const (
	iv  = time.Minute
	key = "disks/TEST0000000001/wear"
)

func start() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }

func rd(v Verdict, text string) []Reading {
	return []Reading{{Key: key, Verdict: v, Text: text, Mode: ModeSMS}}
}

func kinds(alerts []Alert) []AlertKind {
	out := make([]AlertKind, 0, len(alerts))
	for _, e := range alerts {
		out = append(out, e.Kind)
	}
	return out
}

func TestThresholdCountsReadingTime(t *testing.T) {
	t0 := start()
	e := NewEngine()
	th := 3 * time.Minute
	steps := []struct {
		at   time.Duration
		v    Verdict
		want []AlertKind
	}{
		{0, Bad, nil},
		{iv, Bad, nil},
		{2 * iv, Bad, nil},
		{3 * iv, Bad, []AlertKind{AlertProblem}},
		{4 * iv, Bad, nil},
		{5 * iv, Good, []AlertKind{AlertResolved}},
		{6 * iv, Good, nil},
	}
	for i, s := range steps {
		got := kinds(e.Apply(t0.Add(s.at), "disks", iv, th, rd(s.v, "x")))
		if len(got) != len(s.want) || (len(got) == 1 && got[0] != s.want[0]) {
			t.Fatalf("step %d: got %v want %v", i, got, s.want)
		}
	}
	if len(e.Conditions()) != 0 {
		t.Fatalf("healthy condition should not be retained: %v", e.Conditions())
	}
}

func TestThresholdZeroFiresOnFirstBad(t *testing.T) {
	t0 := start()
	e := NewEngine()
	alerts := e.Apply(t0, "c", iv, 0, rd(Bad, "boom"))
	if len(alerts) != 1 || alerts[0].Kind != AlertProblem {
		t.Fatalf("got %v", alerts)
	}
	ev := alerts[0]
	if ev.Check != "c" || ev.Key != key || ev.Text != "boom" || ev.Mode != ModeSMS || !ev.StreakStart.Equal(t0) {
		t.Fatalf("bad event fields: %+v", ev)
	}
}

func TestGapIsCapped(t *testing.T) {
	t0 := start()
	e := NewEngine()
	th := 10 * time.Minute
	e.Apply(t0, "c", iv, th, rd(Bad, "x"))
	// an hour-long gap only counts 1.5 intervals
	if alerts := e.Apply(t0.Add(time.Hour), "c", iv, th, rd(Bad, "x")); len(alerts) != 0 {
		t.Fatalf("unexpected %v", alerts)
	}
	c, _ := e.Condition(key)
	if c.BadFor != 90*time.Second {
		t.Fatalf("BadFor = %v", c.BadFor)
	}
	if !c.StreakStart.Equal(t0) {
		t.Fatalf("streak start moved: %v", c.StreakStart)
	}
}

func TestUnknownChangesNothing(t *testing.T) {
	t0 := start()
	e := NewEngine()
	th := 2 * time.Minute
	e.Apply(t0, "c", iv, th, rd(Bad, "x"))
	if alerts := e.Apply(t0.Add(iv), "c", iv, th, rd(Unknown, "err")); len(alerts) != 0 {
		t.Fatalf("unexpected %v", alerts)
	}
	c, _ := e.Condition(key)
	if !c.LastReadingAt.Equal(t0) || c.BadFor != 0 {
		t.Fatalf("unknown altered state: %+v", c)
	}
	// next judged reading is capped relative to t0: 1.5 min < 2 min
	if alerts := e.Apply(t0.Add(5*iv), "c", iv, th, rd(Bad, "x")); len(alerts) != 0 {
		t.Fatalf("unexpected %v", alerts)
	}
	if alerts := e.Apply(t0.Add(6*iv), "c", iv, th, rd(Bad, "x")); len(alerts) != 1 {
		t.Fatalf("expected problem, got %v", alerts)
	}
}

func TestUnknownOnHealthyCreatesNothing(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Unknown, "err"))
	if len(e.Conditions()) != 0 {
		t.Fatal("unknown created a condition")
	}
}

func TestGoodResetsStreakWithoutMessage(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, time.Hour, rd(Bad, "x"))
	if alerts := e.Apply(t0.Add(iv), "c", iv, time.Hour, rd(Good, "")); len(alerts) != 0 {
		t.Fatalf("unexpected %v", alerts)
	}
	if len(e.Conditions()) != 0 {
		t.Fatal("streak not reset")
	}
}

func TestResolvedCarriesStreakStart(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "boom"))
	alerts := e.Apply(t0.Add(iv), "c", iv, 0, rd(Good, ""))
	if len(alerts) != 1 || alerts[0].Kind != AlertResolved || !alerts[0].StreakStart.Equal(t0) ||
		alerts[0].Text != "boom" {
		t.Fatalf("got %+v", alerts)
	}
}

func TestEventReadings(t *testing.T) {
	t0 := start()
	e := NewEngine()
	rs := []Reading{
		{Key: "a", Verdict: Bad, Text: "one", Kind: Event, Mode: ModeTelegram},
		{Key: "a", Verdict: Bad, Text: "two", Kind: Event},
		{Key: "a", Verdict: Good, Text: "three", Kind: Event},
		{Key: "a", Text: "four", Kind: Event},
	}
	alerts := e.Apply(t0, "c", iv, 0, rs)
	if len(alerts) != 4 || alerts[0].Kind != AlertEvent || alerts[1].Text != "two" || alerts[3].Text != "four" ||
		alerts[0].Mode != ModeTelegram {
		t.Fatalf("got %+v", alerts)
	}
	if len(e.Conditions()) != 0 {
		t.Fatal("event readings kept state")
	}
}

func TestClear(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "x"))
	if !e.Clear(key) || e.Clear(key) {
		t.Fatal("Clear should report presence once")
	}
	if alerts := e.Apply(t0.Add(iv), "c", iv, 0, rd(Good, "")); len(alerts) != 0 {
		t.Fatalf("resolved after clear: %v", alerts)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "x"))
	e.Apply(t0, "c", iv, time.Hour, []Reading{{Key: "b", Verdict: Bad, Text: "y"}})
	saved := e.Conditions()
	if len(saved) != 2 || saved[0].Key != "b" {
		t.Fatalf("not sorted/complete: %+v", saved)
	}
	f := NewEngine()
	f.Restore(saved)
	if open := f.OpenConditions(); len(open) != 1 || open[0].Key != key {
		t.Fatalf("open = %+v", open)
	}
	if alerts := f.Apply(t0.Add(iv), "c", iv, 0, rd(Good, "")); len(alerts) != 1 || alerts[0].Kind != AlertResolved {
		t.Fatalf("restored open not resolved: %v", alerts)
	}
}

func TestNegativeElapsedAddsNothing(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, time.Hour, rd(Bad, "x"))
	e.Apply(t0.Add(-time.Hour), "c", iv, time.Hour, rd(Bad, "x"))
	if c, _ := e.Condition(key); c.BadFor != 0 {
		t.Fatalf("BadFor = %v", c.BadFor)
	}
}

func TestRestoredOpenDoesNotRepeatProblem(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "x"))
	f := NewEngine()
	f.Restore(e.Conditions())

	if alerts := f.Apply(t0.Add(iv), "c", iv, 0, rd(Bad, "x")); len(alerts) != 0 {
		t.Fatalf("duplicate after restore: %v", alerts)
	}
}

func TestUnknownKeepsOpen(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "x"))

	if alerts := e.Apply(t0.Add(iv), "c", iv, 0, rd(Unknown, "err")); len(alerts) != 0 {
		t.Fatalf("unexpected %v", alerts)
	}

	if c, _ := e.Condition(key); !c.Open {
		t.Fatal("unknown closed the condition")
	}
}

func TestMergeAddsOnlyAbsentKeys(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "memory"))

	e.Merge([]Condition{
		{Key: key, Check: "c", LastText: "stored", Open: true},
		{Key: "other/x", Check: "o", LastText: "stored other", Open: true},
	})

	if c, _ := e.Condition(key); c.LastText != "memory" {
		t.Fatalf("merge replaced the in-memory condition: %+v", c)
	}
	if c, ok := e.Condition("other/x"); !ok || c.LastText != "stored other" {
		t.Fatalf("merge did not add the absent key: %+v %v", c, ok)
	}
}

func TestZeroVerdictIsUnknown(t *testing.T) {
	t0 := start()
	e := NewEngine()
	e.Apply(t0, "c", iv, 0, rd(Bad, "x"))

	if alerts := e.Apply(t0.Add(iv), "c", iv, 0, []Reading{{Key: key}}); len(alerts) != 0 {
		t.Fatalf("zero-verdict reading resolved: %v", alerts)
	}
}
