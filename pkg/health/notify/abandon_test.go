package notify

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// abandoningSender gives up on the sends listed in hang: it returns an
// AbandonedError whose result the test delivers later. Other sends succeed.
type abandoningSender struct {
	mu    sync.Mutex
	hang  map[string]chan error
	sent  []string
	calls []string
}

// Send matches and records the text without its host prefix.
func (a *abandoningSender) Send(_ context.Context, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, body, ok := strings.Cut(text, ": "); ok {
		text = body
	}

	a.calls = append(a.calls, text)
	if ch, ok := a.hang[text]; ok {
		delete(a.hang, text)

		return &AbandonedError{Cause: context.DeadlineExceeded, Result: ch}
	}
	a.sent = append(a.sent, text)

	return nil
}

func (a *abandoningSender) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.calls)
}

// waitQueue polls until the queue has want items.
func waitQueue(t *testing.T, n *Notifier, want int) []QueuedMessage {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		q := n.Export()
		if len(q) == want {
			return q
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue = %+v, want %d items", q, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func abandonFixture(hang ...string) (*Notifier, *abandoningSender, map[string]chan error) {
	results := map[string]chan error{}
	s := &abandoningSender{hang: map[string]chan error{}}
	for _, h := range hang {
		ch := make(chan error, 1)
		results[h], s.hang[h] = ch, ch
	}
	n := New(Options{Telegram: s, Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }})

	return n, s, results
}

func TestAbandonedSendDeliveredLateIsDroppedFromTheQueue(t *testing.T) {
	n, s, results := abandonFixture("first")

	_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "first"})
	if q := n.Export(); len(q) != 1 || !strings.HasSuffix(q[0].Text, "first") {
		t.Fatalf("queue = %+v, want the abandoned send queued", q)
	}
	_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "second"})

	// While the abandoned send runs, Flush neither re-sends it nor overtakes it.
	calls := s.callCount()
	n.Flush(context.Background())
	if s.callCount() != calls {
		t.Fatalf("flush sent %q while the abandoned send was in flight", s.calls[calls:])
	}

	results["first"] <- nil
	q := waitQueue(t, n, 0)
	n.Flush(context.Background())

	if !slices.Equal(s.sent, []string{"second"}) || len(q) != 0 {
		t.Fatalf("sent = %q, queue = %+v; want the late success dropped and never re-sent", s.sent, q)
	}
}

func TestAbandonedSendFailingLateIsRetried(t *testing.T) {
	n, s, results := abandonFixture("first")

	_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "first"})

	results["first"] <- errors.New("telegram timeout")
	deadline := time.Now().Add(5 * time.Second)
	for n.Export()[0].Attempts == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	n.Flush(context.Background())

	if !slices.Equal(s.sent, []string{"first"}) || len(n.Export()) != 0 {
		t.Fatalf("sent = %q, queue = %+v; want the late failure retried by the next flush", s.sent, n.Export())
	}
}

func TestAbandonedFlushIsTrackedAcrossAMerge(t *testing.T) {
	n, s, _ := abandonFixture()
	_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "x"}) // Sent.
	n.Restore([]QueuedMessage{{ID: 1, Channel: ChannelTelegram, Text: "queued"}})
	ch := make(chan error, 1)
	s.mu.Lock()
	s.hang["queued"] = ch
	s.mu.Unlock()

	n.Flush(context.Background())
	n.Merge([]QueuedMessage{{ID: 7, Channel: ChannelTelegram, Text: "stored"}})

	ch <- nil
	q := waitQueue(t, n, 1)

	if q[0].Text != "stored" {
		t.Fatalf("queue = %+v, want only the stored message once the renumbered send succeeded", q)
	}
}

func TestAbandonedTelegramOrSMSFallsBackAndTracksTheQueuedCopy(t *testing.T) {
	results := make(chan error, 1)
	tlg := &abandoningSender{hang: map[string]chan error{"RESOLVED: up": results}}
	sms := &fakeSender{fail: true}
	n := New(Options{Host: "host1.example.org", Telegram: tlg, SMS: sms})

	_ = n.Notify(context.Background(), Message{Kind: KindResolved, Check: CheckNetwork, Text: "up"})

	results <- nil

	waitQueue(t, n, 0)
}

func TestCheckerErrorMailNotAttemptedKeepsTheMark(t *testing.T) {
	marks := health.NewRateMarks()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mailer := &refusingMailer{err: ErrNotSent}
	ce := NewCheckerErrors(
		CheckerOptions{Host: "h", Mailer: mailer, Marks: marks, Now: func() time.Time { return now }},
	)

	ce.CheckerError("smart/sda/read", "x")
	if len(ce.Drain()) != 0 {
		t.Fatal("a checker error that went nowhere must not be recorded as sent")
	}
	mailer.err = nil
	ce.CheckerError("smart/sda/read", "x")

	if mailer.calls != 2 || mailer.sent != 1 {
		t.Fatalf("mail calls = %d, sent = %d; a refused email must not use up its 24 h mark", mailer.calls, mailer.sent)
	}
	if len(ce.Drain()) != 1 {
		t.Fatal("the email sent on the retry must be recorded")
	}
}

func TestCheckerErrorMailFailureStillUsesTheMark(t *testing.T) {
	marks := health.NewRateMarks()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mailer := &refusingMailer{err: errors.New("relay down")}
	ce := NewCheckerErrors(
		CheckerOptions{Host: "h", Mailer: mailer, Marks: marks, Now: func() time.Time { return now }},
	)

	ce.CheckerError("smart/sda/read", "x")
	ce.CheckerError("smart/sda/read", "x")

	if mailer.calls != 1 {
		t.Fatalf("mail calls = %d, want an attempted (failed) email rate-limited as before", mailer.calls)
	}
}

type refusingMailer struct {
	err   error
	calls int
	sent  int
}

func (m *refusingMailer) Send(context.Context, string, string) error {
	m.calls++
	if m.err != nil {
		return m.err
	}
	m.sent++

	return nil
}

// TestAbandonedSendIsMarkedAcrossAMergeAfterQueueing lands a Merge (the first
// Load of the runner's Do) right after the abandoned message is queued, via
// the OnDrop hook that its queueing triggers on a full queue. The message
// marked in flight must be the one just queued, so a late success removes
// exactly it.
func TestAbandonedSendIsMarkedAcrossAMergeAfterQueueing(t *testing.T) {
	n, _, results := abandonFixture("late")
	merged := false
	n.opts.OnDrop = func(QueuedMessage) {
		if !merged {
			merged = true
			n.Merge(nil) // Renumbers the queued messages from 1.
		}
	}

	n.SetOffline(true)

	for i := range MaxQueue {
		err := n.Notify(context.Background(), Message{Kind: KindEvent, Text: "fill " + strconv.Itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
	}

	n.SetOffline(false)

	if err := n.Notify(context.Background(), Message{Kind: KindEvent, Text: "late"}); err != nil {
		t.Fatal(err)
	}

	if !merged {
		t.Fatal("the Merge did not run")
	}

	results["late"] <- nil

	for _, item := range waitQueue(t, n, MaxQueue-1) {
		if strings.HasSuffix(item.Text, ": late") {
			t.Fatalf("the late message is still queued: %+v", item)
		}
	}
}

// TestAbandonedTelegramSendTracksTheChannel: the abandoned send counts as a
// failure; its late success is a successful Telegram send.
func TestAbandonedTelegramSendTracksTheChannel(t *testing.T) {
	n, _, results := abandonFixture("first")

	_ = n.Notify(context.Background(), Message{Kind: KindEvent, Text: "first"})
	if h := n.TelegramHealth(); !h.Failing() {
		t.Fatalf("health = %+v, want failing while the send hangs", h)
	}

	results["first"] <- nil
	waitQueue(t, n, 0)

	if h := n.TelegramHealth(); h.Failing() {
		t.Fatalf("health = %+v, want OK after the late success", h)
	}
}

// hangingSender records every text and abandons the send of hang.
type hangingSender struct {
	mu     sync.Mutex
	hang   string
	result chan error
	calls  []string
}

func (h *hangingSender) Send(_ context.Context, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = append(h.calls, text)
	if strings.HasSuffix(text, h.hang) {
		return &AbandonedError{Cause: context.DeadlineExceeded, Result: h.result}
	}

	return nil
}

func (h *hangingSender) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.calls)
}

// TestUndeliveredAbandonedSend: a send carrying the undelivered prefix that
// is abandoned holds the prefix (no other send shows it meanwhile) until its
// result: a late success acknowledges it, a late failure keeps it listed; the
// queued message still settles on the same result.
func TestUndeliveredAbandonedSend(t *testing.T) {
	const prefix = "Undelivered earlier:\nPROBLEM: lost, no longer open\n\n"
	tests := []struct {
		name   string
		result error
		after  string
	}{
		{"late success", nil, "h: after"},
		{"late failure", errors.New("send failed"), prefix + "h: after"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			tlg := &hangingSender{hang: "h: hang", result: make(chan error, 1)}
			n := New(Options{Host: "h", Telegram: tlg, Location: time.UTC, Now: func() time.Time {
				return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			}})
			n.ListUndelivered(QueuedMessage{Kind: string(KindProblem), Key: "k", Plain: "lost"})

			_ = n.Notify(ctx, Message{Kind: KindEvent, Text: "hang"})
			_ = n.Notify(ctx, Message{Kind: KindEvent, Text: "during"})
			if got := tlg.sent(); !slices.Equal(got, []string{prefix + "h: hang", "h: during"}) {
				t.Fatalf("sent = %q, want the prefix held by the abandoned send", got)
			}

			tlg.result <- tt.result
			want := 0
			if tt.result != nil {
				want = 1
			}
			waitQueue(t, n, want)
			waitUnheld(t, n)

			_ = n.Notify(ctx, Message{Kind: KindEvent, Text: "after"})
			if got := tlg.sent(); got[len(got)-1] != tt.after {
				t.Fatalf("after = %q, want %q", got[len(got)-1], tt.after)
			}
		})
	}
}

// waitUnheld polls until no send holds the undelivered prefix.
func waitUnheld(t *testing.T, n *Notifier) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		n.mu.Lock()
		held := n.showing
		n.mu.Unlock()
		if !held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the prefix is still held")
		}
		time.Sleep(time.Millisecond)
	}
}
