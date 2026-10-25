// Package systemctl restarts and stops systemd services by running systemctl.
package systemctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBinary = "systemctl"

	// waitDelay bounds how long a run waits for I/O after systemctl is killed.
	waitDelay = time.Second
	// maxStderr bounds the stderr text kept for error reports.
	maxStderr = 4096
)

// ErrInvalidUnit is returned, before anything is executed, for a name that is not a service unit.
var ErrInvalidUnit = errors.New("invalid service unit name")

// A leading "-" is refused on top of the pattern, so a name can never look like an option.
var unitPattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`)

// runner runs argv and returns the (bounded) stderr text.
type runner func(ctx context.Context, argv []string) (string, error)

// Client runs systemctl.
type Client struct {
	binary string
	run    runner
}

// New creates a Client over the given systemctl binary.
func New(binary string) *Client {
	return &Client{binary: binary, run: execRun}
}

// NewDefault creates a Client over the systemctl found in PATH.
func NewDefault() *Client {
	return New(defaultBinary)
}

// Restart restarts the service unit. It is bounded by ctx only.
func (c *Client) Restart(ctx context.Context, unit string) error {
	return c.do(ctx, "restart", unit)
}

// Stop stops the service unit. It is bounded by ctx only.
func (c *Client) Stop(ctx context.Context, unit string) error {
	return c.do(ctx, "stop", unit)
}

func (c *Client) do(ctx context.Context, verb, unit string) error {
	if strings.HasPrefix(unit, "-") || !unitPattern.MatchString(unit) {
		return fmt.Errorf("%w: %q", ErrInvalidUnit, unit)
	}
	argv := []string{c.binary, "--no-ask-password", verb, "--", unit}
	stderr, err := c.run(ctx, argv)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		if msg := strings.TrimSpace(stderr[:min(len(stderr), maxStderr)]); msg != "" {
			return fmt.Errorf("systemctl %s %s: %w: %s", verb, unit, err, msg)
		}

		return fmt.Errorf("systemctl %s %s: %w", verb, unit, err)
	}

	return nil
}

// execRun runs argv without a shell and returns its stderr.
func execRun(ctx context.Context, argv []string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // binary is configured; the unit is validated
	cmd.WaitDelay = waitDelay
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	return stderr.String(), err
}

// limitedBuffer keeps the first maxStderr bytes written to it.
type limitedBuffer struct{ buf bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxStderr - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}

	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
