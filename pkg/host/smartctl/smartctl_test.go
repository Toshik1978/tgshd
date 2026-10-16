package smartctl_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/host/smartctl"
)

type fakeDev struct {
	name      string
	device    bool
	removable string
	size      string
}

func buildTree(t *testing.T, devs []fakeDev) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range devs {
		dir := filepath.Join(root, d.name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if d.device {
			if err := os.Symlink("../../devices/example", filepath.Join(dir, "device")); err != nil {
				t.Fatal(err)
			}
		}
		if d.removable != "" {
			writeFile(t, filepath.Join(dir, "removable"), d.removable)
		}
		if d.size != "" {
			writeFile(t, filepath.Join(dir, "size"), d.size)
		}
	}

	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDrives(t *testing.T) {
	root := buildTree(t, []fakeDev{
		{"sda", true, "0\n", "1000\n"},
		{"nvme0n1", true, "0\n", "2000\n"},
		{"sdb", true, "1\n", "1000\n"},
		{"sdc", true, "0\n", "0\n"},
		{"sdd", false, "0\n", "1000\n"},
		{"loop0", true, "0\n", "1000\n"},
		{"ram0", true, "0\n", "1000\n"},
		{"zram0", true, "0\n", "1000\n"},
		{"dm-0", true, "0\n", "1000\n"},
		{"md0", true, "0\n", "1000\n"},
		{"sr0", true, "0\n", "1000\n"},
		{"sde", true, "", ""},
	})

	got, err := smartctl.New(root, "smartctl", time.Second).Drives()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, n := range got {
		names = append(names, n.Name)
	}
	want := []string{"nvme0n1", "sda", "sde"}
	if !slices.Equal(names, want) {
		t.Fatalf("Drives() = %v, want %v", names, want)
	}
}

func TestDrivesMissingRoot(t *testing.T) {
	_, err := smartctl.New(filepath.Join(t.TempDir(), "nope"), "smartctl", time.Second).Drives()
	if err == nil {
		t.Fatal("expected error")
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-smartctl")
	writeFile(t, path, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestRunReturnsOutputAndExitStatus(t *testing.T) {
	bin := script(t, `echo "$@"; exit 8`)
	out, exit, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda", false)
	if err != nil {
		t.Fatalf("non-zero exit must not be an error: %v", err)
	}
	if exit != 8 {
		t.Errorf("exit = %d, want 8", exit)
	}
	// -n standby,3: a sleeping drive is not spun up and gets its own status.
	if want := "--json=c -n standby,3 -a /dev/sda\n"; string(out) != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestRunStandbyIsAStatusNotAnError(t *testing.T) {
	bin := script(t, `echo '{"smartctl":{"exit_status":3}}'; exit 3`)
	_, exit, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda", false)
	if err != nil || exit != smartctl.StandbyStatus {
		t.Fatalf("got %d, %v; want status %d", exit, err, smartctl.StandbyStatus)
	}
}

func TestDefaultTimeoutIsWellUnderTheRunBudget(t *testing.T) {
	if smartctl.DefaultTimeout > 10*time.Second {
		t.Fatalf("DefaultTimeout = %v, want at most 10s per drive", smartctl.DefaultTimeout)
	}
}

func TestRunSuccess(t *testing.T) {
	bin := script(t, `echo '{}'`)
	out, exit, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda", false)
	if err != nil || exit != 0 || string(out) != "{}\n" {
		t.Fatalf("got %q, %d, %v", out, exit, err)
	}
}

func TestRunErrors(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		_, _, err := smartctl.New("", filepath.Join(t.TempDir(), "absent"), time.Second).
			Run(context.Background(), "sda", false)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		bin := script(t, `exec sleep 10`)
		start := time.Now()
		_, _, err := smartctl.New("", bin, 100*time.Millisecond).Run(context.Background(), "sda", false)
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if time.Since(start) > 5*time.Second {
			t.Error("timeout not enforced")
		}
	})
	t.Run("killed by signal", func(t *testing.T) {
		bin := script(t, `kill -9 $$`)
		if _, _, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda", false); err == nil {
			t.Fatal("expected error for signal-killed process")
		}
	})
	t.Run("bad node", func(t *testing.T) {
		for _, node := range []string{"", "../etc", "sda/x", "-a", ".", ".."} {
			if _, _, err := smartctl.New("", "true", time.Second).Run(context.Background(), node, false); err == nil {
				t.Errorf("node %q: expected error", node)
			}
		}
	})
}

// TestDevicePath: Path is the raw EvalSymlinks result of the block entry.
func TestDevicePath(t *testing.T) {
	base := t.TempDir()
	dev := filepath.Join(base, "devices", "pci0000:00", "sda")
	if err := os.MkdirAll(dev, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../example", filepath.Join(dev, "device")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dev, "removable"), "0\n")
	writeFile(t, filepath.Join(dev, "size"), "1000\n")
	root := filepath.Join(base, "block")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../devices/pci0000:00/sda", filepath.Join(root, "sda")); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dev)
	if err != nil {
		t.Fatal(err)
	}

	got, err := smartctl.New(root, "smartctl", time.Second).Drives()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "sda" || got[0].Path != want {
		t.Fatalf("Drives() = %+v, want sda at %q", got, want)
	}
}

// TestWakeFlag: wake switches -n standby,3 to -n never and swaps the
// Runner's timeout for ForcedTimeout.
func TestWakeFlag(t *testing.T) {
	bin := script(t, `echo "$@"`)
	r := smartctl.New("", bin, 5*time.Second)
	for _, tc := range []struct {
		wake bool
		want string
	}{
		{false, "--json=c -n standby,3 -a /dev/sda\n"},
		{true, "--json=c -n never -a /dev/sda\n"},
	} {
		out, _, err := r.Run(context.Background(), "sda", tc.wake)
		if err != nil || string(out) != tc.want {
			t.Errorf("wake=%v: got %q, %v; want %q", tc.wake, out, err, tc.want)
		}
	}

	slow := script(t, `sleep 0.5; echo '{}'`)
	short := smartctl.New("", slow, 100*time.Millisecond)
	if _, _, err := short.Run(context.Background(), "sda", false); err == nil {
		t.Error("wake=false: expected the Runner timeout to apply")
	}
	if _, _, err := short.Run(context.Background(), "sda", true); err != nil {
		t.Errorf("wake=true: ForcedTimeout must apply, got %v", err)
	}
	if smartctl.ForcedTimeout != 20*time.Second {
		t.Errorf("ForcedTimeout = %v, want 20s", smartctl.ForcedTimeout)
	}
}

// TestDrivesResolveFailure: a drive that vanishes between the listing and the
// resolution of its path is skipped; any other failure is still an error.
func TestDrivesResolveFailure(t *testing.T) {
	root := buildTree(t, []fakeDev{
		{"sda", true, "0\n", "1000\n"},
		{"sdb", true, "0\n", "1000\n"},
	})
	fail := func(err error) func(string) (string, error) {
		return func(p string) (string, error) {
			if filepath.Base(p) == "sda" {
				return "", err
			}

			return p, nil
		}
	}

	t.Run("vanished", func(t *testing.T) {
		r := smartctl.New(root, "smartctl", time.Second).WithEvalSymlinks(fail(fs.ErrNotExist))
		got, err := r.Drives()
		if err != nil {
			t.Fatalf("Drives() error = %v, want the vanished drive skipped", err)
		}
		if len(got) != 1 || got[0].Name != "sdb" {
			t.Fatalf("Drives() = %+v, want only sdb", got)
		}
	})
	t.Run("other error", func(t *testing.T) {
		r := smartctl.New(root, "smartctl", time.Second).WithEvalSymlinks(fail(fs.ErrPermission))
		if _, err := r.Drives(); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Drives() error = %v, want permission error", err)
		}
	})
}
