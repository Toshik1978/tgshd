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

// waitDead fails with msg (formatted with pid) unless the process is dead
// within 2 s. A killed child of a command that already exited is reparented
// to PID 1 and stays a zombie until PID 1 reaps it, which a busy host can
// put off for seconds; kill(pid, 0) still finds a zombie, so it counts as
// dead here.
func waitDead(t *testing.T, pid int, msg string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil || zombie(pid) {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf(msg, pid)
}

// zombie reports whether /proc shows pid as a zombie (state Z): dead, not
// yet reaped.
func zombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the command name, which is in parentheses and may
	// itself hold spaces or parentheses: "31685 (sleep) Z 1 …".
	line := string(stat)

	return strings.HasPrefix(line[strings.LastIndexByte(line, ')')+1:], " Z")
}

func TestRun(t *testing.T) {
	needBin(t, "true", "false")

	ctx := context.Background()
	if code, _, err := (Runner{}).Run(ctx, []string{"true"}); code != 0 || err != nil {
		t.Fatalf("true: %d %v", code, err)
	}
	if code, _, err := (Runner{}).Run(ctx, []string{"false"}); code != 1 || err != nil {
		t.Fatalf("false: %d %v", code, err)
	}
	if _, _, err := (Runner{}).Run(ctx, []string{"/nonexistent/tgshd-probe"}); err == nil {
		t.Fatal("want start error")
	}
	if _, _, err := (Runner{}).Run(ctx, nil); err == nil {
		t.Fatal("want error for empty argv")
	}
}

func TestRunTimeout(t *testing.T) {
	needBin(t, "sleep")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := (Runner{}).Run(ctx, []string{"sleep", "5"})
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

	_, _, err := (Runner{}).Run(ctx, []string{"sh", "-c", script})
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

	waitDead(t, pid, "child %d survived the timeout")
}

// TestBackgroundChildHoldingOutput: a command that exits while a background
// child still holds its output is judged by its own exit code and output,
// not reported as a start failure, and the leftover child is killed.
func TestBackgroundChildHoldingOutput(t *testing.T) {
	needBin(t, "sh", "sleep")

	pidFile := t.TempDir() + "/pid"
	script := "sleep 30 & echo $! > " + pidFile + "; echo out"

	start := time.Now()

	code, out, err := (Runner{}).Run(context.Background(), []string{"sh", "-c", script})
	if err != nil || code != 0 || string(out) != "out\n" {
		t.Fatalf("code=%d out=%q err=%v, want 0, \"out\\n\", nil", code, out, err)
	}

	if elapsed := time.Since(start); elapsed > WaitDelay+2*time.Second {
		t.Fatalf("returned after %v", elapsed)
	}

	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil {
		t.Fatal(perr)
	}

	waitDead(t, pid, "background child %d survived")
}

func TestRunCapturesOutput(t *testing.T) {
	needBin(t, "sh")

	code, out, err := (Runner{}).Run(context.Background(), []string{"sh", "-c", "echo out; echo err >&2; exit 3"})
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got := string(out)
	if !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
		t.Fatalf("output = %q", got)
	}
}

func TestOutputIsCapped(t *testing.T) {
	needBin(t, "sh", "head")

	code, out, err := (Runner{}).Run(context.Background(),
		[]string{"sh", "-c", "head -c 200000 /dev/zero | tr '\\0' x"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(out) != MaxOutput || MaxOutput != 64<<10 {
		t.Fatalf("len = %d, want %d", len(out), 64<<10)
	}
}
