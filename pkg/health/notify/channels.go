package notify

import (
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// kv keys of the delivery state (data-model "Delivery").
const (
	KVChannels    = "notify.channels"
	KVSMSProblems = "notify.sms_problems"
	KVUndelivered = "notify.undelivered"

	// MaxUndelivered bounds the undelivered list; PROBLEMs listed while it
	// is full are counted in Folded instead (R19).
	MaxUndelivered = 50

	undeliveredHeader = "Undelivered earlier:"

	// maxUndeliveredText caps the text of one listed PROBLEM, in UTF-16 code
	// units of its HTML form, so one long item cannot stall the list.
	maxUndeliveredText = 256
)

// sinceSuffix matches the " (since …)" suffix render puts on a PROBLEM; the
// group is the since as formatSince wrote it.
var sinceSuffix = regexp.MustCompile(` \(since (\d{2}:\d{2}|\d{2} [A-Z][a-z]{2} \d{2}:\d{2})\)$`)

// ChannelHealth is the delivery health of one channel (R17). The zero value
// is OK.
type ChannelHealth struct {
	// FailingSince is the first failed send (or message queued while
	// offline) since the last successful send; zero while OK.
	FailingSince time.Time `json:"failing_since,omitzero"`
	// Reported is set once the failure notice went out.
	Reported bool `json:"reported,omitzero"`
}

// Failing reports whether the channel is failing.
func (h *ChannelHealth) Failing() bool { return !h.FailingSince.IsZero() }

// Fail records a failed send, or a message queued while offline, at now. An
// already failing channel keeps its first since.
func (h *ChannelHealth) Fail(now time.Time) {
	if !h.Failing() {
		h.FailingSince = now
	}
}

// Succeed records a successful send: the channel is OK again.
func (h *ChannelHealth) Succeed() { *h = ChannelHealth{} }

// NoticeDue reports whether the failure notice is due at now: failing for at
// least after and not yet reported.
func (h *ChannelHealth) NoticeDue(now time.Time, after time.Duration) bool {
	return h.Failing() && !h.Reported && now.Sub(h.FailingSince) >= after
}

// MarkReported records that the failure notice went out. It does nothing
// for an OK channel.
func (h *ChannelHealth) MarkReported() {
	if h.Failing() {
		h.Reported = true
	}
}

// Channels is the health of each channel by name (kv notify.channels).
type Channels map[string]ChannelHealth

// Encode returns the kv value.
func (c Channels) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode channels: %w", err)
	}

	return string(b), nil
}

// DecodeChannels parses a kv value produced by Encode; empty means none.
func DecodeChannels(s string) (Channels, error) {
	c := Channels{}
	if s == "" {
		return c, nil
	}
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, fmt.Errorf("decode channels: %w", err)
	}

	return c, nil
}

// SMSProblems is the set of condition keys whose PROBLEM went by SMS through
// the fallback (R18, kv notify.sms_problems). The zero value is empty.
type SMSProblems struct {
	keys map[string]struct{}
}

// Add puts key in the set.
func (s *SMSProblems) Add(key string) {
	if s.keys == nil {
		s.keys = map[string]struct{}{}
	}
	s.keys[key] = struct{}{}
}

// Has reports whether key is in the set.
func (s *SMSProblems) Has(key string) bool {
	_, ok := s.keys[key]

	return ok
}

// Remove takes key out of the set and reports whether it was there.
func (s *SMSProblems) Remove(key string) bool {
	if !s.Has(key) {
		return false
	}
	delete(s.keys, key)

	return true
}

// Len returns the number of keys.
func (s *SMSProblems) Len() int { return len(s.keys) }

// Encode returns the kv value: the keys as a sorted JSON array.
func (s *SMSProblems) Encode() (string, error) {
	keys := slices.Sorted(maps.Keys(s.keys))
	if keys == nil {
		keys = []string{}
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("encode sms problems: %w", err)
	}

	return string(b), nil
}

// DecodeSMSProblems parses a kv value produced by Encode; empty means none.
func DecodeSMSProblems(v string) (SMSProblems, error) {
	var s SMSProblems
	if v == "" {
		return s, nil
	}

	var keys []string
	if err := json.Unmarshal([]byte(v), &keys); err != nil {
		return s, fmt.Errorf("decode sms problems: %w", err)
	}
	for _, k := range keys {
		s.Add(k)
	}

	return s, nil
}

// UndeliveredProblem is a PROBLEM dropped from the queue undelivered whose
// condition has since resolved or started a new streak (R19).
type UndeliveredProblem struct {
	Key string `json:"key"`
	// Text is the condition text, plain (no host prefix, label or since).
	Text string `json:"text"`
	// Since is the start of the problem streak; zero omits it.
	Since     time.Time `json:"since,omitzero"`
	DroppedAt time.Time `json:"dropped_at,omitzero"`
}

// Undelivered is the list of undelivered PROBLEMs (kv notify.undelivered).
// Items holds at most MaxUndelivered, oldest first; PROBLEMs listed while it
// is full are counted in Folded. Nothing leaves it until a delivered message
// has shown it.
type Undelivered struct {
	Items  []UndeliveredProblem `json:"items"`
	Folded int                  `json:"folded"`
}

// Shown is what a Prefix showed: the oldest Items items and Folded of the
// counted PROBLEMs.
type Shown struct {
	Items  int
	Folded int
}

// Add lists p, or counts it in Folded when the list is full.
func (u *Undelivered) Add(p UndeliveredProblem) {
	if len(u.Items) >= MaxUndelivered {
		u.Folded++

		return
	}
	u.Items = append(u.Items, p)
}

// Empty reports whether there is nothing to show.
func (u *Undelivered) Empty() bool { return len(u.Items) == 0 && u.Folded == 0 }

// Prefix returns the HTML block to put ahead of the next Telegram message,
// at most limit UTF-16 code units including its trailing blank line, and
// what it shows. It is "Undelivered earlier:", then one line per item, oldest
// first, as many as fit, then "… and N more undelivered PROBLEMs" when Folded
// > 0; the counted line's space is reserved before the item lines. It is
// empty when there is nothing to show or no room for it. now picks the since
// format, as in the messages themselves.
func (u *Undelivered) Prefix(limit int, now time.Time) (string, Shown) {
	if u.Empty() {
		return "", Shown{}
	}

	head := undeliveredHeader + "\n"
	var tail string
	if u.Folded > 0 {
		tail = "… and " + strconv.Itoa(u.Folded) + " more undelivered PROBLEMs\n"
	}

	used := utf16Len(head) + utf16Len(tail) + utf16Len("\n")
	if used > limit {
		return "", Shown{}
	}

	var b strings.Builder
	b.WriteString(head)

	n := 0
	for _, it := range u.Items {
		line := undeliveredLine(it, now)
		units := utf16Len(line)
		if used+units > limit {
			break
		}
		used += units
		b.WriteString(line)
		n++
	}

	if n == 0 && u.Folded == 0 {
		return "", Shown{}
	}

	b.WriteString(tail)
	b.WriteString("\n")

	return b.String(), Shown{Items: n, Folded: u.Folded}
}

// Ack records that a delivered message showed what Prefix returned: those
// items leave the list and Folded drops by the count shown, so items and
// folds added since the Prefix stay.
func (u *Undelivered) Ack(s Shown) {
	n := min(s.Items, len(u.Items))
	u.Items = slices.Delete(u.Items, 0, n)
	u.Folded = max(u.Folded-s.Folded, 0)
}

// undeliveredFrom returns the listing of a queued PROBLEM dropped at now: its
// plain text, capped at maxUndeliveredText, and its since in loc (a since
// read back from the store is UTC). A message without its plain text
// (restored from a store older than migration 3) falls back to reading them
// back from the rendered text (see renderedProblem).
func undeliveredFrom(item QueuedMessage, host string, loc *time.Location, now time.Time) UndeliveredProblem {
	text, since := item.Plain, item.Since
	if text == "" {
		text, since = renderedProblem(item, host, loc)
	}
	if !since.IsZero() {
		since = since.In(loc)
	}

	return UndeliveredProblem{
		Key:       item.Key,
		Text:      truncate(strings.ToValidUTF8(text, "\uFFFD"), maxUndeliveredText),
		Since:     since,
		DroppedAt: now,
	}
}

// renderedProblem reads the plain text and since back from a queued PROBLEM
// as render wrote it: without the host prefix, label and since suffix, the
// since read in loc relative to the message's creation. It cannot tell a
// text that itself ends in a since suffix and had no since: that suffix is
// read as the since, dated relative to the creation, so the listed line can
// differ from the text. Dropped at the 7-day expiry, a text ending in
// "(since 10:00)" is listed as "since 02 Jan 10:00", the day it was queued.
func renderedProblem(item QueuedMessage, host string, loc *time.Location) (string, time.Time) {
	text := item.Text
	if item.Channel != ChannelSMS {
		text = html.UnescapeString(text)
	}
	text = strings.TrimPrefix(text, host+": PROBLEM: ")

	var since time.Time
	if m := sinceSuffix.FindStringSubmatchIndex(text); m != nil {
		since = parseSince(text[m[2]:m[3]], item.CreatedAt.In(loc))
		if !since.IsZero() {
			text = text[:m[0]]
		}
	}

	return text, since
}

// parseSince reads back a since written by formatSince for a message created
// at ref: "15:04" on ref's day, or the day before when that is after ref (it
// was rendered just before midnight), "02 Jan 15:04" in the latest year that
// does not put it after ref. It returns zero for anything else.
func parseSince(s string, ref time.Time) time.Time {
	loc := ref.Location()
	if t, err := time.ParseInLocation("15:04", s, loc); err == nil {
		y, m, d := ref.Date()
		since := time.Date(y, m, d, t.Hour(), t.Minute(), 0, 0, loc)
		if since.After(ref) {
			since = since.AddDate(0, 0, -1)
		}

		return since
	}

	t, err := time.ParseInLocation("02 Jan 15:04", s, loc)
	if err != nil {
		return time.Time{}
	}
	since := time.Date(ref.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	if since.After(ref) {
		since = since.AddDate(-1, 0, 0)
	}

	return since
}

// undeliveredLine renders one item as an HTML line.
func undeliveredLine(it UndeliveredProblem, now time.Time) string {
	var since string
	if !it.Since.IsZero() {
		since = " (since " + formatSince(it.Since, now) + ")"
	}

	return "PROBLEM: " + html.EscapeString(strings.ToValidUTF8(it.Text, "�")) +
		html.EscapeString(since) + ", no longer open\n"
}

// Encode returns the kv value.
func (u *Undelivered) Encode() (string, error) {
	out := *u
	if out.Items == nil {
		out.Items = []UndeliveredProblem{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode undelivered: %w", err)
	}

	return string(b), nil
}

// DecodeUndelivered parses a kv value produced by Encode; empty means none.
// Items over MaxUndelivered (the newest) are counted in Folded, not lost.
func DecodeUndelivered(s string) (Undelivered, error) {
	var u Undelivered
	if s == "" {
		return u, nil
	}
	if err := json.Unmarshal([]byte(s), &u); err != nil {
		return Undelivered{}, fmt.Errorf("decode undelivered: %w", err)
	}

	u.Folded = max(u.Folded, 0)
	if extra := len(u.Items) - MaxUndelivered; extra > 0 {
		u.Items = u.Items[:MaxUndelivered]
		u.Folded += extra
	}

	return u, nil
}

// ExportChannels returns the kv value of the channel health
// (notify.channels).
func (n *Notifier) ExportChannels() (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return Channels{ChannelTelegram: n.telegram}.Encode()
}

// RestoreChannels merges the stored channel health into memory. A Telegram
// success seen since start wins: the stored failure is over. Otherwise a
// stored failure is the same outage as one seen in memory: it keeps the
// earlier since and is reported when either side reported it.
func (n *Notifier) RestoreChannels(v string) error {
	c, err := DecodeChannels(v)
	if err != nil {
		return err
	}
	stored := c[ChannelTelegram]

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.telegramOK || !stored.Failing() {
		return nil
	}

	mem := n.telegram
	if !mem.Failing() || stored.FailingSince.Before(mem.FailingSince) {
		n.telegram.FailingSince = stored.FailingSince
	}
	n.telegram.Reported = stored.Reported || mem.Reported

	return nil
}

// ExportSMSProblems returns the kv value of the SMS-sent set
// (notify.sms_problems).
func (n *Notifier) ExportSMSProblems() (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.smsProblems.Encode()
}

// RestoreSMSProblems adds the stored SMS-sent keys to memory, except those
// whose RESOLVED was seen since start: their PROBLEM is no longer news.
func (n *Notifier) RestoreSMSProblems(v string) error {
	stored, err := DecodeSMSProblems(v)
	if err != nil {
		return err
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	for key := range stored.keys {
		if _, resolved := n.resolvedBefore[key]; !resolved {
			n.smsProblems.Add(key)
		}
	}

	return nil
}

// ExportUndelivered returns the kv value of the undelivered list
// (notify.undelivered).
func (n *Notifier) ExportUndelivered() (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.undelivered.Encode()
}

// RestoreUndelivered merges the stored undelivered list into memory: the
// stored items, older, go ahead of those listed since start, except that a
// prefix held by a send in progress keeps the items it shows in front (its
// acknowledgement removes them by count). Folded counts add up, and items
// over MaxUndelivered are counted in Folded.
func (n *Notifier) RestoreUndelivered(v string) error {
	stored, err := DecodeUndelivered(v)
	if err != nil {
		return err
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	u := &n.undelivered
	at := min(n.held, len(u.Items))
	u.Items = slices.Insert(slices.Clone(u.Items), at, stored.Items...)
	u.Folded += stored.Folded
	if extra := len(u.Items) - MaxUndelivered; extra > 0 {
		u.Items = u.Items[:MaxUndelivered]
		u.Folded += extra
	}

	return nil
}
