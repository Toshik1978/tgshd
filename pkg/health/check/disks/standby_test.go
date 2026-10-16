package disks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/store"
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

// TestAsleepOrphanIsForceReadToIdentifyIt: a node asleep that no drive can
// be attributed to suspends the missing judgement silently until it has been
// asleep for force_read_after; then it takes the run's forced read, which
// identifies it. Identified as the known drive that moved there, that drive
// is not missing; identified as another drive, the known one is.
func TestAsleepOrphanIsForceReadToIdentifyIt(t *testing.T) {
	for _, tt := range []struct {
		name        string
		woken       string
		wantMissing bool
	}{
		{"moved known drive", serialSATA, false},
		{"another drive", serialNew, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			src.set("sda", sata(serialSATA))
			src.set("nvme0n1", nvme(serialNVMe))
			clk := newClock()
			c := fixtureCheck(t, src, clk, nil)

			src.remove("sda")
			src.nodes = append(src.nodes, "sdb")
			src.runs["sdb"] = output{status: StandbyStatus}
			src.woken = map[string]output{"sdb": {stdout: sata(tt.woken).json()}}
			if r, ok := find(c.Run(t.Context()), "disks/"+serialSATA+"/missing"); ok {
				t.Fatalf("missing judged while a node first sleeps: %+v", r)
			}

			clk.add(24*time.Hour - time.Minute)
			if r, ok := find(c.Run(t.Context()), "disks/"+serialSATA+"/missing"); ok {
				t.Fatalf("missing judged before the forced read: %+v", r)
			}
			if len(src.wakes) != 0 {
				t.Fatalf("woken before force_read_after: %v", src.wakes)
			}

			clk.add(time.Minute)
			res := c.Run(t.Context())
			if res.Err != nil {
				t.Fatalf("Err = %v, want none", res.Err)
			}
			if len(src.wakes) != 1 || src.wakes[0] != "sdb" {
				t.Fatalf("wakes = %v, want one forced read of sdb", src.wakes)
			}
			r, ok := find(res, "disks/"+serialSATA+"/missing")
			switch {
			case tt.wantMissing && (!ok || r.Verdict != health.Bad):
				t.Errorf("missing = %+v, %v; want Bad", r, ok)
			case !tt.wantMissing && (!ok || r.Verdict != health.Good):
				t.Errorf("missing = %+v, %v; want Good for the identified drive", r, ok)
			}
		})
	}
}

// TestAsleepOrphanForcedReadFails: a sleeping orphan whose forced read fails
// is a forced-read checker error naming its node, keeps suspending the
// missing judgement, and is not woken again before force_read_after.
func TestAsleepOrphanForcedReadFails(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.remove("sda")
	src.nodes = append(src.nodes, "sdb")
	src.runs["sdb"] = output{status: StandbyStatus}
	src.woken = map[string]output{"sdb": {err: errors.New("example I/O error")}}
	_ = c.Run(t.Context())
	clk.add(24 * time.Hour)
	res := c.Run(t.Context())
	if r, ok := find(res, "disks/"+serialSATA+"/missing"); ok {
		t.Errorf("missing judged after a failed forced read: %+v", r)
	}
	ce := forcedReadError(t, res, "sdb")
	if !strings.Contains(ce.Error(), "example I/O error") {
		t.Errorf("error = %v, want the cause", ce)
	}

	for _, step := range []time.Duration{time.Hour, 22 * time.Hour} {
		clk.add(step)
		res = c.Run(t.Context())
		if r, ok := find(res, "disks/"+serialSATA+"/missing"); ok {
			t.Errorf("missing judged while the node sleeps: %+v", r)
		}
	}
	if len(src.wakes) != 1 {
		t.Fatalf("wakes = %v, want no retry before force_read_after", src.wakes)
	}
	clk.add(time.Hour)
	_ = c.Run(t.Context())
	if len(src.wakes) != 2 {
		t.Errorf("wakes = %v, want a retry after force_read_after", src.wakes)
	}
}

// forcedReadError returns the run's forced-read checker error of subject,
// failing when there is none.
func forcedReadError(t *testing.T, res health.CheckResult, subject string) health.CheckError {
	t.Helper()

	for _, ce := range health.CheckErrors(res.Err) {
		if ce.Key(checkName) == checkName+"/"+subject+"/forced-read" {
			return ce
		}
	}
	t.Fatalf("Err = %v, want a %s/forced-read checker error", res.Err, subject)

	return health.CheckError{}
}

// dataOf returns the stored baseline of the drive with serial and its data.
func dataOf(t *testing.T, c *Check, serial string) (store.Baseline, baselineData) {
	t.Helper()

	bases := c.Baselines()
	for i := range bases {
		if bases[i].Serial != serial {
			continue
		}
		var d baselineData
		if err := json.Unmarshal(bases[i].Data, &d); err != nil {
			t.Fatal(err)
		}

		return bases[i], d
	}
	t.Fatalf("no baseline for %s", serial)

	return store.Baseline{}, baselineData{}
}

// TestForcedRead: a drive asleep at every check is read, waking it, on the
// first check after its last successful read became force_read_after old,
// and so woken at most once per 24 h over a week (US1 acceptance 4, SC-003).
// While asleep its baseline records since when; the forced read records its
// attempt and, read, the drive's last read.
func TestForcedRead(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	start := clk.now()
	c := fixtureCheck(t, src, clk, nil)

	src.runs["sda"] = output{status: StandbyStatus}
	src.woken = map[string]output{"sda": {stdout: sata(serialSATA).json()}}
	firstAsleep := clk.now()
	var wokenAt []time.Time
	for clk.now().Before(start.Add(7 * day)) {
		n := len(src.wakes)
		res := c.Run(t.Context())
		if res.Err != nil {
			t.Fatalf("at %v: Err = %v", clk.now(), res.Err)
		}
		b, d := dataOf(t, c, serialSATA)
		if len(src.wakes) > n {
			wokenAt = append(wokenAt, clk.now())
			if r := mustFind(t, res, "disks/"+serialSATA+"/health"); r.Verdict != health.Good {
				t.Fatalf("forced read = %+v", r)
			}
			if !b.LastSeenAt.Equal(clk.now()) || d.AsleepSince != 0 || d.ForcedReadAt != clk.now().Unix() {
				t.Fatalf("after the forced read: last seen %v, data %+v", b.LastSeenAt, d)
			}
		} else {
			noReadingsFor(t, res, serialSATA)
			if d.AsleepSince == 0 || d.AsleepSince > clk.now().Unix() {
				t.Fatalf("asleep at %v: data %+v, want asleep_since recorded", clk.now(), d)
			}
		}
		if len(wokenAt) == 0 && d.AsleepSince != firstAsleep.Unix() {
			t.Fatalf("asleep_since = %d, want the first asleep run %d", d.AsleepSince, firstAsleep.Unix())
		}
		clk.add(15 * time.Minute)
	}

	if len(wokenAt) != 6 {
		t.Fatalf("woken %d times over a week (%v), want 6", len(wokenAt), wokenAt)
	}
	last := start
	for _, w := range wokenAt {
		if w.Sub(last) != day {
			t.Errorf("woken at %v, %v after the last read; want exactly 24 h", w, w.Sub(last))
		}
		last = w
	}
}

// TestForcedReadNotRetried: a forced read that fails is not retried before
// force_read_after has passed again (SC-003).
func TestForcedReadNotRetried(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	start := clk.now()
	c := fixtureCheck(t, src, clk, nil)

	src.runs["sda"] = output{status: StandbyStatus}
	src.woken = map[string]output{"sda": {status: StandbyStatus}}
	_ = c.Run(t.Context())
	clk.t = start.Add(day)
	res := c.Run(t.Context())
	if len(src.wakes) != 1 {
		t.Fatalf("wakes = %v, want one forced read", src.wakes)
	}
	ce := forcedReadError(t, res, serialSATA)
	if msg := ce.Err.Error(); !strings.HasPrefix(msg, "drive asleep since ") ||
		!strings.Contains(msg, " could not be read when woken: ") {
		t.Errorf("error = %q", msg)
	}
	_, d := dataOf(t, c, serialSATA)
	if d.ForcedReadAt != clk.now().Unix() || d.UnreadableSince != clk.now().Unix() {
		t.Errorf("data = %+v, want forced_read_at and unreadable_since now", d)
	}

	failedAt := clk.now()
	for _, after := range []time.Duration{time.Hour, 23 * time.Hour} {
		clk.t = failedAt.Add(after)
		res = c.Run(t.Context())
		if len(src.wakes) != 1 {
			t.Fatalf("+%v: wakes = %v, want no retry", after, src.wakes)
		}
		if res.Err != nil {
			t.Errorf("+%v: Err = %v, want none while asleep", after, res.Err)
		}
	}
	_, d = dataOf(t, c, serialSATA)
	if d.UnreadableSince != failedAt.Unix() {
		t.Errorf("unreadable_since = %d, want kept at %d", d.UnreadableSince, failedAt.Unix())
	}
	clk.t = failedAt.Add(day)
	_ = c.Run(t.Context())
	if len(src.wakes) != 2 {
		t.Errorf("+24h: wakes = %v, want the retry", src.wakes)
	}
}

// TestOneForcedReadPerRun: of three drives due a forced read, one is woken
// per run, after the routine reads, the one read longest ago first.
func TestOneForcedReadPerRun(t *testing.T) {
	src := &fakeSource{}
	serials := map[string]string{"sda": "TEST0000000004", "sdb": "TEST0000000005", "sdc": "TEST0000000006"}
	src.woken = map[string]output{}
	for _, node := range []string{"sda", "sdb", "sdc"} {
		src.set(node, sata(serials[node]))
		src.woken[node] = src.runs[node]
		src.runs[node] = output{status: StandbyStatus}
	}
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	now := clk.now()
	// Last read 30, 50 and 40 h ago: sdb first, then sdc, then sda.
	ago := map[string]time.Duration{"sda": 30 * time.Hour, "sdb": 50 * time.Hour, "sdc": 40 * time.Hour}
	bases := make([]store.Baseline, 0, len(serials))
	for node, serial := range serials {
		bases = append(bases, store.Baseline{
			Serial: serial, Model: "Example SSD", Bus: "SATA",
			Data:        json.RawMessage(`{"node":"` + node + `"}`),
			FirstSeenAt: now.Add(-60 * 24 * time.Hour), LastSeenAt: now.Add(-ago[node]),
		})
	}
	c := New(src, disksCfg(), nil, clk.now)
	c.LoadBaselines(bases)

	for i, want := range []string{"sdb", "sdc", "sda"} {
		src.calls = nil
		res := c.Run(t.Context())
		if len(src.wakes) != i+1 || src.wakes[i] != want {
			t.Fatalf("run %d: wakes = %v, want %s woken", i+1, src.wakes, want)
		}
		if last := src.calls[len(src.calls)-1]; len(src.calls) != 5 || last != want {
			t.Errorf("run %d: calls = %v, want the forced read after the 4 routine reads", i+1, src.calls)
		}
		if r := mustFind(t, res, "disks/"+serials[want]+"/health"); r.Verdict != health.Good {
			t.Errorf("run %d: forced read = %+v", i+1, r)
		}
		clk.add(15 * time.Minute)
	}
	_ = c.Run(t.Context())
	if len(src.wakes) != 3 {
		t.Errorf("wakes = %v, want no fourth forced read", src.wakes)
	}
}

// TestForcedReadFailureKeepsProblems: a forced read that fails judges
// nothing of the drive, so every open problem stays open and the drive is
// not missing; it is a forced-read checker error (SC-002).
func TestForcedReadFailureKeepsProblems(t *testing.T) {
	for _, tt := range []struct {
		name  string
		woken output
	}{
		{"run error", output{err: errors.New("example I/O error")}},
		{"still in standby", output{status: StandbyStatus}},
		{"undecodable", output{stdout: []byte("not json"), status: 2}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			failing := sata(serialSATA)
			failing.failed = true
			src.set("sda", failing)
			src.set("nvme0n1", nvme(serialNVMe))
			clk := newClock()
			c := newLoaded(src, disksCfg(), nil, clk.now)
			if r := mustFind(t, c.Run(t.Context()), "disks/"+serialSATA+"/health"); r.Verdict != health.Bad {
				t.Fatalf("health = %+v, want the problem open", r)
			}

			src.runs["sda"] = output{status: StandbyStatus}
			src.woken = map[string]output{"sda": tt.woken}
			clk.add(day)
			res := c.Run(t.Context())
			if len(src.wakes) != 1 {
				t.Fatalf("wakes = %v, want one forced read", src.wakes)
			}
			noReadingsFor(t, res, serialSATA)
			forcedReadError(t, res, serialSATA)
			if r := mustFind(t, res, "disks/"+serialNVMe+"/health"); r.Verdict != health.Good {
				t.Errorf("other drive = %+v", r)
			}
		})
	}
}

// TestWithoutForcedReadNeverWakes: a check built WithoutForcedRead (the
// one-shot CLI) leaves a drive asleep for more than force_read_after alone,
// and its status says since when it sleeps.
func TestWithoutForcedReadNeverWakes(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := New(src, disksCfg(), nil, clk.now, WithoutForcedRead())
	c.LoadBaselines(nil)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("first run: %v", res.Err)
	}
	readAt := clk.now()

	src.runs["sda"] = output{status: StandbyStatus}
	clk.add(15 * time.Minute)
	asleepAt := clk.now()
	for clk.now().Before(readAt.Add(3 * day)) {
		res := c.Run(t.Context())
		if res.Err != nil {
			t.Fatalf("at %v: Err = %v", clk.now(), res.Err)
		}
		noReadingsFor(t, res, serialSATA)
		clk.add(time.Hour)
	}

	if len(src.wakes) != 0 {
		t.Fatalf("wakes = %v, want none without the forced read", src.wakes)
	}
	st := c.Status()
	if len(st) != 1 || !st[0].AsleepSince.Equal(asleepAt) || !st[0].LastReadAt.Equal(readAt) {
		t.Fatalf("Status() = %+v, want asleep since %v, last read %v", st, asleepAt, readAt)
	}
}
