package lockfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
)

func flockFile(t *testing.T, path string, how int) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err = syscall.Flock(int(f.Fd()), how); err != nil {
		t.Fatal(err)
	}

	return f
}

func TestMaintenanceHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maint.lock")

	held, err := lockfile.MaintenanceHeld(path)
	if err != nil || held {
		t.Fatalf("missing file: held=%v err=%v", held, err)
	}

	f := flockFile(t, path, syscall.LOCK_SH)
	if held, err = lockfile.MaintenanceHeld(path); err != nil || held {
		t.Fatalf("shared lock: held=%v err=%v", held, err)
	}
	_ = f.Close()

	f2 := flockFile(t, path, syscall.LOCK_EX)
	if held, err = lockfile.MaintenanceHeld(path); err != nil || !held {
		t.Fatalf("exclusive lock: held=%v err=%v", held, err)
	}
	_ = f2.Close()

	// The probe must release at once: an exclusive try-lock afterwards succeeds.
	if held, err = lockfile.MaintenanceHeld(path); err != nil || held {
		t.Fatalf("after release: held=%v err=%v", held, err)
	}
	f3 := flockFile(t, path, syscall.LOCK_EX|syscall.LOCK_NB)
	_ = f3.Close()
}

func TestMaintenanceHeldError(t *testing.T) {
	// A path under a regular file yields ENOTDIR, which is not "missing".
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lockfile.MaintenanceHeld(filepath.Join(file, "x")); err == nil {
		t.Fatal("expected error")
	}
}

func TestInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "tgshd.lock")

	first, err := lockfile.AcquireInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lockfile.AcquireInstance(path); !errors.Is(err, lockfile.ErrAlreadyLocked) {
		t.Fatalf("second acquire err = %v, want ErrAlreadyLocked", err)
	}
	if err = first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := lockfile.AcquireInstance(path)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if err = second.Release(); err != nil {
		t.Fatal(err)
	}
	if err = second.Release(); err != nil {
		t.Fatalf("double release: %v", err)
	}
}

func TestParseHeartbeat(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    lockfile.Heartbeat
		wantErr bool
	}{
		{
			"ok",
			"ts=1700000000 planned_until=1700000600\n",
			lockfile.Heartbeat{TS: 1700000000, PlannedUntil: 1700000600},
			false,
		},
		{"none", "ts=5 planned_until=0\n", lockfile.Heartbeat{TS: 5}, false},
		{"empty", "", lockfile.Heartbeat{}, true},
		{"bad", "ts=x planned_until=0\n", lockfile.Heartbeat{}, true},
		{"missing field", "ts=5\n", lockfile.Heartbeat{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lockfile.ParseHeartbeat(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestHeartbeatFormat(t *testing.T) {
	got := lockfile.Heartbeat{TS: 10, PlannedUntil: 20}.String()
	if got != "ts=10 planned_until=20\n" {
		t.Fatalf("got %q", got)
	}
}

func TestHeartbeatTickPreservesAndExpiresPlanned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "heartbeat")
	now := time.Unix(1_700_000_000, 0)

	hb, err := lockfile.ReadHeartbeat(path)
	if err != nil || hb != (lockfile.Heartbeat{}) {
		t.Fatalf("missing: %+v %v", hb, err)
	}

	if err = lockfile.SetPlanned(path, now, 600); err != nil {
		t.Fatal(err)
	}
	if err = lockfile.WriteTick(path, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if hb, err = lockfile.ReadHeartbeat(path); err != nil {
		t.Fatal(err)
	}
	want := lockfile.Heartbeat{TS: now.Unix() + 30, PlannedUntil: now.Unix() + 600}
	if hb != want {
		t.Fatalf("got %+v, want %+v", hb, want)
	}

	// Once passed, the tick writes 0.
	if err = lockfile.WriteTick(path, now.Add(601*time.Second)); err != nil {
		t.Fatal(err)
	}
	if hb, _ = lockfile.ReadHeartbeat(path); hb.PlannedUntil != 0 || hb.TS != now.Unix()+601 {
		t.Fatalf("expired: %+v", hb)
	}

	// SetPlanned preserves ts; 0 clears.
	if err = lockfile.SetPlanned(path, now.Add(700*time.Second), 86400); err != nil {
		t.Fatal(err)
	}
	if hb, _ = lockfile.ReadHeartbeat(path); hb.TS != now.Unix()+601 || hb.PlannedUntil != now.Unix()+700+86400 {
		t.Fatalf("set: %+v", hb)
	}
	if err = lockfile.SetPlanned(path, now, 0); err != nil {
		t.Fatal(err)
	}
	if hb, _ = lockfile.ReadHeartbeat(path); hb.PlannedUntil != 0 {
		t.Fatalf("clear: %+v", hb)
	}
}

func TestSetPlannedOnAFreshFileWritesTheCurrentTime(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	missing := filepath.Join(t.TempDir(), "heartbeat")
	corrupt := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(corrupt, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": missing, "corrupt": corrupt} {
		if err := lockfile.SetPlanned(path, now, 600); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hb, err := lockfile.ReadHeartbeat(path)
		want := lockfile.Heartbeat{TS: now.Unix(), PlannedUntil: now.Unix() + 600}
		if err != nil || hb != want {
			t.Fatalf("%s: got %+v %v, want %+v", name, hb, err, want)
		}
	}
}

func TestSetPlannedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	for _, s := range []int{-1, 86401} {
		if err := lockfile.SetPlanned(path, time.Unix(1, 0), s); err == nil {
			t.Fatalf("seconds=%d: expected error", s)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file must not be created on invalid input")
	}
}

func TestHeartbeatFileModesAndNoTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	path := filepath.Join(dir, "heartbeat")
	if err := lockfile.WriteTick(path, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("file: %v %v", fi, err)
	}
	di, err := os.Stat(dir)
	if err != nil || di.Mode().Perm() != 0o755 {
		t.Fatalf("dir: %v %v", di, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "heartbeat" && e.Name() != "heartbeat.lock" {
			t.Fatalf("stray file %s", e.Name())
		}
	}
}

// TestHeartbeatReadErrorKeepsThePlannedWindow: a heartbeat that cannot be
// read (not missing, not corrupt) is not rewritten, so a live planned_until
// is never dropped; the error is returned for the caller to report.
func TestHeartbeatReadErrorKeepsThePlannedWindow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without read permission")
	}
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(path, []byte("ts=10 planned_until=1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}

	if err := lockfile.WriteTick(path, time.Unix(50, 0)); err == nil {
		t.Fatal("WriteTick must report an unreadable heartbeat")
	}
	if err := lockfile.SetPlanned(path, time.Unix(50, 0), 0); err == nil {
		t.Fatal("SetPlanned must report an unreadable heartbeat")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	hb, err := lockfile.ReadHeartbeat(path)
	if err != nil || hb.TS != 10 || hb.PlannedUntil != 1000 {
		t.Fatalf("heartbeat = %+v, %v; want it untouched", hb, err)
	}
}

func TestParseErrorsAreCorrupt(t *testing.T) {
	for _, s := range []string{"garbage", "ts=1", "ts=x planned_until=0"} {
		if _, err := lockfile.ParseHeartbeat(s); !errors.Is(err, lockfile.ErrCorrupt) {
			t.Errorf("%q: err = %v, want ErrCorrupt", s, err)
		}
	}
}

func TestHeartbeatCorruptTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "heartbeat")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lockfile.WriteTick(path, time.Unix(50, 0)); err != nil {
		t.Fatal(err)
	}
	hb, err := lockfile.ReadHeartbeat(path)
	if err != nil || hb.TS != 50 || hb.PlannedUntil != 0 {
		t.Fatalf("%+v %v", hb, err)
	}
}
