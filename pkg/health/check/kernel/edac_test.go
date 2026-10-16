package kernel_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
)

func TestEDACEpisodeLifecycle(t *testing.T) {
	r := newRig(t)
	set := func(ce, ue uint64) {
		r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: ce, Uncorrectable: ue}, {Controller: "mc1"}}
	}
	set(1, 0)
	r.run(t)

	// A rise starts an episode: one message.
	set(2, 0)
	res := r.run(t)
	if len(res.Readings) != 1 || res.Readings[0].Key != "kernel/edac/mc0/ce" ||
		strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("start: %+v", res.Readings)
	}

	// Further rises within 6 h are silent.
	r.clk.t = r.clk.t.Add(time.Hour)
	set(5, 0)
	if res = r.run(t); len(res.Readings) != 0 {
		t.Fatalf("within window: %+v", res.Readings)
	}

	// A different error type of the same controller has its own episode.
	set(5, 1)
	res = r.run(t)
	if len(res.Readings) != 1 || res.Readings[0].Key != "kernel/edac/mc0/ue" {
		t.Fatalf("ue start: %+v", res.Readings)
	}

	// After 6 h, "still happening" for ce once.
	r.clk.t = r.clk.t.Add(5*time.Hour + time.Minute)
	set(6, 1)
	res = r.run(t)
	if len(res.Readings) != 1 || res.Readings[0].Key != "kernel/edac/mc0/ce" ||
		!strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("still: %+v", res.Readings)
	}
	r.clk.t = r.clk.t.Add(time.Hour)
	set(7, 1)
	if res = r.run(t); len(res.Readings) != 0 {
		t.Fatalf("after still: %+v", res.Readings)
	}

	// No rise for over 24 h: the unreported rise is still announced, then a new rise
	// starts a new episode.
	r.clk.t = r.clk.t.Add(25 * time.Hour)
	if res = r.run(t); len(res.Readings) != 1 || !strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("unreported: %+v", res.Readings)
	}
	set(8, 1)
	res = r.run(t)
	if len(res.Readings) != 1 || strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("new episode: %+v", res.Readings)
	}
}

func TestEDACEpisodePersisted(t *testing.T) {
	r := newRig(t)
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 1}}
	r.run(t)
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 2}}
	r.run(t)
	state, _ := r.k.Export()

	r2 := newRig(t)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	r2.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 3}}
	if res := r2.run(t); len(res.Readings) != 0 {
		t.Fatalf("episode survives restart: %+v", res.Readings)
	}
}

func TestRestoreLegacyState(t *testing.T) {
	// State written before EDAC episodes existed: no edac episode keys.
	legacy := `{"cursor":{"boot_id":"boot-1","seq":0,"seen":true},"episodes":{},"edac":{"mc0":{"controller":"mc0","correctable":2,"uncorrectable":0}}}`
	r := newRig(t)
	if err := r.k.Restore(legacy); err != nil {
		t.Fatal(err)
	}
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 3}}
	res := r.run(t)
	if len(res.Readings) != 1 || strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("legacy: %+v", res.Readings)
	}
}

func TestUncorrectableEveryRise(t *testing.T) {
	r := newRig(t)
	set := func(ue uint64) {
		r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Uncorrectable: ue}}
	}
	set(1)
	r.run(t)

	// Two rises within the hour, each reported at once with its own text.
	want := []string{"rose 1→2", "rose 2→5"}
	for i, ue := range []uint64{2, 5} {
		r.clk.t = r.clk.t.Add(10 * time.Minute)
		set(ue)
		res := r.run(t)
		if len(res.Readings) != 1 || res.Readings[0].Key != "kernel/edac/mc0/ue" ||
			res.Readings[0].Kind != health.Event || !strings.Contains(res.Readings[0].Text, want[i]) ||
			!strings.HasPrefix(res.Readings[0].Text, "Kernel: uncorrectable memory errors on mc0 ") {
			t.Fatalf("rise %d: %+v", i, res.Readings)
		}
	}

	// No unreported episode is left behind for a later "still" message.
	r.clk.t = r.clk.t.Add(12 * time.Hour)
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("after window: %+v", res.Readings)
	}
}

func TestUnreadableController(t *testing.T) {
	r := newRig(t)
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 1}, {Controller: "mc1", Uncorrectable: 1}}
	r.run(t)

	r.edac.ctrls = []kernel.Counters{
		{Controller: "mc0", Correctable: 1, Err: errors.New("no ce_count")},
		{Controller: "mc1", Uncorrectable: 2},
	}
	res := r.run(t)
	if len(res.Readings) != 1 || res.Readings[0].Key != "kernel/edac/mc1/ue" {
		t.Fatalf("other controller judged: %+v", res.Readings)
	}
	ces := health.CheckErrors(res.Err)
	if len(ces) != 1 || ces[0].Subject != "mc0" || ces[0].Kind != "edac-unreadable" ||
		ces[0].Error() != "mc0/edac-unreadable: memory controller mc0 counters unreadable: no ce_count" {
		t.Fatalf("checker errors: %v", res.Err)
	}

	// The stored values were kept: a rise against them is still seen.
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 2}, {Controller: "mc1", Uncorrectable: 2}}
	res = r.run(t)
	if len(res.Readings) != 1 || res.Readings[0].Key != "kernel/edac/mc0/ce" || res.Err != nil {
		t.Fatalf("after recovery: %+v", res)
	}
}
