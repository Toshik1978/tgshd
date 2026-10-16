// Package execprobe runs argv commands for the command health check.
package execprobe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay is a safety net: the whole process group is killed on cancel, so
// this only bounds the wait if something still holds the output pipes.
// health.Bound ends a probe min(health.RunMargin, a tenth of the time left)
// before the run's deadline, so a probe cut there still returns in time to
// be judged, not abandoned, when at least about 10×WaitDelay of the run was
// left at bound time; with less left the margin is shorter than WaitDelay.
const WaitDelay = time.Second

// MaxOutput caps the captured output; the excess is discarded.
const MaxOutput = 64 << 10

// Runner runs commands directly (no shell) and captures their output.
type Runner struct{}

// capWriter keeps the first MaxOutput bytes and silently drops the rest, so
// the child never blocks on a full pipe.
type capWriter struct{ buf []byte }

func (w *capWriter) Write(p []byte) (int, error) {
	if room := MaxOutput - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}

	return len(p), nil
}

// Run executes argv and returns its exit code and its stdout and stderr
// together (at most MaxOutput bytes; never logged here). A command that exits
// non-zero is not an error. When ctx ends the context error is returned; a
// command that cannot be started returns the start error. A command whose
// background child holds the output past WaitDelay keeps its own exit code
// and the output captured so far, and that child's process group is killed.
func (Runner) Run(ctx context.Context, argv []string) (int, []byte, error) {
	if len(argv) == 0 {
		return -1, nil, errors.New("empty command")
	}
	//nolint:gosec // argv is operator configuration and runs without a shell.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Own process group, so a timeout kills wrapper scripts' children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killGroup(cmd.Process)
	}
	cmd.WaitDelay = WaitDelay
	out := &capWriter{}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, nil, fmt.Errorf("run %s: %w", argv[0], ctxErr)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, out.buf, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), out.buf, nil
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The command exited, but a background child it left kept the output
		// open past WaitDelay. Judge the command itself and kill the leftover
		// group, which would otherwise pile up run after run.
		_ = killGroup(cmd.Process)

		return cmd.ProcessState.ExitCode(), out.buf, nil
	}

	return -1, nil, fmt.Errorf("start %s: %w", argv[0], err)
}

// killGroup sends SIGKILL to the whole process group of p.
func killGroup(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process group %d: %w", p.Pid, err)
	}

	return nil
}
