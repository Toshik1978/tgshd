package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// mailShareDiv reserves 1/mailShareDiv of the tick budget, at its end, for
// email only: a Telegram hang that uses up its part of the tick never
// starves the checker-error email, the backup channel.
const mailShareDiv = 10

var (
	// errDeliveryDeadline refuses a send once the tick's deadline has passed.
	errDeliveryDeadline = errors.New("tick delivery deadline passed")
	// errChannelBusy refuses a send while an abandoned send on the same
	// channel is still in progress.
	errChannelBusy = errors.New("an abandoned send on this channel is still in progress")
)

// deliveries holds the delivery deadlines of the running tick. Every send of
// the notifier and the checker errors goes through a lane bound to it.
type deliveries struct {
	mu sync.Mutex
	// sendStop is closed when the tick's Telegram and SMS sends must end,
	// mailStop when its emails must; both nil between ticks.
	sendStop, mailStop <-chan struct{}
}

// begin bounds the sends from now on by the given stops, until end.
func (d *deliveries) begin(sendStop, mailStop <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.sendStop, d.mailStop = sendStop, mailStop
}

// end lifts the tick's bounds.
func (d *deliveries) end() {
	d.begin(nil, nil)
}

func (d *deliveries) stop(mail bool) <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()

	if mail {
		return d.mailStop
	}

	return d.sendStop
}

// lane is one delivery channel (Telegram, SMS or email). Its sends go one at
// a time: a send made while another is in progress (e.g. a command's push
// during a tick) waits for it. A send waits no longer than its context and
// the running tick allow. A sender that ignores its context (the Telegram
// publisher does) is left running: the caller gets a notify.AbandonedError
// carrying the send's eventual result, so the notifier keeps the message
// queued without re-sending it and drops it on a late success. While such an
// abandoned send is in progress the lane refuses new sends, waiting ones
// included, so at most one hangs per channel.
type lane struct {
	d    *deliveries
	mail bool

	mu sync.Mutex
	// inFlight tells that a send is in progress, abandoned that it was
	// abandoned; changed is closed (and replaced) when either changes. seq
	// numbers the sends, so a late abandon never marks a newer one.
	inFlight, abandoned bool
	seq                 uint64
	changed             chan struct{}
}

// run calls send unless the tick's deadline has passed or the lane is busy;
// a refusal wraps notify.ErrNotSent.
func (l *lane) run(ctx context.Context, send func(context.Context) error) error {
	stop := l.d.stop(l.mail)
	select {
	case <-stop:
		return pastDeadline()
	default:
	}
	seq, err := l.acquire(ctx, stop)
	if err != nil {
		return err
	}

	sendCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		err := send(sendCtx)
		cancel()
		l.release()

		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-stop:
		cancel()
		l.abandon(seq)

		return &notify.AbandonedError{Cause: errDeliveryDeadline, Result: done}
	case <-ctx.Done():
		l.abandon(seq)

		return &notify.AbandonedError{Cause: context.Cause(ctx), Result: done}
	}
}

// acquire waits until no send is in progress and takes the lane, returning
// the send's number. It refuses while a send is abandoned, and when stop
// closes or ctx ends first.
func (l *lane) acquire(ctx context.Context, stop <-chan struct{}) (uint64, error) {
	for {
		l.mu.Lock()
		switch {
		case l.abandoned:
			l.mu.Unlock()

			return 0, fmt.Errorf("%w: %w", errChannelBusy, notify.ErrNotSent)
		case !l.inFlight:
			l.inFlight = true
			l.seq++
			seq := l.seq
			l.mu.Unlock()

			return seq, nil
		}
		if l.changed == nil {
			l.changed = make(chan struct{})
		}
		changed := l.changed
		l.mu.Unlock()

		select {
		case <-changed:
		case <-stop:
			return 0, pastDeadline()
		case <-ctx.Done():
			// The tick's send context ends from the stop: report the deadline.
			select {
			case <-stop:
				return 0, pastDeadline()
			default:
			}

			return 0, fmt.Errorf("%w: %w", context.Cause(ctx), notify.ErrNotSent)
		}
	}
}

// release frees the lane once a send has returned.
func (l *lane) release() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.inFlight, l.abandoned = false, false
	l.signal()
}

// abandon marks send seq as abandoned; it does nothing when that send has
// returned meanwhile, even if a newer one has taken the lane.
func (l *lane) abandon(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.inFlight && l.seq == seq {
		l.abandoned = true
		l.signal()
	}
}

// signal wakes the waiting sends; the caller holds l.mu.
func (l *lane) signal() {
	if l.changed != nil {
		close(l.changed)
		l.changed = nil
	}
}

// pastDeadline is the refusal of a send past the tick's deadline.
func pastDeadline() error {
	return fmt.Errorf("%w: %w: %w", errDeliveryDeadline, notify.ErrPastDeadline, notify.ErrNotSent)
}

// boundSends starts the tick's delivery deadlines, from now: Telegram and SMS
// sends end by the tick budget less the email share, emails by the full
// budget. Do calls it once its runs have ended, so the runs never use up the
// delivery window. Until
// the returned end is called, every send is bounded by them, also one made
// with a context of its own (the checker errors'); shutdown ends only the
// sends made with the returned context, which carries the send deadline.
func (r *Runner) boundSends(ctx context.Context) (context.Context, func()) {
	budget := r.opts.TickBudget
	sendBudget := budget - budget/mailShareDiv
	base := context.WithoutCancel(ctx)
	sendDeadline, cancelSend := context.WithTimeout(base, sendBudget)
	mailDeadline, cancelMail := context.WithTimeout(base, budget)
	r.sends.begin(sendDeadline.Done(), mailDeadline.Done())
	// The send context ends from the send stop itself, not from a timer of
	// its own: two timers for the same instant fire in either order, and a
	// send context ending first would let a send on a free lane (the SMS
	// fallback after a Telegram hang) start past the deadline. The parent
	// ctx (the worker's) carries no deadline, so the one reported is the send
	// stop's. Once the stop closes, sendCtx.Err() is context.Canceled, not
	// DeadlineExceeded; the cause, context.Cause(sendCtx), keeps
	// DeadlineExceeded.
	sendCtx, cancel := context.WithCancelCause(ctx)
	stopAfter := context.AfterFunc(sendDeadline, func() { cancel(context.DeadlineExceeded) })
	deadline, _ := sendDeadline.Deadline()
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	return sendContext{Context: sendCtx, deadline: deadline}, func() {
		stopAfter()
		cancel(nil)
		r.sends.end()
		cancelSend()
		cancelMail()
	}
}

// sendContext is the tick's send context: it ends when the send stop has
// closed (or its parent ends) and still reports the send deadline, which
// adapters use to bound their I/O.
type sendContext struct {
	context.Context //nolint:containedctx // wraps the context to report its deadline

	deadline time.Time
}

// Deadline implements context.Context.
func (c sendContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

// boundedSender is a notify.Sender whose sends go through its lane.
type boundedSender struct {
	next notify.Sender
	lane *lane
}

// Send implements notify.Sender.
func (s boundedSender) Send(ctx context.Context, text string) error {
	return s.lane.run(ctx, func(ctx context.Context) error { return s.next.Send(ctx, text) })
}

// boundedTelegram is a notify.TelegramSender whose sends go through its lane.
type boundedTelegram struct {
	next notify.TelegramSender
	lane *lane
}

// Send implements notify.TelegramSender.
func (s boundedTelegram) Send(ctx context.Context, r route.Route, text string) error {
	return s.lane.run(ctx, func(ctx context.Context) error { return s.next.Send(ctx, r, text) })
}

// boundedMailer is a notify.Mailer whose sends go through its lane.
type boundedMailer struct {
	next notify.Mailer
	lane *lane
}

// Send implements notify.Mailer.
func (m boundedMailer) Send(ctx context.Context, subject, body string) error {
	return m.lane.run(ctx, func(ctx context.Context) error { return m.next.Send(ctx, subject, body) })
}

// bind wraps the channels of o that the notifier and the checker errors send
// on, each in its own lane; an absent (nil) channel stays nil.
func (d *deliveries) bind(o *Options) {
	if o.Telegram != nil {
		o.Telegram = boundedTelegram{next: o.Telegram, lane: &lane{d: d}}
	}
	if o.SMS != nil {
		o.SMS = boundedSender{next: o.SMS, lane: &lane{d: d}}
	}
	if o.Mailer != nil {
		o.Mailer = boundedMailer{next: o.Mailer, lane: &lane{d: d, mail: true}}
	}
}

// noticeTelegramFailing sends the one email notice of a Telegram delivery
// failing for TelegramFailingAfter (FR-029). Do calls it after
// the tick's deliveries, within its delivery window, so the tick's own failed
// sends count and the email is bounded like every other send.
func (r *Runner) noticeTelegramFailing() {
	r.checker.TelegramFailing(r.opts.TelegramFailingAfter)
}
