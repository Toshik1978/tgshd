package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeConsumer implements TelegramConsumer for tests.
type fakeConsumer struct {
	mu        sync.Mutex
	startErr  error
	stopErr   error
	startCall int
	stopCall  int
}

func (f *fakeConsumer) Start(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCall++
	return f.startErr
}

func (f *fakeConsumer) Stop(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCall++
	return f.stopErr
}

func (f *fakeConsumer) started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startCall
}

func (f *fakeConsumer) stopped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopCall
}

// fakeWorker implements Worker for tests.
type fakeWorker struct {
	name     string
	duration time.Duration
	doErr    error
	doCalls  atomic.Int32
}

func (f *fakeWorker) Name() string {
	return f.name
}

func (f *fakeWorker) Duration() time.Duration {
	return f.duration
}

func (f *fakeWorker) Do(_ context.Context) error {
	f.doCalls.Add(1)
	return f.doErr
}

func newTestApplication(tlg *fakeConsumer, workers []Worker) *Application {
	params := ApplicationParams{
		Logger:   zap.NewNop(),
		Telegram: tlg,
		Workers:  workers,
	}

	return NewApplication(params, "commit", "stamp")
}

func TestOnStartHappyPath(t *testing.T) {
	tlg := &fakeConsumer{}
	worker := &fakeWorker{name: "worker1", duration: 50 * time.Millisecond}
	app := newTestApplication(tlg, []Worker{worker})

	ctx, cancel := context.WithCancel(context.Background())

	if err := app.OnStart(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() {
		if err := app.OnStop(context.Background(), cancel); err != nil {
			t.Errorf("OnStop failed: %v", err)
		}
	}()

	if tlg.started() != 1 {
		t.Errorf("expected Telegram.Start called once, got %d", tlg.started())
	}

	// Poll with a short timeout to check that the worker ran at least once,
	// rather than relying on a fixed sleep.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if worker.doCalls.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if worker.doCalls.Load() == 0 {
		t.Error("expected worker.Do to have run at least once")
	}
}

func TestOnStartTelegramError(t *testing.T) {
	tlg := &fakeConsumer{startErr: errors.New("boom")}
	worker := &fakeWorker{name: "worker1", duration: time.Second}
	app := newTestApplication(tlg, []Worker{worker})

	ctx := t.Context()

	err := app.OnStart(ctx)
	if err == nil {
		t.Fatal("expected an error when Telegram.Start fails")
	}

	// Scheduler was still created/started, so shut it down to avoid leaks.
	if err := app.onStop(context.Background()); err != nil {
		t.Errorf("cleanup onStop failed: %v", err)
	}
}

func TestOnStop(t *testing.T) {
	tlg := &fakeConsumer{}
	worker := &fakeWorker{name: "worker1", duration: time.Second}
	app := newTestApplication(tlg, []Worker{worker})

	ctx, cancel := context.WithCancel(context.Background())
	if err := app.OnStart(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cancelCalled := false
	wrappedCancel := func() {
		cancelCalled = true
		cancel()
	}

	if err := app.OnStop(context.Background(), wrappedCancel); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cancelCalled {
		t.Error("expected cancel func to be called")
	}
	if tlg.stopped() != 1 {
		t.Errorf("expected Telegram.Stop called once, got %d", tlg.stopped())
	}
}

func TestOnStopTelegramError(t *testing.T) {
	tlg := &fakeConsumer{}
	worker := &fakeWorker{name: "worker1", duration: time.Second}
	app := newTestApplication(tlg, []Worker{worker})

	ctx, cancel := context.WithCancel(context.Background())
	if err := app.OnStart(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tlg.mu.Lock()
	tlg.stopErr = errors.New("stop boom")
	tlg.mu.Unlock()

	if err := app.OnStop(context.Background(), cancel); err == nil {
		t.Fatal("expected an error when Telegram.Stop fails")
	}
}

func TestBootstrap(_ *testing.T) {
	app := newTestApplication(&fakeConsumer{}, nil)
	app.Bootstrap() // no-op, exercised for coverage.
}
