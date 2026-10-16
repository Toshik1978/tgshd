// Package modemprobe implements the weekly modem round-trip probe: it sends
// a text to the modem's own number and expects the inbox worker to hand the
// reply back through Match.
package modemprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

// Key is the single condition this check judges.
const Key = "modem_probe/roundtrip"

const (
	tokenBytes  = 6 // 12 hex characters
	matchDigits = 9
	probePrefix = "tgshd probe "
	day         = 24 * time.Hour
	daysPerWeek = 7
)

// Sender sends an SMS; it matches command.SmsSender.
type Sender interface {
	Publish(ctx context.Context, phone, msg string) error
}

// LookupEnv resolves an environment variable; os.LookupEnv satisfies it.
type LookupEnv func(string) (string, bool)

// State is the persisted probe state (data-model "ProbeState"). Zero times
// mean "unset".
type State struct {
	SentAt         time.Time `json:"sent_at"`
	Token          string    `json:"token"`
	LastOKAt       time.Time `json:"last_ok_at"`
	FirstMissingAt time.Time `json:"first_missing_at"`
	// BadAt is when the check last decided Bad; a Good needs a return that
	// is not older than this, so a restart never resolves an open problem.
	BadAt time.Time `json:"bad_at,omitzero"`
	// GoodForOKAt is the LastOKAt value whose Good was already decided.
	GoodForOKAt time.Time `json:"good_for_ok_at,omitzero"`
}

// Outstanding reports whether a probe was sent and has not returned yet.
func (s State) Outstanding() bool {
	return s.Token != "" && !s.SentAt.IsZero()
}

// Probe is the modem round-trip check. It is safe for concurrent use: Match
// is called from the inbox worker and Run from the runner.
type Probe struct {
	sender      Sender
	lookup      LookupEnv
	now         func() time.Time
	phoneEnv    string
	weekday     time.Weekday
	hour, min   int
	replyWithin time.Duration
	maxSilence  time.Duration

	mu    sync.Mutex
	state State
}

// New builds the probe. lookup resolves environment variable names (use
// os.LookupEnv) and now supplies the clock. A nil sender makes the probe
// report-only (tgshd check --once): it never sends a probe and describes the
// state it would otherwise leave undecided.
func New(cfg config.ModemProbe, sender Sender, lookup LookupEnv, now func() time.Time) (*Probe, error) {
	at, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return nil, fmt.Errorf("parse modem probe time %q: %w", cfg.Time, err)
	}

	return &Probe{
		sender:      sender,
		lookup:      lookup,
		now:         now,
		phoneEnv:    cfg.PhoneEnv,
		weekday:     cfg.Weekday,
		hour:        at.Hour(),
		min:         at.Minute(),
		replyWithin: cfg.ReplyWithin.Duration(),
		maxSilence:  cfg.MaxSilence.Duration(),
	}, nil
}

// Export serialises the probe state for the key-value store.
func (p *Probe) Export() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := json.Marshal(p.state)
	if err != nil {
		return nil, fmt.Errorf("marshal probe state: %w", err)
	}

	return data, nil
}

// Status returns a copy of the probe state, for the weekly summary's last
// round trip and outstanding probe.
func (p *Probe) Status() State {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.state
}

// Restore loads state produced by Export. Empty input is a no-op.
func (p *Probe) Restore(data []byte) error {
	if len(data) == 0 {
		return nil
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("unmarshal probe state: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.state = st

	return nil
}

// FromModem reports whether sender is the modem's own number, compared as
// Match compares it; false while the number is not configured.
func (p *Probe) FromModem(sender string) bool {
	phone, ok := p.lookup(p.phoneEnv)

	return ok && phone != "" && samePhone(phone, sender)
}

// Match reports whether an incoming message is the reply to the outstanding
// probe: sent by the modem's own number and carrying the exact probe text. A
// match marks the probe as returned.
func (p *Probe) Match(sender, text string, _ time.Time) bool {
	phone, ok := p.lookup(p.phoneEnv)
	if !ok || phone == "" {
		return false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.outstanding() || text != probeText(p.state.Token) || !samePhone(phone, sender) {
		return false
	}

	p.state.LastOKAt = p.now() // host clock: the message timestamp may be skewed
	p.state.Token = ""
	p.state.FirstMissingAt = time.Time{}

	return true
}

// Run advances the probe: it sends a due probe and judges the round trip.
// It returns no reading while nothing is decided.
func (p *Probe) Run(ctx context.Context) health.CheckResult {
	phone, ok := p.lookup(p.phoneEnv)
	if !ok || phone == "" {
		return health.CheckResult{Err: fmt.Errorf("modem probe phone: %w", errNoPhone)}
	}

	now := p.now()

	timedOut, token, prev := p.prepare(now)

	sendFailed := false

	if token != "" {
		if err := p.sender.Publish(ctx, phone, probeText(token)); err != nil {
			// Undo the send so the slot stays due and the next run retries
			// it, instead of waiting a week with the problem open.
			p.mu.Lock()
			p.state.SentAt, p.state.Token = prev.SentAt, prev.Token
			p.mu.Unlock()

			// A send cut short by the run's context (shutdown) says nothing
			// about the modem: the run failed, it is not Bad.
			if ctx.Err() != nil {
				return health.CheckResult{Err: fmt.Errorf("send modem probe: %w", err)}
			}

			sendFailed = true
		}
	}

	res := p.judge(now, timedOut, sendFailed)
	if p.sender == nil && len(res.Readings) == 0 {
		res.Readings = []health.Reading{p.pending(now)}
	}

	return res
}

// pending describes an undecided state for a report-only run.
func (p *Probe) pending(now time.Time) health.Reading {
	p.mu.Lock()
	defer p.mu.Unlock()

	slot := p.lastSlot(now)
	next := "; next scheduled " + slot.AddDate(0, 0, daysPerWeek).Format(time.RFC3339)

	var text string

	switch {
	case p.outstanding():
		text = "Modem probe sent at " + p.state.SentAt.Format(time.RFC3339) + " is waiting for its reply"
	case p.due(now):
		text = "A modem probe has been due since " + slot.Format(time.RFC3339) + "; report-only mode has not sent it"
	case !p.state.LastOKAt.IsZero():
		text = "Modem probe last came back at " + p.state.LastOKAt.Format(time.RFC3339) + next
	default:
		text = "No modem probe has come back yet" + next
	}

	return health.Reading{Key: Key, Verdict: health.Unknown, Text: text}
}

// prepare records the first run, detects a timed-out probe and, when a slot is
// due, allocates the new probe's token (empty when nothing is to be sent). It
// also returns the state before the new probe, to undo a failed send.
func (p *Probe) prepare(now time.Time) (bool, string, State) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state.SentAt.IsZero() && p.state.LastOKAt.IsZero() && p.state.FirstMissingAt.IsZero() {
		p.state.FirstMissingAt = now
	}

	timedOut := p.outstanding() && now.Sub(p.state.SentAt) >= p.replyWithin
	if timedOut && p.state.FirstMissingAt.IsZero() {
		p.state.FirstMissingAt = p.state.SentAt
	}

	prev := p.state

	if p.sender == nil || !p.due(now) {
		return timedOut, "", prev
	}

	token := newToken()
	p.state.SentAt = now
	p.state.Token = token

	return timedOut, token, prev
}

var errNoPhone = errors.New("phone number is not configured")

func (p *Probe) judge(now time.Time, timedOut, sendFailed bool) health.CheckResult {
	p.mu.Lock()
	defer p.mu.Unlock()

	bad := func(text string) health.CheckResult {
		p.state.BadAt = now

		return health.CheckResult{Readings: []health.Reading{{Key: Key, Verdict: health.Bad, Text: text}}}
	}

	switch {
	case sendFailed:
		return bad("Modem probe could not be sent: the SMS send failed")
	case timedOut:
		return bad(fmt.Sprintf("Modem probe did not come back within %s", p.replyWithin))
	}

	if ref := p.silenceRef(); !ref.IsZero() && now.Sub(ref) >= p.maxSilence {
		return bad(fmt.Sprintf("No modem probe has come back for %d days", int(now.Sub(ref)/day)))
	}

	if p.goodDue() {
		p.state.GoodForOKAt = p.state.LastOKAt

		return health.CheckResult{Readings: []health.Reading{{
			Key: Key, Verdict: health.Good, Text: "Modem probe came back",
		}}}
	}

	return health.CheckResult{}
}

// goodDue reports whether a return newer than the last Bad (and not yet
// reported) is waiting to be judged Good. Callers hold p.mu.
func (p *Probe) goodDue() bool {
	st := p.state

	return !st.LastOKAt.IsZero() && !p.outstanding() &&
		!st.LastOKAt.Equal(st.GoodForOKAt) && !st.LastOKAt.Before(st.BadAt)
}

// outstanding reports whether a probe was sent and has not returned yet.
// Callers hold p.mu.
func (p *Probe) outstanding() bool {
	return p.state.Outstanding()
}

// silenceRef is the moment silence is counted from. Callers hold p.mu.
func (p *Probe) silenceRef() time.Time {
	if !p.state.LastOKAt.IsZero() {
		return p.state.LastOKAt
	}

	return p.state.FirstMissingAt
}

// due reports whether a scheduled slot has passed since the last send (or
// since the first run when nothing was ever sent). Callers hold p.mu.
func (p *Probe) due(now time.Time) bool {
	ref := p.state.SentAt
	if ref.IsZero() {
		ref = p.state.FirstMissingAt
	}

	return p.lastSlot(now).After(ref)
}

// lastSlot is the most recent weekday/time slot not after now.
func (p *Probe) lastSlot(now time.Time) time.Time {
	back := (int(now.Weekday()) - int(p.weekday) + 7) % 7
	slot := time.Date(now.Year(), now.Month(), now.Day()-back, p.hour, p.min, 0, 0, now.Location())

	if slot.After(now) {
		slot = time.Date(slot.Year(), slot.Month(), slot.Day()-7, p.hour, p.min, 0, 0, now.Location())
	}

	return slot
}

func probeText(token string) string { return probePrefix + token }

func newToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // never fails on supported platforms

	return hex.EncodeToString(b)
}

// samePhone compares numbers by digits; when the digit counts differ it
// compares the last nine digits, requiring both numbers to have that many.
func samePhone(a, b string) bool {
	da, db := digits(a), digits(b)

	if len(da) == len(db) {
		return da != "" && da == db
	}

	if len(da) < matchDigits || len(db) < matchDigits {
		return false
	}

	return da[len(da)-matchDigits:] == db[len(db)-matchDigits:]
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}

		return -1
	}, s)
}
