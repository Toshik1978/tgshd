package kernel_test

import (
	"strings"
	"testing"
	"time"

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

	// No rise for over 24 h, then a new rise starts a new episode.
	r.clk.t = r.clk.t.Add(25 * time.Hour)
	if res = r.run(t); len(res.Readings) != 0 {
		t.Fatalf("quiet: %+v", res.Readings)
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
