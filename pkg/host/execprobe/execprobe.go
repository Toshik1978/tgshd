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

// Runner runs commands directly (no shell) and discards their output.
type Runner struct{}

// Run executes argv and returns its exit code. A command that exits non-zero
// is not an error. When ctx ends the context error is returned; a command
// that cannot be started returns the start error.
func (Runner) Run(ctx context.Context, argv []string) (int, error) {
	if len(argv) == 0 {
		return -1, errors.New("empty command")
	}
	//nolint:gosec // argv is operator configuration and runs without a shell.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Own process group, so a timeout kills wrapper scripts' children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return killGroup(cmd.Process)
	}
	cmd.WaitDelay = WaitDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, fmt.Errorf("run %s: %w", argv[0], ctxErr)
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	}

	return -1, fmt.Errorf("start %s: %w", argv[0], err)
}

// killGroup sends SIGKILL to the whole process group of p.
func killGroup(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process group %d: %w", p.Pid, err)
	}

	return nil
}
