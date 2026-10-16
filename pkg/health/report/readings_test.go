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
	cases := []struct {
		name   string
		drives []Drive
		stats  []store.MetricStats
		want   string
	}{
		{
			"full",
			[]Drive{{
				Bus: "nvme", Model: "Example <NVMe> SSD", Serial: s, HealthOK: true,
				SparePct: new(float64(100)), SelfTestAge: new(50 * time.Hour),
			}},
			stats,
			"nvme Example &lt;NVMe&gt; SSD …0001: health OK, wear 7% (7d 3/5/7), spare 100%, temp 38°C (7d 35/38/44), written 7d 500 GB, self-test 2d ago",
		},
		{
			"stats only",
			[]Drive{{Bus: "sata", Model: "M", Serial: "XXXX1234", HealthOK: true}},
			[]store.MetricStats{{Metric: "temp.drive.XXXX1234", Min: 35, Avg: 38, Max: 44, N: 3}},
			"sata M …1234: health OK, wear n/a, spare n/a, temp n/a (7d 35/38/44), written 7d n/a, self-test n/a",
		},
		{
			"unknowns",
			[]Drive{{Bus: "sata", Model: "Example HDD", Serial: "AB", SelfTestAge: new(90 * time.Minute)}},
			nil,
			"sata Example HDD …AB: health FAILING, wear n/a, spare n/a, temp n/a, written 7d n/a, self-test 1h ago",
		},
		{
			"recent self-test",
			[]Drive{{Bus: "sata", Model: "M", Serial: "XXXX1234", HealthOK: true, SelfTestAge: new(5 * time.Minute)}},
			nil,
			"sata M …1234: health OK, wear n/a, spare n/a, temp n/a, written 7d n/a, self-test 5m ago",
		},
		{
			"missing",
			[]Drive{{Bus: "nvme", Model: "M", Serial: "XXXX5678", HealthOK: true, Missing: true}},
			nil,
			"nvme M …5678: health missing, wear n/a, spare n/a, temp n/a, written 7d n/a, self-test n/a",
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
