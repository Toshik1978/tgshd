package runner

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

func TestPushSendsTheTextAndLogsTheShortRecord(t *testing.T) {
	st := &fakeStore{}
	f := newFixture(t, st)
	f.tick(t)

	f.r.Push(context.Background(), "summary", "Weekly summary\n<all> & OK", "Weekly summary sent")
	texts := f.tlg.texts()
	if len(texts) != 1 || texts[0] != host+": Weekly summary\n&lt;all&gt; &amp; OK" {
		t.Fatalf("pushed = %q", texts)
	}

	f.next(t)
	i := slices.IndexFunc(st.alerts, func(a store.Alert) bool { return a.Kind == store.KindEvent })
	if i < 0 {
		t.Fatalf("alert log = %v, want the pushed event", st.alertKinds())
	}
	if a := st.alerts[i]; a.Check != "summary" || a.Key != "summary" || a.Text != "Weekly summary sent" {
		t.Fatalf("record = %+v, want the one-line record, not the pushed text", a)
	}
}

func TestPushQueuesAFailedSendAndRetriesIt(t *testing.T) {
	st := &fakeStore{}
	f := newFixture(t, st)
	f.tick(t)

	f.tlg.setFail(true)
	f.r.Push(context.Background(), "summary", "Weekly summary", "Weekly summary sent")
	if len(f.r.notifier.Export()) != 1 {
		t.Fatalf("queue = %+v, want the failed push queued", f.r.notifier.Export())
	}

	f.tlg.setFail(false)
	f.next(t)
	if got := f.tlg.texts(); countContaining(got, host+": Weekly summary") != 1 {
		t.Fatalf("sent = %q, want the queued push retried once", got)
	}
}

func TestPushFromTheWeeklyJobIsLoggedThatTick(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	f := jobFixture(st, j, t0())
	f.tick(t) // Seeds the weekly slot; nothing runs on install.

	f.r.opts.Weekly = func(ctx context.Context, _ time.Time) error {
		f.r.Push(ctx, "summary", "Weekly summary", "Weekly summary sent")

		return nil
	}
	f.now = time.Date(2026, 1, 9, 4, 30, 0, 0, time.UTC) // Past Friday 06:00 local.
	f.tick(t)

	if countContaining(f.tlg.texts(), host+": Weekly summary") != 1 {
		t.Fatalf("sent = %q", f.tlg.texts())
	}
	if !slices.Contains(st.alertKinds(), store.KindEvent) {
		t.Fatalf("alert log = %v, want the summary event recorded by this tick's checkpoint", st.alertKinds())
	}
}

func TestWeeklyJobRunsAfterQueuedMessagesAndThisTicksAlerts(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	chk := newFakeCheck(&orderLog{}, "c", health.CheckResult{})
	f := jobFixture(st, j, t0(), checkOf(chk))
	f.tick(t)

	f.r.opts.Weekly = func(ctx context.Context, _ time.Time) error {
		f.r.Push(ctx, "summary", "Weekly summary", "Weekly summary sent")

		return nil
	}
	f.tlg.setFail(true)
	f.r.Push(context.Background(), "test", "older alert", "older alert")
	f.tlg.setFail(false)
	chk.set(health.CheckResult{Readings: []health.Reading{
		{Key: "c/x", Verdict: health.Good, Kind: health.Event, Text: "this tick event"},
	}})
	f.now = time.Date(2026, 1, 9, 4, 30, 0, 0, time.UTC)
	f.tick(t)

	got := f.tlg.texts()
	older := slices.IndexFunc(got, func(s string) bool { return strings.HasSuffix(s, "older alert") })
	event := slices.IndexFunc(got, func(s string) bool { return strings.HasSuffix(s, "this tick event") })
	summary := slices.IndexFunc(got, func(s string) bool { return strings.HasSuffix(s, "Weekly summary") })
	if older < 0 || event < 0 || summary < 0 || summary < older || summary < event {
		t.Fatalf("sent = %q, want the queued alert and the tick's event before the summary", got)
	}
}

func TestExpiredQueueEntryIsNeverFlushedAndReportedOnce(t *testing.T) {
	now := t0()
	old := notify.QueuedMessage{
		ID:        1,
		CreatedAt: now.Add(-8 * 24 * time.Hour),
		Channel:   notify.ChannelTelegram,
		Text:      host + ": PROBLEM: stale",
	}
	st := &fakeStore{state: store.State{Queue: []notify.QueuedMessage{old}}}
	core, logs := observer.New(zap.WarnLevel)
	tlg := &fakeSender{}
	var r *Runner
	r = New(Options{
		Store: st, Host: host, Telegram: asTelegram(tlg), Logger: zap.New(core), Now: func() time.Time { return now },
		Location: zone(), Uptime: &fakeUptime{up: time.Hour},
		Daily: func(_ context.Context, at time.Time) error {
			if dropped := r.ExpireQueue(at, 7*24*time.Hour); len(dropped) != 1 || dropped[0].ID != 1 {
				t.Errorf("expired = %+v, want the stale message", dropped)
			}

			return nil
		},
	})
	for range 2 {
		if err := r.Do(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(Tick)
	}
	if got := tlg.texts(); countContaining(got, "stale") != 0 {
		t.Fatalf("sent = %q, want the expired message never flushed", got)
	}
	if got := logs.FilterMessage("health message dropped from the retry queue").All(); len(got) != 1 {
		t.Fatalf("drop log = %+v, want it reported once", got)
	}
	if len(st.state.Queue) != 0 {
		t.Fatalf("stored queue = %+v, want the expired message gone", st.state.Queue)
	}
}
