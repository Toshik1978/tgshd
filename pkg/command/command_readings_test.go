package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

type fakeMetrics struct {
	byPrefix map[string][]store.MetricStats
	err      error
	prefixes []string
	now      time.Time
}

func (f *fakeMetrics) Metrics(_ context.Context, prefix string, now time.Time) ([]store.MetricStats, error) {
	f.prefixes = append(f.prefixes, prefix)
	f.now = now

	return f.byPrefix[prefix], f.err
}

type fakeDrives struct{ drives []Drive }

func (f fakeDrives) Drives() []Drive { return f.drives }

func TestTempCommand(t *testing.T) {
	pub := &fakePublisher{}
	m := &fakeMetrics{byPrefix: map[string][]store.MetricStats{
		"temp.": {{Metric: "temp.gpu.0", Current: 60, HasCurrent: true, Min: 40, Avg: 50, Max: 70}},
	}}
	cmd := NewTempCommand(zap.NewNop(), pub, m)
	if cmd.Name() != "temp" || !cmd.Enabled() {
		t.Fatalf("Name=%q Enabled=%v", cmd.Name(), cmd.Enabled())
	}
	if err := cmd.Handle(context.Background(), 4, "/temp"); err != nil {
		t.Fatal(err)
	}
	if pub.lastRecipient != 4 || pub.lastMsg != "gpu.0: 60°C (7d min/avg/max 40/50/70)" {
		t.Errorf("recipient=%d msg=%q", pub.lastRecipient, pub.lastMsg)
	}
	if m.now.IsZero() {
		t.Error("now not passed")
	}
}

func TestTempCommandErrors(t *testing.T) {
	boom := errors.New("boom")
	cmd := NewTempCommand(zap.NewNop(), &fakePublisher{}, &fakeMetrics{err: boom})
	if err := cmd.Handle(context.Background(), 1, "/temp"); !errors.Is(err, boom) {
		t.Errorf("metrics err = %v", err)
	}
	pub := &errPublisher{err: boom}
	cmd = NewTempCommand(zap.NewNop(), pub, &fakeMetrics{})
	if err := cmd.Handle(context.Background(), 1, "/temp"); !errors.Is(err, boom) {
		t.Errorf("publish err = %v", err)
	}
}

func TestDisksCommand(t *testing.T) {
	pub := &fakePublisher{}
	m := &fakeMetrics{byPrefix: map[string][]store.MetricStats{
		"temp.drive.": {{Metric: "temp.drive.TEST0000000001", Current: 35, HasCurrent: true}},
		"drive.": {
			{Metric: "drive.TEST0000000001.wear", Current: 2, HasCurrent: true, Min: 1, Avg: 2, Max: 3, N: 4},
		},
	}}
	d := fakeDrives{drives: []Drive{{
		Node: "nvme0n1", Bus: "NVMe", Model: "Example NVMe SSD", Serial: "TEST0000000001", Transport: "nvme",
		Health: "ok", Wear: new(2), Temp: new(35), SpareFloor: 10, LastReadAge: new(time.Minute),
		SelfTest: report.SelfTest{Format: "ok", Result: "passed", Age: 24 * time.Hour},
	}}}
	cmd := NewDisksCommand(zap.NewNop(), pub, d, m)
	if cmd.Name() != "disks" || !cmd.Enabled() {
		t.Fatalf("Name=%q Enabled=%v", cmd.Name(), cmd.Enabled())
	}
	if err := cmd.Handle(context.Background(), 4, "/disks"); err != nil {
		t.Fatal(err)
	}
	want := "nvme0n1 NVMe Example NVMe SSD …0001: health OK, wear 2% (7d 1/2/3), spare unknown (floor 10%, drive unknown), " +
		"temp 35°C, written 7d n/a, self-test passed 1 d ago, last read 1 m ago"
	if pub.lastMsg != want {
		t.Errorf("msg = %q, want %q", pub.lastMsg, want)
	}
}

func TestDisksCommandNoDrivesSkipsMetrics(t *testing.T) {
	pub := &fakePublisher{}
	m := &fakeMetrics{}
	cmd := NewDisksCommand(zap.NewNop(), pub, fakeDrives{}, m)
	if err := cmd.Handle(context.Background(), 1, "/disks"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pub.lastMsg, "No drives") || len(m.prefixes) != 0 {
		t.Errorf("msg=%q prefixes=%v", pub.lastMsg, m.prefixes)
	}
}

func TestDisksCommandErrors(t *testing.T) {
	boom := errors.New("boom")
	d := fakeDrives{drives: []Drive{{Serial: "S1"}}}
	err := NewDisksCommand(zap.NewNop(), &fakePublisher{}, d, &fakeMetrics{err: boom}).
		Handle(context.Background(), 1, "/disks")
	if !errors.Is(err, boom) {
		t.Errorf("first metrics err = %v", err)
	}
	err = NewDisksCommand(zap.NewNop(), &errPublisher{err: boom}, d, &fakeMetrics{}).
		Handle(context.Background(), 1, "/disks")
	if !errors.Is(err, boom) {
		t.Errorf("publish err = %v", err)
	}
	second := &secondFails{err: boom}
	err = NewDisksCommand(zap.NewNop(), &fakePublisher{}, d, second).
		Handle(context.Background(), 1, "/disks")
	if !errors.Is(err, boom) {
		t.Errorf("second metrics err = %v", err)
	}
}

type secondFails struct {
	err   error
	calls int
}

func (f *secondFails) Metrics(context.Context, string, time.Time) ([]store.MetricStats, error) {
	f.calls++
	if f.calls == 2 {
		return nil, f.err
	}

	return nil, nil
}
