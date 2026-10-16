// Package smartctl discovers physical drives via sysfs and runs smartctl against them.
package smartctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// ForcedTimeout bounds a run that wakes the drive (wake=true): a
	// spinning-up disk needs far longer than DefaultTimeout.
	ForcedTimeout = 20 * time.Second
)

// ErrInvalidNode is returned when a device node name is not a plain block device name.
var ErrInvalidNode = errors.New("invalid device node name")

// skipPrefixes lists virtual or optical device name prefixes that are never monitored.
func skipPrefixes() []string {
	return []string{"loop", "ram", "zram", "dm-", "md", "sr"}
}

// Node is a physical drive: its device node name and the raw resolved sysfs
// path of /sys/block/<Name>. The path is not judged here.
type Node struct {
	Name string
	Path string
}

// Runner lists physical drives and runs smartctl for them.
type Runner struct {
	sysBlock string
	binary   string
	timeout  time.Duration
	// evalSymlinks resolves a sysfs entry; filepath.EvalSymlinks unless a test replaces it.
	evalSymlinks func(string) (string, error)
}

// New creates a Runner over the given sysfs block directory and smartctl binary.
func New(sysBlock, binary string, timeout time.Duration) *Runner {
	return &Runner{sysBlock: sysBlock, binary: binary, timeout: timeout, evalSymlinks: filepath.EvalSymlinks}
}

// NewDefault creates a Runner over the real /sys/block and the smartctl found in PATH.
func NewDefault() *Runner {
	return New(defaultSysBlock, defaultBinary, DefaultTimeout)
}

// Drives returns the nodes, sorted by name (for example "sda", "nvme0n1"), of
// physical, non-removable drives that have media. Each carries the raw
// filepath.EvalSymlinks result of its sysfs block entry.
func (r *Runner) Drives() ([]Node, error) {
	entries, err := os.ReadDir(r.sysBlock)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.sysBlock, err)
	}
	var nodes []Node
	for _, e := range entries {
		if !r.isPhysicalDrive(e.Name()) {
			continue
		}
		path, err := r.evalSymlinks(filepath.Join(r.sysBlock, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			// The drive vanished since it was listed (hot unplug).
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", e.Name(), err)
		}
		nodes = append(nodes, Node{Name: e.Name(), Path: path})
	}
	slices.SortFunc(nodes, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })

	return nodes, nil
}

// Run executes `smartctl --json=c -n standby,3 -a /dev/<node>` and returns its stdout and
// exit status. With wake the standby check is off (`-n never`: the drive is
// spun up and read) and the run is bounded by ForcedTimeout instead of the
// Runner's own timeout. smartctl's exit status is a bitmask, so a non-zero status is not an error;
// only a failure to execute the binary or a timeout is. A drive in standby is not spun up:
// the run exits with StandbyStatus.
func (r *Runner) Run(ctx context.Context, node string, wake bool) ([]byte, int, error) {
	if node == "" || strings.HasPrefix(node, "-") || strings.ContainsAny(node, `/\`) || node == "." || node == ".." {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidNode, node)
	}
	timeout, power := r.modes(wake)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext( //nolint:gosec // binary is configured, node validated, no shell
		ctx,
		r.binary,
		"--json=c",
		"-n", power,
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

// modes returns the run's timeout and its -n argument.
func (r *Runner) modes(wake bool) (time.Duration, string) {
	if wake {
		return ForcedTimeout, "never"
	}

	return r.timeout, "standby," + strconv.Itoa(StandbyStatus)
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
