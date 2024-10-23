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
