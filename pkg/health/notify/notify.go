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
}

// QueuedMessage is a message awaiting delivery (data-model "QueuedMessage").
type QueuedMessage struct {
	ID        int64
	CreatedAt time.Time
	Channel   string
	Text      string
	Attempts  int
}

// Options configures a Notifier.
type Options struct {
	// Host is the prefix of every pushed message.
	Host string
	// Mode is the default delivery mode; empty means tlg.
	Mode     Mode
	Telegram Sender
	// SMS may be nil when no SMS backend is configured.
	SMS Sender
	// OnCheckerError is called once when mode sms degrades to tlg.
	OnCheckerError func(key, text string)
	// OnDrop is called for every message dropped from a full queue.
	OnDrop func(QueuedMessage)
	// Now defaults to time.Now.
	Now func() time.Time
}

// Notifier formats, routes and queues pushed messages.
type Notifier struct {
	opts Options

	mu       sync.Mutex
	queue    []QueuedMessage
	nextID   int64
	offline  bool
	degraded bool
	// inflight are the queued messages whose abandoned send is still in
	// progress, by ID.
	inflight map[int64]*flight
}

// New creates a Notifier.
func New(opts Options) *Notifier {
	if opts.Mode == "" {
		opts.Mode = ModeTelegram
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Notifier{opts: opts, nextID: 1, inflight: map[int64]*flight{}}
}

// SetOffline marks the outside as known down: Telegram messages are queued
// silently (FR-009) and Flush does nothing until it is cleared.
func (n *Notifier) SetOffline(offline bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.offline = offline
}

// Notify delivers a message. Failed deliveries are queued, so the returned
// error is reserved for future use and is currently always nil.
func (n *Notifier) Notify(ctx context.Context, msg Message) error {
	mode := n.resolveMode(msg)
	plain, rich := n.render(msg)

	n.mu.Lock()
	hook := n.degrade(&mode)
	offline := n.offline
	n.mu.Unlock()

	if hook != nil {
		hook(SMSUnavailableKey, "SMS alerts are configured but no SMS backend is available; using Telegram")
	}

	switch mode {
	case ModeSMS:
		n.sendSMS(ctx, plain)
	case ModeTelegramOrSMS:
		n.sendTelegramOrSMS(ctx, plain, rich, offline)
	case ModeTelegram:
		n.sendTelegram(ctx, rich, offline)
	}

	return nil
}

// Flush retries queued messages in order, per channel. It stops a channel at
// its first failure, or at a message whose abandoned send is still in
// progress, so ordering is kept and nothing is sent twice; it does nothing
// for Telegram while offline.
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

	for _, item := range pending {
		if blocked[item.Channel] {
			continue
		}

		if n.inFlight(item.ID) {
			blocked[item.Channel] = true

			continue
		}

		if err := n.send(ctx, item.Channel, item.Text); err != nil {
			blocked[item.Channel] = true

			n.bumpAttempts(item.ID)
			n.track(item.ID, err)

			continue
		}

		n.remove(item.ID)
	}
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

	for _, item := range items {
		n.nextID = max(n.nextID, item.ID+1)
	}

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
	for _, item := range stored {
		top = max(top, item.ID)
	}

	merged := append([]QueuedMessage(nil), stored...)
	ids := make(map[int64]int64, len(n.queue))
	for _, item := range n.queue {
		top++
		ids[item.ID] = top
		item.ID = top
		merged = append(merged, item)
	}
	n.renumberLocked(ids)

	n.queue = merged
	n.nextID = max(n.nextID, top+1)
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
	for _, item := range n.queue {
		if item.CreatedAt.Before(cutoff) {
			dropped = append(dropped, item)
		} else {
			kept = append(kept, item)
		}
	}
	n.queue = kept
	n.mu.Unlock()

	n.reportDropped(dropped)

	return dropped
}

func (n *Notifier) send(ctx context.Context, channel, text string) error {
	sender := n.opts.Telegram
	if channel == ChannelSMS {
		sender = n.opts.SMS
	}

	if err := sender.Send(ctx, text); err != nil {
		return fmt.Errorf("send %s: %w", channel, err)
	}

	return nil
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

func (n *Notifier) sendSMS(ctx context.Context, plain string) {
	if err := n.opts.SMS.Send(ctx, plain); err != nil {
		n.enqueueTracked(ChannelSMS, plain, err)
	}
}

func (n *Notifier) sendTelegram(ctx context.Context, rich string, offline bool) {
	if offline || n.opts.Telegram == nil {
		n.enqueue(ChannelTelegram, rich)

		return
	}

	if err := n.opts.Telegram.Send(ctx, rich); err != nil {
		n.enqueueTracked(ChannelTelegram, rich, err)
	}
}

func (n *Notifier) sendTelegramOrSMS(ctx context.Context, plain, rich string, offline bool) {
	if n.opts.SMS == nil {
		n.sendTelegram(ctx, rich, offline)

		return
	}

	var tlgErr error
	if !offline && n.opts.Telegram != nil {
		if tlgErr = n.opts.Telegram.Send(ctx, rich); tlgErr == nil {
			return
		}
		// Past the tick's deadline the SMS lane refuses too: queue the
		// Telegram copy. A busy lane (an abandoned Telegram send still hangs,
		// a sign Telegram is down) falls back to SMS like a failure.
		if errors.Is(tlgErr, ErrPastDeadline) {
			n.enqueue(ChannelTelegram, rich)

			return
		}
	}

	if err := n.opts.SMS.Send(ctx, plain); err != nil {
		// The queued Telegram copy follows an abandoned Telegram send.
		n.enqueueTracked(ChannelTelegram, rich, tlgErr)
	}
}

// enqueue queues a message.
func (n *Notifier) enqueue(channel, text string) {
	n.enqueueTracked(channel, text, nil)
}

// enqueueTracked queues a message and, when err says its send was abandoned
// while still in progress, marks it in flight in the same lock step: a Merge
// cannot renumber the message in between and leave the mark on a stale ID.
func (n *Notifier) enqueueTracked(channel, text string, err error) {
	n.mu.Lock()

	id := n.nextID
	n.queue = append(n.queue, QueuedMessage{
		ID:        id,
		CreatedAt: n.opts.Now(),
		Channel:   channel,
		Text:      text,
	})
	n.nextID++

	n.trackLocked(id, err)

	dropped := n.trim()
	n.mu.Unlock()

	n.reportDropped(dropped)
}

func (n *Notifier) reportDropped(dropped []QueuedMessage) {
	if n.opts.OnDrop == nil {
		return
	}

	for _, item := range dropped {
		n.opts.OnDrop(item)
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

// sinceText renders the start of a streak as HH:MM, with the date ("02 Jan
// 15:04") when it began on an earlier day than now; days are taken in since's
// own zone (the host's).
func (n *Notifier) sinceText(since time.Time) string {
	sy, sm, sd := since.Date()
	ny, nm, nd := n.opts.Now().In(since.Location()).Date()
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
