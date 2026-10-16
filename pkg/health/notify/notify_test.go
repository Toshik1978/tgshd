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
)

type fakeSender struct {
	sent []string
	fail bool
	// err, when set, is returned instead of sending.
	err error
}

func (f *fakeSender) Send(_ context.Context, text string) error {
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
		Telegram: fx.tlg,
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
	fx.n = New(Options{Host: "a<b>", Telegram: fx.tlg})
	_ = fx.n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"})

	if fx.tlg.sent[0] != "a&lt;b&gt;: x" {
		t.Fatalf("got %q", fx.tlg.sent[0])
	}
}

func TestDroppedAreReported(t *testing.T) {
	var dropped []QueuedMessage

	tlg := &fakeSender{fail: true}
	n := New(Options{Host: "h", Telegram: tlg, OnDrop: func(m QueuedMessage) { dropped = append(dropped, m) }})

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
