package memory_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
)

func TestFiguresReadTheHostAndTheLargestServices(t *testing.T) {
	host := healthy()
	host.FullAvg10 = 3
	src := &fakeSource{host: host, services: []memory.Service{
		{Name: "worker", Anon: 2 * gb},
		{Name: "web-app", Anon: 13 * gb},
		{Name: "backup.service", Anon: gb},
		{Name: "db", Anon: 9 * gb},
	}}

	got, err := memory.New(src, settings()).Figures(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != host {
		t.Fatalf("host = %+v, want %+v", got.Host, host)
	}
	want := []memory.Service{
		{Name: "web-app", Anon: 13 * gb},
		{Name: "db", Anon: 9 * gb},
		{Name: "worker", Anon: 2 * gb},
	}
	if !slices.Equal(got.Top, want) {
		t.Fatalf("top = %+v, want %+v", got.Top, want)
	}
	if src.services[0].Name != "worker" {
		t.Fatal("Figures must not reorder the source's slice")
	}
}

func TestFiguresErrors(t *testing.T) {
	broken := memory.New(&fakeSource{hostErr: errors.New("no procfs")}, settings())
	if _, err := broken.Figures(context.Background(), 3); err == nil {
		t.Fatal("a failed host read must be returned")
	}

	got, err := memory.New(&fakeSource{host: healthy(), svcErr: errors.New("no cgroups")}, settings()).
		Figures(context.Background(), 3)
	if err == nil || got.Host != healthy() || got.Top != nil {
		t.Fatalf("figures = %+v, %v; want the host and the services error", got, err)
	}
}

// TestEngineKeepsOneProblemWhileTheTopConsumerChanges feeds the check's runs
// through the alert engine: a PROBLEM whose text changes only in its top
// consumer is not raised again, the text carries no "(since …)" (the notifier
// adds it from the engine's streak start), and the RESOLVED follows the
// latest text.
func TestEngineKeepsOneProblemWhileTheTopConsumerChanges(t *testing.T) {
	host := healthy()
	host.Available = gb / 10 * 56
	src := &fakeSource{host: host, services: []memory.Service{{Name: "web-app", Anon: 31 * gb}}}
	chk := memory.New(src, settings())
	eng := health.NewEngine()
	t0 := time.Date(2026, 10, 2, 1, 10, 0, 0, time.UTC)

	apply := func(at time.Time) []health.Alert {
		return eng.Apply(at, "memory", 30*time.Second, 0, chk.Run(context.Background()).Readings)
	}

	first := apply(t0)
	if len(first) != 1 || first[0].Kind != health.AlertProblem {
		t.Fatalf("first run alerts = %+v, want one PROBLEM", first)
	}
	if strings.Contains(first[0].Text, "since") || !first[0].StreakStart.Equal(t0) {
		t.Fatalf("problem = %+v; want no since in the text and the streak start %v", first[0], t0)
	}

	src.services = []memory.Service{{Name: "worker", Anon: 33 * gb}}
	if again := apply(t0.Add(30 * time.Second)); len(again) != 0 {
		t.Fatalf("top consumer changed: alerts = %+v, want none", again)
	}
	c, ok := eng.Condition("memory/available")
	if !ok || !c.Open || c.LastText != "memory: 9% available (5.6 of 62 GB); top: worker 33 GB" ||
		!c.StreakStart.Equal(t0) {
		t.Fatalf("condition = %+v; want open since %v with the latest text", c, t0)
	}

	src.host = healthy()
	resolved := apply(t0.Add(time.Minute))
	if len(resolved) != 1 || resolved[0].Kind != health.AlertResolved || resolved[0].Text != "memory: 45% available" {
		t.Fatalf("resolved = %+v", resolved)
	}
}
