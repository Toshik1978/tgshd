package health

import (
	"context"
	"time"
)

// Check is one configured check as the runner schedules it (research R2).
//
// Run performs one run and must honour ctx. Err is reported as a checker
// error keyed "<name>/run/error"; any Readings returned with it (a partial
// failure) are still applied, and conditions absent from them are untouched,
// so a run that failed outright returns no readings. A State reading with
// Verdict Unknown changes no condition and is reported as a checker error
// keyed "<reading key>/unreadable", so a condition that is deliberately not judged
// this run (for example a disk in standby) is omitted
// rather than returned as Unknown.
type Check struct {
	// Name is the check name, the first segment of its condition keys.
	Name string
	// Interval is how often the check runs.
	Interval time.Duration
	// Threshold is how long a condition must be bad before a PROBLEM.
	Threshold time.Duration
	// Mode is the default delivery mode of the check's messages.
	Mode Mode
	// NeedsInternet skips the check while the outside is known down.
	NeedsInternet bool
	// PauseInQuiet holds the check back during quiet windows (maintenance and
	// boot grace); a check without it keeps running through them.
	PauseInQuiet bool
	// DependsOn is a condition key; the check is skipped while it is bad.
	DependsOn string
	// Run performs one run of the check.
	Run func(ctx context.Context) CheckResult
}

// CheckError is a checker error that carries its own key, so a run can raise
// one checker error per distinct problem instead of one per check (FR-028,
// research R6). Checks join them into CheckResult.Err.
//
// Subject is a drive serial, sensor key, controller, channel, or a fixed
// component name such as "discovery" or "kmsg"; it is never message text.
// It may be empty for per-entry checks whose name already names the subject.
// Kind matches [a-z0-9-]+, for example "no-verdict" or "unreadable".
type CheckError struct {
	Subject string
	Kind    string
	Err     error
}

// Key returns the checker-error key "<check>/<Subject>/<Kind>", or
// "<check>/<Kind>" when Subject is empty.
func (e CheckError) Key(check string) string {
	if e.Subject == "" {
		return check + "/" + e.Kind
	}
	return check + "/" + e.Subject + "/" + e.Kind
}

// Error implements error. It never includes the check name.
func (e CheckError) Error() string {
	id := e.Kind
	if e.Subject != "" {
		id = e.Subject + "/" + e.Kind
	}
	if e.Err == nil {
		return id
	}

	return id + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e CheckError) Unwrap() error { return e.Err }

// CheckErrors walks err (errors.Join trees, fmt.Errorf %w wrappers and
// multi-%w errors) depth first and returns every CheckError found, in order.
// It does not descend into a CheckError, so a CheckError wrapping another is
// reported once. Errors that are not CheckErrors are ignored; SplitErrors
// returns them too.
func CheckErrors(err error) []CheckError {
	checks, _ := SplitErrors(err)
	return checks
}

// SplitErrors walks err like CheckErrors and also returns the plain errors:
// the outermost subtrees that hold no CheckError, whole and unchanged so their
// wrapping text is kept. Callers key the plain ones with their own fallback,
// so no error is dropped.
func SplitErrors(err error) (checks []CheckError, plain []error) {
	splitErrors(err, &checks, &plain)
	return checks, plain
}

func splitErrors(err error, checks *[]CheckError, plain *[]error) {
	switch e := err.(type) { //nolint:errorlint // this is the tree walker, it unwraps by hand
	case nil:
		return
	case CheckError:
		*checks = append(*checks, e)
		return
	}
	before := len(*checks)
	switch e := err.(type) { //nolint:errorlint // this is the tree walker, it unwraps by hand
	case interface{ Unwrap() []error }:
		var sub []error
		for _, c := range e.Unwrap() {
			splitErrors(c, checks, &sub)
		}
		if len(*checks) == before {
			*plain = append(*plain, err)
			return
		}
		*plain = append(*plain, sub...)
	case interface{ Unwrap() error }:
		var sub []error
		splitErrors(e.Unwrap(), checks, &sub)
		if len(*checks) == before {
			*plain = append(*plain, err)
			return
		}
		*plain = append(*plain, sub...)
	default:
		*plain = append(*plain, err)
	}
}
