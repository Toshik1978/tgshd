package telegram

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryUntilSucceedsAfterFailures(t *testing.T) {
	calls := 0
	var delays []time.Duration
	err := retryUntil(context.Background(), time.Millisecond, 4*time.Millisecond,
		func() error {
			calls++
			if calls < 5 {
				return errors.New("offline")
			}

			return nil
		},
		func(_ error, delay time.Duration) { delays = append(delays, delay) },
	)
	if err != nil {
		t.Fatalf("retryUntil() = %v, want nil", err)
	}
	if calls != 5 {
		t.Fatalf("calls = %d, want 5", calls)
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays = %v, want %v", delays, want)
		}
	}
}

func TestRetryUntilStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryUntil(ctx, time.Hour, time.Hour,
		func() error {
			calls++
			cancel()

			return errors.New("offline")
		},
		func(error, time.Duration) {},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retryUntil() = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}
