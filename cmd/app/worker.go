package app

import (
	"context"
	"time"
)

// Worker define interface for workers.
type Worker interface {
	// Name returns name of the worker.
	Name() string
	// Duration returns duration to run worker.
	Duration() time.Duration
	// Do is the entry point of worker.
	Do(ctx context.Context) error
}

// TelegramConsumer consumes and dispatches incoming Telegram messages.
type TelegramConsumer interface {
	// Start starts consuming messages.
	Start(ctx context.Context) error
	// Stop stops consuming messages.
	Stop(ctx context.Context) error
}
