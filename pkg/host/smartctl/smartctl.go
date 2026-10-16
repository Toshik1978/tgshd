// Package smartctl discovers physical drives via sysfs and runs smartctl against them.
package smartctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// WaitDelay bounds how long Run waits for I/O after the process is
	// killed. health.Bound ends the disks reads min(health.RunMargin, a
	// tenth of the time left) before the run's deadline, so a read cut there
	// still returns in time for the check to report it when at least about
	// 10×WaitDelay of the run was left at bound time; with less left the
	// margin is shorter than WaitDelay.
	WaitDelay       = time.Second
	defaultSysBlock = "/sys/block"
	defaultBinary   = "smartctl"
	// DefaultTimeout bounds a single smartctl run (one drive), well under the
	// health runner's 40 s run budget, so a slow drive costs only its own
	// result.
	DefaultTimeout = 10 * time.Second
	// StandbyStatus is the exit status of a run that found the drive in a
	// low-power mode (sleep or standby) and did not read it: Run passes
	// `-n standby,3` so the drive is not spun up and the case gets a status of
	// its own (smartctl's default, 2, also means the open failed).
	StandbyStatus = 3
)

// ErrInvalidNode is returned when a device node name is not a plain block device name.
var ErrInvalidNode = errors.New("invalid device node name")

// skipPrefixes lists virtual or optical device name prefixes that are never monitored.
func skipPrefixes() []string {
	return []string{"loop", "ram", "zram", "dm-", "md", "sr"}
}

// Runner lists physical drives and runs smartctl for them.
type Runner struct {
	sysBlock string
	binary   string
	timeout  time.Duration
}

// New creates a Runner over the given sysfs block directory and smartctl binary.
func New(sysBlock, binary string, timeout time.Duration) *Runner {
	return &Runner{sysBlock: sysBlock, binary: binary, timeout: timeout}
}

// NewDefault creates a Runner over the real /sys/block and the smartctl found in PATH.
func NewDefault() *Runner {
	return New(defaultSysBlock, defaultBinary, DefaultTimeout)
}

// Drives returns the sorted node names (for example "sda", "nvme0n1") of physical,
// non-removable drives that have media.
func (r *Runner) Drives() ([]string, error) {
	entries, err := os.ReadDir(r.sysBlock)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.sysBlock, err)
	}
	var nodes []string
	for _, e := range entries {
		if r.isPhysicalDrive(e.Name()) {
			nodes = append(nodes, e.Name())
		}
	}
	slices.Sort(nodes)

	return nodes, nil
}

// Run executes `smartctl --json=c -n standby,3 -a /dev/<node>` and returns its stdout and
// exit status. smartctl's exit status is a bitmask, so a non-zero status is not an error;
// only a failure to execute the binary or a timeout is. A drive in standby is not spun up:
// the run exits with StandbyStatus.
func (r *Runner) Run(ctx context.Context, node string) ([]byte, int, error) {
	if node == "" || strings.HasPrefix(node, "-") || strings.ContainsAny(node, `/\`) || node == "." || node == ".." {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidNode, node)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext( //nolint:gosec // binary is configured, node validated, no shell
		ctx,
		r.binary,
		"--json=c",
		"-n", "standby,"+strconv.Itoa(StandbyStatus),
		"-a",
		"/dev/"+node,
	)
	cmd.Stdout = &stdout
	cmd.WaitDelay = WaitDelay
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, fmt.Errorf("run %s for %s: %w", r.binary, node, ctxErr)
	}
	if err == nil {
		return stdout.Bytes(), 0, nil
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		code := exitErr.ExitCode()
		if code < 0 {
			return nil, 0, fmt.Errorf("run %s for %s: terminated abnormally: %w", r.binary, node, err)
		}

		return stdout.Bytes(), code, nil
	}

	return nil, 0, fmt.Errorf("run %s for %s: %w", r.binary, node, err)
}

func (r *Runner) isPhysicalDrive(name string) bool {
	for _, p := range skipPrefixes() {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	dir := filepath.Join(r.sysBlock, name)
	if _, err := os.Lstat(filepath.Join(dir, "device")); err != nil {
		return false
	}
	if readTrim(filepath.Join(dir, "removable")) == "1" {
		return false
	}

	return readTrim(filepath.Join(dir, "size")) != "0"
}

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
