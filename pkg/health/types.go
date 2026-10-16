package health

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
}

// Sample is one numeric metric value recorded for history.
type Sample struct {
	Metric string
	Value  float64
}

// CheckResult is everything one run of a check returns. A non-nil Err is
// reported as a checker error; Readings returned alongside it (a partial
// failure) are still judged, and conditions absent from them are untouched,
// so a run that failed outright returns no Readings and changes nothing.
type CheckResult struct {
	Readings []Reading
	Samples  []Sample
	Err      error
}
