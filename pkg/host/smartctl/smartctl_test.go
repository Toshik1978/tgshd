package smartctl_test

import (
	"context"
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
	want := []string{"nvme0n1", "sda", "sde"}
	if !slices.Equal(got, want) {
		t.Fatalf("Drives() = %v, want %v", got, want)
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
	out, exit, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda")
	if err != nil {
		t.Fatalf("non-zero exit must not be an error: %v", err)
	}
	if exit != 8 {
		t.Errorf("exit = %d, want 8", exit)
	}
	if want := "--json=c -a /dev/sda\n"; string(out) != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestRunSuccess(t *testing.T) {
	bin := script(t, `echo '{}'`)
	out, exit, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda")
	if err != nil || exit != 0 || string(out) != "{}\n" {
		t.Fatalf("got %q, %d, %v", out, exit, err)
	}
}

func TestRunErrors(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		_, _, err := smartctl.New("", filepath.Join(t.TempDir(), "absent"), time.Second).
			Run(context.Background(), "sda")
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		bin := script(t, `exec sleep 10`)
		start := time.Now()
		_, _, err := smartctl.New("", bin, 100*time.Millisecond).Run(context.Background(), "sda")
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if time.Since(start) > 5*time.Second {
			t.Error("timeout not enforced")
		}
	})
	t.Run("killed by signal", func(t *testing.T) {
		bin := script(t, `kill -9 $$`)
		if _, _, err := smartctl.New("", bin, 5*time.Second).Run(context.Background(), "sda"); err == nil {
			t.Fatal("expected error for signal-killed process")
		}
	})
	t.Run("bad node", func(t *testing.T) {
		for _, node := range []string{"", "../etc", "sda/x", "-a", ".", ".."} {
			if _, _, err := smartctl.New("", "true", time.Second).Run(context.Background(), node); err == nil {
				t.Errorf("node %q: expected error", node)
			}
		}
	})
}
