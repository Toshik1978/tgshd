package notify

import (
	"context"
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

	checkerLabel   = "CHECKER ERROR: "
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
}

// CheckerErrors reports problems of the monitoring itself (an unreadable
// reading, a missing tool, a bad configuration entry, a missing secret).
type CheckerErrors struct {
	opts CheckerOptions

	mu       sync.Mutex
	recorded []CheckerAlert
}

// NewCheckerErrors creates a CheckerErrors.
func NewCheckerErrors(opts CheckerOptions) *CheckerErrors {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}

	return &CheckerErrors{opts: opts}
}

// CheckerError reports an error under key ("check/subject/kind"). It is always
// logged; Telegram and email each get it at most once per 24 hours per key,
// and every send is recorded for Drain. It matches the notifier's
// OnCheckerError hook.
func (c *CheckerErrors) CheckerError(key, text string) {
	check, _, _ := strings.Cut(key, "/")
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
		sent = true

		c.mail(ctx, key, check, text)
	}

	if sent {
		c.mu.Lock()
		c.recorded = append(c.recorded, CheckerAlert{At: now, Check: check, Key: key, Text: text})
		c.mu.Unlock()
	}
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

func (c *CheckerErrors) mail(ctx context.Context, key, check, text string) {
	subject := c.opts.Host + ": " + checkerLabel + check
	body := c.opts.Host + ": " + checkerLabel + check + ": " + text

	if err := c.opts.Mailer.Send(ctx, subject, body); err != nil {
		c.opts.Logger.Warn("checker error email not sent", zap.String("key", key), zap.Error(err))
	}
}
