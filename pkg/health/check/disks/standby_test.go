package disks

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// noReadingsFor fails when res has a reading of the drive with serial.
func noReadingsFor(t *testing.T, res health.CheckResult, serial string) {
	t.Helper()

	for _, r := range res.Readings {
		if strings.Contains(r.Key, serial) || strings.Contains(r.Text, serial) {
			t.Errorf("drive %s judged: %+v", serial, r)
		}
	}
}

// TestStandbyDriveIsSkipped: a known drive in standby is not read, so it is
// neither judged nor missing, nor a checker error; it is judged again, as a
// known drive, once it is awake.
func TestStandbyDriveIsSkipped(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, tempsCfg(0))

	src.runs["sda"] = output{stdout: []byte(`{"smartctl":{"exit_status":3}}`), status: StandbyStatus}
	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v, want none for a drive in standby", res.Err)
	}
	noReadingsFor(t, res, serialSATA)
	if r := mustFind(t, res, "disks/"+serialNVMe+"/health"); r.Verdict != health.Good {
		t.Errorf("awake drive = %+v", r)
	}

	clk.add(15 * time.Minute)
	src.set("sda", sata(serialSATA))
	res = c.Run(t.Context())
	if res.Err != nil || len(events(res)) != 0 {
		t.Fatalf("awake again = %+v", res)
	}
	if r := mustFind(t, res, "disks/"+serialSATA+"/health"); r.Verdict != health.Good {
		t.Errorf("woken drive = %+v", r)
	}
}

// TestStandbyUnknownNodeSuspendsMissingSilently: a node in standby that no
// known drive was last read from may be a known drive that moved, so missing
// is not judged that run, and nothing is reported.
func TestStandbyUnknownNodeSuspendsMissingSilently(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.remove("sda")
	src.nodes = append(src.nodes, "sdb")
	src.runs["sdb"] = output{status: StandbyStatus}
	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v, want none", res.Err)
	}
	if r, ok := find(res, "disks/"+serialSATA+"/missing"); ok {
		t.Errorf("missing judged while a node sleeps: %+v", r)
	}
}

// TestStandbyOnFirstRunIsNotAnError: a drive asleep on the first run is not
// in the inventory and is not an error.
func TestStandbyOnFirstRunIsNotAnError(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.nodes = append(src.nodes, "sdb")
	src.runs["sdb"] = output{status: StandbyStatus}
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if ev := events(res); len(ev) != 1 || strings.Contains(ev[0].Text, "sdb") {
		t.Errorf("events = %+v", ev)
	}
}

// TestSlowDriveKeepsTheOtherDrives: a drive still being read when the run's
// context ends is unreadable this run; the drives already read keep their
// readings.
func TestSlowDriveKeepsTheOtherDrives(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.hook = func(ctx context.Context, node string) error {
		if node != "nvme0n1" {
			return nil
		}
		<-ctx.Done()

		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	res := c.Run(ctx)

	checkUnreadable(t, res, "/dev/nvme0n1", serialNVMe)
	noReadingsFor(t, res, serialNVMe)
	if r := mustFind(t, res, "disks/"+serialSATA+"/health"); r.Verdict != health.Good {
		t.Errorf("readable drive = %+v", r)
	}
}

// TestDrivesAreReadConcurrently: one slow drive does not hold up the others,
// so each drive's own timeout bounds the run.
func TestDrivesAreReadConcurrently(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	c := newLoaded(src, disksCfg(), nil, newClock().now)

	var started sync.WaitGroup
	started.Add(2)
	all := make(chan struct{})
	go func() {
		started.Wait()
		close(all)
	}()
	errSerial := errors.New("drives read one after the other")
	src.hook = func(ctx context.Context, _ string) error {
		started.Done()
		select {
		case <-all:
			return nil
		case <-ctx.Done():
			return errSerial
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if res := c.Run(ctx); res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
}

// serialExcluded is an excluded drive in the excluded-node tests.
const serialExcluded = "TEST0000000009"

// excludedFixture returns a check that has read the known drive serialSATA
// on sda and the excluded drive serialExcluded on sdb.
func excludedFixture(t *testing.T) (*Check, *fakeSource) {
	t.Helper()

	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("sdb", sata(serialExcluded))
	cfg := disksCfg()
	cfg.ExcludeSerials = []string{serialExcluded}
	clk := newClock()
	c := newLoaded(src, cfg, nil, clk.now)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("first run: %v", res.Err)
	}

	return c, src
}

// TestExcludedDriveNodeIsAttributed: the node an excluded drive was read
// from is remembered while the set of nodes stays the same. The drive asleep
// there later is credited to it silently and does not suspend the missing
// judgement of the other drives; unreadable there, it is still a checker
// error, but it does not suspend that judgement either.
func TestExcludedDriveNodeIsAttributed(t *testing.T) {
	for _, tt := range []struct {
		name    string
		out     output
		wantErr bool
	}{
		{"asleep", output{status: StandbyStatus}, false},
		{"unreadable", output{status: 2}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, src := excludedFixture(t)

			// The known drive was swapped for a new one on the same node.
			src.set("sda", sata(serialNew))
			src.runs["sdb"] = tt.out
			res := c.Run(t.Context())
			switch {
			case !tt.wantErr && res.Err != nil:
				t.Fatalf("Err = %v, want none for the excluded drive asleep", res.Err)
			case tt.wantErr && (res.Err == nil || !strings.Contains(res.Err.Error(), "/dev/sdb") ||
				!strings.Contains(res.Err.Error(), serialExcluded)):
				t.Fatalf("Err = %v, want the unreadable /dev/sdb naming the excluded drive", res.Err)
			}
			if r := mustFind(t, res, "disks/"+serialSATA+"/missing"); r.Verdict != health.Bad {
				t.Errorf("missing = %+v, want Bad", r)
			}
			for _, r := range res.Readings {
				if strings.Contains(r.Key, serialExcluded) {
					t.Errorf("excluded drive judged: %+v", r)
				}
			}
		})
	}
}

// TestExcludedNodeCreditIsNotStale: once the excluded drive is pulled and a
// known drive re-enumerated onto its old node is asleep there, the node is
// not credited to the excluded drive (the set of nodes changed), so the known
// drive is not falsely judged missing: the node is a sleeping orphan.
func TestExcludedNodeCreditIsNotStale(t *testing.T) {
	c, src := excludedFixture(t)

	src.remove("sda")
	src.runs["sdb"] = output{status: StandbyStatus}
	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v, want none", res.Err)
	}
	if r, ok := find(res, "disks/"+serialSATA+"/missing"); ok {
		t.Errorf("known drive judged missing through a stale credit: %+v", r)
	}
}

// TestExcludedNodeCreditFollowsTheReads: a non-excluded drive read on the
// excluded drive's node takes the credit away; the excluded drive read on
// another node moves it there.
func TestExcludedNodeCreditFollowsTheReads(t *testing.T) {
	c, src := excludedFixture(t)

	src.set("sdb", sata(serialNew))
	src.set("sda", sata(serialExcluded))
	_ = c.Run(t.Context())

	if got := c.excludedNodes; len(got) != 1 || got["sda"] != serialExcluded {
		t.Fatalf("excludedNodes = %v, want only sda", got)
	}
}

// TestStandbyUnknownNodeSuspendsMissingForABoundedTime: a node asleep that
// no drive can be attributed to suspends the missing judgement only for
// asleepOrphanLimit; after that a known drive gone is reported missing.
func TestStandbyUnknownNodeSuspendsMissingForABoundedTime(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.remove("sda")
	src.nodes = append(src.nodes, "sdb")
	src.runs["sdb"] = output{status: StandbyStatus}
	if r, ok := find(c.Run(t.Context()), "disks/"+serialSATA+"/missing"); ok {
		t.Fatalf("missing judged while a node first sleeps: %+v", r)
	}

	clk.add(asleepOrphanLimit - time.Minute)
	if r, ok := find(c.Run(t.Context()), "disks/"+serialSATA+"/missing"); ok {
		t.Fatalf("missing judged within the limit: %+v", r)
	}

	clk.add(time.Minute)
	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v, want none", res.Err)
	}
	if r := mustFind(t, res, "disks/"+serialSATA+"/missing"); r.Verdict != health.Bad {
		t.Errorf("missing = %+v, want Bad once the limit has passed", r)
	}

	// A run that finds the node not asleep restarts the clock for its next
	// sleep.
	clk.add(15 * time.Minute)
	src.runs["sdb"] = output{status: 2}
	_ = c.Run(t.Context())
	clk.add(15 * time.Minute)
	src.runs["sdb"] = output{status: StandbyStatus}
	if r, ok := find(c.Run(t.Context()), "disks/"+serialSATA+"/missing"); ok {
		t.Errorf("missing judged while the node sleeps again: %+v", r)
	}
}
