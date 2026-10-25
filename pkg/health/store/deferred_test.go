package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestDeferredRetriesAFailedOpen: until the database opens every call fails
// with the open error and the store reads as degraded; a later call opens it
// and the store then works normally.
func TestDeferredRetriesAFailedOpen(t *testing.T) {
	dir := t.TempDir()
	errUnavailable := errors.New("state directory unavailable")
	opens := 0
	d := NewDeferred(nil, func(ctx context.Context) (*Store, error) {
		opens++
		if opens == 1 {
			return nil, errUnavailable
		}

		return Open(ctx, dir)
	})
	ctx := t.Context()

	if _, err := d.Load(ctx); !errors.Is(err, errUnavailable) {
		t.Fatalf("Load = %v, want the open error", err)
	}
	if !d.Degraded() {
		t.Fatal("a store that is not open must read as degraded")
	}
	if opens != 1 {
		t.Fatalf("Degraded opened the store: %d opens", opens)
	}

	if _, err := d.Load(ctx); err != nil {
		t.Fatalf("Load after the retry: %v", err)
	}
	if d.Degraded() {
		t.Fatal("degraded once open")
	}
	now := time.Unix(1_700_000_000, 0)
	alert := Alert{At: now, Kind: KindEvent, Check: "disks", Key: "disks/x", Text: "x"}
	if err := d.Checkpoint(ctx, State{}, []Alert{alert}, nil); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got, err := d.Alerts(ctx, "", now.Add(-time.Hour), 10); err != nil || len(got) != 1 {
		t.Fatalf("Alerts = %+v, %v", got, err)
	}
	if _, err := d.Metrics(ctx, "drive.", now); err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if _, err := d.Retention(ctx, now); err != nil {
		t.Fatalf("Retention: %v", err)
	}
	if opens != 2 {
		t.Fatalf("opens = %d, want 2 (the store stays open)", opens)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDeferredUsesAnOpenStore(t *testing.T) {
	st, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := NewDeferred(st, func(context.Context) (*Store, error) {
		t.Fatal("an open store must not be opened again")

		return nil, nil //nolint:nilnil // Never reached.
	})
	defer func() { _ = d.Close() }()

	if _, err := d.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// readsFail asserts that the reads and the retention of d fail with want.
func readsFail(t *testing.T, d *Deferred, want error) {
	t.Helper()

	ctx := t.Context()
	now := time.Unix(1_700_000_000, 0)
	if _, err := d.Alerts(ctx, "", now, 1); !errors.Is(err, want) {
		t.Errorf("Alerts = %v, want %v", err, want)
	}
	if _, err := d.Metrics(ctx, "", now); !errors.Is(err, want) {
		t.Errorf("Metrics = %v, want %v", err, want)
	}
	if _, err := d.Retention(ctx, now); !errors.Is(err, want) {
		t.Errorf("Retention = %v, want %v", err, want)
	}
}

// TestDeferredCallsFailUntilOpen: only the runner's Load and Checkpoint open
// the database; the reads and the retention fail fast with ErrNotOpen (and
// the last open error, once there is one) instead of opening it themselves.
func TestDeferredCallsFailUntilOpen(t *testing.T) {
	errUnavailable := errors.New("state directory unavailable")
	opens := 0
	d := NewDeferred(nil, func(context.Context) (*Store, error) {
		opens++

		return nil, errUnavailable
	})

	readsFail(t, d, ErrNotOpen)
	if opens != 0 {
		t.Fatalf("the reads opened the store: %d opens", opens)
	}
	if err := d.Checkpoint(t.Context(), State{}, nil, nil); !errors.Is(err, errUnavailable) {
		t.Errorf("Checkpoint = %v", err)
	}
	readsFail(t, d, ErrNotOpen)
	readsFail(t, d, errUnavailable)
	if opens != 1 {
		t.Fatalf("opens = %d, want only the Checkpoint's", opens)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close of a store never opened = %v", err)
	}
}

// TestDeferredReadsDoNotWaitForAPendingOpen: a read while the runner's open
// is still in progress fails at once rather than waiting for it.
func TestDeferredReadsDoNotWaitForAPendingOpen(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d := NewDeferred(nil, func(context.Context) (*Store, error) {
		once.Do(func() { close(started) })
		<-release

		return nil, errors.New("state directory unavailable")
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = d.Load(t.Context())
	}()
	<-started

	// The reads run aside so a regression (a read waiting for the open)
	// fails at once instead of hanging the test.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		readsFail(t, d, ErrNotOpen)
	}()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Error("a read waited for the pending open")
	}
	close(release)
	<-readDone
	<-done
}

// TestDeferredClosedFailsFast: once closed, the store is never opened again;
// every call fails with ErrClosed.
func TestDeferredClosedFailsFast(t *testing.T) {
	for _, tt := range []struct {
		name string
		open bool
	}{{"never opened", false}, {"opened", true}} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			opens := 0
			d := NewDeferred(nil, func(ctx context.Context) (*Store, error) {
				opens++

				return Open(ctx, dir)
			})
			if tt.open {
				if _, err := d.Load(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			before := opens

			if _, err := d.Load(t.Context()); !errors.Is(err, ErrClosed) {
				t.Errorf("Load = %v, want ErrClosed", err)
			}
			if err := d.Checkpoint(t.Context(), State{}, nil, nil); !errors.Is(err, ErrClosed) {
				t.Errorf("Checkpoint = %v, want ErrClosed", err)
			}
			readsFail(t, d, ErrClosed)
			if opens != before {
				t.Errorf("opened after Close: %d opens, want %d", opens, before)
			}
			if err := d.Close(); err != nil {
				t.Errorf("second Close = %v", err)
			}
		})
	}
}

// TestDeferredPutKV: PutKV opens the database like Checkpoint, and fails
// once closed.
func TestDeferredPutKV(t *testing.T) {
	dir := t.TempDir()
	errUnavailable := errors.New("state directory unavailable")
	opens := 0
	d := NewDeferred(nil, func(ctx context.Context) (*Store, error) {
		opens++
		if opens == 1 {
			return nil, errUnavailable
		}

		return Open(ctx, dir)
	})
	ctx := t.Context()

	if err := d.PutKV(ctx, "memory.guard", "[]"); !errors.Is(err, errUnavailable) {
		t.Fatalf("PutKV = %v, want the open error", err)
	}
	if err := d.PutKV(ctx, "memory.guard", "[1]"); err != nil {
		t.Fatalf("PutKV after the retry: %v", err)
	}
	st, err := d.Load(ctx)
	if err != nil || st.KV["memory.guard"] != "[1]" {
		t.Fatalf("Load = %+v, %v", st.KV, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.PutKV(ctx, "memory.guard", "[2]"); !errors.Is(err, ErrClosed) {
		t.Fatalf("PutKV after Close = %v, want ErrClosed", err)
	}
}
