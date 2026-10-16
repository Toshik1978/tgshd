package sms

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"go.uber.org/zap"
)

// incompleteTimeout is how long a partially received multipart message waits
// for its missing parts before being forwarded as is.
const incompleteTimeout = 10 * time.Minute

// maxMessageUnits is Telegram's message length limit, in UTF-16 code units.
const maxMessageUnits = 4096

const incompleteNote = "\n\n<i>(incomplete: some parts were not received)</i>"

const (
	iei8BitRef  = 0x00
	iei16BitRef = 0x08
)

// InboxMessage is one received SMS row of the gammu inbox.
type InboxMessage struct {
	ID                int64
	SenderNumber      string
	TextDecoded       string
	UDH               string
	ReceivingDateTime time.Time
}

// Inbox is the source of received, not yet processed SMS messages.
type Inbox interface {
	Unprocessed(ctx context.Context) ([]InboxMessage, error)
	MarkProcessed(ctx context.Context, ids []int64) error
}

// ProbeMatcher recognises replies to the modem probe so they are not forwarded.
type ProbeMatcher interface {
	// Defer reports that messages from sender must stay unprocessed for now,
	// because the probe cannot judge a reply yet (its state is not loaded).
	Defer(sender string) bool
	Match(sender, text string, at time.Time) bool
}

// InboxWorker forwards SMS messages from the gammu inbox to Telegram.
type InboxWorker struct {
	logger    *zap.Logger
	publisher Publisher
	chatID    int64
	inbox     Inbox
	matcher   ProbeMatcher
	now       func() time.Time
}

// NewInboxWorker instantiates the inbox worker. The matcher and clock may be nil.
func NewInboxWorker(
	logger *zap.Logger,
	publisher Publisher,
	chatID int64,
	inbox Inbox,
	matcher ProbeMatcher,
	now func() time.Time,
) *InboxWorker {
	if now == nil {
		now = time.Now
	}

	return &InboxWorker{
		logger:    logger,
		publisher: publisher,
		chatID:    chatID,
		inbox:     inbox,
		matcher:   matcher,
		now:       now,
	}
}

func (w *InboxWorker) Name() string {
	return "sms-inbox"
}

func (w *InboxWorker) Duration() time.Duration {
	return 15 * time.Second
}

func (w *InboxWorker) Do(ctx context.Context) error {
	msgs, err := w.inbox.Unprocessed(ctx)
	if err != nil {
		return fmt.Errorf("read sms inbox: %w", err)
	}

	var errs []error
	for _, g := range groupInbox(msgs) {
		if err := w.handle(ctx, g); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (w *InboxWorker) handle(ctx context.Context, g *inboxGroup) error {
	if w.matcher != nil && w.matcher.Defer(g.sender) {
		w.logger.Debug("sms deferred until the modem probe is ready", zap.String("sender", MaskPhone(g.sender)))

		return nil
	}

	complete := g.complete()
	if !complete && w.now().Sub(g.first) < incompleteTimeout {
		return nil
	}

	if g.dups > 0 {
		w.logger.Debug("dropped duplicate sms parts",
			zap.String("sender", MaskPhone(g.sender)), zap.Int("count", g.dups))
	}

	if err := w.deliver(ctx, g, complete); err != nil {
		return err
	}

	// If this fails after a successful publish the rows stay unprocessed and
	// the message is sent again on the next tick (at-least-once delivery).
	if err := w.inbox.MarkProcessed(ctx, g.ids); err != nil {
		return fmt.Errorf("mark sms processed: %w", err)
	}

	return nil
}

// deliver consumes a probe reply or forwards the message to Telegram.
func (w *InboxWorker) deliver(ctx context.Context, g *inboxGroup, complete bool) error {
	text := g.text()
	if w.matcher != nil && complete && w.matcher.Match(g.sender, text, g.last) {
		w.logger.Debug("sms consumed as modem probe reply", zap.String("sender", MaskPhone(g.sender)))

		return nil
	}

	note := ""
	if !complete {
		w.logger.Info("forwarding incomplete multipart sms after timeout", zap.String("sender", MaskPhone(g.sender)))
		note = incompleteNote
	}
	if err := w.publisher.Publish(ctx, w.chatID, formatInbox(g.sender, g.last, text, note)); err != nil {
		return fmt.Errorf("publish sms from %s: %w", MaskPhone(g.sender), err)
	}

	return nil
}

// visiblePhoneRunes is how many trailing characters MaskPhone keeps.
const visiblePhoneRunes = 2

// MaskPhone hides a phone number (or sender name) for logs, keeping only its
// last two characters.
func MaskPhone(phone string) string {
	r := []rune(phone)
	if len(r) <= visiblePhoneRunes {
		return strings.Repeat("*", len(r))
	}

	return strings.Repeat("*", len(r)-visiblePhoneRunes) + string(r[len(r)-visiblePhoneRunes:])
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

// escapeText replaces invalid UTF-8 and HTML-escapes s.
func escapeText(s string) string {
	return html.EscapeString(strings.ToValidUTF8(s, "\uFFFD"))
}

// formatInbox renders the Telegram HTML message. The final string never
// exceeds maxMessageUnits UTF-16 code units; the body is truncated (with an
// ellipsis) if needed.
func formatInbox(sender string, at time.Time, text, note string) string {
	head := "<b>SMS</b>\n\n"
	head += fmt.Sprintf("<i>From</i>: %s\n", escapeText(sender))
	head += fmt.Sprintf("<i>Date</i>: %s\n\n", at.Format(time.RFC1123))

	budget := maxMessageUnits - utf16Len(head) - utf16Len(note)
	body := escapeText(text)
	if utf16Len(body) > budget {
		var b strings.Builder
		used := 1 // reserve room for the ellipsis
		for _, r := range strings.ToValidUTF8(text, "\uFFFD") {
			esc := html.EscapeString(string(r))
			used += utf16Len(esc)
			if used > budget {
				break
			}
			b.WriteString(esc)
		}
		body = b.String() + "…"
	}

	return head + body + note
}

type udhInfo struct {
	ref   int
	total int
	seq   int
}

// parseUDH extracts the concatenation information element (8 or 16 bit
// reference) from a hex-encoded user data header.
func parseUDH(udh string) (udhInfo, bool) {
	raw, err := hex.DecodeString(udh)
	if err != nil || len(raw) == 0 || int(raw[0]) > len(raw)-1 {
		return udhInfo{}, false
	}

	elems := raw[1 : 1+int(raw[0])]
	for len(elems) >= 2 {
		n := int(elems[1])
		if len(elems) < 2+n {
			return udhInfo{}, false
		}
		info, ok := concatElement(elems[0], elems[2:2+n])
		elems = elems[2+n:]
		if !ok {
			continue
		}

		if info.total == 0 || info.seq == 0 || info.seq > info.total {
			return udhInfo{}, false
		}

		return info, true
	}

	return udhInfo{}, false
}

// concatElement decodes a concatenation information element, if iei is one.
func concatElement(iei byte, data []byte) (udhInfo, bool) {
	switch {
	case iei == iei8BitRef && len(data) == 3:
		return udhInfo{ref: int(data[0]), total: int(data[1]), seq: int(data[2])}, true
	case iei == iei16BitRef && len(data) == 4:
		return udhInfo{ref: int(data[0])<<8 | int(data[1]), total: int(data[2]), seq: int(data[3])}, true
	default:
		return udhInfo{}, false
	}
}

type inboxGroup struct {
	sender string
	total  int // 0 for a single-part message
	parts  map[int]string
	ids    []int64
	first  time.Time
	last   time.Time
	dups   int
}

func (g *inboxGroup) add(m *InboxMessage, seq int) {
	g.ids = append(g.ids, m.ID)
	if _, ok := g.parts[seq]; ok {
		g.dups++
	} else {
		g.parts[seq] = m.TextDecoded
	}
	if g.first.IsZero() || m.ReceivingDateTime.Before(g.first) {
		g.first = m.ReceivingDateTime
	}
	if m.ReceivingDateTime.After(g.last) {
		g.last = m.ReceivingDateTime
	}
}

func (g *inboxGroup) complete() bool {
	return g.total == 0 || len(g.parts) >= g.total
}

func (g *inboxGroup) text() string {
	seqs := make([]int, 0, len(g.parts))
	for s := range g.parts {
		seqs = append(seqs, s)
	}
	slices.Sort(seqs)

	var b strings.Builder
	for _, s := range seqs {
		b.WriteString(g.parts[s])
	}

	return b.String()
}

type groupKey struct {
	sender string
	ref    int
	total  int
}

// groupInbox groups concatenated parts by sender and reference; everything
// else becomes a single-part group. The result keeps first-seen order.
func groupInbox(msgs []InboxMessage) []*inboxGroup {
	var out []*inboxGroup
	byKey := map[groupKey]*inboxGroup{}

	for i := range msgs {
		m := &msgs[i]
		info, ok := parseUDH(m.UDH)
		if !ok {
			g := &inboxGroup{sender: m.SenderNumber, parts: map[int]string{}}
			g.add(m, 1)
			out = append(out, g)

			continue
		}

		key := groupKey{sender: m.SenderNumber, ref: info.ref, total: info.total}
		g, found := byKey[key]
		if !found {
			g = &inboxGroup{sender: m.SenderNumber, total: info.total, parts: map[int]string{}}
			byKey[key] = g
			out = append(out, g)
		}
		g.add(m, info.seq)
	}

	return out
}
