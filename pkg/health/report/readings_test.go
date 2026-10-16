package report

import (
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/store"
)

//go:fix inline
func TestTemps(t *testing.T) {
	stats := []store.MetricStats{
		{Metric: "temp.cpu.chip.Package <0>", Current: 51.4, HasCurrent: true, Min: 30, Avg: 45.2, Max: 80.6},
		{Metric: "temp.drive.TEST0000000001", Current: 40, HasCurrent: true},
		{Metric: "temp.gpu.0", HasCurrent: false, Min: 1, Avg: 2, Max: 3},
		{Metric: "drive.TEST0000000001.wear", Current: 3, HasCurrent: true},
	}
	got := Temps(stats)
	want := "cpu.chip.Package &lt;0&gt;: 51°C (7d min/avg/max 30/45/81)\ngpu.0: n/a (7d min/avg/max 1/2/3)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := Temps(nil); got != "No temperature sensors" {
		t.Errorf("empty = %q", got)
	}
	many := make([]store.MetricStats, 500)
	for i := range many {
		many[i] = store.MetricStats{Metric: "temp.cpu.chip." + strings.Repeat("x", 20), HasCurrent: true}
	}
	if got := Temps(many); units(got) > MaxMessageUnits || !strings.HasSuffix(got, "…") {
		t.Errorf("not truncated: %d units", units(got))
	}
}

func TestDisks(t *testing.T) {
	const s = "TEST0000000001"
	stats := []store.MetricStats{
		{Metric: "drive." + s + ".wear", Current: 7, HasCurrent: true, Min: 3, Avg: 5.4, Max: 7, N: 10},
		{Metric: "drive." + s + ".written_bytes", Current: 5.5e12, Min: 5.0e12, HasCurrent: true},
		{Metric: "temp.drive." + s, Current: 38.2, HasCurrent: true, Min: 35, Avg: 37.6, Max: 44, N: 10},
	}
	since := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	nvme := Drive{
		Node: "nvme0n1", Bus: "NVMe", Model: "Example <NVMe> SSD", Serial: s, Transport: "nvme", Health: "ok",
		Temp: new(38), Wear: new(7), Spare: new(100), SpareThreshold: new(10), SpareFloor: 20,
		SelfTest:    SelfTest{Format: "ok", Result: "passed", Age: 12 * 24 * time.Hour},
		LastReadAge: new(30 * time.Second),
	}
	day := 24 * time.Hour
	with := func(d Drive, f func(*Drive)) []Drive {
		f(&d)

		return []Drive{d}
	}
	cases := []struct {
		name   string
		drives []Drive
		stats  []store.MetricStats
		want   string
	}{
		{
			"full nvme",
			[]Drive{nvme},
			stats,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7% (7d 3/5/7), " +
				"spare 100% (floor 20%, drive 10%), temp 38°C (7d 35/38/44), written 7d 500 GB, " +
				"self-test passed 12 d ago, last read <1 m ago",
		},
		{
			"nil values read unknown, never 0",
			with(nvme, func(d *Drive) { d.Temp, d.Wear, d.Spare, d.SpareThreshold = nil, nil, nil, nil }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear unknown, " +
				"spare unknown (floor 20%, drive unknown), temp unknown, written 7d n/a, " +
				"self-test passed 12 d ago, last read <1 m ago",
		},
		{
			"sata ssd has wear and no spare",
			[]Drive{{
				Node: "sda", Bus: "ATA", Model: "Example SSD", Serial: "XXXX1234", Transport: "sata", Health: "unknown",
				Temp: new(34), Wear: new(12), SelfTest: SelfTest{Format: "ok"}, LastReadAge: new(5 * time.Hour),
			}},
			nil,
			"sda ATA Example SSD …1234: health unknown, wear 12%, temp 34°C, written 7d n/a, " +
				"self-test none (first seen <1 m ago), last read 5 h ago",
		},
		{
			"failed self-test with an age",
			with(nvme, func(d *Drive) { d.SelfTest.Result, d.SelfTest.NewestAge = "failed", new(3*day) }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test failed 3 d ago, last read <1 m ago",
		},
		{
			"passed uses the power-on age when known",
			with(nvme, func(d *Drive) { d.SelfTest.NewestAge = new(2 * day) }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test passed 2 d ago, last read <1 m ago",
		},
		{
			"passed before first seen",
			with(nvme, func(d *Drive) { d.SelfTest.Unobserved = true }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test passed before first seen 12 d ago, last read <1 m ago",
		},
		{
			"failed self-test",
			with(nvme, func(d *Drive) { d.SelfTest.Result = "failed" }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test failed, last read <1 m ago",
		},
		{
			"first seen",
			with(nvme, func(d *Drive) { d.SelfTest = SelfTest{Format: "ok", Age: 5 * 24 * time.Hour} }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test none (first seen 5 d ago), last read <1 m ago",
		},
		{
			"unsupported log is not readable",
			with(nvme, func(d *Drive) { d.SelfTest = SelfTest{Format: "unsupported"} }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test not readable, last read <1 m ago",
		},
		{
			"sas is never readable",
			with(nvme, func(d *Drive) {
				d.Transport, d.Bus, d.SelfTest = "sas", "SCSI", SelfTest{Format: "ok", Result: "passed"}
			}),
			nil,
			"nvme0n1 SCSI Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, temp 38°C, written 7d n/a, " +
				"self-test not readable, last read <1 m ago",
		},
		{
			"empty format is not read yet, not unreadable",
			with(nvme, func(d *Drive) { d.SelfTest = SelfTest{} }), nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health OK, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test not read yet, last read <1 m ago",
		},
		{
			"asleep and unreadable",
			with(nvme, func(d *Drive) {
				d.Health, d.LastReadAge, d.AsleepSince, d.UnreadableSince = "failing", new(50*time.Hour), since, since
			}),
			nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health FAILING, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test passed 12 d ago, last read 2 d ago, " +
				"asleep since 2026-03-04 05:06, unreadable since 2026-03-04 05:06",
		},
		{
			"missing wins, never read",
			with(nvme, func(d *Drive) { d.Missing, d.LastReadAge = true, nil }),
			nil,
			"nvme0n1 NVMe Example &lt;NVMe&gt; SSD …0001: health missing, wear 7%, spare 100% (floor 20%, drive 10%), " +
				"temp 38°C, written 7d n/a, self-test passed 12 d ago, last read n/a",
		},
		{"none", nil, nil, "No drives"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Disks(c.drives, c.stats)
			if got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
			if strings.Contains(got, s) {
				t.Error("full serial leaked")
			}
		})
	}
}

func TestSize(t *testing.T) {
	cases := map[float64]string{
		0:      "0 B",
		999:    "999 B",
		1500:   "1.5 kB",
		2.5e9:  "2.5 GB",
		1.5e12: "1.5 TB",
		250e9:  "250 GB",
		3e15:   "3.0 PB",
	}
	for in, want := range cases {
		if got := size(in); got != want {
			t.Errorf("size(%v) = %q, want %q", in, got, want)
		}
	}
}
