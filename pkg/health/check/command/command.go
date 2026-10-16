// Package command implements the custom-command health check: a probe
// command judged by its exit status, with an optional fix action.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const fixWindow = time.Hour

// Runner executes an argv without a shell and returns its exit code and its
// combined stdout and stderr (capped by the adapter). A non-nil error means
// the command did not run to completion: it could not be started, or ctx
// ended (the adapter returns the context error then).
type Runner interface {
	Run(ctx context.Context, argv []string) (exitCode int, output []byte, err error)
}

// Docker is the container side of the fix action.
type Docker interface {
	Restart(ctx context.Context, name string) error
	StartedAt(ctx context.Context, name string) (time.Time, error)
}

// FixRecord is one fix attempt. The runner persists every record as an alert
// of kind "fix"; it pushes the text to Telegram only when Send is true
// (report: alert). These records are not Event readings so the alert log gets
// kind "fix" and a journal-only fix is never sent.
type FixRecord struct {
	At   time.Time
	Key  string
	Text string
	Send bool
}

// Check is one configured command check.
type Check struct {
	cfg    config.Command
	runner Runner
	docker Docker
	now    func() time.Time
	log    *zap.Logger

	// expect and forbid are the compiled output patterns (nil when unset);
	// patternErr is a compile failure, reported as a checker error per run.
	expect, forbid *regexp.Regexp
	patternErr     error

	// dryRun reports fixes without taking them (check --once).
	dryRun bool

	mu       sync.Mutex
	strikes  int
	fixes    []time.Time
	lastFix  time.Time
	inFlight bool
	pending  []FixRecord
}

// Option configures a Check.
type Option func(*Check)

// WithDryRun makes the fix action a report: when a fix is due it is recorded
// as "would restart container X" or "would run X" and neither Docker nor the
// fix command is called. The probe and the start-time read still run, so the
// decision is the one the daemon would take.
func WithDryRun() Option {
	return func(c *Check) { c.dryRun = true }
}

// New builds the check. docker may be nil (the container runtime may be
// missing at start); a container fix then fails at run time with a checker
// error. log may be nil.
func New(
	cfg config.Command, runner Runner, docker Docker, now func() time.Time, log *zap.Logger, opts ...Option,
) *Check {
	if log == nil {
		log = zap.NewNop()
	}
	c := &Check{cfg: cfg, runner: runner, docker: docker, now: now, log: log}
	c.expect, c.forbid, c.patternErr = compilePatterns(cfg)
	for _, o := range opts {
		o(c)
	}

	return c
}

// compilePatterns compiles expect_output and forbid_output. The config loader
// validates them, so a failure here means a hand-built config.
func compilePatterns(cfg config.Command) (expect, forbid *regexp.Regexp, err error) {
	if cfg.ExpectOutput != "" {
		if expect, err = regexp.Compile(cfg.ExpectOutput); err != nil {
			return nil, nil, fmt.Errorf("command %q: expect_output: %w", cfg.Name, err)
		}
	}
	if cfg.ForbidOutput != "" {
		if forbid, err = regexp.Compile(cfg.ForbidOutput); err != nil {
			return nil, nil, fmt.Errorf("command %q: forbid_output: %w", cfg.Name, err)
		}
	}

	return expect, forbid, nil
}

// TakeFixes returns and clears the fix attempts made since the last call.
func (c *Check) TakeFixes() []FixRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.pending
	c.pending = nil
	return out
}

type state struct {
	Strikes int         `json:"strikes"`
	Fixes   []time.Time `json:"fixes,omitempty"`
	LastFix time.Time   `json:"last_fix,omitzero"`
}

// Export serialises the in-memory state for the key-value store.
func (c *Check) Export() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(state{Strikes: c.strikes, Fixes: c.fixes, LastFix: c.lastFix})
	if err != nil {
		return nil, fmt.Errorf("export command state: %w", err)
	}

	return data, nil
}

// Restore loads state written by Export. Empty data is a no-op.
func (c *Check) Restore(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("restore command state: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.strikes, c.fixes, c.lastFix = s.Strikes, s.Fixes, s.LastFix

	return nil
}

// Run executes the probe and, when configured, the fix action.
//
// The verdict order is: a probe that cannot be started (missing binary,
// permission denied) is a configuration problem and a checker error with no
// readings; a timeout is judged per timeout_verdict (broken: Bad, the guarded
// thing is hung; unreadable: Unknown, with the fix strike count unchanged); a
// non-zero exit is Bad; output matching forbid_output is Bad; output not
// matching expect_output is Bad; else Good. Texts name the pattern key, never
// the output, which may hold secrets.
//
// The probe gets its timeout, but ends before the run's deadline (see
// health.Bound): a probe that hangs past what is left of the run is judged
// like a timeout, so the run is never abandoned with its reading.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	if c.patternErr != nil {
		return health.CheckResult{Err: c.patternErr}
	}
	timeout := c.cfg.Timeout.Duration()
	pctx, cancel, limit := health.Bound(ctx, timeout)
	code, output, err := c.runner.Run(pctx, c.cfg.Run)
	timedOut := ctx.Err() == nil &&
		(errors.Is(pctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded))
	cancel()

	var text string
	switch {
	case timedOut && c.cfg.TimeoutVerdict == config.TimeoutUnreadable:
		// No strike, no fix: the probe could not be read, not judged.
		return c.unreadableTimeout(timeout, limit)
	case timedOut:
		text = timeoutText(timeout, limit)
	case err != nil:
		return health.CheckResult{Err: fmt.Errorf("run probe %q: %w", c.cfg.Name, err)}
	case code != 0:
		text = fmt.Sprintf("probe exited with status %d", code)
	default:
		text = c.judgeOutput(output)
	}

	return c.record(ctx, text)
}

// record turns the probe's Bad text ("" for Good) into the reading, counts
// the strike and takes the fix when it is due.
func (c *Check) record(ctx context.Context, text string) health.CheckResult {
	c.mu.Lock()
	if text == "" {
		c.strikes = 0
		c.mu.Unlock()

		return health.CheckResult{Readings: []health.Reading{{Key: c.key(), Verdict: health.Good}}}
	}
	c.strikes++
	c.mu.Unlock()

	res := health.CheckResult{Readings: []health.Reading{{
		Key: c.key(), Verdict: health.Bad, Text: fmt.Sprintf("command %s: %s", c.cfg.Name, text),
	}}}
	res.Err = c.maybeFix(ctx)

	return res
}

// unreadableTimeout is the result of a timeout under timeout_verdict
// unreadable: an Unknown reading plus a checker error.
func (c *Check) unreadableTimeout(timeout, limit time.Duration) health.CheckResult {
	text := fmt.Sprintf("command %s: %s", c.cfg.Name, timeoutText(timeout, limit))

	return health.CheckResult{
		Readings: []health.Reading{{Key: c.key(), Verdict: health.Unknown, Text: text}},
		Err:      fmt.Errorf("%s: %w", text, context.DeadlineExceeded),
	}
}

func timeoutText(timeout, limit time.Duration) string {
	if limit < timeout {
		return "probe did not finish before the run deadline"
	}

	return fmt.Sprintf("probe timed out after %s", timeout)
}

// judgeOutput returns the Bad text for output that matches forbid_output or
// misses expect_output, or "" when the output is acceptable.
func (c *Check) judgeOutput(output []byte) string {
	if c.forbid != nil && c.forbid.Match(output) {
		return "output matched forbid_output"
	}
	if c.expect != nil && !c.expect.Match(output) {
		return "output did not match expect_output"
	}

	return ""
}

// key is the condition key of the probe.
func (c *Check) key() string { return "command/" + c.cfg.Name }

// maybeFix runs the fix when the strike count, rate limit and grace allow it.
// The lock is not held while the fix action runs.
func (c *Check) maybeFix(ctx context.Context) error {
	fix := c.cfg.Fix
	if fix == nil {
		return nil
	}

	grace := fix.GraceAfterStart.Duration()
	now := c.now()

	saved, ok := c.reserveFix(now, grace)
	if !ok {
		return nil
	}

	if fix.RestartContainer != "" {
		if c.docker == nil {
			c.abortFix(now, saved)

			return errors.New("container runtime unavailable")
		}

		started, err := c.docker.StartedAt(ctx, fix.RestartContainer)
		if err != nil {
			c.abortFix(now, saved)

			return fmt.Errorf("start time of %q: %w", fix.RestartContainer, err)
		}

		if !started.IsZero() && now.Sub(started) < grace {
			c.log.Debug(
				"fix skipped: target started recently",
				zap.String("check", c.cfg.Name),
				zap.Time("started", started),
			)
			c.abortFix(now, saved)

			return nil
		}
	}

	outcome := c.doFix(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.inFlight = false
	c.lastFix = c.now()
	c.pending = append(c.pending, FixRecord{
		At: now, Key: c.key() + "/fix", Send: c.cfg.Report != config.ReportJournal,
		Text: fmt.Sprintf("command %s: fix %s", c.cfg.Name, outcome),
	})

	return nil
}

// reserveFix applies the strike, in-flight, rate-limit and (for commands)
// grace rules. When the fix may run it reserves it under the lock: the fix
// time is recorded and the strikes are taken (returned as saved), so an
// overlapping Run cannot start a second fix.
func (c *Check) reserveFix(now time.Time, grace time.Duration) (int, bool) {
	fix := c.cfg.Fix

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.strikes < fix.After {
		return 0, false
	}

	if c.inFlight {
		c.log.Debug("fix skipped: another fix is in flight", zap.String("check", c.cfg.Name))

		return 0, false
	}

	c.prune(now)

	if len(c.fixes) >= fix.MaxPerHour {
		c.log.Debug(
			"fix skipped: hourly limit reached",
			zap.String("check", c.cfg.Name),
			zap.Int("max_per_hour", fix.MaxPerHour),
		)

		return 0, false
	}

	if fix.RestartContainer == "" && !c.lastFix.IsZero() && now.Sub(c.lastFix) < grace {
		c.log.Debug("fix skipped: within grace of the last fix", zap.String("check", c.cfg.Name))

		return 0, false
	}

	saved := c.strikes
	c.strikes = 0
	c.inFlight = true
	c.fixes = append(c.fixes, now)

	return saved, true
}

// abortFix releases a reservation when the fix did not run.
func (c *Check) abortFix(now time.Time, saved int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.inFlight = false
	c.strikes += saved

	for i, v := range slices.Backward(c.fixes) {
		if v.Equal(now) {
			c.fixes = append(c.fixes[:i], c.fixes[i+1:]...)

			break
		}
	}
}

func (c *Check) doFix(ctx context.Context) string {
	fix := c.cfg.Fix
	if c.dryRun {
		if fix.RestartContainer != "" {
			return "would restart container " + fix.RestartContainer
		}

		// Only the program: its arguments may carry secrets.
		return "would run " + fix.Run[0]
	}
	// Bounded like the probe, so the fix record is returned with the run.
	fctx, cancel, _ := health.Bound(ctx, c.cfg.Timeout.Duration())
	defer cancel()
	if fix.RestartContainer != "" {
		if err := c.docker.Restart(fctx, fix.RestartContainer); err != nil {
			return fmt.Sprintf("failed to restart container %s: %v", fix.RestartContainer, err)
		}
		return "restarted container " + fix.RestartContainer
	}
	code, _, err := c.runner.Run(fctx, fix.Run)
	switch {
	case err != nil:
		return fmt.Sprintf("failed to run %s: %v", fix.Run[0], err)
	case code != 0:
		return fmt.Sprintf("ran %s, exit status %d", fix.Run[0], code)
	}

	return "ran " + fix.Run[0]
}

// prune drops fix times that left the sliding hour.
func (c *Check) prune(now time.Time) {
	kept := c.fixes[:0]
	for _, t := range c.fixes {
		if now.Sub(t) < fixWindow {
			kept = append(kept, t)
		}
	}
	c.fixes = kept
}
