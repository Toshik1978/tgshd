package modemprobe

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const phone = "+10000000000"

type fakeSender struct {
	err   error
	phone string
	texts []string
}

func (f *fakeSender) Publish(_ context.Context, p, msg string) error {
	f.phone = p
	if f.err != nil {
		return f.err
	}

	f.texts = append(f.texts, msg)

	return nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func (c *clock) add(d time.Duration) { c.now = c.now.Add(d) }

func lookupOK(string) (string, bool) { return phone, true }

func token(p *Probe) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.state.Token
}

// 2026-10-14 is a Wednesday; the probe is due on Sundays at 11:02.
func wednesday() *clock { return &clock{time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)} }

func sunday(h, m int) time.Time { return time.Date(2026, 10, 18, h, m, 0, 0, time.UTC) }

func newProbe(t *testing.T, s *fakeSender, c *clock) *Probe {
	t.Helper()

	cfg := config.ModemProbe{
		PhoneEnv: "MODEM_PHONE", Time: "11:02", Weekday: time.Sunday,
		ReplyWithin: config.Duration(30 * time.Minute), MaxSilence: config.Duration(8 * 24 * time.Hour),
	}

	p, err := New(cfg, s, lookupOK, c.Now)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func only(t *testing.T, r health.CheckResult) (health.Reading, bool) {
	t.Helper()

	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}

	switch len(r.Readings) {
	case 0:
		return health.Reading{}, false
	case 1:
		return r.Readings[0], true
	}

	t.Fatalf("want at most one reading, got %d", len(r.Readings))

	return health.Reading{}, false
}

func TestFirstRunDoesNotSendUntilScheduled(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)

	if _, ok := only(t, p.Run(t.Context())); ok || len(s.texts) != 0 {
		t.Fatalf("expected no reading and no send, sent %v", s.texts)
	}

	c.now = sunday(11, 1)
	p.Run(t.Context())

	if len(s.texts) != 0 {
		t.Fatal("sent before scheduled time")
	}

	c.now = sunday(11, 2)
	p.Run(t.Context())

	if len(s.texts) != 1 || s.phone != phone {
		t.Fatalf("want one send to the phone, got %v to %q", s.texts, s.phone)
	}

	tok := token(p)
	if len(tok) != 12 || !strings.Contains(s.texts[0], tok) {
		t.Fatalf("text %q lacks 12-hex token %q", s.texts[0], tok)
	}

	if s.texts[0] != probeText(tok) {
		t.Fatalf("unexpected probe text %q", s.texts[0])
	}

	c.add(time.Minute)
	p.Run(t.Context())

	if len(s.texts) != 1 {
		t.Fatal("sent twice in one week")
	}
}

func TestRoundTripMatchesGoodAndRecovers(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.now = sunday(11, 2)
	p.Run(t.Context())

	text := s.texts[0]

	if p.Match("+10000000001", text, c.now) {
		t.Fatal("matched foreign sender")
	}

	if p.Match(phone, text+"x", c.now) {
		t.Fatal("matched different text")
	}

	c.add(time.Minute)

	if !p.Match("10000000000", text, c.now) {
		t.Fatal("did not match own number")
	}

	r, ok := only(t, p.Run(t.Context()))
	if !ok || r.Verdict != health.Good || r.Key != Key {
		t.Fatalf("want Good, got %+v ok=%v", r, ok)
	}

	if _, ok := only(t, p.Run(t.Context())); ok {
		t.Fatal("Good repeated")
	}

	if p.Match(phone, text, c.now) {
		t.Fatal("matched an already returned probe")
	}
}

func TestNoReplyWithinWindowIsBadThenLateReplyResolves(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.now = sunday(11, 2)
	p.Run(t.Context())

	c.add(29 * time.Minute)

	if _, ok := only(t, p.Run(t.Context())); ok {
		t.Fatal("decided too early")
	}

	c.add(time.Minute)

	r, ok := only(t, p.Run(t.Context()))
	if !ok || r.Verdict != health.Bad {
		t.Fatalf("want Bad, got %+v", r)
	}

	if !p.Match(phone, s.texts[0], c.now) {
		t.Fatal("late reply not matched")
	}

	if r, ok = only(t, p.Run(t.Context())); !ok || r.Verdict != health.Good {
		t.Fatalf("want Good, got %+v", r)
	}
}

func TestSendFailureIsBadAtOnce(t *testing.T) {
	s, c := &fakeSender{err: errors.New("outbox down")}, wednesday()
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.now = sunday(11, 5)

	r, ok := only(t, p.Run(t.Context()))
	if !ok || r.Verdict != health.Bad || !strings.Contains(r.Text, "send") {
		t.Fatalf("want Bad about sending, got %+v", r)
	}

	if p.Match(phone, "anything", c.now) {
		t.Fatal("matched with no probe outstanding")
	}

	if _, ok := only(t, p.Run(t.Context())); ok {
		t.Fatal("send failure retried within the same week")
	}
}

func TestMaxSilenceCountedFromFirstRunWhenNeverReturned(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.add(8*24*time.Hour - time.Minute)

	if r, ok := only(t, p.Run(t.Context())); ok && r.Verdict == health.Bad && strings.Contains(r.Text, "days") {
		t.Fatalf("silent too early: %+v", r)
	}

	c.add(time.Minute)

	r, ok := only(t, p.Run(t.Context()))
	if !ok || r.Verdict != health.Bad {
		t.Fatalf("want Bad for silence, got %+v", r)
	}
}

func TestMaxSilenceCountedFromLastReturn(t *testing.T) {
	s, c := &fakeSender{}, &clock{sunday(11, 2)}
	p := newProbe(t, s, c)
	p.Run(t.Context()) // baseline only: first run on the scheduled minute

	c.add(7 * 24 * time.Hour)
	p.Run(t.Context())

	if len(s.texts) != 1 {
		t.Fatalf("want one probe, got %d", len(s.texts))
	}

	p.Match(phone, s.texts[0], c.now)
	p.Run(t.Context())

	c.add(8*24*time.Hour + time.Minute)

	if r, ok := only(t, p.Run(t.Context())); !ok || r.Verdict != health.Bad {
		t.Fatalf("want Bad, got %+v ok=%v", r, ok)
	}
}

func TestMissingPhoneIsCheckerError(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)
	p.lookup = func(string) (string, bool) { return "", false }

	r := p.Run(t.Context())
	if r.Err == nil || len(r.Readings) != 0 {
		t.Fatalf("want error and no readings, got %+v", r)
	}

	if strings.Contains(r.Err.Error(), phone) {
		t.Fatal("error leaks the number")
	}

	if p.Match(phone, "x", c.now) {
		t.Fatal("matched without a configured number")
	}
}

func TestSamePhone(t *testing.T) {
	tests := []struct {
		name, own, from string
		want            bool
	}{
		{"same", "+10000000000", "+10000000000", true},
		{"formatting ignored", "+1 (000) 000-0000", "10000000000", true},
		{"different length last nine", "+10000000000", "0000000000", true},
		{"different length mismatch", "+10000000000", "0000000001", false},
		{"same length mismatch", "+10000000000", "+10000000001", false},
		{"too short", "+10000000000", "00000", false},
		{"empty", "+10000000000", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := samePhone(tc.own, tc.from); got != tc.want {
				t.Fatalf("samePhone(%q,%q)=%v", tc.own, tc.from, got)
			}
		})
	}
}

func TestExportRestore(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.now = sunday(11, 2)
	p.Run(t.Context())

	data, err := p.Export()
	if err != nil {
		t.Fatal(err)
	}

	q := newProbe(t, &fakeSender{}, c)
	if err := q.Restore(data); err != nil {
		t.Fatal(err)
	}

	if !q.Match(phone, s.texts[0], c.now) {
		t.Fatal("restored probe does not match")
	}

	if err := q.Restore([]byte("{")); err == nil {
		t.Fatal("want error for bad JSON")
	}

	if err := q.Restore(nil); err != nil {
		t.Fatalf("empty restore should be a no-op: %v", err)
	}
}

func TestNewRejectsBadTime(t *testing.T) {
	if _, err := New(config.ModemProbe{Time: "nope"}, &fakeSender{}, lookupOK, time.Now); err == nil {
		t.Fatal("want error")
	}
}

func TestMatchWithTimestampBeforeSentAt(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.now = sunday(11, 2)
	p.Run(t.Context())

	c.add(time.Minute)

	if !p.Match(phone, s.texts[0], sunday(9, 0)) {
		t.Fatal("skewed timestamp must still match")
	}

	if p.Match(phone, s.texts[0], sunday(9, 0)) {
		t.Fatal("replay matched again")
	}

	c.add(time.Hour)

	r, ok := only(t, p.Run(t.Context()))
	if !ok || r.Verdict != health.Good {
		t.Fatalf("want Good, got %+v ok=%v", r, ok)
	}
}

func TestConcurrentMatchAndRun(t *testing.T) {
	s, c := &fakeSender{}, &clock{sunday(11, 2)}
	p := newProbe(t, s, c)
	p.Run(t.Context())

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			for range 50 {
				p.Run(t.Context())
			}
		})

		wg.Go(func() {
			for range 50 {
				p.Match(phone, probeText(token(p)), c.now)
			}
		})
	}

	wg.Wait()
}

func TestFromModem(t *testing.T) {
	p := newProbe(t, &fakeSender{}, wednesday())
	if !p.FromModem("10000000000") || p.FromModem("+10000000001") {
		t.Fatal("FromModem must compare the sender with the modem's own number")
	}

	p.lookup = func(string) (string, bool) { return "", false }
	if p.FromModem(phone) {
		t.Fatal("without a configured number nothing comes from the modem")
	}
}

func newReportOnly(t *testing.T, c *clock) *Probe {
	t.Helper()

	cfg := config.ModemProbe{
		PhoneEnv: "MODEM_PHONE", Time: "11:02", Weekday: time.Sunday,
		ReplyWithin: config.Duration(30 * time.Minute), MaxSilence: config.Duration(8 * 24 * time.Hour),
	}

	p, err := New(cfg, nil, lookupOK, c.Now)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestReportOnlyNeverSendsAndDescribesTheState(t *testing.T) {
	c := wednesday()
	p := newReportOnly(t, c)

	r, ok := only(t, p.Run(t.Context()))
	if !ok || r.Key != Key || r.Verdict != health.Unknown || !strings.Contains(r.Text, "No modem probe has come back") {
		t.Fatalf("first run: %+v ok=%v", r, ok)
	}

	c.now = sunday(11, 2)

	r, ok = only(t, p.Run(t.Context()))
	if !ok || r.Verdict != health.Unknown || !strings.Contains(r.Text, "due") ||
		!strings.Contains(r.Text, "not sent") {
		t.Fatalf("due run: %+v ok=%v", r, ok)
	}

	if tok := token(p); tok != "" {
		t.Fatalf("report-only allocated a token %q", tok)
	}
}

func restoreJSON(t *testing.T, p *Probe, st State) {
	t.Helper()

	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Restore(data); err != nil {
		t.Fatal(err)
	}
}

func TestReportOnlyJudgesTheRestoredState(t *testing.T) {
	c := wednesday()

	outstanding := newReportOnly(t, c)
	restoreJSON(t, outstanding, State{
		SentAt: c.now.Add(-time.Minute), Token: "0123456789ab", FirstMissingAt: c.now.Add(-time.Minute),
	})

	r, ok := only(t, outstanding.Run(t.Context()))
	if !ok || r.Verdict != health.Unknown || !strings.Contains(r.Text, "waiting for its reply") {
		t.Fatalf("outstanding: %+v ok=%v", r, ok)
	}

	timedOut := newReportOnly(t, c)
	restoreJSON(t, timedOut, State{SentAt: c.now.Add(-time.Hour), Token: "0123456789ab"})

	if r, ok = only(t, timedOut.Run(t.Context())); !ok || r.Verdict != health.Bad {
		t.Fatalf("timed out: %+v ok=%v", r, ok)
	}

	returned := newReportOnly(t, c)
	restoreJSON(t, returned, State{SentAt: c.now.Add(-2 * time.Hour), LastOKAt: c.now.Add(-time.Hour)})

	if r, ok = only(t, returned.Run(t.Context())); !ok || r.Verdict != health.Good {
		t.Fatalf("returned: %+v ok=%v", r, ok)
	}

	if r, ok = only(t, returned.Run(t.Context())); !ok || r.Verdict != health.Unknown ||
		!strings.Contains(r.Text, "last came back") {
		t.Fatalf("returned, again: %+v ok=%v", r, ok)
	}
}

// returnedProbe runs one full round trip, reports its Good and leaves the
// clock a minute after the return.
func returnedProbe(t *testing.T, c *clock) (*Probe, *fakeSender) {
	t.Helper()

	s := &fakeSender{}
	p := newProbe(t, s, c)
	p.Run(t.Context())

	c.now = sunday(11, 2)
	p.Run(t.Context())
	c.add(time.Minute)

	if !p.Match(phone, s.texts[0], c.now) {
		t.Fatal("probe did not match")
	}

	if r, ok := only(t, p.Run(t.Context())); !ok || r.Verdict != health.Good {
		t.Fatalf("want Good, got %+v", r)
	}

	return p, s
}

func restarted(t *testing.T, p *Probe, s Sender, c *clock) *Probe {
	t.Helper()

	data, err := p.Export()
	if err != nil {
		t.Fatal(err)
	}

	q := newProbe(t, &fakeSender{}, c)
	q.sender = s

	if err := q.Restore(data); err != nil {
		t.Fatal(err)
	}

	return q
}

func TestRestartAfterSendFailureDoesNotResolve(t *testing.T) {
	c := wednesday()
	p, _ := returnedProbe(t, c)

	failing := &fakeSender{err: errors.New("outbox down")}
	p.sender = failing
	c.now = sunday(11, 2).AddDate(0, 0, 7)

	if r, ok := only(t, p.Run(t.Context())); !ok || r.Verdict != health.Bad {
		t.Fatalf("want Bad, got %+v", r)
	}

	q := restarted(t, p, failing, c)

	c.add(time.Minute)

	if r, ok := only(t, q.Run(t.Context())); ok {
		t.Fatalf("restart resolved the open problem: %+v", r)
	}

	// A new probe that returns clears the problem.
	good := &fakeSender{}
	q.sender = good
	c.now = sunday(11, 2).AddDate(0, 0, 14)
	q.Run(t.Context())
	c.add(time.Minute)

	if !q.Match(phone, good.texts[0], c.now) {
		t.Fatal("new probe did not match")
	}

	if r, ok := only(t, q.Run(t.Context())); !ok || r.Verdict != health.Good {
		t.Fatalf("want Good, got %+v", r)
	}
}

func TestRestartAfterTimeoutDoesNotResolve(t *testing.T) {
	c := wednesday()
	p, s := returnedProbe(t, c)

	c.now = sunday(11, 2).AddDate(0, 0, 7)
	p.Run(t.Context())
	c.add(time.Hour)

	if r, ok := only(t, p.Run(t.Context())); !ok || r.Verdict != health.Bad {
		t.Fatalf("want Bad, got %+v", r)
	}

	q := restarted(t, p, s, c)

	c.add(time.Minute)

	if r, ok := only(t, q.Run(t.Context())); ok && r.Verdict == health.Good {
		t.Fatalf("restart resolved the open problem: %+v", r)
	}
}

func TestRestartAfterGoodDoesNotRepeatIt(t *testing.T) {
	c := wednesday()
	p, s := returnedProbe(t, c)
	q := restarted(t, p, s, c)

	c.add(time.Minute)

	if r, ok := only(t, q.Run(t.Context())); ok {
		t.Fatalf("Good repeated after restart: %+v", r)
	}
}

// TestStatus: Status is the probe's state for the weekly summary, the last
// round trip and whether a probe is outstanding, and shares nothing with it.
func TestStatus(t *testing.T) {
	s, c := &fakeSender{}, wednesday()
	p := newProbe(t, s, c)

	if st := p.Status(); !st.LastOKAt.IsZero() || st.Outstanding() {
		t.Fatalf("fresh status = %+v, want never returned and nothing outstanding", st)
	}

	p.Run(t.Context())
	c.now = sunday(11, 2)
	p.Run(t.Context())

	if st := p.Status(); !st.Outstanding() || !st.LastOKAt.IsZero() {
		t.Fatalf("status after send = %+v, want outstanding", st)
	}

	c.add(time.Minute)
	if !p.Match(phone, s.texts[0], c.now) {
		t.Fatal("did not match")
	}

	st := p.Status()
	if st.Outstanding() || !st.LastOKAt.Equal(c.now) {
		t.Fatalf("status after return = %+v, want last round trip %v and nothing outstanding", st, c.now)
	}

	st.LastOKAt = time.Time{}
	if p.Status().LastOKAt.IsZero() {
		t.Fatal("Status shares its state with the probe")
	}
}
