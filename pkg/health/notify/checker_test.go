package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

type fakeMailer struct {
	subjects []string
	bodies   []string
	fail     bool
	calls    int
}

func (f *fakeMailer) Send(_ context.Context, subject, body string) error {
	f.calls++
	if f.fail {
		return errors.New("relay down")
	}

	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, body)

	return nil
}

type checkerFixture struct {
	ce  *CheckerErrors
	tlg *fakeSender
	// tlgAd records the route of every Telegram send.
	tlgAd *telegramAdapter
	mail  *fakeMailer
	logs  *observer.ObservedLogs
	now   time.Time
}

func newCheckerFixture(t *testing.T, withMail bool) *checkerFixture {
	t.Helper()

	cf := &checkerFixture{tlg: &fakeSender{}, mail: &fakeMailer{}, now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	core, logs := observer.New(zap.WarnLevel)
	cf.logs = logs
	cf.tlgAd = asTelegram(cf.tlg)

	opts := CheckerOptions{
		Host:     "host1.example.org",
		Notifier: New(Options{Host: "host1.example.org", Telegram: cf.tlgAd}),
		Marks:    health.NewRateMarks(),
		Logger:   zap.New(core),
		Now:      func() time.Time { return cf.now },
	}

	if withMail {
		opts.Mailer = cf.mail
	}

	cf.ce = NewCheckerErrors(opts)

	return cf
}

func TestReportCheckLabelsWithTheCheckName(t *testing.T) {
	cf := newCheckerFixture(t, true)

	cf.ce.ReportCheck("http/web-app", "http/web-app/run/error", "run not started")

	if len(cf.tlg.sent) != 1 || cf.tlg.sent[0] != "host1.example.org: CHECKER ERROR: http/web-app: run not started" {
		t.Fatalf("telegram = %q", cf.tlg.sent)
	}

	if len(cf.mail.subjects) != 1 || cf.mail.subjects[0] != "host1.example.org: CHECKER ERROR: http/web-app" {
		t.Fatalf("subject = %q", cf.mail.subjects)
	}

	if alerts := cf.ce.Drain(); len(alerts) != 1 || alerts[0].Check != "http/web-app" {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestCheckerErrorSendsAndRecords(t *testing.T) {
	cf := newCheckerFixture(t, true)

	cf.ce.CheckerError("smart/sda/read", "a<b")

	if len(cf.tlg.sent) != 1 || cf.tlg.sent[0] != "host1.example.org: CHECKER ERROR: smart: a&lt;b" {
		t.Fatalf("telegram = %q", cf.tlg.sent)
	}

	if len(cf.mail.bodies) != 1 ||
		!strings.Contains(cf.mail.bodies[0], "host1.example.org: CHECKER ERROR: smart: a<b") {
		t.Fatalf("mail = %q", cf.mail.bodies)
	}

	if len(cf.mail.subjects) != 1 || !strings.Contains(cf.mail.subjects[0], "CHECKER ERROR") {
		t.Fatalf("subject = %q", cf.mail.subjects)
	}

	alerts := cf.ce.Drain()
	if len(alerts) != 1 || alerts[0].Check != "smart" || alerts[0].Key != "smart/sda/read" ||
		alerts[0].Text != "a<b" || !alerts[0].At.Equal(cf.now) {
		t.Fatalf("alerts = %+v", alerts)
	}

	if len(cf.ce.Drain()) != 0 {
		t.Fatal("drain must empty the list")
	}
}

func TestCheckerErrorRateLimited(t *testing.T) {
	cf := newCheckerFixture(t, true)

	cf.ce.CheckerError("smart/sda/read", "x")
	cf.now = cf.now.Add(23 * time.Hour)
	cf.ce.CheckerError("smart/sda/read", "x")
	cf.ce.CheckerError("smart/sdb/read", "y")

	if len(cf.tlg.sent) != 2 || len(cf.mail.bodies) != 2 {
		t.Fatalf("tlg=%d mail=%d, want 2 each", len(cf.tlg.sent), len(cf.mail.bodies))
	}

	if got := len(cf.logs.All()); got != 3 {
		t.Fatalf("logged %d, want 3 (always logged)", got)
	}

	cf.now = cf.now.Add(2 * time.Hour)
	cf.ce.CheckerError("smart/sda/read", "x")

	if len(cf.tlg.sent) != 3 {
		t.Fatalf("telegram after 24h = %d, want 3", len(cf.tlg.sent))
	}

	if got := len(cf.ce.Drain()); got != 3 {
		t.Fatalf("recorded %d, want 3 (only sent ones)", got)
	}
}

func TestCheckerErrorWithoutMail(t *testing.T) {
	cf := newCheckerFixture(t, false)

	cf.ce.CheckerError("disk/root/read", "boom")

	if len(cf.tlg.sent) != 1 {
		t.Fatalf("telegram = %q", cf.tlg.sent)
	}
}

func TestCheckerErrorMailFailureKeepsTelegram(t *testing.T) {
	cf := newCheckerFixture(t, true)
	cf.mail.fail = true

	cf.ce.CheckerError("disk/root/read", "boom")

	if len(cf.tlg.sent) != 1 {
		t.Fatalf("telegram = %q", cf.tlg.sent)
	}

	if len(cf.logs.FilterMessageSnippet("email").All()) == 0 {
		t.Fatal("email failure must be logged")
	}
}

func TestReportTellsWhetherTheCheckerErrorWentOut(t *testing.T) {
	cf := newCheckerFixture(t, false)

	if !cf.ce.Report("health/store/write", "cannot save state") {
		t.Fatal("first report = false, want sent")
	}
	if cf.ce.Report("health/store/write", "cannot save state") {
		t.Fatal("rate-limited report = true, want not sent")
	}
}

// TestFailedEmailIsNotReportedAsSent: an email that was attempted and failed
// is not a delivery, so a checker error that reached no channel is neither
// reported as sent nor recorded; its rate mark is kept, so the relay is not
// retried on every call.
func TestFailedEmailIsNotReportedAsSent(t *testing.T) {
	mail := &fakeMailer{fail: true}
	ce := NewCheckerErrors(CheckerOptions{
		Host:   "host1.example.org",
		Mailer: mail,
		Marks:  health.NewRateMarks(),
		Now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	})

	if ce.Report("health/store/write", "cannot save state") {
		t.Fatal("Report = true, want not sent: the only channel failed")
	}
	if got := ce.Drain(); len(got) != 0 {
		t.Fatalf("recorded %+v, want nothing for an undelivered error", got)
	}
	ce.Report("health/store/write", "cannot save state")
	if mail.calls != 1 {
		t.Fatalf("email attempted %d times, want the rate mark kept after a failed attempt", mail.calls)
	}
}

// TestRecovered: the CHECKER OK notice goes by Telegram, and by email only
// when the checker error went by email; the key's rate marks are given back,
// so a new error is reported at once.
func TestRecovered(t *testing.T) {
	tests := []struct {
		name     string
		withMail bool
		mailFail bool
		emails   int
	}{
		{"telegram only", false, false, 0},
		{"error went by email", true, false, 2},
		{"error email refused", true, true, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newCheckerFixture(t, tt.withMail)
			cf.mail.fail = tt.mailFail
			cf.ce.Report("health/store/write", "cannot save state")
			cf.mail.fail = false

			cf.ce.Recovered("health/store/write", "state can be saved again")

			if got := cf.tlgAd.sentRoutes(); len(got) != 2 ||
				got[1] != (route.Route{Topic: route.TopicAlerts, Silent: true}) {
				t.Fatalf("routes = %v, want the CHECKER OK on alerts/silent", got)
			}

			const ok = "host1.example.org: CHECKER OK: health: state can be saved again"
			if len(cf.tlg.sent) != 2 || cf.tlg.sent[1] != ok {
				t.Fatalf("telegram = %q, want the notice after the error", cf.tlg.sent)
			}
			if len(cf.mail.bodies) != tt.emails {
				t.Fatalf("emails = %q, want %d", cf.mail.bodies, tt.emails)
			}
			if tt.emails > 0 &&
				(cf.mail.bodies[1] != ok || cf.mail.subjects[1] != "host1.example.org: CHECKER OK: health") {
				t.Fatalf("email = %q / %q", cf.mail.subjects[1], cf.mail.bodies[1])
			}
			for _, m := range cf.ce.opts.Marks.Export() {
				if m.Key == "health/store/write" {
					t.Fatalf("rate mark %+v kept", m)
				}
			}
			if !cf.ce.Report("health/store/write", "cannot save state") {
				t.Fatal("a new error after the recovery was not sent at once")
			}
		})
	}
}

func TestCheckerErrorKeyWithoutSlash(t *testing.T) {
	cf := newCheckerFixture(t, false)

	cf.ce.CheckerError("config", "bad entry")

	if len(cf.tlg.sent) != 1 || !strings.Contains(cf.tlg.sent[0], "CHECKER ERROR: config: bad entry") {
		t.Fatalf("telegram = %q", cf.tlg.sent)
	}
}

// failingFixture is CheckerErrors over a notifier whose Telegram fails, both
// on one clock the test moves, in a host zone two hours east of UTC.
type failingFixture struct {
	ce   *CheckerErrors
	n    *Notifier
	tlg  *fakeSender
	logs *observer.ObservedLogs
	now  time.Time
}

func newFailingFixture(mailer Mailer) *failingFixture {
	ff := &failingFixture{tlg: &fakeSender{fail: true}, now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	clock := func() time.Time { return ff.now }
	core, logs := observer.New(zap.WarnLevel)
	ff.logs = logs
	ff.n = New(Options{Host: "host1.example.org", Telegram: asTelegram(ff.tlg), Now: clock})
	ff.ce = NewCheckerErrors(CheckerOptions{
		Host: "host1.example.org", Notifier: ff.n, Mailer: mailer, Marks: health.NewRateMarks(),
		Logger: zap.New(core), Now: clock, Location: time.FixedZone("TST", 2*60*60),
	})

	return ff
}

// TestTelegramFailingEmailOnce proves US2 acc. 1 and SC-004: Telegram
// failing for telegram_failing_after brings exactly one email naming
// Telegram and since when, and nothing on Telegram; no second one while the
// failure lasts; a new failure after a successful send is reported again.
func TestTelegramFailingEmailOnce(t *testing.T) {
	mail := &fakeMailer{}
	ff := newFailingFixture(mail)
	after := 15 * time.Minute
	start := ff.now

	_ = ff.n.Notify(context.Background(), Message{Kind: KindProblem, Text: "disk failing"})
	for range 29 { // 14.5 minutes of 30 s ticks: not due yet.
		ff.now = ff.now.Add(30 * time.Second)
		ff.n.Flush(context.Background())
		ff.ce.TelegramFailing(after)
	}
	if len(mail.bodies) != 0 {
		t.Fatalf("emails = %q before telegram_failing_after", mail.bodies)
	}

	for range 11 { // up to 20 minutes.
		ff.now = ff.now.Add(30 * time.Second)
		ff.n.Flush(context.Background())
		ff.ce.TelegramFailing(after)
	}

	wantSubject := "host1.example.org: CHECKER ERROR: notify"
	wantBody := "host1.example.org: CHECKER ERROR: notify: Telegram delivery failing since 05:04"
	if len(mail.bodies) != 1 || mail.subjects[0] != wantSubject || mail.bodies[0] != wantBody {
		t.Fatalf("emails = %q / %q, want one %q", mail.subjects, mail.bodies, wantBody)
	}
	if got := ff.tlg.sent; len(got) != 0 {
		t.Fatalf("telegram = %q, want the notice by email only", got)
	}
	for _, m := range ff.n.Export() {
		if strings.Contains(m.Text, "Telegram delivery failing") {
			t.Fatalf("notice queued for Telegram: %+v", m)
		}
	}
	rec := ff.ce.Drain()
	if len(rec) != 1 || rec[0].Key != TelegramFailingKey || rec[0].Check != "notify" ||
		rec[0].Text != "Telegram delivery failing since 05:04" || !rec[0].At.Equal(start.Add(15*time.Minute)) {
		t.Fatalf("recorded = %+v", rec)
	}
	if h := ff.n.TelegramHealth(); !h.Reported {
		t.Fatalf("health = %+v, want reported", h)
	}

	// Telegram recovers, then fails again: a new failure, a new notice.
	ff.tlg.fail = false
	ff.n.Flush(context.Background())
	ff.ce.TelegramFailing(after)
	ff.tlg.fail = true
	_ = ff.n.Notify(context.Background(), Message{Kind: KindProblem, Text: "fan failing"})
	ff.now = ff.now.Add(after)
	ff.ce.TelegramFailing(after)
	if len(mail.bodies) != 2 {
		t.Fatalf("emails = %q, want a second notice for the new failure", mail.bodies)
	}
}

// TestTelegramFailingWithoutMailerIsLogged: without an SMTP relay the notice
// is logged at error level, once.
func TestTelegramFailingWithoutMailerIsLogged(t *testing.T) {
	ff := newFailingFixture(nil)
	_ = ff.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})
	ff.now = ff.now.Add(15 * time.Minute)

	ff.ce.TelegramFailing(15 * time.Minute)
	ff.ce.TelegramFailing(15 * time.Minute)

	logged := ff.logs.FilterMessageSnippet("Telegram delivery failing since 05:04").All()
	if len(logged) != 1 || logged[0].Level != zap.ErrorLevel {
		t.Fatalf("logs = %+v, want one error-level entry", ff.logs.All())
	}
	if !ff.n.TelegramHealth().Reported || len(ff.tlg.sent) != 0 || len(ff.ce.Drain()) != 0 {
		t.Fatalf("health = %+v, telegram = %q", ff.n.TelegramHealth(), ff.tlg.sent)
	}
}

// TestTelegramFailingEmailNotAttemptedIsRetried: an email that did not go
// out, refused without being attempted (past the tick's deadline) or
// attempted and failed, leaves the notice due and unrecorded; the next call
// sends it. The notice is logged once per failure, not on every retry.
func TestTelegramFailingEmailNotAttemptedIsRetried(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"not attempted", fmt.Errorf("deadline: %w: %w", ErrPastDeadline, ErrNotSent)},
		{"attempted and failed", errors.New("relay down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mail := &refusingMailer{err: tt.err}
			ff := newFailingFixture(mail)
			_ = ff.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})
			ff.now = ff.now.Add(15 * time.Minute)

			ff.ce.TelegramFailing(15 * time.Minute)
			ff.ce.TelegramFailing(15 * time.Minute)
			if h := ff.n.TelegramHealth(); h.Reported {
				t.Fatalf("health = %+v, want the notice still due", h)
			}
			if rec := ff.ce.Drain(); len(rec) != 0 {
				t.Fatalf("recorded = %+v, want nothing for an email that did not go out", rec)
			}
			if got := len(ff.logs.FilterMessage("checker error").All()); got != 1 {
				t.Fatalf("checker error logged %d times, want once per failure", got)
			}

			mail.err = nil
			ff.ce.TelegramFailing(15 * time.Minute)
			ff.ce.TelegramFailing(15 * time.Minute)
			if mail.sent != 1 || !ff.n.TelegramHealth().Reported || len(ff.ce.Drain()) != 1 {
				t.Fatalf("sent = %d, health = %+v; want the retry sent once and recorded",
					mail.sent, ff.n.TelegramHealth())
			}
		})
	}
}
