package kernel_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
)

const (
	webID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	hostHeader = "oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom," +
		"task_memcg=/system.slice/docker-" + webID + ".scope,task=python3,pid=4242,uid=1000"
	hostKilled = "Out of memory: Killed process 4242 (python3) total-vm:60000000kB, anon-rss:56623104kB, " +
		"file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:111000kB oom_score_adj:0"

	limitHeader = "oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=/,mems_allowed=0," +
		"oom_memcg=/system.slice/docker-" + webID + ".scope,task_memcg=/system.slice/docker-" + webID +
		".scope,task=python3,pid=4242,uid=1000"
	limitKilled = "Memory cgroup out of memory: Killed process 4242 (python3) total-vm:9000000kB, " +
		"anon-rss:8283750kB, file-rss:0kB, shmem-rss:0kB, UID:1000 pgtables:20000kB oom_score_adj:0"

	unitHeader = "oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0,global_oom," +
		"task_memcg=/system.slice/backup.service,task=worker,pid=977,uid=0"
	unitKilled = "Out of memory: Killed process 977 (worker) total-vm:4000000kB, anon-rss:3250585kB, " +
		"file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:8000kB oom_score_adj:0"
)

func names(id string) (string, bool) {
	if id == webID {
		return "web-app", true
	}

	return "", false
}

func readingTexts(t *testing.T, r *rig) []string {
	t.Helper()

	res := r.run(t)
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	out := make([]string, 0, len(res.Readings))
	for _, rd := range res.Readings {
		if rd.Key != "kernel/oom" {
			t.Fatalf("unexpected reading %+v", rd)
		}
		out = append(out, rd.Text)
	}

	return out
}

func feed(r *rig, from uint64, lines ...string) {
	for i, l := range lines {
		r.k.Consume(rec(from+uint64(i), l))
	}
}

func TestOOMContainerHostWide(t *testing.T) { // US4-1
	r := newRigWith(t, kernel.WithServices(kernel.CgroupResolver(names)))
	feed(r, 1, hostHeader, hostKilled)

	got := readingTexts(t, r)
	want := "Kernel: out of memory, killed python3 (pid 4242) in container web-app, 54 GB"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if again := r.run(t); len(again.Readings) != 0 {
		t.Fatalf("reported twice: %+v", again.Readings)
	}
}

func TestOOMContainerLimit(t *testing.T) { // US4-3
	r := newRigWith(t, kernel.WithServices(kernel.CgroupResolver(names)))
	feed(r, 1, limitHeader, limitKilled)

	got := readingTexts(t, r)
	want := "Kernel: container web-app hit its memory limit, killed python3 (pid 4242), 7.9 GB"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOOMSystemdUnit(t *testing.T) {
	r := newRigWith(t)
	feed(r, 1, unitHeader, unitKilled)

	got := readingTexts(t, r)
	want := "Kernel: out of memory, killed worker (pid 977) in backup.service, 3.1 GB"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOOMUnknownContainerFallsBackToShortID(t *testing.T) {
	r := newRigWith(t) // no name lookup at all
	feed(r, 1, hostHeader, hostKilled)

	got := readingTexts(t, r)
	want := "Kernel: out of memory, killed python3 (pid 4242) in container 0123456789ab, 54 GB"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCgroupResolver(t *testing.T) {
	tests := []struct {
		cgroup string
		want   kernel.Service
	}{
		{"/system.slice/docker-" + webID + ".scope", kernel.Service{Name: "web-app", Container: true}},
		{"/docker/" + webID, kernel.Service{Name: "web-app", Container: true}},
		{"/system.slice/docker-fedcba9876543210.scope", kernel.Service{Name: "fedcba987654", Container: true}},
		{"/system.slice/backup.service", kernel.Service{Name: "backup.service"}},
		{"/system.slice/backup.service/sub", kernel.Service{Name: "backup.service"}},
		{"/", kernel.Service{}},
		{"", kernel.Service{}},
		{"/user.slice/session-3.scope", kernel.Service{Name: "/user.slice/session-3.scope"}},
	}
	res := kernel.CgroupResolver(names)
	for _, tt := range tests {
		if got := res.Resolve(tt.cgroup); got != tt.want {
			t.Errorf("Resolve(%q) = %+v, want %+v", tt.cgroup, got, tt.want)
		}
	}
}

func TestOOMRootCgroupHasNoLocation(t *testing.T) {
	r := newRig(t)
	feed(r, 1, strings.Replace(hostHeader, "/system.slice/docker-"+webID+".scope", "/", 1), hostKilled)

	got := readingTexts(t, r)
	want := "Kernel: out of memory, killed python3 (pid 4242), 54 GB"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOOMUnpairedLines(t *testing.T) {
	t.Run("kill without header", func(t *testing.T) {
		r := newRig(t)
		feed(r, 1, hostKilled)
		got := readingTexts(t, r)
		want := "Kernel: out of memory, killed python3 (pid 4242), 54 GB"
		if len(got) != 1 || got[0] != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("header without kill", func(t *testing.T) {
		r := newRig(t)
		feed(r, 1, hostHeader, "usb 1-1: new high-speed USB device number 2 using example_hcd")
		if got := readingTexts(t, r); len(got) != 0 {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("kill of another pid does not take the header", func(t *testing.T) {
		r := newRigWith(t, kernel.WithServices(kernel.CgroupResolver(names)))
		feed(r, 1, hostHeader, unitKilled)
		got := readingTexts(t, r)
		want := "Kernel: out of memory, killed worker (pid 977), 3.1 GB"
		if len(got) != 1 || got[0] != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("pair split across runs", func(t *testing.T) {
		r := newRig(t)
		feed(r, 1, hostHeader)
		if got := readingTexts(t, r); len(got) != 0 {
			t.Fatalf("got %q", got)
		}
		feed(r, 2, hostKilled)
		if got := readingTexts(t, r); len(got) != 1 {
			t.Fatalf("got %q", got)
		}
	})
}

func TestOOMSmallVictimInMegabytes(t *testing.T) {
	r := newRig(t)
	feed(r, 1, strings.Replace(unitKilled, "anon-rss:3250585kB", "anon-rss:524288kB", 1))
	got := readingTexts(t, r)
	if len(got) != 1 || !strings.HasSuffix(got[0], ", 512 MB") {
		t.Fatalf("got %q", got)
	}
}

func TestOOMBurstCap(t *testing.T) {
	r := newRig(t)
	for i := range 8 {
		feed(r, uint64(1+2*i), unitHeader, unitKilled)
	}

	got := readingTexts(t, r)
	if len(got) != 6 {
		t.Fatalf("got %d readings: %q", len(got), got)
	}
	if want := "Kernel: … and 3 more out-of-memory kills"; got[5] != want {
		t.Fatalf("last = %q, want %q", got[5], want)
	}

	feed(r, 17, unitHeader, unitKilled, unitHeader, unitKilled, unitHeader, unitKilled,
		unitHeader, unitKilled, unitHeader, unitKilled, unitHeader, unitKilled)
	got = readingTexts(t, r)
	if want := "Kernel: … and 1 more out-of-memory kill"; len(got) != 6 || got[5] != want {
		t.Fatalf("got %q", got)
	}
}

func TestOOMReplay(t *testing.T) { // check --once
	r := newRawWith(t, kernel.WithReplay(), kernel.WithServices(kernel.CgroupResolver(names)))
	feed(r, 1, hostHeader, hostKilled, ioErr)

	res := r.run(t)
	var oom, other int
	for _, rd := range res.Readings {
		if rd.Key == "kernel/oom" {
			oom++
		} else {
			other++
		}
	}
	if oom != 1 || other != 1 {
		t.Fatalf("readings: %+v", res.Readings)
	}
}

func TestOOMPreviousBoot(t *testing.T) { // US4-2
	j := &fakeJournal{recs: []kernel.Record{
		journalRec(100, hostHeader),
		journalRec(101, hostKilled),
		journalRec(102, limitHeader),
		journalRec(103, limitKilled),
	}}
	state := previousBoot(t)
	r := newRigWith(t, kernel.WithJournal(j), kernel.WithServices(kernel.CgroupResolver(names)))
	r.boot.id = "boot-2"
	if err := r.k.Restore(state); err != nil {
		t.Fatal(err)
	}

	res := r.run(t)
	got := make([]string, 0, len(res.Readings))
	for _, rd := range res.Readings {
		got = append(got, rd.Text)
	}
	want := []string{
		"Kernel (previous boot): out of memory, killed python3 (pid 4242) in container web-app, 54 GB",
		"Kernel (previous boot): container web-app hit its memory limit, killed python3 (pid 4242), 7.9 GB",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOOMHeaderBeforeTheCursorStillResolvesItsKill(t *testing.T) {
	r := newRigWith(t, kernel.WithServices(kernel.CgroupResolver(names)))

	// The header is consumed and the cursor moves onto its seq; the Killed
	// record only arrives after that.
	feed(r, 1, hostHeader)
	if got := readingTexts(t, r); len(got) != 0 {
		t.Fatalf("got %q before the kill", got)
	}
	state, _ := r.k.Export()
	if !strings.Contains(state, `"seq":1`) {
		t.Fatalf("cursor not at the header: %s", state)
	}

	feed(r, 2, hostKilled)
	got := readingTexts(t, r)
	want := "Kernel: out of memory, killed python3 (pid 4242) in container web-app, 54 GB"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
