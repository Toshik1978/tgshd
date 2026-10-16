package execprobe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func needBin(t *testing.T, names ...string) {
	t.Helper()

	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not available", n)
		}
	}
}

func TestRun(t *testing.T) {
	needBin(t, "true", "false")

	ctx := context.Background()
	if code, err := (Runner{}).Run(ctx, []string{"true"}); code != 0 || err != nil {
		t.Fatalf("true: %d %v", code, err)
	}
	if code, err := (Runner{}).Run(ctx, []string{"false"}); code != 1 || err != nil {
		t.Fatalf("false: %d %v", code, err)
	}
	if _, err := (Runner{}).Run(ctx, []string{"/nonexistent/tgshd-probe"}); err == nil {
		t.Fatal("want start error")
	}
	if _, err := (Runner{}).Run(ctx, nil); err == nil {
		t.Fatal("want error for empty argv")
	}
}

func TestRunTimeout(t *testing.T) {
	needBin(t, "sleep")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := (Runner{}).Run(ctx, []string{"sleep", "5"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeoutKillsChildren(t *testing.T) {
	needBin(t, "sh", "sleep")

	pidFile := t.TempDir() + "/pid"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	script := "sleep 30 & echo $! > " + pidFile + "; wait"

	_, err := (Runner{}).Run(ctx, []string{"sh", "-c", script})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}

	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil {
		t.Fatal(perr)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("child %d survived the timeout", pid)
}
