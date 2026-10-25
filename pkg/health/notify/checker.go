package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const (
	// CheckerErrorWindow is the minimum time between two sends of the same
	// checker error on one channel (FR-045).
	CheckerErrorWindow = 24 * time.Hour

	// TelegramFailingKey is the checker-error key of the notice that
	// Telegram delivery keeps failing (FR-029).
	TelegramFailingKey = "notify/telegram/failing"
	// telegramFailingCheck is the check part of TelegramFailingKey.
	telegramFailingCheck = "notify"

	checkerLabel   = "CHECKER ERROR: "
	recoveredLabel = "CHECKER OK: "
	checkerTimeout = 30 * time.Second
)

// Mailer sends one email to the configured recipients; pkg/mail satisfies it.
type Mailer interface {
	Send(ctx context.Context, subject, body string) error
}

// CheckerAlert is a checker error that went out, to be recorded in the alert
// log with kind checker_error.
type CheckerAlert struct {
	At    time.Time
	Check string
	Key   string
	Text  string
}

// CheckerOptions configures CheckerErrors.
type CheckerOptions struct {
	Host     string
	Notifier *Notifier
	// Mailer may be nil: without an SMTP relay only Telegram is used.
	Mailer Mailer
	Marks  *health.RateMarks
	Logger *zap.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// Location is the host's zone for "since" times; defaults to time.Local.
	Location *time.Location
}

// CheckerErrors reports problems of the monitoring itself (an unreadable
// reading, a missing tool, a bad configuration entry, a missing secret).
type CheckerErrors struct {
	opts CheckerOptions

	mu       sync.Mutex
	recorded []CheckerAlert
	// warnedSince is the start of the Telegram failure whose notice was
	// logged, so retries of its email do not log it again.
	warnedSince time.Time
	// emailed are the keys whose checker error went out by email, so their
	// recovery notice goes by email too (Recovered).
	emailed map[string]bool
}

// NewCheckerErrors creates a CheckerErrors.
func NewCheckerErrors(opts CheckerOptions) *CheckerErrors {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}

	if opts.Location == nil {
		opts.Location = time.Local //nolint:gosmopolitan // "since" times are shown in the host's wall-clock time
	}

	return &CheckerErrors{opts: opts, emailed: make(map[string]bool)}
}

// CheckerError reports an error under key ("check/subject/kind"). It is always
// logged; Telegram and email each get it at most once per 24 hours per key,
// and every send is recorded for Drain. It matches the notifier's
// OnCheckerError hook.
func (c *CheckerErrors) CheckerError(key, text string) {
	c.Report(key, text)
}

// Report is CheckerError that tells whether the error went out on a channel:
// handed to the notifier for Telegram (which queues and retries it), or
// delivered by email; not when its rate limit held it back or its email
// failed. Only such an error is recorded for Drain. The message is labelled
// with the first segment of key; ReportCheck names the check.
func (c *CheckerErrors) Report(key, text string) bool {
	check, _, _ := strings.Cut(key, "/")

	return c.ReportCheck(check, key, text)
}

// ReportCheck is Report labelled with check, the whole check name (e.g.
// "command/backup"), so that errors of checks sharing a first key
// segment are told apart.
func (c *CheckerErrors) ReportCheck(check, key, text string) bool {
	now := c.opts.Now()

	c.opts.Logger.Warn("checker error", zap.String("check", check), zap.String("key", key), zap.String("text", text))

	ctx, cancel := context.WithTimeout(context.Background(), checkerTimeout)
	defer cancel()

	sent := false

	if c.opts.Notifier != nil && c.opts.Marks.Allow(key, ChannelTelegram, now, CheckerErrorWindow) {
		sent = true

		_ = c.opts.Notifier.Notify(ctx, Message{
			Kind:  KindEvent,
			Check: check,
			Key:   key,
			Text:  checkerLabel + check + ": " + text,
			Mode:  ModeTelegram,
		})
	}

	if c.opts.Mailer != nil && c.opts.Marks.Allow(key, ChannelEmail, now, CheckerErrorWindow) {
		delivered, attempted := c.mail(ctx, key, check, text)
		if delivered {
			sent = true
		}
		if !attempted {
			c.opts.Marks.Revert(key, ChannelEmail, now)
		}
	}

	if sent {
		c.mu.Lock()
		c.recorded = append(c.recorded, CheckerAlert{At: now, Check: check, Key: key, Text: text})
		c.mu.Unlock()
	}

	return sent
}

// Recovered sends the one "CHECKER OK" notice that the checker error under
// key is over (e.g. FR-032): by Telegram, and by email when the error went
// out by email. It gives back the key's rate marks, so a new error is
// reported at once. It is logged, and not recorded for Drain.
func (c *CheckerErrors) Recovered(key, text string) {
	check, _, _ := strings.Cut(key, "/")

	c.opts.Logger.Info("checker ok", zap.String("check", check), zap.String("key", key), zap.String("text", text))

	for _, m := range c.opts.Marks.Export() {
		if m.Key == key {
			c.opts.Marks.Revert(m.Key, m.Channel, m.LastSentAt)
		}
	}
	c.mu.Lock()
	byEmail := c.emailed[key]
	delete(c.emailed, key)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), checkerTimeout)
	defer cancel()

	if c.opts.Notifier != nil {
		_ = c.opts.Notifier.Notify(ctx, Message{
			Kind:  KindEvent,
			Check: check,
			Key:   key,
			Text:  recoveredLabel + check + ": " + text,
			Mode:  ModeTelegram,
		})
	}

	if byEmail && c.opts.Mailer != nil {
		if err := c.sendMail(ctx, recoveredLabel, check, text); err != nil {
			c.opts.Logger.Warn("checker ok email not sent", zap.String("key", key), zap.Error(err))
		}
	}
}

// TelegramFailing sends the one notice that Telegram delivery keeps failing
// (FR-029, R17), when the notifier's Telegram channel has been failing for at
// least after and that failure was not reported yet. The notice goes by email
// only, never to Telegram, the failing channel; without a mailer it is logged
// at error level. Once it went out (or was logged) the failure is marked
// reported, so it is not repeated while it lasts; an email that did not go
// out, for any reason, leaves it due for the next call and is not recorded.
// The notice is logged once per failure, not on every retry. The runner calls
// it after each tick's deliveries.
func (c *CheckerErrors) TelegramFailing(after time.Duration) {
	if c.opts.Notifier == nil {
		return
	}

	now := c.opts.Now()
	h := c.opts.Notifier.TelegramHealth()
	if !h.NoticeDue(now, after) {
		return
	}

	const check = telegramFailingCheck
	text := "Telegram delivery failing since " + formatSince(h.FailingSince.In(c.opts.Location), now)

	if c.opts.Mailer == nil {
		c.opts.Logger.Error(text, zap.String("check", check), zap.String("key", TelegramFailingKey))
		c.opts.Notifier.MarkTelegramReported(h.FailingSince)

		return
	}

	firstTry := c.firstTelegramTry(h.FailingSince)
	if firstTry {
		c.opts.Logger.Warn("checker error", zap.String("check", check), zap.String("key", TelegramFailingKey),
			zap.String("text", text))
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkerTimeout)
	defer cancel()

	if err := c.sendMail(ctx, checkerLabel, check, text); err != nil {
		if firstTry {
			c.opts.Logger.Warn("checker error email not sent", zap.String("key", TelegramFailingKey), zap.Error(err))
		}

		return
	}

	c.opts.Notifier.MarkTelegramReported(h.FailingSince)

	c.mu.Lock()
	c.recorded = append(c.recorded, CheckerAlert{At: now, Check: check, Key: TelegramFailingKey, Text: text})
	c.mu.Unlock()
}

// Drain returns the checker errors sent since the last call, for the runner to
// hand to the alert log.
func (c *CheckerErrors) Drain() []CheckerAlert {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := c.recorded
	c.recorded = nil

	return out
}

// firstTelegramTry reports whether this is the first attempt at the notice of
// the Telegram failure that began at since, and remembers it.
func (c *CheckerErrors) firstTelegramTry(since time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.warnedSince.Equal(since) {
		return false
	}
	c.warnedSince = since

	return true
}

// mail sends the checker error by email and reports whether it was delivered
// and whether it was attempted at all. A send refused without being attempted
// (ErrNotSent) makes the caller give the rate mark back; an attempted send
// that failed keeps it, so the relay is not retried on every call, but is no
// delivery.
func (c *CheckerErrors) mail(ctx context.Context, key, check, text string) (delivered, attempted bool) {
	err := c.sendMail(ctx, checkerLabel, check, text)
	if err != nil {
		c.opts.Logger.Warn("checker error email not sent", zap.String("key", key), zap.Error(err))

		return false, !errors.Is(err, ErrNotSent)
	}
	c.mu.Lock()
	c.emailed[key] = true
	c.mu.Unlock()

	return true, true
}

// sendMail emails a checker message of check with text under label
// (checkerLabel or recoveredLabel).
func (c *CheckerErrors) sendMail(ctx context.Context, label, check, text string) error {
	subject := c.opts.Host + ": " + label + check
	body := c.opts.Host + ": " + label + check + ": " + text

	if err := c.opts.Mailer.Send(ctx, subject, body); err != nil {
		return fmt.Errorf("send checker error email: %w", err)
	}

	return nil
}
