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
// this run (for example the internet while the router is down) is omitted
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
	// DependsOn is a condition key; the check is skipped while it is bad.
	DependsOn string
	// Run performs one run of the check.
	Run func(ctx context.Context) CheckResult
}
