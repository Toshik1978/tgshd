// Package notify routes pushed health messages to Telegram and SMS.
//
// Every message produced here is a pushed alert, so it carries the host
// prefix (FR-010). Replies to commands never go through this package.
package notify

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Mode selects the delivery channel of a message.
type Mode string

// Delivery modes.
const (
	ModeTelegram      Mode = "tlg"
	ModeSMS           Mode = "sms"
	ModeTelegramOrSMS Mode = "tlg-or-sms"
)

func (m Mode) valid() bool {
	return m == ModeTelegram || m == ModeSMS || m == ModeTelegramOrSMS
}

// Kind is the type of a pushed message.
type Kind string

// Message kinds.
const (
	KindProblem  Kind = "problem"
	KindResolved Kind = "resolved"
	KindEvent    Kind = "event"
)

// Queue channels, as persisted in the queue table.
const (
	ChannelTelegram = "tlg"
	ChannelSMS      = "sms"
	ChannelEmail    = "email"
)

const (
	// CheckNetwork is the check whose PROBLEM goes by SMS, since Telegram is
	// unreachable while the network is down.
	CheckNetwork = "network"
	// MaxMessageUnits is the Telegram message length limit, counted in UTF-16
	// code units, for the whole HTML message including the host prefix.
	MaxMessageUnits = 4096
	// MaxQueue bounds the retry queue; the oldest messages are dropped first.
	MaxQueue = 1000

	// SMSUnavailableKey is the checker-error key raised when mode sms has to
	// degrade to tlg for lack of an SMS sender.
	SMSUnavailableKey = "notify/sms/unavailable"

	ellipsis = "…"
)

// Sender sends one text. The Telegram implementation receives HTML, the SMS
// implementation plain text; recipients are bound by the implementation.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// TelegramSender sends one HTML text to Telegram, to the place the route
// asks for; the chat and the topic ids are bound by the implementation.
type TelegramSender interface {
	Send(ctx context.Context, r route.Route, text string) error
}

// defaultRoute is the route of a message that asks for none: its kind
// decides. A PROBLEM and an event ring in the alerts topic, a RESOLVED goes
// there silently.
func defaultRoute(kind string) route.Route {
	if kind == string(KindResolved) {
		return route.Route{Topic: route.TopicAlerts, Silent: true}
	}

	return route.Route{Topic: route.TopicAlerts}
}

// resolveRoute returns r, or the default for kind when r is the zero route.
func resolveRoute(kind string, r route.Route) route.Route {
	if r == (route.Route{}) {
		return defaultRoute(kind)
	}

	return r
}

// Message is an alert to push, as mapped from an alert-engine event.
type Message struct {
	Kind  Kind
	Check string
	Key   string
	Text  string
	// Since is the start of the problem streak; zero omits the suffix.
	Since time.Time
	// Mode overrides the default delivery mode when not empty.
	Mode Mode
	// Route is where a Telegram delivery goes; the zero route resolves by
	// Kind (see defaultRoute).
	Route route.Route
}

// QueuedMessage is a message awaiting delivery (data-model "QueuedMessage").
type QueuedMessage struct {
	ID        int64
	CreatedAt time.Time
	Channel   string
	Text      string
	Attempts  int
	// Kind is "problem", "resolved" or "event"; the notifier always sets it.
	Kind string
	// Check and Key identify the condition a problem or resolved message is about.
	Check string
	Key   string
	// Plain is the message's own text, without host prefix, label or since,
	// and Since its streak start; both are set at enqueue and persisted (zero
	// for messages restored from a store older than migration 3).
	Plain string
	Since time.Time
	// Route is the Telegram route the message was queued with, resolved at
	// enqueue; the zero route (a message restored without one) resolves by
	// Kind when sent.
	Route route.Route
}

// Options configures a Notifier.
type Options struct {
	// Host is the prefix of every pushed message.
	Host string
	// Mode is the default delivery mode; empty means tlg.
	Mode     Mode
	Telegram TelegramSender
	// SMS may be nil when no SMS backend is configured.
	SMS Sender
	// OnCheckerError is called once when mode sms degrades to tlg.
	OnCheckerError func(key, text string)
	// OnDrop is called for every message dropped from the queue undelivered
	// (trimmed at MaxQueue or expired), outside the notifier's lock.
	OnDrop func(QueuedMessage)
	// Now defaults to time.Now.
	Now func() time.Time
	// Location is the host's zone, in which the since of a listed
	// undelivered PROBLEM is read back; defaults to time.Local.
	Location *time.Location
}

// Notifier formats, routes and queues pushed messages.
type Notifier struct {
	opts Options

	mu       sync.Mutex
	queue    []QueuedMessage
	nextID   int64
	offline  bool
	degraded bool
	// telegram is the delivery health of the Telegram channel (R17).
	telegram ChannelHealth
	// telegramOK is set once a Telegram send succeeded: a stored failure
	// restored afterwards is over.
	telegramOK bool
	// smsProblems are the condition keys whose PROBLEM went by SMS through
	// the fallback (R18).
	smsProblems SMSProblems
	// resolvedBefore maps a condition key to the first queue ID after its
	// latest RESOLVED: a PROBLEM of that key queued with a lower ID is
	// resolved and never goes by SMS through the fallback.
	resolvedBefore map[string]int64
	// inflight are the queued messages whose abandoned send is still in
	// progress, by ID.
	inflight map[int64]*flight
	// undelivered are the dropped PROBLEMs listed ahead of the next message
	// delivered to Telegram (R19).
	undelivered Undelivered
	// showing is set while a Telegram send carries the undelivered prefix,
	// so a concurrent send does not show (and acknowledge) the same items;
	// held is the number of items it shows.
	showing bool
	held    int
}

// New creates a Notifier.
func New(opts Options) *Notifier {
	if opts.Mode == "" {
		opts.Mode = ModeTelegram
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Location == nil {
		opts.Location = time.Local //nolint:gosmopolitan // "since" times are the host's wall-clock time
	}

	return &Notifier{opts: opts, nextID: 1, inflight: map[int64]*flight{}, resolvedBefore: map[string]int64{}}
}

// SetOffline marks the outside as known down: Telegram messages are queued
// silently (FR-009) and Flush does nothing until it is cleared.
func (n *Notifier) SetOffline(offline bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.offline = offline
}

// TelegramHealth returns the delivery health of the Telegram channel.
func (n *Notifier) TelegramHealth() ChannelHealth {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.telegram
}

// MarkTelegramReported records that the notice of the Telegram failure that
// began at since went out. It does nothing when Telegram is OK or failing
// since another moment (it recovered and failed again meanwhile).
func (n *Notifier) MarkTelegramReported(since time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.telegram.FailingSince.Equal(since) {
		n.telegram.MarkReported()
	}
}

// ListUndelivered lists a PROBLEM dropped from the queue undelivered whose
// condition is no longer open: its plain text and since go ahead of the next
// message delivered to Telegram, and stay listed until one has shown them.
func (n *Notifier) ListUndelivered(item QueuedMessage) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.undelivered.Add(undeliveredFrom(item, n.opts.Host, n.opts.Location, n.opts.Now()))
}

// ForgetEnded takes out of the SMS-sent set every key for which ended
// reports true: its condition ended without a RESOLVED (cleared, retired,
// forgotten, or good again after a reopen). Its PROBLEMs queued so far are
// marked resolved, so the fallback never sends one of them by SMS again,
// while the PROBLEM of a later streak goes by SMS as usual. ended is called
// with the notifier's lock held.
func (n *Notifier) ForgetEnded(ended func(key string) bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for key := range n.smsProblems.keys {
		if ended(key) {
			n.smsProblems.Remove(key)
			n.resolvedBefore[key] = n.nextID
		}
	}
}

// Notify delivers a message. Failed deliveries are queued, so the returned
// error is reserved for future use and is currently always nil.
func (n *Notifier) Notify(ctx context.Context, msg Message) error {
	msg.Route = resolveRoute(string(queuedKind(msg.Kind)), msg.Route)
	mode := n.resolveMode(msg)
	plain, rich := n.render(msg)

	n.mu.Lock()
	hook := n.degrade(&mode)
	offline := n.offline
	n.mu.Unlock()

	if hook != nil {
		hook(SMSUnavailableKey, "SMS alerts are configured but no SMS backend is available; using Telegram")
	}

	viaSMS := false

	switch mode {
	case ModeSMS:
		n.sendSMS(ctx, msg, plain)
		viaSMS = true
	case ModeTelegramOrSMS:
		viaSMS = n.sendTelegramOrSMS(ctx, msg, plain, rich, offline)
	case ModeTelegram:
		n.sendTelegram(ctx, msg, rich, offline)
	}

	if msg.Kind == KindResolved {
		n.resolvedBySMS(ctx, msg, plain, viaSMS)
	}

	n.smsFallback(ctx)

	return nil
}

// Flush retries queued messages in order, per channel. It stops a channel at
// its first failure, or at a message whose abandoned send is still in
// progress, so ordering is kept and nothing is sent twice; it does nothing
// for Telegram while offline. A PROBLEM queued for SMS whose condition has
// resolved since is dropped unsent.
func (n *Notifier) Flush(ctx context.Context) {
	n.mu.Lock()
	pending := append([]QueuedMessage(nil), n.queue...)
	offline := n.offline
	n.mu.Unlock()

	blocked := map[string]bool{ChannelEmail: true}
	if offline || n.opts.Telegram == nil {
		blocked[ChannelTelegram] = true
	}

	if n.opts.SMS == nil {
		blocked[ChannelSMS] = true
	}

	for i := range pending {
		item := &pending[i]
		if blocked[item.Channel] {
			continue
		}

		if n.inFlight(item.ID) {
			blocked[item.Channel] = true

			continue
		}

		if n.staleSMSProblem(item) {
			n.remove(item.ID)

			continue
		}

		err := n.send(ctx, item.Channel, resolveRoute(item.Kind, item.Route), item.Text)
		if item.Channel == ChannelTelegram {
			n.telegramSent(ctx, err)
		}
		if err != nil {
			blocked[item.Channel] = true

			n.bumpAttempts(item.ID)
			n.track(item.ID, err)

			continue
		}

		n.remove(item.ID)
	}

	n.smsFallback(ctx)
}

// Export returns a copy of the retry queue for persistence.
func (n *Notifier) Export() []QueuedMessage {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]QueuedMessage(nil), n.queue...)
}

// Restore replaces the retry queue with persisted messages.
func (n *Notifier) Restore(items []QueuedMessage) {
	n.mu.Lock()

	n.queue = append([]QueuedMessage(nil), items...)
	n.inflight = map[int64]*flight{}

	for i := range items {
		item := &items[i]
		n.nextID = max(n.nextID, item.ID+1)
	}
	n.moveResolvedLocked(nil)

	dropped := n.trim()
	n.mu.Unlock()

	n.reportDropped(dropped)
}

// Merge folds a persisted queue, loaded after messages were already queued in
// memory, into the retry queue under the notifier's lock: the stored messages
// first, then the in-memory ones renumbered after the highest stored ID.
// Messages queued concurrently are kept. It must not run during Flush, which
// refers to queued messages by ID.
func (n *Notifier) Merge(stored []QueuedMessage) {
	n.mu.Lock()

	var top int64
	for i := range stored {
		item := &stored[i]
		top = max(top, item.ID)
	}

	merged := append([]QueuedMessage(nil), stored...)
	ids := make(map[int64]int64, len(n.queue))
	for i := range n.queue {
		item := n.queue[i]
		top++
		ids[item.ID] = top
		item.ID = top
		merged = append(merged, item)
	}
	n.renumberLocked(ids)

	n.queue = merged
	n.nextID = max(n.nextID, top+1)
	n.moveResolvedLocked(ids)
	dropped := n.trim()
	n.mu.Unlock()

	n.reportDropped(dropped)
}

// Expire drops the queued messages created more than maxAge before now,
// reports each through OnDrop and returns them. The store's retention drops
// the same rows, so memory and the stored queue agree.
func (n *Notifier) Expire(now time.Time, maxAge time.Duration) []QueuedMessage {
	cutoff := now.Add(-maxAge)

	n.mu.Lock()
	var dropped, kept []QueuedMessage
	for i := range n.queue {
		item := &n.queue[i]
		if item.CreatedAt.Before(cutoff) {
			dropped = append(dropped, *item)
		} else {
			kept = append(kept, *item)
		}
	}
	n.queue = kept
	n.mu.Unlock()

	n.reportDropped(dropped)

	return dropped
}

// telegramSent records the outcome of a Telegram send made with ctx: nil is
// a success, any other error a failure, except a refusal past the tick's
// delivery deadline and a send that failed because ctx was canceled for a
// stop, which say nothing about Telegram. A ctx ended by a deadline (the
// tick's send stop cancels with that cause) still counts as a failure.
func (n *Notifier) telegramSent(ctx context.Context, err error) {
	if errors.Is(err, ErrPastDeadline) || err != nil && errors.Is(context.Cause(ctx), context.Canceled) {
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.telegramSentLocked(err)
}

// telegramSentLocked is telegramSent for a caller that holds the lock.
func (n *Notifier) telegramSentLocked(err error) {
	if err == nil {
		n.telegram.Succeed()
		n.telegramOK = true
	} else {
		n.telegram.Fail(n.opts.Now())
	}
}

func (n *Notifier) send(ctx context.Context, channel string, r route.Route, text string) error {
	var err error
	if channel == ChannelSMS {
		err = n.opts.SMS.Send(ctx, text)
	} else {
		err = n.sendTelegramText(ctx, r, text)
	}

	if err != nil {
		return fmt.Errorf("send %s: %w", channel, err)
	}

	return nil
}

// sendTelegramText sends one Telegram message with the undelivered prefix
// ahead of it (alerts topic only), as much as fits the message limit; a successful send
// acknowledges what the prefix showed. An abandoned send holds the prefix
// until its result: the returned AbandonedError carries that result on to
// the caller once the prefix is settled. The caller wraps the error.
func (n *Notifier) sendTelegramText(ctx context.Context, r route.Route, text string) error {
	var (
		prefix string
		shown  Shown
		held   bool
	)
	// Only the alerts topic carries the prefix, and a message that carries it
	// rings, whatever its route asked for.
	if r.Topic == route.TopicAlerts {
		prefix, shown, held = n.takePrefix(MaxMessageUnits - utf16Len(text))
		if held {
			r.Silent = false
		}
	}

	err := n.opts.Telegram.Send(ctx, r, prefix+text)
	if !held {
		return err //nolint:wrapcheck // the callers wrap or classify it
	}

	var ab *AbandonedError
	if !errors.As(err, &ab) || ab.Result == nil {
		n.releasePrefix(shown, err == nil)

		return err //nolint:wrapcheck // the callers wrap or classify it
	}

	// The send is still in progress: the prefix stays held until its result,
	// which is passed on for the queued message to settle on.
	result := make(chan error, 1)
	go func() {
		late := <-ab.Result
		n.releasePrefix(shown, late == nil)

		result <- late
	}()

	return &AbandonedError{Cause: ab.Cause, Result: result}
}

// takePrefix returns the undelivered prefix for a message with limit units
// to spare, and whether it holds the prefix: then no other send shows it
// until releasePrefix.
func (n *Notifier) takePrefix(limit int) (string, Shown, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.showing || n.undelivered.Empty() {
		return "", Shown{}, false
	}

	prefix, shown := n.undelivered.Prefix(limit, n.opts.Now())
	if prefix == "" {
		return "", Shown{}, false
	}
	n.showing = true
	n.held = shown.Items

	return prefix, shown, true
}

// releasePrefix ends a send that carried the prefix; delivered acknowledges
// what it showed. An undelivered send leaves the items listed.
func (n *Notifier) releasePrefix(shown Shown, delivered bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.showing = false
	n.held = 0
	if delivered {
		n.undelivered.Ack(shown)
	}
}

func (n *Notifier) resolveMode(msg Message) Mode {
	switch {
	case msg.Check == CheckNetwork && msg.Kind == KindProblem:
		return ModeSMS
	case msg.Check == CheckNetwork && msg.Kind == KindResolved:
		return ModeTelegramOrSMS
	case msg.Mode.valid():
		return msg.Mode
	case n.opts.Mode.valid():
		return n.opts.Mode
	default:
		return ModeTelegram
	}
}

// degrade switches sms to tlg when there is no SMS sender and returns the
// checker-error hook the first time it happens. The caller holds the lock.
func (n *Notifier) degrade(mode *Mode) func(key, text string) {
	if *mode != ModeSMS || n.opts.SMS != nil {
		return nil
	}

	*mode = ModeTelegram

	if n.degraded {
		return nil
	}

	n.degraded = true

	return n.opts.OnCheckerError
}

func (n *Notifier) sendSMS(ctx context.Context, msg Message, plain string) {
	if err := n.opts.SMS.Send(ctx, plain); err != nil {
		n.enqueueTracked(msg, ChannelSMS, plain, err)
	}
}

func (n *Notifier) sendTelegram(ctx context.Context, msg Message, rich string, offline bool) {
	if offline || n.opts.Telegram == nil {
		n.enqueueOffline(msg, rich)

		return
	}

	err := n.sendTelegramText(ctx, msg.Route, rich)
	n.telegramSent(ctx, err)
	if err != nil {
		n.enqueueTracked(msg, ChannelTelegram, rich, err)
	}
}

// enqueueOffline queues a Telegram message without trying to send it (the
// outside is known down, or there is no Telegram sender): the channel is
// failing.
func (n *Notifier) enqueueOffline(msg Message, rich string) {
	n.mu.Lock()
	n.telegram.Fail(n.opts.Now())
	n.mu.Unlock()

	n.enqueue(msg, ChannelTelegram, rich)
}

// sendTelegramOrSMS sends by Telegram, or by SMS when Telegram fails, and
// reports whether the SMS went out.
func (n *Notifier) sendTelegramOrSMS(ctx context.Context, msg Message, plain, rich string, offline bool) bool {
	if n.opts.SMS == nil {
		n.sendTelegram(ctx, msg, rich, offline)

		return false
	}

	var tlgErr error
	if !offline && n.opts.Telegram != nil {
		tlgErr = n.sendTelegramText(ctx, msg.Route, rich)
		n.telegramSent(ctx, tlgErr)
		if tlgErr == nil {
			return false
		}
		// Past the tick's deadline the SMS lane refuses too: queue the
		// Telegram copy. A busy lane (an abandoned Telegram send still hangs,
		// a sign Telegram is down) falls back to SMS like a failure.
		if errors.Is(tlgErr, ErrPastDeadline) {
			n.enqueue(msg, ChannelTelegram, rich)

			return false
		}
	}

	if err := n.opts.SMS.Send(ctx, plain); err != nil {
		n.claimAbandonedProblem(msg, err)
		if tlgErr == nil {
			// Telegram was not tried: offline, or no Telegram sender.
			n.enqueueOffline(msg, rich)

			return false
		}
		// The queued Telegram copy follows an abandoned Telegram send.
		n.enqueueTracked(msg, ChannelTelegram, rich, tlgErr)

		return false
	}

	return true
}

// moveResolvedLocked moves the resolved marks after a Restore or Merge:
// stored messages predate every RESOLVED seen in memory, so each mark ends
// past them, and an in-memory message renumbered by a Merge (ids, old ID to
// new ID, in queue order) stays on its side of the mark. The caller holds the
// lock and has set nextID.
func (n *Notifier) moveResolvedLocked(ids map[int64]int64) {
	for key, mark := range n.resolvedBefore {
		moved := n.nextID
		for from, to := range ids {
			if from >= mark && to < moved {
				moved = to
			}
		}
		n.resolvedBefore[key] = moved
	}
}

// claimAbandonedProblem puts the key of a PROBLEM whose own SMS was
// abandoned while in progress in the SMS-sent set, so the fallback does not
// send its queued Telegram copy by SMS again; a late failure takes it out.
// The caller queues the Telegram copy after it.
func (n *Notifier) claimAbandonedProblem(msg Message, err error) {
	var ab *AbandonedError
	if msg.Kind != KindProblem || !errors.As(err, &ab) || ab.Result == nil {
		return
	}

	n.mu.Lock()
	n.smsProblems.Add(msg.Key)
	n.mu.Unlock()

	go n.settleFallback(msg.Key, ab.Result)
}

// resolvedBySMS sends a RESOLVED by SMS as well when its PROBLEM went by SMS
// through the fallback, unless its own route already sent it by SMS; either
// way its key leaves the SMS-sent set. A failed SMS is queued for retry. It
// also marks the PROBLEMs of the key queued so far as resolved, whichever
// channel the RESOLVED went on, so the fallback skips them.
func (n *Notifier) resolvedBySMS(ctx context.Context, msg Message, plain string, viaSMS bool) {
	n.mu.Lock()
	n.resolvedBefore[msg.Key] = n.nextID
	wasSMS := n.smsProblems.Remove(msg.Key)
	n.mu.Unlock()

	if !wasSMS || viaSMS || n.opts.SMS == nil {
		return
	}

	n.sendSMS(ctx, msg, plain)
}

// smsFallback sends by SMS, in plain form, each PROBLEM queued for Telegram
// while Telegram is failing, once: its key enters the SMS-sent set and its
// Telegram copy stays queued. It stops at the first failed SMS; the PROBLEMs
// not sent are tried again at the next call. An SMS abandoned while in
// progress keeps its key until it returns, so it is not sent twice.
func (n *Notifier) smsFallback(ctx context.Context) {
	if n.opts.SMS == nil {
		return
	}

	n.mu.Lock()
	due := n.fallbackDueLocked()
	n.mu.Unlock()

	for i := range due {
		item := &due[i]
		err := n.opts.SMS.Send(ctx, html.UnescapeString(item.Text))
		if err == nil {
			continue
		}

		unsent := due[i:]
		var ab *AbandonedError
		if errors.As(err, &ab) && ab.Result != nil {
			unsent = due[i+1:]
			go n.settleFallback(item.Key, ab.Result)
		}

		n.mu.Lock()
		for i := range unsent {
			rest := &unsent[i]
			n.smsProblems.Remove(rest.Key)
		}
		n.mu.Unlock()

		return
	}
}

// settleFallback waits for an abandoned fallback SMS: a late failure takes
// its key out of the SMS-sent set, so the next call sends it again.
func (n *Notifier) settleFallback(key string, result <-chan error) {
	if err := <-result; err == nil {
		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.smsProblems.Remove(key)
}

// fallbackDueLocked returns the queued Telegram PROBLEMs to send by SMS and
// claims their keys in the SMS-sent set, so a concurrent call skips them. A
// PROBLEM is due while Telegram is failing, when its key is not in the set,
// no RESOLVED of the key came after it (queued or sent directly; once
// resolved, the PROBLEM is news no longer) and no later problem or RESOLVED
// of the key is queued (this also holds for a queue restored after a
// restart). The caller holds the lock.
func (n *Notifier) fallbackDueLocked() []QueuedMessage {
	if !n.telegram.Failing() {
		return nil
	}

	last := map[string]int64{}
	for i := range n.queue {
		item := &n.queue[i]
		if item.Channel == ChannelTelegram && item.Kind != string(KindEvent) {
			last[item.Key] = item.ID
		}
	}

	var due []QueuedMessage
	for i := range n.queue {
		item := &n.queue[i]
		if !n.fallbackDueItemLocked(item, last[item.Key]) {
			continue
		}

		n.smsProblems.Add(item.Key)
		due = append(due, *item)
	}

	return due
}

// fallbackDueItemLocked reports whether item is a queued Telegram PROBLEM
// due for the fallback, given the ID of the last queued problem or RESOLVED
// of its key. The caller holds the lock.
func (n *Notifier) fallbackDueItemLocked(item *QueuedMessage, last int64) bool {
	return item.Channel == ChannelTelegram && item.Kind == string(KindProblem) && item.ID == last &&
		item.ID >= n.resolvedBefore[item.Key] && !n.smsProblems.Has(item.Key)
}

// staleSMSProblem reports whether item is a PROBLEM queued for SMS whose
// condition has resolved since: sending it now would report a problem that is
// over, with no RESOLVED by SMS to follow unless the RESOLVED took that route.
func (n *Notifier) staleSMSProblem(item *QueuedMessage) bool {
	if item.Channel != ChannelSMS || item.Kind != string(KindProblem) {
		return false
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	mark, resolved := n.resolvedBefore[item.Key]

	return resolved && item.ID < mark
}

// enqueue queues a message.
func (n *Notifier) enqueue(msg Message, channel, text string) {
	n.enqueueTracked(msg, channel, text, nil)
}

// enqueueTracked queues a message and, when err says its send was abandoned
// while still in progress, marks it in flight in the same lock step: a Merge
// cannot renumber the message in between and leave the mark on a stale ID.
func (n *Notifier) enqueueTracked(msg Message, channel, text string, err error) {
	n.mu.Lock()

	id := n.nextID
	n.queue = append(n.queue, QueuedMessage{
		ID:        id,
		CreatedAt: n.opts.Now(),
		Channel:   channel,
		Text:      text,
		Kind:      string(queuedKind(msg.Kind)),
		Check:     msg.Check,
		Key:       msg.Key,
		Plain:     msg.Text,
		Since:     msg.Since,
		Route:     msg.Route,
	})
	n.nextID++

	n.trackLocked(id, err)

	dropped := n.trim()
	n.mu.Unlock()

	n.reportDropped(dropped)
}

// queuedKind is the kind a queued message is stored under; a message without
// one is an event.
func queuedKind(k Kind) Kind {
	if k == "" {
		return KindEvent
	}

	return k
}

func (n *Notifier) reportDropped(dropped []QueuedMessage) {
	if n.opts.OnDrop == nil {
		return
	}

	for i := range dropped {
		item := &dropped[i]
		n.opts.OnDrop(*item)
	}
}

// trim drops the oldest messages beyond MaxQueue and returns them so the
// caller can report them. The caller holds the lock.
func (n *Notifier) trim() []QueuedMessage {
	extra := len(n.queue) - MaxQueue
	if extra <= 0 {
		return nil
	}

	dropped := append([]QueuedMessage(nil), n.queue[:extra]...)
	n.queue = append([]QueuedMessage(nil), n.queue[extra:]...)

	return dropped
}

func (n *Notifier) bumpAttempts(id int64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.bumpLocked(id)
}

// bumpLocked counts a failed attempt; the caller holds the lock.
func (n *Notifier) bumpLocked(id int64) {
	for i := range n.queue {
		if n.queue[i].ID == id {
			n.queue[i].Attempts++
		}
	}
}

func (n *Notifier) remove(id int64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.removeLocked(id)
}

// removeLocked drops a message from the queue; the caller holds the lock.
func (n *Notifier) removeLocked(id int64) {
	for i := range n.queue {
		if n.queue[i].ID == id {
			n.queue = append(n.queue[:i], n.queue[i+1:]...)

			return
		}
	}
}

// render returns the plain (SMS) and HTML (Telegram) forms of a message, both
// with the host prefix. The HTML form, as sent, is at most MaxMessageUnits
// UTF-16 code units; the body is truncated with an ellipsis if needed.
func (n *Notifier) render(msg Message) (plain, rich string) {
	var label, suffix string

	switch msg.Kind {
	case KindProblem:
		label = "PROBLEM: "
		if !msg.Since.IsZero() {
			suffix = " (since " + n.sinceText(msg.Since) + ")"
		}
	case KindResolved:
		label = "RESOLVED: "
	case KindEvent:
	}

	prefix := n.opts.Host + ": "
	richPrefix := html.EscapeString(prefix + label)
	richSuffix := html.EscapeString(suffix)
	budget := MaxMessageUnits - utf16Len(richPrefix) - utf16Len(richSuffix)
	text := truncate(strings.ToValidUTF8(msg.Text, "\uFFFD"), budget)

	plain = prefix + label + text + suffix
	rich = richPrefix + html.EscapeString(text) + richSuffix

	return plain, rich
}

// sinceText renders the start of a streak with formatSince at the
// notifier's now.
func (n *Notifier) sinceText(since time.Time) string {
	return formatSince(since, n.opts.Now())
}

// formatSince renders the start of a streak as HH:MM, with the date ("02 Jan
// 15:04") when it began on an earlier day than now; days are taken in since's
// own zone (the host's).
func formatSince(since, now time.Time) string {
	sy, sm, sd := since.Date()
	ny, nm, nd := now.In(since.Location()).Date()
	if sy == ny && sm == nm && sd == nd {
		return since.Format("15:04")
	}

	return since.Format("02 Jan 15:04")
}

// utf16Len returns the length of s in UTF-16 code units, the unit Telegram
// measures message length in.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}

	return n
}

// truncate shortens s so that its HTML-escaped form is at most limit UTF-16
// code units, ending in an ellipsis when it had to cut. It cuts between
// runes, so neither a surrogate pair nor an HTML entity is split.
func truncate(s string, limit int) string {
	if limit < 1 {
		return ellipsis
	}

	if utf16Len(html.EscapeString(s)) <= limit {
		return s
	}

	used := utf16Len(ellipsis)

	for i, r := range s {
		used += utf16Len(html.EscapeString(string(r)))
		if used > limit {
			return s[:i] + ellipsis
		}
	}

	return s
}
