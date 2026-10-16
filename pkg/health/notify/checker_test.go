package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
)

type fakeMailer struct {
	subjects []string
	bodies   []string
	fail     bool
}

func (f *fakeMailer) Send(_ context.Context, subject, body string) error {
	if f.fail {
		return errors.New("relay down")
	}

	f.subjects = append(f.subjects, subject)
	f.bodies = append(f.bodies, body)

	return nil
}

type checkerFixture struct {
	ce   *CheckerErrors
	tlg  *fakeSender
	mail *fakeMailer
	logs *observer.ObservedLogs
	now  time.Time
}

func newCheckerFixture(t *testing.T, withMail bool) *checkerFixture {
	t.Helper()

	cf := &checkerFixture{tlg: &fakeSender{}, mail: &fakeMailer{}, now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	core, logs := observer.New(zap.WarnLevel)
	cf.logs = logs

	opts := CheckerOptions{
		Host:     "host1.example.org",
		Notifier: New(Options{Host: "host1.example.org", Telegram: cf.tlg}),
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

func TestCheckerErrorKeyWithoutSlash(t *testing.T) {
	cf := newCheckerFixture(t, false)

	cf.ce.CheckerError("config", "bad entry")

	if len(cf.tlg.sent) != 1 || !strings.Contains(cf.tlg.sent[0], "CHECKER ERROR: config: bad entry") {
		t.Fatalf("telegram = %q", cf.tlg.sent)
	}
}
