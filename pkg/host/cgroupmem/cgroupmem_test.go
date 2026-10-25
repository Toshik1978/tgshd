package cgroupmem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func unit(t *testing.T, root, rel string, anon, shmem, current int) {
	t.Helper()
	write(t, root, rel+"/memory.stat", "anon "+strconv.Itoa(anon)+"\nfile 999\nshmem "+strconv.Itoa(shmem)+"\nslab 5\n")
	write(t, root, rel+"/memory.current", strconv.Itoa(current)+"\n")
}

func TestServices(t *testing.T) {
	root := t.TempDir()
	unit(t, root, "system.slice/backup.service", 1000, 24, 5000)
	unit(t, root, "system.slice/docker-abc123.scope", 2000, 0, 3000)
	unit(t, root, "system.slice/system-foo.slice/worker.service", 300, 7, 400)
	unit(t, root, "system.slice/system-foo.slice/deeper.slice/ignored.service", 1, 1, 1)
	unit(t, root, "system.slice/cron.socket", 1, 1, 1)
	unit(t, root, "system.slice/session-1.scope", 1, 1, 1)
	unit(t, root, "user.slice/user.service", 1, 1, 1)
	write(t, root, "system.slice/memory.stat", "anon 1\n")

	got, err := New(root, t.TempDir()).Services()
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b Service) int { return strings.Compare(a.Path, b.Path) })
	want := []Service{
		{Kind: KindUnit, Name: "backup.service", Path: "system.slice/backup.service", Anon: 1024, Current: 5000},
		{Kind: KindContainer, Name: "abc123", Path: "system.slice/docker-abc123.scope", Anon: 2000, Current: 3000},
		{
			Kind:    KindUnit,
			Name:    "worker.service",
			Path:    "system.slice/system-foo.slice/worker.service",
			Anon:    307,
			Current: 400,
		},
	}
	slices.SortFunc(want, func(a, b Service) int { return strings.Compare(a.Path, b.Path) })
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestServicesSkipsVanished(t *testing.T) {
	root := t.TempDir()
	unit(t, root, "system.slice/ok.service", 10, 0, 20)
	// A directory listed but already gone/without files reads as ErrNotExist.
	if err := os.MkdirAll(filepath.Join(root, "system.slice", "gone.service"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := New(root, t.TempDir()).Services()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "ok.service" {
		t.Fatalf("got %+v", got)
	}
}

// TestServicesWithoutMemoryAccounting: with the memory controller off the
// service cgroups exist but expose no memory.stat. That is not a vanished
// cgroup: when no service is accounted the reader says so.
func TestServicesWithoutMemoryAccounting(t *testing.T) {
	root := t.TempDir()
	write(t, root, "system.slice/backup.service/cgroup.procs", "")
	write(t, root, "system.slice/docker-abc123.scope/cgroup.procs", "")
	write(t, root, "system.slice/apps.slice/worker.service/cgroup.procs", "")

	got, err := New(root, t.TempDir()).Services()
	if !errors.Is(err, ErrNoMemoryAccounting) {
		t.Fatalf("Services() = %+v, %v, want ErrNoMemoryAccounting", got, err)
	}

	// One accounted service is enough: the walk returns it.
	unit(t, root, "system.slice/ok.service", 10, 0, 20)
	got, err = New(root, t.TempDir()).Services()
	if err != nil || len(got) != 1 || got[0].Name != "ok.service" {
		t.Fatalf("Services() = %+v, %v, want ok.service only", got, err)
	}
}

// TestSurveyCountsUnaccountedServices: beside accounted services, the
// cgroups without memory.stat are counted, so the guard can say they are not
// guarded.
func TestSurveyCountsUnaccountedServices(t *testing.T) {
	root := t.TempDir()
	unit(t, root, "system.slice/ok.service", 10, 0, 20)
	write(t, root, "system.slice/backup.service/cgroup.procs", "")
	write(t, root, "system.slice/apps.slice/worker.service/cgroup.procs", "")

	got, unaccounted, err := New(root, t.TempDir()).Survey()
	want := []string{"system.slice/apps.slice/worker.service", "system.slice/backup.service"}
	slices.Sort(unaccounted)
	if err != nil || len(got) != 1 || got[0].Name != "ok.service" || !slices.Equal(unaccounted, want) {
		t.Fatalf("Survey() = %+v, %v, %v; want ok.service and unaccounted %v", got, unaccounted, err, want)
	}
}

// TestNestedSliceGoneIsSkipped: a nested slice removed while the walk reads it
// is skipped without an error.
func TestNestedSliceGoneIsSkipped(t *testing.T) {
	w := walk{r: New(t.TempDir(), t.TempDir())}
	if err := w.nested("system.slice/gone.slice"); err != nil || len(w.out) != 0 || len(w.unaccounted) != 0 {
		t.Fatalf("nested(gone) = %v, out %+v, unaccounted %v; want skipped", err, w.out, w.unaccounted)
	}
}

// TestServicesUnreadableMemoryStat: a memory.stat that exists but cannot be
// read is an error, not a vanished cgroup.
func TestServicesUnreadableMemoryStat(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0 file")
	}
	root := t.TempDir()
	unit(t, root, "system.slice/locked.service", 10, 0, 20)
	if err := os.Chmod(filepath.Join(root, "system.slice", "locked.service", "memory.stat"), 0); err != nil {
		t.Fatal(err)
	}
	_, err := New(root, t.TempDir()).Services()
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Services() error = %v, want a permission error", err)
	}
}

func TestServicesReadError(t *testing.T) {
	root := t.TempDir()
	write(t, root, "system.slice/bad.service/memory.stat", "anon notanumber\nshmem 0\n")
	write(t, root, "system.slice/bad.service/memory.current", "1\n")
	if _, err := New(root, t.TempDir()).Services(); err == nil {
		t.Fatal("want error for malformed memory.stat")
	}

	root = t.TempDir()
	write(t, root, "system.slice/bad.service/memory.stat", "file 1\n")
	write(t, root, "system.slice/bad.service/memory.current", "1\n")
	if _, err := New(root, t.TempDir()).Services(); err == nil {
		t.Fatal("want error for missing anon")
	}
}

func TestServicesNoSystemSlice(t *testing.T) {
	_, err := New(t.TempDir(), t.TempDir()).Services()
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want wrapped ErrNotExist", err)
	}
}

func TestSelf(t *testing.T) {
	proc := t.TempDir()
	write(t, proc, "self/cgroup", "12:cpu:/x\n0::/system.slice/tgshd.service\n")
	got, err := New(t.TempDir(), proc).Self()
	if err != nil {
		t.Fatal(err)
	}
	if got != "system.slice/tgshd.service" {
		t.Fatalf("got %q", got)
	}

	write(t, proc, "self/cgroup", "1:cpu:/x\n")
	if _, err := New(t.TempDir(), proc).Self(); err == nil {
		t.Fatal("want error without 0:: line")
	}
	if _, err := New(t.TempDir(), t.TempDir()).Self(); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestKill(t *testing.T) {
	root := t.TempDir()
	write(t, root, "system.slice/a.service/cgroup.kill", "")
	r := New(root, t.TempDir())
	if err := r.Kill("system.slice/a.service"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "system.slice", "a.service", "cgroup.kill"))
	if err != nil || string(b) != "1" {
		t.Fatalf("got %q, %v", b, err)
	}
	for _, bad := range []string{"", "..", "../etc", "system.slice/../../x", "/", "."} {
		if err := r.Kill(bad); err == nil {
			t.Errorf("Kill(%q) must fail", bad)
		}
	}
	if err := r.Kill("system.slice/missing.service"); err == nil {
		t.Error("want error for missing cgroup")
	}
}

func TestIsGone(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"enoent", fmt.Errorf("read: %w", os.ErrNotExist), true},
		{"enodev", fmt.Errorf("read: %w", syscall.ENODEV), true},
		{"esrch", fmt.Errorf("read: %w", syscall.ESRCH), true},
		{"eacces", fmt.Errorf("read: %w", syscall.EACCES), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGone(tt.err); got != tt.want {
				t.Fatalf("isGone = %v, want %v", got, tt.want)
			}
		})
	}
}
