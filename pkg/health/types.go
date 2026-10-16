package health

import "time"

// Verdict is the judgement a check reaches about one condition in one run.
type Verdict int

// Verdicts a reading can carry.
const (
	// Unknown means the condition could not be read. It is the zero value so
	// a reading that forgets its verdict can never resolve an open problem.
	Unknown Verdict = iota
	// Good means the condition is healthy.
	Good
	// Bad means the condition is violated.
	Bad
)

// Kind tells how a reading is treated by the engine.
type Kind int

// Reading kinds.
const (
	// State readings follow the threshold rule.
	State Kind = iota
	// Event readings produce one message per occurrence and keep no state.
	Event
)

// Mode is the delivery channel override of a reading.
type Mode string

// Delivery modes. ModeDefault means "use the check's mode".
const (
	ModeDefault       Mode = ""
	ModeTelegram      Mode = "tlg"
	ModeSMS           Mode = "sms"
	ModeTelegramOrSMS Mode = "tlg-or-sms"
)

// Reading is one judgement a check produces for one condition in one run.
type Reading struct {
	Key     string
	Verdict Verdict
	Text    string
	Mode    Mode
	Kind    Kind
	// Since, when set, is when the check itself saw the violation begin. A
	// check that withholds a Bad reading until its own threshold passes sets
	// it so the PROBLEM states the real start of the streak. The engine uses
	// it only when it first tracks the condition, and ignores a future time.
	Since time.Time
}

// The plausible physical range of a temperature reading in °C, shared by the
// checks that read temperatures. A value outside it is a reading fault, not
// heat.
const (
	MinPlausibleCelsius = -40.0
	MaxPlausibleCelsius = 150.0
)

// Sample is one numeric metric value recorded for history.
type Sample struct {
	Metric string
	Value  float64
}

// CheckResult is everything one run of a check returns. A non-nil Err is
// reported as a checker error; Readings returned alongside it (a partial
// failure) are still judged, and conditions absent from them are untouched,
// so a run that failed outright returns no Readings and changes nothing.
// Forget lists condition keys to clear silently once the readings are
// applied, as /clear does: no message is sent and the alert log records the
// clear. A check uses it for a subject that is gone for good. The runner
// honours it only for the check's own conditions, and also when Err is set.
type CheckResult struct {
	Readings []Reading
	Forget   []string
	Samples  []Sample
	Err      error
}
