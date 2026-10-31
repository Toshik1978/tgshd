package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

type fakeSender struct {
	sent []string
	fail bool
	// err, when set, is returned instead of sending.
	err error
	// calls counts every send, failed ones included.
	calls int
}

func (f *fakeSender) Send(_ context.Context, text string) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	if f.fail {
		return errors.New("send failed")
	}

	f.sent = append(f.sent, text)

	return nil
}

type fixture struct {
	n      *Notifier
	tlg    *fakeSender
	sms    *fakeSender
	hooked []string
}

func newFixture(t *testing.T, mode Mode, withSMS bool) *fixture {
	t.Helper()

	fx := &fixture{tlg: &fakeSender{}, sms: &fakeSender{}}
	opts := Options{
		Host:     "host1.example.org",
		Mode:     mode,
		Telegram: asTelegram(fx.tlg),
		OnCheckerError: func(key, _ string) {
			fx.hooked = append(fx.hooked, key)
		},
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}

	if withSMS {
		opts.SMS = fx.sms
	}

	fx.n = New(opts)

	return fx
}

func since() time.Time { return time.Date(2026, 1, 2, 9, 5, 0, 0, time.UTC) }

func TestFormatting(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want string
	}{
		{
			"problem",
			Message{Kind: KindProblem, Check: "disk", Text: "a<b & c", Since: since()},
			"host1.example.org: PROBLEM: a&lt;b &amp; c (since 09:05)",
		},
		{
			"problem without since",
			Message{Kind: KindProblem, Check: "disk", Text: "x"},
			"host1.example.org: PROBLEM: x",
		},
		{"resolved", Message{Kind: KindResolved, Check: "disk", Text: "ok"}, "host1.example.org: RESOLVED: ok"},
		{"event", Message{Kind: KindEvent, Check: "smart", Text: "counter rose"}, "host1.example.org: counter rose"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, ModeTelegram, false)
			if err := fx.n.Notify(context.Background(), tt.msg); err != nil {
				t.Fatal(err)
			}

			if len(fx.tlg.sent) != 1 || fx.tlg.sent[0] != tt.want {
				t.Fatalf("got %q, want %q", fx.tlg.sent, tt.want)
			}
		})
	}
}

func utf16Units(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func TestTruncate(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	long := strings.Repeat("é", 5000)

	if err := fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: long}); err != nil {
		t.Fatal(err)
	}

	got := fx.tlg.sent[0]
	if n := utf16Units(got); n != 4096 {
		t.Fatalf("message is %d units, want 4096", n)
	}

	if !strings.HasSuffix(got, "…") {
		t.Fatal("missing ellipsis")
	}

	if err := fx.n.Notify(context.Background(), Message{Kind: KindProblem, Text: long, Since: since()}); err != nil {
		t.Fatal(err)
	}

	got = fx.tlg.sent[1]
	if utf16Units(got) != 4096 || !strings.HasSuffix(got, "… (since 09:05)") {
		t.Fatalf("bad problem message (%d units)", utf16Units(got))
	}
}

func TestTruncateCountsUTF16Units(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"emoji", strings.Repeat("\U0001F600", 4000)},
		{"escaped", strings.Repeat("<", 2000) + strings.Repeat("&", 2000)},
		{"escaped boundary", strings.Repeat("a", 4050) + strings.Repeat("&", 100)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, ModeTelegram, true)
			msg := Message{Kind: KindProblem, Text: tt.text, Since: since()}

			if err := fx.n.Notify(context.Background(), msg); err != nil {
				t.Fatal(err)
			}

			got := fx.tlg.sent[0]
			if n := utf16Units(got); n > 4096 {
				t.Fatalf("message is %d units, want <= 4096", n)
			}

			if !strings.Contains(got, "…") || !utf8.ValidString(got) {
				t.Fatal("want a valid text truncated with an ellipsis")
			}

			// No entity is cut: every ampersand starts a complete entity.
			for _, part := range strings.Split(got, "&")[1:] {
				if !strings.HasPrefix(part, "amp;") && !strings.HasPrefix(part, "lt;") {
					t.Fatalf("cut entity near %q", part[:min(len(part), 8)])
				}
			}
		})
	}
}

func TestSMSRouting(t *testing.T) {
	tests := []struct {
		name    string
		mode    Mode
		msg     Message
		wantTlg int
		wantSMS int
	}{
		{"tlg mode", ModeTelegram, Message{Kind: KindEvent, Text: "x"}, 1, 0},
		{"sms mode", ModeSMS, Message{Kind: KindEvent, Text: "x"}, 0, 1},
		{"tlg-or-sms prefers tlg", ModeTelegramOrSMS, Message{Kind: KindEvent, Text: "x"}, 1, 0},
		{"override", ModeTelegram, Message{Kind: KindEvent, Text: "x", Mode: ModeSMS}, 0, 1},
		{"network problem by sms", ModeTelegram, Message{Kind: KindProblem, Check: CheckNetwork, Text: "x"}, 0, 1},
		{
			"network resolved ignores mode",
			ModeSMS,
			Message{Kind: KindResolved, Check: CheckNetwork, Text: "x", Mode: ModeSMS},
			1,
			0,
		},
		{
			"network problem ignores mode",
			ModeSMS,
			Message{Kind: KindProblem, Check: CheckNetwork, Text: "x", Mode: ModeTelegram},
			0,
			1,
		},
		{"network resolved tlg-or-sms", ModeSMS, Message{Kind: KindResolved, Check: CheckNetwork, Text: "x"}, 1, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, tt.mode, true)
			if err := fx.n.Notify(context.Background(), tt.msg); err != nil {
				t.Fatal(err)
			}

			if len(fx.tlg.sent) != tt.wantTlg || len(fx.sms.sent) != tt.wantSMS {
				t.Fatalf("tlg=%d sms=%d", len(fx.tlg.sent), len(fx.sms.sent))
			}
		})
	}
}

func TestSMSIsPlainText(t *testing.T) {
	fx := newFixture(t, ModeSMS, true)
	_ = fx.n.Notify(context.Background(), Message{Kind: KindResolved, Text: "a<b"})

	if fx.sms.sent[0] != "host1.example.org: RESOLVED: a<b" {
		t.Fatalf("got %q", fx.sms.sent[0])
	}
}

func TestFallbackToSMS(t *testing.T) {
	fx := newFixture(t, ModeTelegramOrSMS, true)
	fx.tlg.fail = true

	if err := fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"}); err != nil {
		t.Fatal(err)
	}

	if len(fx.sms.sent) != 1 || len(fx.n.Export()) != 0 {
		t.Fatalf("sms=%d queue=%d", len(fx.sms.sent), len(fx.n.Export()))
	}

	fx.tlg.fail = false
	fx.n.SetOffline(true)
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "y"})

	if len(fx.sms.sent) != 2 || len(fx.tlg.sent) != 0 {
		t.Fatalf("sms=%d tlg=%d", len(fx.sms.sent), len(fx.tlg.sent))
	}
}

// TestRefusedTelegramInTelegramOrSMS: a Telegram send refused past the
// tick's delivery deadline is queued for Telegram and spends no SMS (the SMS
// lane is past the same deadline), while one refused because an abandoned
// Telegram send still hangs (a sign Telegram is down) falls back to SMS.
func TestRefusedTelegramInTelegramOrSMS(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		smsFails  bool
		wantSMS   int
		wantQueue int
	}{
		{"past the deadline", fmt.Errorf("deadline: %w: %w", ErrPastDeadline, ErrNotSent), false, 0, 1},
		{"lane busy", fmt.Errorf("lane busy: %w", ErrNotSent), false, 1, 0},
		{"lane busy and SMS fails", fmt.Errorf("lane busy: %w", ErrNotSent), true, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, ModeTelegramOrSMS, true)
			fx.tlg.err = tt.err
			fx.sms.fail = tt.smsFails

			_ = fx.n.Notify(context.Background(), Message{Kind: KindProblem, Text: "x"})

			q := fx.n.Export()
			if len(fx.sms.sent) != tt.wantSMS || len(q) != tt.wantQueue {
				t.Fatalf("sms = %q, queue = %+v; want %d SMS, %d queued", fx.sms.sent, q, tt.wantSMS, tt.wantQueue)
			}
			if len(q) == 1 && q[0].Channel != ChannelTelegram {
				t.Fatalf("queued on %q, want Telegram", q[0].Channel)
			}
		})
	}
}

func TestSinceOnAnEarlierDayHasTheDate(t *testing.T) {
	tests := []struct {
		name  string
		since time.Time
		want  string
	}{
		// The fixture's now is 2026-01-02 03:04:05 UTC.
		{"same day", time.Date(2026, 1, 2, 1, 5, 0, 0, time.UTC), "(since 01:05)"},
		{"earlier day", time.Date(2026, 1, 1, 15, 4, 0, 0, time.UTC), "(since 01 Jan 15:04)"},
		{"days ago", time.Date(2025, 12, 25, 8, 0, 0, 0, time.UTC), "(since 25 Dec 08:00)"},
		// Days are compared in the since time's own (host) zone: an earlier
		// UTC day, the same local day.
		{
			"same day in the host zone",
			time.Date(2026, 1, 1, 21, 0, 0, 0, time.UTC).In(time.FixedZone("E", 5*60*60)),
			"(since 02:00)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, ModeTelegram, false)
			_ = fx.n.Notify(context.Background(), Message{Kind: KindProblem, Text: "x", Since: tt.since})

			if len(fx.tlg.sent) != 1 || !strings.HasSuffix(fx.tlg.sent[0], tt.want) {
				t.Fatalf("got %q, want suffix %q", fx.tlg.sent, tt.want)
			}
		})
	}
}

func TestSMSDegradesOnce(t *testing.T) {
	fx := newFixture(t, ModeSMS, false)

	for range 3 {
		if err := fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	if len(fx.tlg.sent) != 3 {
		t.Fatalf("tlg=%d, want 3", len(fx.tlg.sent))
	}

	if len(fx.hooked) != 1 || fx.hooked[0] != SMSUnavailableKey {
		t.Fatalf("hook called %d times, want 1", len(fx.hooked))
	}
}

func TestQueueAndFlush(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true

	for _, s := range []string{"a", "b"} {
		if err := fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: s}); err != nil {
			t.Fatal(err)
		}
	}

	q := fx.n.Export()
	if len(q) != 2 || q[0].Channel != ChannelTelegram || q[0].CreatedAt.IsZero() {
		t.Fatalf("queue = %+v", q)
	}

	fx.n.Flush(context.Background())

	q = fx.n.Export()
	if len(q) != 2 || q[0].Attempts != 1 {
		t.Fatalf("queue after failed flush = %+v", q)
	}

	fx.tlg.fail = false
	fx.n.Flush(context.Background())

	if len(fx.n.Export()) != 0 || len(fx.tlg.sent) != 2 || !strings.HasSuffix(fx.tlg.sent[0], ": a") {
		t.Fatalf("sent=%v queue=%v", fx.tlg.sent, fx.n.Export())
	}
}

func TestOfflineQueuesSilently(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	fx.n.SetOffline(true)
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})
	fx.n.Flush(context.Background())

	if len(fx.tlg.sent) != 0 || len(fx.n.Export()) != 1 || fx.n.Export()[0].Attempts != 0 {
		t.Fatalf("sent=%v queue=%+v", fx.tlg.sent, fx.n.Export())
	}

	fx.n.SetOffline(false)
	fx.n.Flush(context.Background())

	if len(fx.tlg.sent) != 1 || len(fx.n.Export()) != 0 {
		t.Fatal("not flushed after coming online")
	}
}

func TestRestore(t *testing.T) {
	fx := newFixture(t, ModeTelegram, true)
	fx.n.Restore([]QueuedMessage{
		{ID: 7, CreatedAt: since(), Channel: ChannelTelegram, Text: "old", Attempts: 2},
		{ID: 9, CreatedAt: since(), Channel: ChannelSMS, Text: "oldsms"},
	})
	fx.n.Flush(context.Background())

	if len(fx.tlg.sent) != 1 || len(fx.sms.sent) != 1 || len(fx.n.Export()) != 0 {
		t.Fatalf("tlg=%v sms=%v", fx.tlg.sent, fx.sms.sent)
	}

	fx.tlg.fail = true
	fx.n.Restore([]QueuedMessage{{ID: 9, Channel: ChannelTelegram, Text: "r"}})
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "n"})

	if q := fx.n.Export(); len(q) != 2 || q[1].ID <= 9 {
		t.Fatalf("queue = %+v", q)
	}
}

func TestMergePutsStoredFirstAndReIDs(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	fx.n.SetOffline(true)
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "c"})
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "d"})

	fx.n.Merge([]QueuedMessage{
		{ID: 7, Channel: ChannelTelegram, Text: "a"},
		{ID: 9, Channel: ChannelTelegram, Text: "b"},
	})
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "e"})

	q := fx.n.Export()
	ids := make([]int64, 0, len(q))
	texts := make([]string, 0, len(q))
	for _, m := range q {
		ids = append(ids, m.ID)
		texts = append(texts, strings.TrimPrefix(m.Text, "host1.example.org: "))
	}
	if want := []int64{7, 9, 10, 11, 12}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if want := "a b c d e"; strings.Join(texts, " ") != want {
		t.Fatalf("texts = %v, want %s", texts, want)
	}
}

func TestMergeKeepsConcurrentEnqueues(t *testing.T) {
	const writers, each = 4, 50

	fx := newFixture(t, ModeTelegram, false)
	fx.n.SetOffline(true)
	stored := []QueuedMessage{{ID: 1, Channel: ChannelTelegram, Text: "stored"}}

	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range each {
				_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "live"})
			}
		})
	}
	fx.n.Merge(stored)
	wg.Wait()

	q := fx.n.Export()
	if len(q) != writers*each+1 || q[0].Text != "stored" {
		t.Fatalf("queue has %d messages (first %q), want %d with the stored one first",
			len(q), q[0].Text, writers*each+1)
	}
	seen := map[int64]bool{}
	for _, m := range q {
		if seen[m.ID] {
			t.Fatalf("duplicate queue id %d", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestBothChannelsFail(t *testing.T) {
	fx := newFixture(t, ModeTelegramOrSMS, true)
	fx.tlg.fail = true
	fx.sms.fail = true

	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})

	if q := fx.n.Export(); len(q) != 1 || q[0].Channel != ChannelTelegram {
		t.Fatalf("queue = %+v", q)
	}
}

func TestSMSFailureQueuesSMS(t *testing.T) {
	fx := newFixture(t, ModeSMS, true)
	fx.sms.fail = true

	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})

	if q := fx.n.Export(); len(q) != 1 || q[0].Channel != ChannelSMS {
		t.Fatalf("queue = %+v", q)
	}
}

func TestTelegramOrSMSWithoutSMS(t *testing.T) {
	fx := newFixture(t, ModeTelegramOrSMS, false)
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})

	if len(fx.tlg.sent) != 1 || len(fx.hooked) != 0 {
		t.Fatalf("tlg=%d hooked=%v", len(fx.tlg.sent), fx.hooked)
	}
}

func TestNilTelegramQueues(t *testing.T) {
	n := New(Options{Host: "h"})
	_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})
	n.Flush(context.Background())

	if len(n.Export()) != 1 {
		t.Fatalf("queue = %+v", n.Export())
	}
}

func TestHostIsEscapedForTelegram(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	fx.n = New(Options{Host: "a<b>", Telegram: asTelegram(fx.tlg)})
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})

	if fx.tlg.sent[0] != "a&lt;b&gt;: x" {
		t.Fatalf("got %q", fx.tlg.sent[0])
	}
}

func TestDroppedAreReported(t *testing.T) {
	var dropped []QueuedMessage

	tlg := &fakeSender{fail: true}
	n := New(
		Options{Host: "h", Telegram: asTelegram(tlg), OnDrop: func(m QueuedMessage) { dropped = append(dropped, m) }},
	)

	for range MaxQueue + 3 {
		_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})
	}

	if len(dropped) != 3 || dropped[0].ID != 1 {
		t.Fatalf("dropped = %+v", dropped)
	}

	dropped = nil
	n.Restore(append(n.Export(), QueuedMessage{ID: 5000, Channel: ChannelTelegram, Text: "y"}))

	if len(dropped) != 1 {
		t.Fatalf("restore dropped %d, want 1", len(dropped))
	}
}

func TestQueueCap(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true

	for range MaxQueue + 5 {
		_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})
	}

	if got := len(fx.n.Export()); got != MaxQueue {
		t.Fatalf("queue len %d, want %d", got, MaxQueue)
	}
}

func TestExpireDropsOldMessagesAndReportsThem(t *testing.T) {
	var reported []QueuedMessage

	now := time.Date(2026, 1, 9, 3, 0, 0, 0, time.UTC)
	n := New(Options{Host: "h", OnDrop: func(m QueuedMessage) { reported = append(reported, m) }})
	n.Restore([]QueuedMessage{
		{ID: 1, CreatedAt: now.Add(-8 * 24 * time.Hour), Channel: ChannelTelegram, Text: "old"},
		{ID: 2, CreatedAt: now.Add(-7 * 24 * time.Hour), Channel: ChannelTelegram, Text: "exactly 7 days"},
		{ID: 3, CreatedAt: now.Add(-time.Hour), Channel: ChannelSMS, Text: "new"},
	})

	dropped := n.Expire(now, 7*24*time.Hour)
	if len(dropped) != 1 || dropped[0].ID != 1 || len(reported) != 1 || reported[0].ID != 1 {
		t.Fatalf("dropped = %+v, reported = %+v; want message 1", dropped, reported)
	}
	if got := n.Export(); len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("queue = %+v", got)
	}
	if again := n.Expire(now, 7*24*time.Hour); len(again) != 0 || len(reported) != 1 {
		t.Fatalf("expired twice: %+v", again)
	}
}

func TestRenderReplacesInvalidUTF8(t *testing.T) {
	fx := newFixture(t, ModeTelegram, true)
	msg := Message{Kind: KindEvent, Text: "bad \xff\xfe bytes", Mode: ModeSMS}

	if err := fx.n.Notify(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	if err := fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: msg.Text}); err != nil {
		t.Fatal(err)
	}

	for _, got := range []string{fx.sms.sent[0], fx.tlg.sent[0]} {
		if !utf8.ValidString(got) || !strings.Contains(got, "bad \uFFFD bytes") {
			t.Errorf("got %q, want valid UTF-8 with a replacement character", got)
		}
	}
}

func TestTruncateWithoutBudgetKeepsOnlyTheEllipsis(t *testing.T) {
	n := New(Options{Host: strings.Repeat("h", MaxMessageUnits)})

	plain, rich := n.render(Message{Kind: KindProblem, Text: "body text"})

	for _, got := range []string{plain, rich} {
		if strings.Contains(got, "body") || !strings.Contains(got, "…") {
			t.Errorf("body not cut to the ellipsis: %q", got[len(got)-30:])
		}
	}
}

func TestQueuedMessageCarriesKindCheckAndKey(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want QueuedMessage
	}{
		{
			name: "problem",
			msg:  Message{Kind: KindProblem, Check: "disks", Key: "disks/TEST0000000001/wear", Text: "p"},
			want: QueuedMessage{Kind: "problem", Check: "disks", Key: "disks/TEST0000000001/wear"},
		},
		{
			name: "resolved",
			msg:  Message{Kind: KindResolved, Check: "disks", Key: "disks/TEST0000000001/wear", Text: "r"},
			want: QueuedMessage{Kind: "resolved", Check: "disks", Key: "disks/TEST0000000001/wear"},
		},
		{
			name: "event",
			msg:  Message{Kind: KindEvent, Check: "kernel", Key: "kernel", Text: "e"},
			want: QueuedMessage{Kind: "event", Check: "kernel", Key: "kernel"},
		},
		{
			name: "unset kind reads as event",
			msg:  Message{Text: "u"},
			want: QueuedMessage{Kind: "event"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, ModeTelegram, false)
			fx.tlg.fail = true

			if err := fx.n.Notify(context.Background(), tt.msg); err != nil {
				t.Fatal(err)
			}

			q := fx.n.Export()
			if len(q) != 1 {
				t.Fatalf("queue = %+v, want one message", q)
			}
			if q[0].Kind != tt.want.Kind || q[0].Check != tt.want.Check || q[0].Key != tt.want.Key {
				t.Errorf("queued kind/check/key = %q/%q/%q, want %q/%q/%q",
					q[0].Kind, q[0].Check, q[0].Key, tt.want.Kind, tt.want.Check, tt.want.Key)
			}
		})
	}
}

func TestQueuedSMSCopyCarriesKindCheckAndKey(t *testing.T) {
	fx := newFixture(t, ModeTelegram, true)
	fx.tlg.fail = true
	fx.sms.fail = true

	msg := Message{Kind: KindProblem, Check: CheckNetwork, Key: "network/ping", Text: "down"}
	if err := fx.n.Notify(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	q := fx.n.Export()
	if len(q) != 1 || q[0].Channel != ChannelSMS || q[0].Kind != "problem" ||
		q[0].Check != CheckNetwork || q[0].Key != "network/ping" {
		t.Fatalf("queue = %+v, want the SMS problem with its check and key", q)
	}
}

func TestFlushDropsResolvedSMSProblem(t *testing.T) {
	fx := newFixture(t, ModeTelegram, true)
	fx.sms.fail = true

	ctx := context.Background()
	key := "network/connectivity"
	_ = fx.n.Notify(ctx, Message{Kind: KindProblem, Check: CheckNetwork, Key: key, Text: "down"})
	if q := fx.n.Export(); len(q) != 1 || q[0].Channel != ChannelSMS {
		t.Fatalf("queue = %+v, want the SMS problem", q)
	}

	_ = fx.n.Notify(ctx, Message{Kind: KindResolved, Check: CheckNetwork, Key: key, Text: "up"})
	fx.sms.fail = false
	fx.n.Flush(ctx)

	if len(fx.sms.sent) != 0 {
		t.Fatalf("sms sent %q, want the resolved problem dropped", fx.sms.sent)
	}
	if q := fx.n.Export(); len(q) != 0 {
		t.Fatalf("queue = %+v, want empty", q)
	}
	if len(fx.tlg.sent) != 1 || !strings.Contains(fx.tlg.sent[0], "RESOLVED") {
		t.Fatalf("tlg sent %q, want the RESOLVED", fx.tlg.sent)
	}
}

// clockFixture is a notifier with a Telegram and an SMS fake and a clock the
// test moves.
type clockFixture struct {
	n   *Notifier
	tlg *fakeSender
	sms *fakeSender
	now time.Time
}

func newClockFixture(mode Mode) *clockFixture {
	cf := &clockFixture{tlg: &fakeSender{}, sms: &fakeSender{}, now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	cf.n = New(Options{
		Host: "host1.example.org", Mode: mode, Telegram: asTelegram(cf.tlg), SMS: cf.sms,
		Now: func() time.Time { return cf.now },
	})

	return cf
}

func (cf *clockFixture) notify(t *testing.T, text string) {
	t.Helper()

	if err := cf.n.Notify(context.Background(), Message{Kind: KindEvent, Text: text}); err != nil {
		t.Fatal(err)
	}
}

// TestTelegramFailureIsTracked: a failed Telegram send, on Notify or Flush,
// or a message queued while offline, starts the failure at its first moment;
// any successful Telegram send ends it, Reported included (R17).
func TestTelegramFailureIsTracked(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	start := cf.now

	if h := cf.n.TelegramHealth(); h.Failing() {
		t.Fatalf("health = %+v, want OK before any send", h)
	}

	cf.tlg.fail = true
	cf.notify(t, "a")
	cf.now = start.Add(time.Minute)
	cf.n.Flush(context.Background())
	cf.notify(t, "b")
	if h := cf.n.TelegramHealth(); !h.FailingSince.Equal(start) || h.Reported {
		t.Fatalf("health = %+v, want failing since the first failed send %v", h, start)
	}

	cf.n.MarkTelegramReported(start)
	if h := cf.n.TelegramHealth(); !h.Reported {
		t.Fatalf("health = %+v, want reported", h)
	}

	cf.tlg.fail = false
	cf.n.Flush(context.Background())
	if h := cf.n.TelegramHealth(); h != (ChannelHealth{}) {
		t.Fatalf("health = %+v, want OK after a successful flush", h)
	}

	cf.n.SetOffline(true)
	cf.now = start.Add(time.Hour)
	cf.notify(t, "c")
	if h := cf.n.TelegramHealth(); !h.FailingSince.Equal(cf.now) {
		t.Fatalf("health = %+v, want failing since the message queued while offline", h)
	}

	cf.n.SetOffline(false)
	cf.notify(t, "d")
	if h := cf.n.TelegramHealth(); h.Failing() {
		t.Fatalf("health = %+v, want OK after a successful send", h)
	}
}

// TestTelegramRefusedPastTheDeadlineIsNotAFailure: a send refused because
// the tick's delivery deadline passed says nothing about Telegram; a refusal
// because an abandoned send still hangs, or a send that failed while the SMS
// copy went out, does.
func TestTelegramRefusedPastTheDeadlineIsNotAFailure(t *testing.T) {
	tests := []struct {
		name        string
		mode        Mode
		err         error
		wantFailing bool
	}{
		{"past the deadline", ModeTelegram, fmt.Errorf("deadline: %w: %w", ErrPastDeadline, ErrNotSent), false},
		{
			"past the deadline, tlg-or-sms",
			ModeTelegramOrSMS,
			fmt.Errorf("deadline: %w: %w", ErrPastDeadline, ErrNotSent),
			false,
		},
		{"lane busy", ModeTelegram, fmt.Errorf("lane busy: %w", ErrNotSent), true},
		{"failed, SMS sent", ModeTelegramOrSMS, errors.New("send failed"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newClockFixture(tt.mode)
			cf.tlg.err = tt.err

			cf.notify(t, "x")
			cf.n.Flush(context.Background())

			h := cf.n.TelegramHealth()
			if got := h.Failing(); got != tt.wantFailing {
				t.Fatalf("failing = %v, want %v", got, tt.wantFailing)
			}
		})
	}
}

// TestTelegramFailedOnShutdownIsNotAFailure: a send that failed because the
// daemon is stopping (its ctx canceled) says nothing about Telegram: the
// message is queued and the channel is not marked failing; a failure with a
// live ctx still is.
func TestTelegramFailedOnShutdownIsNotAFailure(t *testing.T) {
	for _, mode := range []Mode{ModeTelegram, ModeTelegramOrSMS} {
		t.Run(string(mode), func(t *testing.T) {
			cf := newClockFixture(mode)
			cf.tlg.err = context.Canceled
			cf.sms.err = context.Canceled
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			if err := cf.n.Notify(ctx, Message{Kind: KindEvent, Text: "killed web-app"}); err != nil {
				t.Fatal(err)
			}
			cf.n.Flush(ctx)
			if h := cf.n.TelegramHealth(); h.Failing() {
				t.Fatalf("health = %+v after a send canceled by the stop, want OK", h)
			}
			if q := cf.n.Export(); len(q) == 0 {
				t.Fatal("the message canceled by the stop is not queued")
			}

			cf.n.Flush(context.Background())
			if h := cf.n.TelegramHealth(); !h.Failing() {
				t.Fatalf("health = %+v after a failed send with a live ctx, want failing", h)
			}
		})
	}
}

// TestMarkTelegramReportedOnlyForThatFailure: the mark applies to the
// failure the notice was about, not to a new one that began meanwhile.
func TestMarkTelegramReportedOnlyForThatFailure(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	first := cf.now
	cf.tlg.fail = true
	cf.notify(t, "a")
	cf.tlg.fail = false
	cf.notify(t, "b")
	cf.now = first.Add(time.Minute)
	cf.tlg.fail = true
	cf.notify(t, "c")

	cf.n.MarkTelegramReported(first)

	if h := cf.n.TelegramHealth(); h.Reported || !h.FailingSince.Equal(cf.now) {
		t.Fatalf("health = %+v, want the new failure unreported", h)
	}
}

// TestTelegramCopyQueuedOfflineInTelegramOrSMSIsAFailure: offline, a
// tlg-or-sms message whose SMS fails leaves its Telegram copy queued unsent;
// one whose SMS went out queues nothing for Telegram.
func TestTelegramCopyQueuedOfflineInTelegramOrSMSIsAFailure(t *testing.T) {
	cf := newClockFixture(ModeTelegramOrSMS)
	cf.n.SetOffline(true)

	cf.notify(t, "a")
	if h := cf.n.TelegramHealth(); h.Failing() {
		t.Fatalf("health = %+v, want OK: nothing was queued for Telegram", h)
	}

	cf.sms.fail = true
	cf.notify(t, "b")
	if h, q := cf.n.TelegramHealth(), cf.n.Export(); !h.FailingSince.Equal(cf.now) || len(q) != 1 {
		t.Fatalf("health = %+v, queue = %+v; want the queued Telegram copy to start the failure", h, q)
	}
}

// send notifies msg through the clock fixture's notifier.
func (cf *clockFixture) send(t *testing.T, msg Message) {
	t.Helper()

	if err := cf.n.Notify(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
}

func problem(key, text string) Message {
	return Message{Kind: KindProblem, Check: "disks", Key: key, Text: text, Since: since()}
}

func resolved(key, text string) Message {
	return Message{Kind: KindResolved, Check: "disks", Key: key, Text: text}
}

// queuedTelegram counts the queued Telegram copies.
func queuedTelegram(n *Notifier) int {
	count := 0
	queued := n.Export()
	for i := range queued {
		item := &queued[i]
		if item.Channel == ChannelTelegram {
			count++
		}
	}

	return count
}

// TestSMSFallbackProblem: while Telegram is failing, each queued PROBLEM goes
// by SMS once, in plain form, and its Telegram copy stays queued; PROBLEMs
// already queued when the failure starts go too; events, RESOLVED of PROBLEMs
// not sent by SMS and PROBLEMs raised while Telegram works do not (US2 acc. 2,
// SC-004, FR-030).
func TestSMSFallbackProblem(t *testing.T) {
	cf := newClockFixture(ModeTelegram)

	cf.send(t, problem("disks/a", "early")) // Telegram works: no SMS.
	cf.tlg.fail = true
	cf.send(t, resolved("disks/a", "early")) // Its PROBLEM did not go by SMS.
	cf.send(t, Message{Kind: KindEvent, Check: "ups", Key: "ups", Text: "test"})
	cf.send(t, problem("disks/b", "a<b & c"))
	if want := []string{"host1.example.org: PROBLEM: a<b & c (since 09:05)"}; !slices.Equal(cf.sms.sent, want) {
		t.Fatalf("SMS = %q, want %q", cf.sms.sent, want)
	}

	cf.n.Flush(context.Background())
	cf.send(t, problem("disks/c", "second"))
	cf.n.Flush(context.Background())
	want := []string{
		"host1.example.org: PROBLEM: a<b & c (since 09:05)",
		"host1.example.org: PROBLEM: second (since 09:05)",
	}
	if !slices.Equal(cf.sms.sent, want) {
		t.Fatalf("SMS = %q, want each PROBLEM once: %q", cf.sms.sent, want)
	}
	if got := queuedTelegram(cf.n); got != 4 {
		t.Fatalf("queued Telegram copies = %d, want all 4 kept", got)
	}

	cf.tlg.fail = false
	cf.n.Flush(context.Background())
	cf.send(t, problem("disks/d", "after recovery"))
	if len(cf.sms.sent) != 2 || len(cf.tlg.sent) != 6 {
		t.Fatalf("SMS = %q, Telegram = %q; want the queue flushed and no SMS once Telegram works",
			cf.sms.sent, cf.tlg.sent)
	}
}

// TestSMSFallbackQueuedBeforeTheFailure: PROBLEMs already queued (restored
// after a restart) go by SMS once Telegram starts failing, each once.
func TestSMSFallbackQueuedBeforeTheFailure(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.n.Restore([]QueuedMessage{
		{
			ID: 1, Channel: ChannelTelegram, Kind: "problem", Check: "disks", Key: "disks/a",
			Text: "host1.example.org: PROBLEM: x &amp; y",
		},
		{ID: 2, Channel: ChannelTelegram, Kind: "event", Check: "ups", Key: "ups", Text: "event"},
	})
	if h := cf.n.TelegramHealth(); h.Failing() {
		t.Fatalf("health = %+v, want OK before a send", h)
	}

	cf.tlg.fail = true
	cf.n.Flush(context.Background())
	cf.n.Flush(context.Background())
	if want := []string{"host1.example.org: PROBLEM: x & y"}; !slices.Equal(cf.sms.sent, want) {
		t.Fatalf("SMS = %q, want %q", cf.sms.sent, want)
	}
	if got := queuedTelegram(cf.n); got != 2 {
		t.Fatalf("queued Telegram copies = %d, want 2", got)
	}
}

// TestSMSFallbackKeepsOwnRouting: a check whose own mode is sms or
// tlg-or-sms keeps its routing, so a PROBLEM never goes by SMS twice; without
// an SMS sender PROBLEMs stay queued.
func TestSMSFallbackKeepsOwnRouting(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
	}{
		{"sms", Message{Kind: KindProblem, Check: "ups", Key: "ups/main", Text: "x", Mode: ModeSMS}},
		{"tlg-or-sms", Message{Kind: KindProblem, Check: "ups", Key: "ups/main", Text: "x", Mode: ModeTelegramOrSMS}},
		{"network", Message{Kind: KindProblem, Check: CheckNetwork, Key: "network/wan", Text: "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cf := newClockFixture(ModeTelegram)
			cf.tlg.fail = true
			cf.send(t, problem("disks/a", "start the failure"))

			cf.send(t, tt.msg)
			cf.n.Flush(context.Background())
			if got := countContaining(cf.sms.sent, ": PROBLEM: x"); got != 1 {
				t.Fatalf("SMS = %q, want the PROBLEM once", cf.sms.sent)
			}
		})
	}

	t.Run("no SMS sender", func(t *testing.T) {
		fx := newFixture(t, ModeTelegram, false)
		fx.tlg.fail = true
		if err := fx.n.Notify(context.Background(), problem("disks/a", "x")); err != nil {
			t.Fatal(err)
		}
		fx.n.Flush(context.Background())
		if got := queuedTelegram(fx.n); got != 1 {
			t.Fatalf("queued Telegram copies = %d, want the PROBLEM kept", got)
		}
	})
}

// TestResolvedBySMSWhenProblemWasSMS: the RESOLVED of a PROBLEM that went by
// SMS goes by SMS too, as well as by its normal route, and only once; the
// queued PROBLEM is not sent by SMS again once its RESOLVED is queued
// (FR-030, R18).
func TestResolvedBySMSWhenProblemWasSMS(t *testing.T) {
	t.Run("telegram still failing", func(t *testing.T) {
		cf := newClockFixture(ModeTelegram)
		cf.tlg.fail = true
		cf.send(t, problem("disks/a", "x"))

		cf.send(t, resolved("disks/a", "x"))
		cf.n.Flush(context.Background())
		cf.send(t, resolved("disks/a", "x again"))
		cf.n.Flush(context.Background())

		want := []string{
			"host1.example.org: PROBLEM: x (since 09:05)",
			"host1.example.org: RESOLVED: x",
		}
		if !slices.Equal(cf.sms.sent, want) {
			t.Fatalf("SMS = %q, want %q", cf.sms.sent, want)
		}
		if got := queuedTelegram(cf.n); got != 3 {
			t.Fatalf("queued Telegram copies = %d, want 3", got)
		}
	})

	t.Run("telegram recovered", func(t *testing.T) {
		cf := newClockFixture(ModeTelegram)
		cf.tlg.fail = true
		cf.send(t, problem("disks/a", "x"))
		cf.tlg.fail = false
		cf.n.Flush(context.Background())

		cf.send(t, resolved("disks/a", "x"))
		if got := countContaining(cf.sms.sent, "RESOLVED: x"); got != 1 {
			t.Fatalf("SMS = %q, want the RESOLVED by SMS", cf.sms.sent)
		}
		if got := countContaining(cf.tlg.sent, "RESOLVED: x"); got != 1 {
			t.Fatalf("Telegram = %q, want the RESOLVED by Telegram too", cf.tlg.sent)
		}
	})

	t.Run("own route went by SMS", func(t *testing.T) {
		cf := newClockFixture(ModeTelegram)
		cf.tlg.fail = true
		cf.send(t, problem("disks/a", "x"))

		msg := resolved("disks/a", "x")
		msg.Mode = ModeTelegramOrSMS
		cf.send(t, msg)
		if got := countContaining(cf.sms.sent, "RESOLVED: x"); got != 1 {
			t.Fatalf("SMS = %q, want the RESOLVED once", cf.sms.sent)
		}
	})
}

// countContaining counts the texts that contain sub.
func countContaining(texts []string, sub string) int {
	count := 0
	for _, s := range texts {
		if strings.Contains(s, sub) {
			count++
		}
	}

	return count
}

// TestSMSFallbackRetriesAfterAFailedSMS: the fallback stops at the first
// failed SMS and tries every unsent PROBLEM again at the next flush; the
// RESOLVED of a PROBLEM whose SMS never went out does not go by SMS.
func TestSMSFallbackRetriesAfterAFailedSMS(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.tlg.fail = true
	cf.sms.fail = true
	cf.send(t, problem("disks/a", "a"))
	cf.send(t, problem("disks/b", "b"))
	cf.send(t, resolved("disks/b", "b"))
	// One failed try per notify: the second stops at a, before b; the third
	// skips b, whose RESOLVED is queued.
	if cf.sms.calls != 3 {
		t.Fatalf("SMS calls = %d, want one failed try per notify, stopping at the failure", cf.sms.calls)
	}

	cf.sms.fail = false
	cf.n.Flush(context.Background())
	cf.n.Flush(context.Background())
	if want := []string{"host1.example.org: PROBLEM: a (since 09:05)"}; !slices.Equal(cf.sms.sent, want) {
		t.Fatalf("SMS = %q, want %q", cf.sms.sent, want)
	}
}

// TestSMSFallbackAbandonedSendNotRepeated: a fallback SMS abandoned while in
// progress is not sent again while it runs or after it succeeds; a late
// failure leaves the PROBLEM for the next flush.
func TestSMSFallbackAbandonedSendNotRepeated(t *testing.T) {
	for _, late := range []error{nil, errors.New("send failed")} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			sms := &abandoningSender{hang: map[string]chan error{}}
			result := make(chan error, 1)
			sms.hang["PROBLEM: x"] = result
			tlg := &fakeSender{fail: true}
			n := New(Options{Host: "host1.example.org", Telegram: asTelegram(tlg), SMS: sms})

			msg := Message{Kind: KindProblem, Key: "disks/a", Text: "x"}
			if err := n.Notify(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			n.Flush(context.Background())
			if got := sms.callCount(); got != 1 {
				t.Fatalf("SMS calls = %d, want no second send while the first runs", got)
			}

			result <- late
			want := 1
			if late != nil {
				want = 2
			}
			deadline := time.Now().Add(5 * time.Second)
			for sms.callCount() < want && time.Now().Before(deadline) {
				n.Flush(context.Background())
				time.Sleep(time.Millisecond)
			}
			n.Flush(context.Background())
			if got := sms.callCount(); got != want {
				t.Fatalf("SMS calls = %d, want %d", got, want)
			}
		})
	}
}

// TestResolvedSMSFailureIsQueued: a RESOLVED whose SMS fails is queued for
// SMS and goes out at a later flush.
func TestResolvedSMSFailureIsQueued(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.tlg.fail = true
	cf.send(t, problem("disks/a", "x"))

	cf.sms.fail = true
	cf.send(t, resolved("disks/a", "x"))
	cf.sms.fail = false
	cf.n.Flush(context.Background())
	if got := countContaining(cf.sms.sent, "RESOLVED: x"); got != 1 {
		t.Fatalf("SMS = %q, want the RESOLVED retried", cf.sms.sent)
	}
}

// TestResolvedWithoutSMSSender: a key left in the SMS-sent set when no SMS
// sender is configured any more leaves the set; the RESOLVED goes by
// Telegram only.
func TestResolvedWithoutSMSSender(t *testing.T) {
	fx := newFixture(t, ModeTelegram, false)
	fx.n.smsProblems.Add("disks/a")

	if err := fx.n.Notify(context.Background(), resolved("disks/a", "x")); err != nil {
		t.Fatal(err)
	}
	if fx.n.smsProblems.Has("disks/a") || len(fx.tlg.sent) != 1 {
		t.Fatalf("set has key = %v, Telegram = %q; want the key gone and the RESOLVED by Telegram",
			fx.n.smsProblems.Has("disks/a"), fx.tlg.sent)
	}
}

// TestTelegramOrSMSProblemSMSNotRepeated: a tlg-or-sms PROBLEM whose own SMS
// is abandoned while in progress is not sent again by the fallback while it
// runs or after it succeeds; after a late failure, or a plain failed SMS, the
// fallback sends it once.
func TestTelegramOrSMSProblemSMSNotRepeated(t *testing.T) {
	msg := Message{Kind: KindProblem, Check: "ups", Key: "ups/main", Text: "x", Mode: ModeTelegramOrSMS}

	for _, late := range []error{nil, errors.New("send failed")} {
		t.Run("abandoned, late "+fmt.Sprint(late), func(t *testing.T) {
			sms := &abandoningSender{hang: map[string]chan error{}}
			result := make(chan error, 1)
			sms.hang["PROBLEM: x"] = result
			n := New(Options{Host: "host1.example.org", Telegram: asTelegram(&fakeSender{fail: true}), SMS: sms})

			if err := n.Notify(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			n.Flush(context.Background())
			if got := sms.callCount(); got != 1 {
				t.Fatalf("SMS calls = %d, want no second send while the first runs", got)
			}

			result <- late
			want := 1
			if late != nil {
				want = 2
			}
			deadline := time.Now().Add(5 * time.Second)
			for sms.callCount() < want && time.Now().Before(deadline) {
				n.Flush(context.Background())
				time.Sleep(time.Millisecond)
			}
			n.Flush(context.Background())
			if got := sms.callCount(); got != want {
				t.Fatalf("SMS calls = %d, want %d", got, want)
			}
		})
	}

	t.Run("failed", func(t *testing.T) {
		cf := newClockFixture(ModeTelegram)
		cf.tlg.fail = true
		cf.sms.fail = true
		cf.send(t, msg)

		cf.sms.fail = false
		cf.n.Flush(context.Background())
		cf.n.Flush(context.Background())
		if want := []string{"host1.example.org: PROBLEM: x"}; !slices.Equal(cf.sms.sent, want) {
			t.Fatalf("SMS = %q, want the PROBLEM once: %q", cf.sms.sent, want)
		}
	})
}

// TestResolvedDirectlyMakesQueuedProblemStale: a PROBLEM still queued when
// its RESOLVED went out directly (Telegram flapping) is never sent by SMS,
// before or after a Merge renumbers it, and does not hold its key, so the
// next PROBLEM of that key goes by SMS.
func TestResolvedDirectlyMakesQueuedProblemStale(t *testing.T) {
	for _, merge := range []bool{false, true} {
		t.Run(fmt.Sprint("merge ", merge), func(t *testing.T) {
			cf := newClockFixture(ModeTelegram)
			cf.tlg.fail = true
			cf.sms.fail = true
			cf.send(t, problem("disks/a", "old")) // Queued; its SMS fails.

			cf.tlg.fail = false
			cf.send(t, resolved("disks/a", "old")) // Sent directly.
			if merge {
				cf.n.Merge([]QueuedMessage{{ID: 7, Channel: ChannelTelegram, Kind: "event", Text: "stored"}})
			}

			cf.tlg.fail = true
			cf.sms.fail = false
			cf.send(t, Message{Kind: KindEvent, Check: "ups", Key: "ups", Text: "test"})
			cf.n.Flush(context.Background())
			if len(cf.sms.sent) != 0 {
				t.Fatalf("SMS = %q, want no SMS for the resolved PROBLEM", cf.sms.sent)
			}

			cf.send(t, problem("disks/a", "new"))
			if want := []string{"host1.example.org: PROBLEM: new (since 09:05)"}; !slices.Equal(cf.sms.sent, want) {
				t.Fatalf("SMS = %q, want %q", cf.sms.sent, want)
			}
		})
	}
}

// TestResolvedMarkCoversRestoredQueue: a restored queue predates every
// RESOLVED seen in memory, so its PROBLEM of a resolved key is stale.
func TestResolvedMarkCoversRestoredQueue(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.send(t, resolved("disks/a", "x")) // Sent directly.
	cf.n.Restore([]QueuedMessage{
		{ID: 5, Channel: ChannelTelegram, Kind: "problem", Check: "disks", Key: "disks/a", Text: "old"},
	})

	cf.tlg.fail = true
	cf.n.Flush(context.Background())
	if len(cf.sms.sent) != 0 {
		t.Fatalf("SMS = %q, want no SMS for the resolved PROBLEM", cf.sms.sent)
	}
}

// TestMergeKeepsProblemsAfterTheResolvedMark: a PROBLEM queued after the
// RESOLVED of its key stays due when a Merge renumbers it; the one queued
// before stays stale.
func TestMergeKeepsProblemsAfterTheResolvedMark(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.tlg.fail = true
	cf.sms.fail = true
	cf.send(t, problem("disks/a", "old"))
	cf.tlg.fail = false
	cf.send(t, resolved("disks/a", "old")) // Sent directly.
	cf.tlg.fail = true
	cf.send(t, problem("disks/a", "new"))

	cf.n.Merge([]QueuedMessage{{ID: 7, Channel: ChannelTelegram, Kind: "event", Text: "stored"}})
	cf.sms.fail = false
	cf.n.Flush(context.Background())
	if want := []string{"host1.example.org: PROBLEM: new (since 09:05)"}; !slices.Equal(cf.sms.sent, want) {
		t.Fatalf("SMS = %q, want %q", cf.sms.sent, want)
	}
}

// TestAbandonedResolvedSMSClaimsNothing: an abandoned tlg-or-sms RESOLVED
// is sent once and does not hold its key, so the next PROBLEM of that key
// goes by SMS.
func TestAbandonedResolvedSMSClaimsNothing(t *testing.T) {
	sms := &abandoningSender{hang: map[string]chan error{}}
	result := make(chan error, 1)
	sms.hang["RESOLVED: x"] = result
	n := New(Options{Host: "host1.example.org", Telegram: asTelegram(&fakeSender{fail: true}), SMS: sms})

	msg := resolved("ups/main", "x")
	msg.Mode = ModeTelegramOrSMS
	if err := n.Notify(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), problem("ups/main", "y")); err != nil {
		t.Fatal(err)
	}

	result <- nil
	if got := sms.callCount(); got != 2 {
		t.Fatalf("SMS calls = %d, want 2: the RESOLVED once, then the new PROBLEM", got)
	}
}

// TestUndeliveredPrefix: a dropped PROBLEM listed as undelivered is put, in
// plain capped text with its since, ahead of the next message delivered to
// Telegram, within the limit; it stays until a delivery that showed it
// succeeds, and what did not fit goes with the next delivered message.
func TestUndeliveredPrefix(t *testing.T) {
	t.Run("shown once, after a delivery succeeds", testUndeliveredShownOnce)
	t.Run("SMS PROBLEM is listed in plain text", testUndeliveredFromSMS)
	t.Run("since on an earlier day and capped text", testUndeliveredDatedAndCapped)
	t.Run("no room: waits for the next message", testUndeliveredNoRoom)
	t.Run("lines that do not fit and the counted line go with the next message", testUndeliveredSpread)
}

const droppedPrefix = "Undelivered earlier:\nPROBLEM: a&lt;b (since 09:05), no longer open\n\n"

// dropAll expires every queued message and lists each, as the runner does
// for a dropped PROBLEM whose condition is no longer open.
func dropAll(t *testing.T, n *Notifier) {
	t.Helper()

	dropped := n.Expire(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), 7*24*time.Hour)
	for i := range dropped {
		item := &dropped[i]
		n.ListUndelivered(*item)
	}
	if q := n.Export(); len(q) != 0 {
		t.Fatalf("queue = %+v, want everything dropped", q)
	}
}

func testUndeliveredShownOnce(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true
	_ = fx.n.Notify(ctx, Message{Kind: KindProblem, Check: "disk", Key: "disk/a", Text: "a<b", Since: since()})
	dropAll(t, fx.n)

	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "first"})
	fx.tlg.fail = false
	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "second"})
	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "third"})

	want := []string{droppedPrefix + "host1.example.org: second", "host1.example.org: third"}
	if !slices.Equal(fx.tlg.sent, want) {
		t.Fatalf("sent = %q, want %q", fx.tlg.sent, want)
	}
	// The queued "first" carries no prefix any more.
	fx.n.Flush(ctx)
	if got := fx.tlg.sent[len(fx.tlg.sent)-1]; got != "host1.example.org: first" {
		t.Fatalf("flushed = %q", got)
	}
}

func testUndeliveredFromSMS(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, ModeTelegram, true)
	fx.sms.fail = true
	_ = fx.n.Notify(ctx, Message{
		Kind: KindProblem, Check: CheckNetwork, Key: "network", Text: "a<b", Since: since(),
	})
	fx.sms.fail = false
	dropAll(t, fx.n)

	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "next"})
	if want := droppedPrefix + "host1.example.org: next"; len(fx.tlg.sent) != 1 || fx.tlg.sent[0] != want {
		t.Fatalf("sent = %q, want %q", fx.tlg.sent, want)
	}
}

func testUndeliveredDatedAndCapped(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true
	_ = fx.n.Notify(ctx, Message{
		Kind: KindProblem, Key: "disk/a", Text: strings.Repeat("q", 1000),
		Since: time.Date(2025, 12, 30, 22, 15, 0, 0, time.UTC),
	})
	dropAll(t, fx.n)
	fx.tlg.fail = false

	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "next"})
	got := fx.tlg.sent[0]
	if !strings.Contains(got, "q… (since 30 Dec 22:15), no longer open\n") {
		t.Fatalf("sent = %q, want the capped text and the dated since", got)
	}
	if n := strings.Count(got, "q") + 1; n > maxUndeliveredText {
		t.Fatalf("listed text is %d units, want at most %d", n, maxUndeliveredText)
	}
}

func testUndeliveredNoRoom(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true
	_ = fx.n.Notify(ctx, Message{Kind: KindProblem, Key: "disk/a", Text: "a<b", Since: since()})
	dropAll(t, fx.n)
	fx.tlg.fail = false

	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: strings.Repeat("y", 4080)})
	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "next"})

	if n := utf16Units(fx.tlg.sent[0]); n > MaxMessageUnits || strings.HasPrefix(fx.tlg.sent[0], "Undelivered") {
		t.Fatalf("long message (%d units) carries the prefix", n)
	}
	if want := droppedPrefix + "host1.example.org: next"; fx.tlg.sent[1] != want {
		t.Fatalf("next = %q, want %q", fx.tlg.sent[1], want)
	}
}

func testUndeliveredSpread(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true
	for i := range MaxUndelivered + 2 {
		_ = fx.n.Notify(ctx, Message{Kind: KindProblem, Key: fmt.Sprintf("disk/%d", i), Text: strings.Repeat("z", 200)})
	}
	dropAll(t, fx.n)
	fx.tlg.fail = false

	for range 6 {
		_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "next"})
	}

	lines, counted, prefixed := 0, 0, 0
	for _, s := range fx.tlg.sent {
		if n := utf16Units(s); n > MaxMessageUnits {
			t.Fatalf("message is %d units", n)
		}
		lines += strings.Count(s, "no longer open")
		if strings.Contains(s, "… and 2 more undelivered PROBLEMs") {
			counted++
		}
		if strings.HasPrefix(s, "Undelivered earlier:") {
			prefixed++
		}
	}
	if lines != MaxUndelivered || counted != 1 || prefixed < 2 {
		t.Fatalf("lines = %d, counted = %d, prefixed = %d; want %d lines over several messages, counted once",
			lines, counted, prefixed, MaxUndelivered)
	}
	if last := fx.tlg.sent[len(fx.tlg.sent)-1]; last != "host1.example.org: next" {
		t.Fatalf("last = %q, want no prefix once everything was shown", last)
	}
}

// TestUndeliveredFrom: the listing of a dropped PROBLEM comes from its plain
// text and since; a message restored without them (an older store) falls
// back to reading the rendered text.
func TestUndeliveredFrom(t *testing.T) {
	loc := time.UTC
	at := func(d, h, m int) time.Time { return time.Date(2026, 1, d, h, m, 0, 0, loc) }
	tests := []struct {
		name  string
		host  string
		item  QueuedMessage
		text  string
		since time.Time
	}{
		{
			name: "plain text and since win",
			host: "h",
			item: QueuedMessage{
				Channel: ChannelTelegram, Text: "h: PROBLEM: other (since 01:00)",
				Plain: "up (since 10:00)", CreatedAt: at(2, 11, 0),
			},
			text: "up (since 10:00)",
		},
		{
			name: "host with an ampersand",
			host: "a&b",
			item: QueuedMessage{
				Channel:   ChannelTelegram,
				Text:      "a&amp;b: PROBLEM: x&lt;y (since 10:00)",
				CreatedAt: at(2, 11, 0),
			},
			text:  "x<y",
			since: at(2, 10, 0),
		},
		{
			name: "SMS text is not unescaped",
			host: "h",
			item: QueuedMessage{Channel: ChannelSMS, Text: "h: PROBLEM: a &amp; b", CreatedAt: at(2, 11, 0)},
			text: "a &amp; b",
		},
		{
			name: "dated since across the year",
			host: "h",
			item: QueuedMessage{
				Channel:   ChannelTelegram,
				Text:      "h: PROBLEM: x (since 30 Dec 22:15)",
				CreatedAt: at(2, 11, 0),
			},
			text:  "x",
			since: time.Date(2025, 12, 30, 22, 15, 0, 0, loc),
		},
		{
			name: "rendered before midnight, queued after",
			host: "h",
			item: QueuedMessage{
				Channel: ChannelTelegram, Text: "h: PROBLEM: x (since 23:50)",
				CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 100000000, loc),
			},
			text: "x", since: at(2, 23, 50),
		},
		{
			name: "no since",
			host: "h",
			item: QueuedMessage{Channel: ChannelTelegram, Text: "h: PROBLEM: x", CreatedAt: at(2, 11, 0)},
			text: "x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := undeliveredFrom(tt.item, tt.host, loc, at(9, 0, 0))
			if got.Text != tt.text || !got.Since.Equal(tt.since) {
				t.Fatalf("got %q since %v, want %q since %v", got.Text, got.Since, tt.text, tt.since)
			}
		})
	}
}

// TestUndeliveredSinceSuffixInText: a condition text that itself ends in
// "(since 10:00)", with no since, is listed verbatim.
func TestUndeliveredSinceSuffixInText(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, ModeTelegram, false)
	fx.tlg.fail = true
	_ = fx.n.Notify(ctx, Message{Kind: KindProblem, Key: "k", Text: "up (since 10:00)"})
	dropAll(t, fx.n)
	fx.tlg.fail = false

	_ = fx.n.Notify(ctx, Message{Kind: KindEvent, Text: "next"})
	want := "Undelivered earlier:\nPROBLEM: up (since 10:00), no longer open\n\nhost1.example.org: next"
	if len(fx.tlg.sent) != 1 || fx.tlg.sent[0] != want {
		t.Fatalf("sent = %q, want %q", fx.tlg.sent, want)
	}
}

// routedNotifier is a Notifier over a Telegram fake that records routes.
func routedNotifier(tlg *fakeSender) (*Notifier, *telegramAdapter) {
	ad := asTelegram(tlg)

	return New(Options{Host: "h", Telegram: ad, Location: time.UTC, Now: func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	}}), ad
}

// TestMessageRoute: a message without a route goes by its kind (PROBLEM and
// event loud, RESOLVED silent, all to alerts); an explicit route is kept.
func TestMessageRoute(t *testing.T) {
	alerts := route.Route{Topic: route.TopicAlerts}
	alertsSilent := route.Route{Topic: route.TopicAlerts, Silent: true}
	system := route.Route{Topic: route.TopicSystem, Silent: true}
	tests := []struct {
		name string
		msg  Message
		want route.Route
	}{
		{"problem", Message{Kind: KindProblem, Text: "x"}, alerts},
		{"resolved", Message{Kind: KindResolved, Text: "x"}, alertsSilent},
		{"event", Message{Kind: KindEvent, Text: "x"}, alerts},
		{"no kind", Message{Text: "x"}, alerts},
		{"explicit system", Message{Kind: KindEvent, Text: "x", Route: system}, system},
		{
			"explicit sms",
			Message{Kind: KindProblem, Text: "x", Route: route.Route{Topic: route.TopicSMS}},
			route.Route{Topic: route.TopicSMS},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ad := routedNotifier(&fakeSender{})
			_ = n.Notify(context.Background(), tt.msg)
			if got := ad.sentRoutes(); len(got) != 1 || got[0] != tt.want {
				t.Fatalf("routes = %v, want [%v]", got, tt.want)
			}
		})
	}
}

// TestRetryKeepsRoute: a failed send is queued with its resolved route and
// Flush retries it with that route.
func TestRetryKeepsRoute(t *testing.T) {
	system := route.Route{Topic: route.TopicSystem, Silent: true}
	tests := []struct {
		name string
		msg  Message
		want route.Route
	}{
		{
			"default resolved",
			Message{Kind: KindResolved, Text: "up"},
			route.Route{Topic: route.TopicAlerts, Silent: true},
		},
		{"explicit", Message{Kind: KindEvent, Text: "x", Route: system}, system},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlg := &fakeSender{fail: true}
			n, ad := routedNotifier(tlg)
			_ = n.Notify(context.Background(), tt.msg)

			if q := n.Export(); len(q) != 1 || q[0].Route != tt.want {
				t.Fatalf("queue = %+v, want route %v", q, tt.want)
			}

			tlg.fail = false
			n.Flush(context.Background())
			got := ad.sentRoutes()
			if len(got) != 2 || got[1] != tt.want || len(n.Export()) != 0 {
				t.Fatalf("routes = %v, want the retry on %v", got, tt.want)
			}
		})
	}

	t.Run("restored without a route", func(t *testing.T) {
		n, ad := routedNotifier(&fakeSender{})
		n.Restore([]QueuedMessage{{ID: 1, Channel: ChannelTelegram, Text: "h: up", Kind: string(KindResolved)}})
		n.Flush(context.Background())
		if got := ad.sentRoutes(); len(got) != 1 || got[0] != (route.Route{Topic: route.TopicAlerts, Silent: true}) {
			t.Fatalf("routes = %v, want the kind's default", got)
		}
	})
}

// TestUndeliveredPrefixByRoute: only an alerts message carries the
// undelivered prefix, and a message that carries it rings.
func TestUndeliveredPrefixByRoute(t *testing.T) {
	const prefix = "Undelivered earlier:"
	tests := []struct {
		name       string
		msg        Message
		wantPrefix bool
		want       route.Route
	}{
		{"silent alerts rings", Message{Kind: KindResolved, Text: "up"}, true, route.Route{Topic: route.TopicAlerts}},
		{"loud alerts", Message{Kind: KindProblem, Text: "down"}, true, route.Route{Topic: route.TopicAlerts}},
		{
			"system skips",
			Message{Kind: KindEvent, Text: "x", Route: route.Route{Topic: route.TopicSystem, Silent: true}},
			false,
			route.Route{Topic: route.TopicSystem, Silent: true},
		},
		{
			"sms topic skips",
			Message{Kind: KindEvent, Text: "x", Route: route.Route{Topic: route.TopicSMS}},
			false,
			route.Route{Topic: route.TopicSMS},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tlg := &fakeSender{}
			n, ad := routedNotifier(tlg)
			n.ListUndelivered(QueuedMessage{Kind: string(KindProblem), Key: "k", Plain: "lost"})

			_ = n.Notify(context.Background(), tt.msg)

			if len(tlg.sent) != 1 || strings.HasPrefix(tlg.sent[0], prefix) != tt.wantPrefix {
				t.Fatalf("sent = %q, want prefix = %v", tlg.sent, tt.wantPrefix)
			}
			if got := ad.sentRoutes(); len(got) != 1 || got[0] != tt.want {
				t.Fatalf("routes = %v, want [%v]", got, tt.want)
			}
		})
	}

	t.Run("skipped prefix stays listed", func(t *testing.T) {
		tlg := &fakeSender{}
		n, _ := routedNotifier(tlg)
		n.ListUndelivered(QueuedMessage{Kind: string(KindProblem), Key: "k", Plain: "lost"})
		_ = n.Notify(
			context.Background(),
			Message{Kind: KindEvent, Text: "x", Route: route.Route{Topic: route.TopicSystem, Silent: true}},
		)
		_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "y"})
		if !strings.HasPrefix(tlg.sent[1], prefix) {
			t.Fatalf("sent = %q, want the prefix on the next alerts message", tlg.sent)
		}
	})
}
