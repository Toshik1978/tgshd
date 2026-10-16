package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/Toshik1978/tgshd/pkg/health/notify"
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

// lane is one delivery channel (Telegram, SMS or email). A send waits no
// longer than its context and the running tick allow. A sender that ignores
// its context (the Telegram publisher does) is left running: the caller gets
// a notify.AbandonedError carrying the send's eventual result, so the
// notifier keeps the message queued without re-sending it and drops it on a
// late success. While such a send is in progress the lane refuses new sends,
// so at most one hangs per channel.
type lane struct {
	d    *deliveries
	mail bool
	busy atomic.Bool
}

// run calls send unless the tick's deadline has passed or the lane is busy;
// a refusal wraps notify.ErrNotSent.
func (l *lane) run(ctx context.Context, send func(context.Context) error) error {
	stop := l.d.stop(l.mail)
	select {
	case <-stop:
		return fmt.Errorf("%w: %w: %w", errDeliveryDeadline, notify.ErrPastDeadline, notify.ErrNotSent)
	default:
	}
	if !l.busy.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: %w", errChannelBusy, notify.ErrNotSent)
	}

	sendCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		err := send(sendCtx)
		cancel()
		l.busy.Store(false)

		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-stop:
		cancel()

		return &notify.AbandonedError{Cause: errDeliveryDeadline, Result: done}
	case <-ctx.Done():
		return &notify.AbandonedError{Cause: context.Cause(ctx), Result: done}
	}
}

// boundSends starts the tick's delivery deadlines, from now: Telegram and SMS
// sends end by the tick budget less the email share, emails by the full
// budget. Do calls it once its runs have ended (at the start of a quiet tick),
// so the runs never use up the delivery window. Until
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
	sendCtx, cancel := context.WithTimeout(ctx, sendBudget)

	return sendCtx, func() {
		cancel()
		r.sends.end()
		cancelSend()
		cancelMail()
	}
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
		o.Telegram = boundedSender{next: o.Telegram, lane: &lane{d: d}}
	}
	if o.SMS != nil {
		o.SMS = boundedSender{next: o.SMS, lane: &lane{d: d}}
	}
	if o.Mailer != nil {
		o.Mailer = boundedMailer{next: o.Mailer, lane: &lane{d: d, mail: true}}
	}
}
