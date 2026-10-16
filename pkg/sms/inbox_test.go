package sms

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
)

const testNumber = "+10000000000"

type fakeInbox struct {
	msgs       []InboxMessage
	listErr    error
	markErr    error
	markedSets [][]int64
}

func (f *fakeInbox) Unprocessed(_ context.Context) ([]InboxMessage, error) {
	return f.msgs, f.listErr
}

func (f *fakeInbox) MarkProcessed(_ context.Context, ids []int64) error {
	f.markedSets = append(f.markedSets, ids)

	return f.markErr
}

func (f *fakeInbox) marked() map[int64]bool {
	out := map[int64]bool{}
	for _, s := range f.markedSets {
		for _, id := range s {
			out[id] = true
		}
	}

	return out
}

type fakeMatcher struct {
	match   func(sender, text string) bool
	deferTo func(sender string) bool
}

func (m *fakeMatcher) Match(sender, text string, _ time.Time) bool {
	return m.match != nil && m.match(sender, text)
}

func (m *fakeMatcher) Defer(sender string) bool {
	return m.deferTo != nil && m.deferTo(sender)
}

func TestInboxWorker_DeferredSenderStaysUnprocessed(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "probe-token", ReceivingDateTime: received()},
		InboxMessage{ID: 2, SenderNumber: "+10000000001", TextDecoded: "other", ReceivingDateTime: received()},
	)
	deferring := true
	e.match.deferTo = func(sender string) bool { return deferring && sender == testNumber }
	e.match.match = func(_, text string) bool { return text == "probe-token" }

	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !strings.HasSuffix(e.pub.calls[0].msg, "other") {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	if m := e.inbox.marked(); m[1] || !m[2] {
		t.Fatalf("marked = %v, want the deferred message left unprocessed", m)
	}

	deferring = false
	e.inbox.msgs = e.inbox.msgs[:1]
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !e.inbox.marked()[1] {
		t.Fatalf(
			"calls = %+v, marked = %v; want the reply consumed once no longer deferred",
			e.pub.calls,
			e.inbox.marked(),
		)
	}
}

func TestMaskPhone(t *testing.T) {
	tests := map[string]string{
		"":             "",
		"7":            "*",
		"42":           "**",
		"+10000000099": "**********99",
		"BANK":         "**NK",
	}
	for in, want := range tests {
		if got := MaskPhone(in); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUDH(t *testing.T) {
	tests := []struct {
		name string
		udh  string
		want udhInfo
		ok   bool
	}{
		{"empty", "", udhInfo{}, false},
		{"8-bit", "050003CC0201", udhInfo{ref: 0xCC, total: 2, seq: 1}, true},
		{"lowercase", "050003cc0202", udhInfo{ref: 0xCC, total: 2, seq: 2}, true},
		{"16-bit", "06080412340302", udhInfo{ref: 0x1234, total: 3, seq: 2}, true},
		{"after other element", "0801010000030702" + "02", udhInfo{ref: 7, total: 2, seq: 2}, true},
		{"other IEI", "0A0101", udhInfo{}, false},
		{"bad hex", "ZZ", udhInfo{}, false},
		{"truncated", "050003CC", udhInfo{}, false},
		{"seq beyond total", "050003CC0203", udhInfo{}, false},
		{"zero total", "050003CC0001", udhInfo{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseUDH(tt.udh)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Errorf("parseUDH(%q) = %+v, %v; want %+v, %v", tt.udh, got, ok, tt.want, tt.ok)
			}
		})
	}
}

type inboxEnv struct {
	inbox *fakeInbox
	pub   *fakePublisher
	match *fakeMatcher
	now   time.Time
	w     *InboxWorker
}

func newInboxEnv(msgs ...InboxMessage) *inboxEnv {
	e := &inboxEnv{
		inbox: &fakeInbox{msgs: msgs},
		pub:   &fakePublisher{},
		match: &fakeMatcher{},
		now:   time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
	}
	e.w = NewInboxWorker(zap.NewNop(), e.pub, 42, e.inbox, e.match, func() time.Time { return e.now })

	return e
}

func received() time.Time { return time.Date(2026, 1, 2, 11, 59, 0, 0, time.UTC) }

func TestInboxWorker_NameDuration(t *testing.T) {
	e := newInboxEnv()
	if e.w.Name() != "sms-inbox" {
		t.Errorf("Name() = %q", e.w.Name())
	}
	if e.w.Duration() != 15*time.Second {
		t.Errorf("Duration() = %v", e.w.Duration())
	}
}

func TestInboxWorker_SingleMessage(t *testing.T) {
	at := received()
	e := newInboxEnv(InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "hello", ReceivingDateTime: at})
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || e.pub.calls[0].recipientID != 42 {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	msg := e.pub.calls[0].msg
	for _, want := range []string{"<b>SMS</b>", "<i>From</i>: " + testNumber, "<i>Date</i>: " + at.Format(time.RFC1123), "hello"} {
		if !strings.Contains(msg, want) {
			t.Errorf("msg %q lacks %q", msg, want)
		}
	}
	if !e.inbox.marked()[1] {
		t.Error("message 1 not marked processed")
	}
}

func TestInboxWorker_OtherUDHIsSingle(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "x", UDH: "0A0101", ReceivingDateTime: received()},
	)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 {
		t.Fatalf("calls = %d", len(e.pub.calls))
	}
}

func TestInboxWorker_Multipart(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{
			ID:                2,
			SenderNumber:      testNumber,
			TextDecoded:       "world",
			UDH:               "050003CC0202",
			ReceivingDateTime: received(),
		},
		InboxMessage{
			ID:                1,
			SenderNumber:      testNumber,
			TextDecoded:       "hello ",
			UDH:               "050003CC0201",
			ReceivingDateTime: received(),
		},
	)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !strings.HasSuffix(e.pub.calls[0].msg, "hello world") {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	if strings.Contains(e.pub.calls[0].msg, "incomplete") {
		t.Error("complete message flagged incomplete")
	}
	if m := e.inbox.marked(); !m[1] || !m[2] {
		t.Errorf("marked = %v", m)
	}
}

func TestInboxWorker_Multipart16Bit(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{
			ID:                1,
			SenderNumber:      testNumber,
			TextDecoded:       "a",
			UDH:               "06080412340201",
			ReceivingDateTime: received(),
		},
		InboxMessage{
			ID:                2,
			SenderNumber:      testNumber,
			TextDecoded:       "b",
			UDH:               "06080412340202",
			ReceivingDateTime: received(),
		},
	)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !strings.HasSuffix(e.pub.calls[0].msg, "ab") {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
}

func TestInboxWorker_IncompleteWaitsThenForwards(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{
			ID:                1,
			SenderNumber:      testNumber,
			TextDecoded:       "part one",
			UDH:               "050003CC0301",
			ReceivingDateTime: received(),
		},
	)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 0 || len(e.inbox.markedSets) != 0 {
		t.Fatalf("forwarded too early: %+v %v", e.pub.calls, e.inbox.markedSets)
	}

	e.now = received().Add(10 * time.Minute)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !strings.Contains(e.pub.calls[0].msg, "part one") ||
		!strings.Contains(e.pub.calls[0].msg, "incomplete") {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	if !e.inbox.marked()[1] {
		t.Error("not marked")
	}
}

func TestInboxWorker_GroupsBySenderAndRef(t *testing.T) {
	at := received()
	e := newInboxEnv(
		InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "a1", UDH: "050003CC0201", ReceivingDateTime: at},
		InboxMessage{
			ID:                2,
			SenderNumber:      "+10000000001",
			TextDecoded:       "b1",
			UDH:               "050003CC0201",
			ReceivingDateTime: at,
		},
		InboxMessage{ID: 3, SenderNumber: testNumber, TextDecoded: "a2", UDH: "050003CC0202", ReceivingDateTime: at},
		InboxMessage{ID: 4, SenderNumber: testNumber, TextDecoded: "c1", UDH: "050003DD0201", ReceivingDateTime: at},
		InboxMessage{
			ID:                5,
			SenderNumber:      "+10000000001",
			TextDecoded:       "b2",
			UDH:               "050003CC0202",
			ReceivingDateTime: at,
		},
		InboxMessage{ID: 6, SenderNumber: testNumber, TextDecoded: "c2", UDH: "050003DD0202", ReceivingDateTime: at},
	)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 3 {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	want := map[string]string{"a1a2": testNumber, "b1b2": "+10000000001", "c1c2": testNumber}
	for _, c := range e.pub.calls {
		found := false
		for text, from := range want {
			if strings.HasSuffix(c.msg, text) && strings.Contains(c.msg, "<i>From</i>: "+from+"\n") {
				found = true
			}
		}
		if !found {
			t.Errorf("unexpected message %q", c.msg)
		}
	}
}

func TestInboxWorker_EscapesHTML(t *testing.T) {
	e := newInboxEnv(InboxMessage{ID: 1, SenderNumber: "<+1>", TextDecoded: "a<b & c", ReceivingDateTime: received()})
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := e.pub.calls[0].msg
	if !strings.HasSuffix(msg, "a&lt;b &amp; c") || !strings.Contains(msg, "&lt;+1&gt;") {
		t.Errorf("msg = %q", msg)
	}
}

func TestInboxWorker_TruncatesLongText(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", 5000), strings.Repeat("<", 3000)} {
		e := newInboxEnv(
			InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: body, ReceivingDateTime: received()},
		)
		if err := e.w.Do(context.Background()); err != nil {
			t.Fatal(err)
		}
		msg := e.pub.calls[0].msg
		if n := utf16Len(msg); n > 4096 {
			t.Errorf("len = %d, want <= 4096", n)
		}
		if !strings.HasSuffix(msg, "…") {
			t.Error("no ellipsis")
		}
	}
}

func TestInboxWorker_TruncatesIncompleteKeepsNote(t *testing.T) {
	e := newInboxEnv(InboxMessage{
		ID: 1, SenderNumber: testNumber, TextDecoded: strings.Repeat("x", 5000),
		UDH: "050003CC0301", ReceivingDateTime: received().Add(-time.Hour),
	})
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := e.pub.calls[0].msg
	if utf16Len(msg) > 4096 || !strings.Contains(msg, "incomplete") {
		t.Errorf("bad msg len=%d", utf16Len(msg))
	}
}

func TestInboxWorker_DuplicatePart(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{
			ID:                1,
			SenderNumber:      testNumber,
			TextDecoded:       "a",
			UDH:               "050003CC0201",
			ReceivingDateTime: received(),
		},
		InboxMessage{
			ID:                3,
			SenderNumber:      testNumber,
			TextDecoded:       "a",
			UDH:               "050003CC0201",
			ReceivingDateTime: received(),
		},
		InboxMessage{
			ID:                2,
			SenderNumber:      testNumber,
			TextDecoded:       "b",
			UDH:               "050003CC0202",
			ReceivingDateTime: received(),
		},
	)
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !strings.HasSuffix(e.pub.calls[0].msg, "ab") {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	if m := e.inbox.marked(); !m[1] || !m[2] || !m[3] {
		t.Errorf("marked = %v", m)
	}
}

func TestInboxWorker_ProbeMatchedNotForwarded(t *testing.T) {
	e := newInboxEnv(
		InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "probe-token", ReceivingDateTime: received()},
		InboxMessage{ID: 2, SenderNumber: testNumber, TextDecoded: "other", ReceivingDateTime: received()},
	)
	e.match.match = func(_, text string) bool { return text == "probe-token" }
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 || !strings.HasSuffix(e.pub.calls[0].msg, "other") {
		t.Fatalf("calls = %+v", e.pub.calls)
	}
	if m := e.inbox.marked(); !m[1] || !m[2] {
		t.Errorf("marked = %v", m)
	}
}

func TestInboxWorker_NilMatcher(t *testing.T) {
	e := newInboxEnv(InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "x", ReceivingDateTime: received()})
	e.w = NewInboxWorker(zap.NewNop(), e.pub, 42, e.inbox, nil, func() time.Time { return e.now })
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.calls) != 1 {
		t.Fatalf("calls = %d", len(e.pub.calls))
	}
}

func TestInboxWorker_PublishFailureKeepsUnprocessed(t *testing.T) {
	boom := errors.New("boom")
	e := newInboxEnv(
		InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "a", ReceivingDateTime: received()},
		InboxMessage{ID: 2, SenderNumber: testNumber, TextDecoded: "b", ReceivingDateTime: received()},
	)
	e.pub.errs = []error{boom}
	if err := e.w.Do(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if m := e.inbox.marked(); m[1] || !m[2] {
		t.Errorf("marked = %v", m)
	}
}

func TestInboxWorker_Errors(t *testing.T) {
	listErr := errors.New("list")
	e := newInboxEnv()
	e.inbox.listErr = listErr
	if err := e.w.Do(context.Background()); !errors.Is(err, listErr) {
		t.Errorf("err = %v", err)
	}

	markErr := errors.New("mark")
	e = newInboxEnv(InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: "a", ReceivingDateTime: received()})
	e.inbox.markErr = markErr
	if err := e.w.Do(context.Background()); !errors.Is(err, markErr) {
		t.Errorf("err = %v", err)
	}
}

func TestInboxWorker_TruncatesEmojiByUTF16Units(t *testing.T) {
	// 3000 emoji are 3000 runes but 6000 UTF-16 units.
	body := strings.Repeat("\U0001F600", 3000)
	e := newInboxEnv(InboxMessage{ID: 1, SenderNumber: testNumber, TextDecoded: body, ReceivingDateTime: received()})
	if err := e.w.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	msg := e.pub.calls[0].msg
	if n := utf16Len(msg); n > 4096 {
		t.Errorf("utf16 len = %d, want <= 4096", n)
	}
	if !strings.HasSuffix(msg, "\U0001F600…") {
		t.Error("expected truncated emoji body with ellipsis")
	}
}

func TestInboxWorker_InvalidUTF8Replaced(t *testing.T) {
	long := "ok\xff" + strings.Repeat("y", 5000)
	for _, body := range []string{"a\xffb", long} {
		e := newInboxEnv(InboxMessage{
			ID: 1, SenderNumber: "+1\xfe", TextDecoded: body, ReceivingDateTime: received(),
		})
		if err := e.w.Do(context.Background()); err != nil {
			t.Fatal(err)
		}
		msg := e.pub.calls[0].msg
		if !utf8.ValidString(msg) || !strings.Contains(msg, "\uFFFD") {
			t.Errorf("msg not sanitised: %q", msg[:40])
		}
	}
}
