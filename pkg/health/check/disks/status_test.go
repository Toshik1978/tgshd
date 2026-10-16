package disks

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// statusOf returns the status of the drive with serial.
func statusOf(t *testing.T, c *Check, serial string) DriveStatus {
	t.Helper()

	all := c.Status()
	for i := range all {
		if all[i].Serial == serial {
			return all[i]
		}
	}
	t.Fatalf("no status for %s in %+v", serial, c.Status())

	return DriveStatus{}
}

func TestStatusFromLastRead(t *testing.T) {
	src := &fakeSource{}
	src.set("nvme0n1", nvme(serialNVMe))
	src.set("sda", sata(serialSATA))
	clk := newClock()
	cfg := disksCfg()
	cfg.SpareFloorPercent = 20
	c := newLoaded(src, cfg, nil, clk.now)
	first := clk.now()
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("run: %v", res.Err)
	}
	clk.add(2 * day)

	all := c.Status()
	if len(all) != 2 || all[0].Serial != serialSATA || all[1].Serial != serialNVMe {
		t.Fatalf("Status() = %+v, want both drives sorted by serial", all)
	}

	got := statusOf(t, c, serialNVMe)
	want := DriveStatus{
		Serial: serialNVMe, Model: "Example NVMe SSD", Bus: BusNVMe, Transport: transportNVMe,
		Node: "nvme0n1", Health: "ok", LastReadAt: first,
		Temperature: new(38), Wear: new(1), Spare: new(100), SpareThreshold: new(10),
		SpareFloor: 20, WearLimit: 80,
		SelfTestNewest:  &SelfTestNewest{Result: "passed", Description: got.SelfTestNewest.Description},
		SelfTestPassAge: 2 * day, SelfTestReadable: true, SelfTestFormat: SelfTestFormatOK,
		SelfTestNewestAge: new(10 * time.Hour),
	}
	if got.SelfTestNewest == nil || got.SelfTestNewest.Result != "passed" {
		t.Fatalf("SelfTestNewest = %+v", got.SelfTestNewest)
	}
	if !equalStatus(got, want) {
		t.Errorf("nvme status = %+v\nwant %+v", got, want)
	}

	sa := statusOf(t, c, serialSATA)
	if sa.Bus != BusATA || sa.Transport != transportSATA || sa.Wear != nil || sa.Spare != nil ||
		sa.Temperature == nil || *sa.Temperature != 34 {
		t.Errorf("sata status = %+v", sa)
	}
}

// equalStatus compares statuses field by field, following pointers.
func equalStatus(a, b DriveStatus) bool {
	ptr := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	if !ptr(a.Temperature, b.Temperature) || !ptr(a.Wear, b.Wear) || !ptr(a.Spare, b.Spare) ||
		!ptr(a.SpareThreshold, b.SpareThreshold) || (a.SelfTestNewestAge == nil) != (b.SelfTestNewestAge == nil) ||
		(a.SelfTestNewestAge != nil && *a.SelfTestNewestAge != *b.SelfTestNewestAge) {
		return false
	}
	a.Temperature, a.Wear, a.Spare, a.SpareThreshold, a.SelfTestNewestAge = nil, nil, nil, nil, nil
	b.Temperature, b.Wear, b.Spare, b.SpareThreshold, b.SelfTestNewestAge = nil, nil, nil, nil, nil
	if (a.SelfTestNewest == nil) != (b.SelfTestNewest == nil) ||
		(a.SelfTestNewest != nil && a.SelfTestNewest.Result != b.SelfTestNewest.Result) {
		return false
	}
	a.SelfTestNewest, b.SelfTestNewest = nil, nil

	return a == b
}

// TestStatusAsleepKeepsLastValues: a drive found asleep is reported asleep
// since the first such run, with its last successful read and values kept.
func TestStatusAsleepKeepsLastValues(t *testing.T) {
	src := &fakeSource{}
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)
	read := clk.now().Add(-15 * time.Minute)

	src.runs["nvme0n1"] = output{status: StandbyStatus}
	c.Run(t.Context())
	asleep := clk.now()
	clk.add(15 * time.Minute)
	c.Run(t.Context())

	got := statusOf(t, c, serialNVMe)
	if !got.AsleepSince.Equal(asleep) || !got.UnreadableSince.IsZero() {
		t.Errorf("AsleepSince = %v, UnreadableSince = %v, want %v and none",
			got.AsleepSince, got.UnreadableSince, asleep)
	}
	if !got.LastReadAt.Equal(read) {
		t.Errorf("LastReadAt = %v, want the last successful read %v", got.LastReadAt, read)
	}
	if got.Wear == nil || *got.Wear != 1 || got.Spare == nil || *got.Spare != 100 {
		t.Errorf("wear/spare = %v/%v, want the last read's kept", got.Wear, got.Spare)
	}
}

// TestStatusUnreadableSince: a failed read marks the drive unreadable since
// the first failure; a good read clears it.
func TestStatusUnreadableSince(t *testing.T) {
	src := &fakeSource{}
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.runs["nvme0n1"] = output{stdout: []byte("not json")}
	failed := clk.now()
	c.Run(t.Context())
	clk.add(15 * time.Minute)
	c.Run(t.Context())
	if got := statusOf(t, c, serialNVMe); !got.UnreadableSince.Equal(failed) || !got.AsleepSince.IsZero() {
		t.Errorf("UnreadableSince = %v, AsleepSince = %v, want %v and none",
			got.UnreadableSince, got.AsleepSince, failed)
	}

	src.set("nvme0n1", nvme(serialNVMe))
	clk.add(15 * time.Minute)
	c.Run(t.Context())
	if got := statusOf(t, c, serialNVMe); !got.UnreadableSince.IsZero() || !got.LastReadAt.Equal(clk.now()) {
		t.Errorf("after a good read: %+v", got)
	}
}

// TestStatusFromRestoredBaselines: after a restart only the baseline is known,
// so values read live are absent, and a self-test format that is not ok is
// reported as unreadable. Retired drives are not listed.
func TestStatusFromRestoredBaselines(t *testing.T) {
	seen := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	data, err := json.Marshal(baselineData{
		Health: "failing", SelfTestFormat: SelfTestFormatUnsupported, Node: "sdz", Transport: "sas",
		AsleepSince: seen.Unix(), SelfTestOKAt: seen.Add(24 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	c := New(&fakeSource{}, disksCfg(), nil, clk.now)
	c.LoadBaselines([]store.Baseline{
		{Serial: serialSATA, Model: "Example SAS HDD", Bus: "SCSI", Data: data, FirstSeenAt: seen, LastSeenAt: seen},
		{
			Serial: serialNew, Model: "Example SSD", Data: []byte("{}"),
			FirstSeenAt: seen, LastSeenAt: seen, Retired: true,
		},
	})

	all := c.Status()
	if len(all) != 1 {
		t.Fatalf("Status() = %+v, want only the live drive", all)
	}
	got := all[0]
	if got.Health != "failing" || got.Node != "sdz" || got.Transport != "sas" || got.Bus != BusSCSI ||
		!got.LastReadAt.Equal(seen) || !got.AsleepSince.Equal(seen) {
		t.Errorf("status = %+v", got)
	}
	if got.SelfTestReadable {
		t.Error("SelfTestReadable = true for an unsupported log")
	}
	if got.Temperature != nil || got.Wear != nil || got.Spare != nil {
		t.Errorf("live values present after a restore: %+v", got)
	}
	if want := clk.now().Sub(seen.Add(24 * time.Hour)); got.SelfTestPassAge != want {
		t.Errorf("SelfTestPassAge = %v, want %v", got.SelfTestPassAge, want)
	}
}

// TestStatusIsACopy: changing a returned status does not change the check.
func TestStatusIsACopy(t *testing.T) {
	src := &fakeSource{}
	src.set("nvme0n1", nvme(serialNVMe))
	c := fixtureCheck(t, src, newClock(), nil)

	*statusOf(t, c, serialNVMe).Wear = 99
	if w := statusOf(t, c, serialNVMe).Wear; w == nil || *w != 1 {
		t.Errorf("Wear = %v after a caller changed its copy", w)
	}
}

// TestStatusSATAWear: a SATA SSD's wear (100 minus the normalised value of
// attribute 177) reaches the status, and a spinning disk reports none.
func TestStatusSATAWear(t *testing.T) {
	src := &fakeSource{}
	ssd := sata(serialSATA)
	ssd.ssdWear = 88
	src.set("sda", ssd)
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("run: %v", res.Err)
	}

	got := statusOf(t, c, serialSATA)
	if got.Wear == nil || *got.Wear != 12 {
		t.Errorf("Wear = %v, want 12", got.Wear)
	}
	if got.Spare != nil {
		t.Errorf("Spare = %v, want none for a SATA drive", got.Spare)
	}
	if got.SelfTestFormat != SelfTestFormatOK {
		t.Errorf("SelfTestFormat = %q, want ok", got.SelfTestFormat)
	}
}

// TestStatusSelfTestFormatEmptyBeforeRead: a drive restored from a baseline
// and not yet read has no self-test format, so it is not "unreadable".
func TestStatusSelfTestFormatEmptyBeforeRead(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("run: %v", res.Err)
	}
	c.mu.Lock()
	c.bases[serialSATA].data.SelfTestFormat = ""
	c.mu.Unlock()

	if got := statusOf(t, c, serialSATA); got.SelfTestFormat != "" || got.SelfTestReadable {
		t.Errorf("SelfTestFormat = %q, readable = %v, want empty and false", got.SelfTestFormat, got.SelfTestReadable)
	}
}

// TestStatusSelfTestNewestAge: the age of the newest finished self-test is
// power-on time (current hours minus the test's hours), nil when unknown.
func TestStatusSelfTestNewestAge(t *testing.T) {
	src := &fakeSource{}
	bad := sata(serialSATA)
	bad.testBad = true // hours 4200, test ran at 4190
	src.set("sda", bad)
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("run: %v", res.Err)
	}
	got := statusOf(t, c, serialSATA)
	if got.SelfTestNewest == nil || got.SelfTestNewest.Result != "failed" ||
		got.SelfTestNewestAge == nil || *got.SelfTestNewestAge != 10*time.Hour {
		t.Fatalf("newest = %+v, age = %v, want failed 10h ago", got.SelfTestNewest, got.SelfTestNewestAge)
	}

	// After a restart there are no live hours: the age is unknown.
	c.mu.Lock()
	c.live = map[string]liveValues{}
	c.mu.Unlock()
	if got := statusOf(t, c, serialSATA); got.SelfTestNewestAge != nil {
		t.Errorf("age = %v, want nil without the current power-on hours", got.SelfTestNewestAge)
	}
}

// TestStatusSelfTestPassObserved: a pass in the log adopted at first sighting
// was not observed; one that appears later is.
func TestStatusSelfTestPassObserved(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	src.set("sda", d)
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)
	c.Run(t.Context())
	if got := statusOf(t, c, serialSATA); got.SelfTestPassObserved {
		t.Error("PassObserved after first sighting, want false")
	}

	clk.add(time.Hour)
	d.hours += 100
	d.log = []testRun{{hours: d.hours - 1}, {hours: d.hours - 110}}
	src.set("sda", d)
	c.Run(t.Context())
	if got := statusOf(t, c, serialSATA); !got.SelfTestPassObserved {
		t.Error("PassObserved after a new pass, want true")
	}
}
