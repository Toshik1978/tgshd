package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// hangingSender blocks every send until released, ignoring its context like
// the Telegram publisher does.
type hangingSender struct {
	release <-chan struct{}
	calls   atomic.Int32
}

func (h *hangingSender) Send(context.Context, string) error {
	h.calls.Add(1)
	<-h.release

	return errors.New("released")
}

// hangingMailer blocks every email until released, ignoring its context.
type hangingMailer struct {
	release <-chan struct{}
	calls   atomic.Int32
}

func (h *hangingMailer) Send(context.Context, string, string) error {
	h.calls.Add(1)
	<-h.release

	return errors.New("released")
}

// doWithin runs one tick and fails when it does not return within limit.
func doWithin(t *testing.T, r *Runner, limit time.Duration) time.Duration {
	t.Helper()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- r.Do(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
	case <-time.After(limit):
		t.Fatalf("tick still running after %v: its deliveries are not bounded", limit)
	}

	return time.Since(start)
}

func TestSlowDeliveriesEndByTheTickDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	tlg := &hangingSender{release: release}
	sms := &hangingSender{release: release}
	mailer := &hangingMailer{release: release}
	hb := &fakeHeartbeat{}
	src := &fakeFixes{}
	now := t0()
	chk := health.Check{
		Name:     "ups",
		Interval: Tick,
		Mode:     health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			if now.Equal(t0()) {
				src.add(Fix{At: now, Check: "ups", Key: "ups/main/fix", Text: "fix ran restart.sh", Send: true})
			}

			return health.CheckResult{
				Readings: []health.Reading{{Key: "ups/main", Verdict: health.Bad, Text: "on battery"}},
				Err:      errors.New("partial read"),
			}
		},
	}
	stored := store.State{Queue: []notify.QueuedMessage{
		{ID: 1, CreatedAt: now, Channel: notify.ChannelTelegram, Text: "stored message"},
	}}
	const budget = 200 * time.Millisecond
	r := New(Options{
		Checks: []health.Check{chk}, RunBudget: 100 * time.Millisecond, TickBudget: budget,
		Heartbeat: hb, Store: &fakeStore{state: stored}, Fixes: []FixSource{src}, Host: host,
		Telegram: tlg, SMS: sms, Mailer: mailer, Now: func() time.Time { return now },
	})

	elapsed := doWithin(t, r, 5*time.Second)

	if elapsed > budget+time.Second {
		t.Fatalf("tick took %v, want it to end shortly after the %v tick budget", elapsed, budget)
	}
	// One hung send per channel: once Telegram and email hang, the tick's
	// other sends on them are refused, not stacked.
	if tlg.calls.Load() != 1 || mailer.calls.Load() != 1 || sms.calls.Load() != 0 {
		t.Fatalf("telegram tried %d times, email %d, sms %d; want 1, 1, 0",
			tlg.calls.Load(), mailer.calls.Load(), sms.calls.Load())
	}
	wantQueued := []string{"stored message", "on battery", "CHECKER ERROR: ups: partial read", "fix ran restart.sh"}
	assertQueuedOnce(t, r, wantQueued)

	// The next tick is not held up by the abandoned sends: it writes its
	// heartbeat on time and ends within its own budget.
	now = now.Add(Tick)
	if elapsed := doWithin(t, r, 5*time.Second); elapsed > budget+time.Second {
		t.Fatalf("second tick took %v, want it bounded by the tick budget", elapsed)
	}
	if len(hb.ticks) != 2 || !hb.ticks[1].Equal(now) {
		t.Fatalf("heartbeat ticks = %v, want the second at %v", hb.ticks, now)
	}
	// No send is retried while the abandoned ones are still in progress, and
	// every message is still queued exactly once. Telegram is failing, so the
	// queued PROBLEM goes by SMS once, on the now free SMS lane (FR-030).
	if tlg.calls.Load() != 1 || mailer.calls.Load() != 1 || sms.calls.Load() != 1 {
		t.Fatalf("after the second tick telegram tried %d times, email %d, sms %d; want only the SMS fallback",
			tlg.calls.Load(), mailer.calls.Load(), sms.calls.Load())
	}
	assertQueuedOnce(t, r, wantQueued)
}

// assertQueuedOnce fails unless the queue holds exactly the messages
// containing want, each once.
func assertQueuedOnce(t *testing.T, r *Runner, want []string) {
	t.Helper()

	queued := r.notifier.Export()
	texts := make([]string, 0, len(queued))
	for i := range queued {
		q := &queued[i]
		texts = append(texts, q.Text)
	}
	if len(texts) != len(want) {
		t.Fatalf("queue = %q, want %d messages", texts, len(want))
	}
	for _, w := range want {
		if countContaining(texts, w) != 1 {
			t.Fatalf("queue = %q, want %q queued exactly once", texts, w)
		}
	}
}

func TestSlowCheckerErrorsInAQuietTickEndByTheTickDeadline(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	mailer := &hangingMailer{release: release}
	const budget = 100 * time.Millisecond
	r := New(Options{
		TickBudget:  budget,
		Maintenance: &fakeMaintenance{held: true},
		Heartbeat:   &fakeHeartbeat{err: errors.New("disk full")},
		Uptime:      &fakeUptime{err: errors.New("no proc")},
		Telegram:    &fakeSender{},
		Mailer:      mailer,
		Host:        host,
		Now:         t0,
	})

	if elapsed := doWithin(t, r, 5*time.Second); elapsed > budget+time.Second {
		t.Fatalf("quiet tick took %v, want its checker errors bounded by the tick budget", elapsed)
	}
	if mailer.calls.Load() == 0 {
		t.Fatal("no checker error email was attempted")
	}
}

func TestSendOutsideATickIsBoundedByItsOwnContext(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r := New(Options{Telegram: &hangingSender{release: release}, Host: host, Now: t0})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.Push(ctx, "summary", "Weekly summary", "Weekly summary sent")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push still running after its context ended")
	}
	q := r.notifier.Export()
	if len(q) != 1 || !strings.Contains(q[0].Text, "Weekly summary") {
		t.Fatalf("queue = %+v, want the abandoned push queued", q)
	}
}

func TestDeliveryGuardPassesResultsThrough(t *testing.T) {
	var g deliveries
	l := &lane{d: &g}
	want := errors.New("rejected")

	if err := l.run(context.Background(), func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("run = %v, want the sender's error", err)
	}

	stop := make(chan struct{})
	close(stop)
	g.begin(stop, nil)
	called := false
	err := l.run(context.Background(), func(context.Context) error {
		called = true

		return nil
	})
	mailErr := (&lane{d: &g, mail: true}).run(context.Background(), func(context.Context) error { return nil })
	g.end()

	if !errors.Is(err, errDeliveryDeadline) || !errors.Is(err, notify.ErrNotSent) ||
		!errors.Is(err, notify.ErrPastDeadline) || called {
		t.Fatalf("run after the deadline = %v (sender called %v), want a refusal without a send", err, called)
	}
	if mailErr != nil {
		t.Fatalf("email after the send deadline = %v, want it sent within the email share", mailErr)
	}
	if err := l.run(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("run after the tick = %v, want nil", err)
	}
}

func TestLaneRefusesWhileAnAbandonedSendRuns(t *testing.T) {
	var g deliveries
	l := &lane{d: &g}
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := l.run(ctx, func(context.Context) error {
		<-release

		return nil
	})
	var ab *notify.AbandonedError
	if !errors.As(err, &ab) {
		t.Fatalf("run = %v, want an AbandonedError", err)
	}
	err = l.run(context.Background(), func(context.Context) error { return nil })
	if !errors.Is(err, errChannelBusy) || !errors.Is(err, notify.ErrNotSent) ||
		errors.Is(err, notify.ErrPastDeadline) {
		t.Fatalf("second run = %v, want a busy refusal", err)
	}

	close(release)
	if err := <-ab.Result; err != nil {
		t.Fatalf("late result = %v, want the send's own nil", err)
	}
	if err := l.run(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("run after the abandoned send returned = %v, want nil", err)
	}
}

// slowSender delivers every send, but the first one only once released: a
// Telegram call that succeeds after the tick budget.
type slowSender struct {
	release chan struct{}
	calls   atomic.Int32
	mu      sync.Mutex
	sent    []string
}

func (s *slowSender) Send(_ context.Context, text string) error {
	if s.calls.Add(1) == 1 {
		<-s.release // Ignores its context on purpose.
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sent = append(s.sent, text)

	return nil
}

func (s *slowSender) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.sent)
}

func TestLateTelegramSuccessIsDeliveredOnceAndLaterMessagesFlow(t *testing.T) {
	tlg := &slowSender{release: make(chan struct{})}
	now := t0()
	chk := health.Check{
		Name:     "ups",
		Interval: Tick,
		Mode:     health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			if !now.Equal(t0().Add(Tick)) { // The event only on the second tick.
				return reading("ups/main", health.Bad, "on battery")
			}

			return health.CheckResult{Readings: []health.Reading{
				{Key: "ups/main", Verdict: health.Bad, Text: "on battery"},
				{Key: "ups/event", Kind: health.Event, Text: "self-test passed"},
			}}
		},
	}
	const budget = 200 * time.Millisecond
	r := New(Options{
		Checks: []health.Check{chk}, RunBudget: 100 * time.Millisecond, TickBudget: budget,
		Store: &fakeStore{}, Host: host, Telegram: tlg, Now: func() time.Time { return now },
	})

	doWithin(t, r, 5*time.Second) // The PROBLEM's send outlives the tick.
	now = now.Add(Tick)
	doWithin(t, r, 5*time.Second) // Still in progress: nothing is re-sent.

	if tlg.calls.Load() != 1 {
		t.Fatalf("telegram tried %d times while the first send was in progress, want 1", tlg.calls.Load())
	}

	close(tlg.release) // The late send succeeds: the PROBLEM leaves the queue.
	deadline := time.Now().Add(5 * time.Second)
	for len(r.notifier.Export()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	now = now.Add(Tick)
	doWithin(t, r, 5*time.Second)

	sent := tlg.texts()
	if len(sent) != 2 || countContaining(sent, "PROBLEM: on battery") != 1 ||
		countContaining(sent, "self-test passed") != 1 {
		t.Fatalf("telegram = %q, want the PROBLEM exactly once, then the later event", sent)
	}
	if q := r.notifier.Export(); len(q) != 0 {
		t.Fatalf("queue = %+v, want it empty", q)
	}
}

func TestHungTelegramStillLeavesTimeForTheCheckerErrorEmail(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	mailer := &fakeMailer{}
	chk := health.Check{Name: "ups", Interval: Tick, Run: func(context.Context) health.CheckResult {
		return health.CheckResult{
			Readings: []health.Reading{{Key: "ups/main", Verdict: health.Bad, Text: "on battery"}},
			Err:      errors.New("partial read"),
		}
	}}
	r := New(Options{
		Checks: []health.Check{chk}, RunBudget: 100 * time.Millisecond, TickBudget: 300 * time.Millisecond,
		Host: host, Telegram: &hangingSender{release: release}, Mailer: mailer, Now: t0,
	})

	doWithin(t, r, 5*time.Second)

	if got := mailer.count(); got != 1 {
		t.Fatalf("emails = %d, want the checker error emailed although Telegram hung through the tick", got)
	}
}

// fakeMailer records the emails it is asked to send.
type fakeMailer struct {
	mu     sync.Mutex
	bodies []string
}

func (m *fakeMailer) Send(_ context.Context, _, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.bodies = append(m.bodies, body)

	return nil
}

func (m *fakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.bodies)
}

// TestHungCheckStillLeavesTheSendsAWindow: a check that hangs for the whole
// run budget does not use up the tick's Telegram and SMS window; the PROBLEMs
// of the other checks go out in the same tick.
func TestHungCheckStillLeavesTheSendsAWindow(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hung := health.Check{Name: "command", Interval: Tick, Run: func(context.Context) health.CheckResult {
		<-release // ignores its context, like a hung probe

		return health.CheckResult{}
	}}
	ups := health.Check{
		Name:     "ups",
		Interval: Tick,
		Mode:     health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			return health.CheckResult{
				Readings: []health.Reading{{Key: "ups/main", Verdict: health.Bad, Text: "on battery"}},
			}
		},
	}
	modem := health.Check{
		Name:     "modem",
		Interval: Tick,
		Mode:     health.ModeSMS,
		Run: func(context.Context) health.CheckResult {
			return health.CheckResult{
				Readings: []health.Reading{{Key: "modem/sms", Verdict: health.Bad, Text: "no reply"}},
			}
		},
	}
	tlg, sms := &fakeSender{}, &fakeSender{}
	// The run and delivery budgets are equal, as the defaults once were: a
	// send stop counted from the tick start passes as the runs end.
	const runBudget, tickBudget = 200 * time.Millisecond, 200 * time.Millisecond
	r := New(Options{
		Checks: []health.Check{hung, ups, modem}, RunBudget: runBudget, TickBudget: tickBudget,
		Host: host, Telegram: tlg, SMS: sms, Now: t0,
	})

	elapsed := doWithin(t, r, 5*time.Second)

	if elapsed < runBudget {
		t.Fatalf("tick took %v, want the hung check to use the whole %v run budget", elapsed, runBudget)
	}
	if got := tlg.texts(); countContaining(got, "PROBLEM: on battery") != 1 {
		t.Fatalf("telegram = %q, want the ups PROBLEM delivered in the tick of the hung check", got)
	}
	if got := sms.texts(); countContaining(got, "PROBLEM: no reply") != 1 {
		t.Fatalf("sms = %q, want the modem PROBLEM delivered in the tick of the hung check", got)
	}
	if q := r.notifier.Export(); len(q) != 0 {
		t.Fatalf("queue = %+v, want nothing held back", q)
	}
}

// TestHungCheckAndHungTelegramKeepTheTickBounded: with a check and Telegram
// both hanging, a tick still ends within its run budget plus its delivery
// budget, and the next tick writes its heartbeat on time.
func TestHungCheckAndHungTelegramKeepTheTickBounded(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hung := health.Check{Name: "command", Interval: Tick, Run: func(context.Context) health.CheckResult {
		<-release

		return health.CheckResult{}
	}}
	ups := health.Check{
		Name:     "ups",
		Interval: Tick,
		Mode:     health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			return health.CheckResult{
				Readings: []health.Reading{{Key: "ups/main", Verdict: health.Bad, Text: "on battery"}},
			}
		},
	}
	hb := &fakeHeartbeat{}
	now := t0()
	const runBudget, tickBudget = 150 * time.Millisecond, 150 * time.Millisecond
	r := New(Options{
		Checks: []health.Check{hung, ups}, RunBudget: runBudget, TickBudget: tickBudget, Heartbeat: hb,
		Host: host, Telegram: &hangingSender{release: release}, Mailer: &fakeMailer{},
		Now: func() time.Time { return now },
	})

	for range 2 {
		if elapsed := doWithin(t, r, 5*time.Second); elapsed > runBudget+tickBudget+time.Second {
			t.Fatalf("tick took %v, want it within the run budget plus the delivery budget", elapsed)
		}
		now = now.Add(Tick)
	}
	if len(hb.ticks) != 2 || !hb.ticks[1].Equal(t0().Add(Tick)) {
		t.Fatalf("heartbeat ticks = %v, want one per tick", hb.ticks)
	}
}

// TestDefaultBudgetsKeepTheHeartbeatFresh: a tick that uses its whole run
// budget and its whole delivery budget, plus an allowance for the local store,
// still ends strictly before the third 30 s slot, so gocron starts the next
// tick (and its heartbeat) at most 90 s, one cycle plus one minute, after this
// one (SC-006).
func TestDefaultBudgetsKeepTheHeartbeatFresh(t *testing.T) {
	const storeWork = 15 * time.Second
	if worst := MaxRunTime + MaxTickTime + storeWork; worst >= Tick+time.Minute {
		t.Fatalf("worst tick %v (runs %v, deliveries %v, store %v), want strictly under %v",
			worst, MaxRunTime, MaxTickTime, storeWork, Tick+time.Minute)
	}
	if window := MaxTickTime - MaxTickTime/mailShareDiv; window < 20*time.Second {
		t.Fatalf("Telegram and SMS window after the runs = %v, want at least 20s", window)
	}
}

// TestTelegramFailingEmailAfterTheTicksDeliveries: with Telegram failing,
// the tick that reaches telegram_failing_after emails the one notice after
// its deliveries, records it in the alert log, and no later tick repeats it;
// a quiet tick checks it too.
func TestTelegramFailingEmailAfterTheTicksDeliveries(t *testing.T) {
	chk := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	f := newFixture(t, &fakeStore{}, checkOf(chk))
	mailer := &fakeMailer{}
	f.r = New(Options{
		Checks: []health.Check{checkOf(chk)}, Store: f.st, Host: host, Telegram: f.tlg, Mailer: mailer,
		TelegramFailingAfter: 2 * time.Minute, Maintenance: f.maint,
		Now: func() time.Time { return f.now }, Location: zone(),
	})
	f.tlg.setFail(true)

	f.r.Push(context.Background(), "ups", "test", "test") // fails: failing since t0.
	f.tick(t)
	f.next(t)
	f.next(t)
	f.next(t) // 1m30s
	if got := mailer.count(); got != 0 {
		t.Fatalf("emails = %d before telegram_failing_after", got)
	}

	f.next(t) // 2m: due.
	if got := mailer.count(); got != 1 ||
		!strings.HasSuffix(mailer.bodies[0], "Telegram delivery failing since 05:00") {
		t.Fatalf("emails = %q, want the one notice", mailer.bodies)
	}
	if got := countContaining(f.st.alertTexts(), "Telegram delivery failing"); got != 1 {
		t.Fatalf("alert log = %q, want the notice recorded once", f.st.alertTexts())
	}

	f.maint.held = true
	for range 4 {
		f.next(t)
	}
	if got := mailer.count(); got != 1 {
		t.Fatalf("emails = %d, want no second notice while the failure lasts", got)
	}

	// Recovery, then a new failure noticed only in a quiet tick.
	f.tlg.setFail(false)
	f.maint.held = false
	f.next(t)
	f.tlg.setFail(true)
	f.r.Push(context.Background(), "ups", "test", "test")
	f.maint.held = true
	f.now = f.now.Add(2 * time.Minute)
	f.tick(t)
	if got := mailer.count(); got != 2 {
		t.Fatalf("emails = %d, want the new failure noticed in a quiet tick", got)
	}
}

// TestTelegramFailingAfterDefault is 15 minutes.
func TestTelegramFailingAfterDefault(t *testing.T) {
	r := New(Options{})
	if r.opts.TelegramFailingAfter != DefaultTelegramFailingAfter || DefaultTelegramFailingAfter != 15*time.Minute {
		t.Fatalf("telegram_failing_after = %v, want 15m", r.opts.TelegramFailingAfter)
	}
}

// alertTexts returns the texts of the alert-log records written so far.
func (f *fakeStore) alertTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.alerts))
	for _, a := range f.alerts {
		out = append(out, a.Text)
	}

	return out
}

// TestSendContextEndsAfterTheSendStop: the tick's send context never ends
// before the lanes' send stop, so a send whose context ran out at the
// deadline is followed only by refusals, never by a send on another free
// lane (the SMS fallback after a Telegram hang).
func TestSendContextEndsAfterTheSendStop(t *testing.T) {
	r := New(Options{TickBudget: time.Millisecond, Telegram: &fakeSender{}, Host: host})
	for range 500 {
		start := time.Now()
		sendCtx, end := r.boundSends(context.Background())
		stop := r.sends.stop(false)
		if dl, ok := sendCtx.Deadline(); !ok || dl.Before(start) || dl.After(time.Now().Add(time.Millisecond)) {
			end()
			t.Fatalf("send deadline = %v (set %v), want the send budget from the start", dl, ok)
		}
		<-sendCtx.Done()
		if !errors.Is(context.Cause(sendCtx), context.DeadlineExceeded) {
			end()
			t.Fatalf("cause = %v, want the deadline", context.Cause(sendCtx))
		}
		select {
		case <-stop:
		default:
			end()
			t.Fatal("the send context ended while the send stop was still open")
		}
		end()
	}
}
