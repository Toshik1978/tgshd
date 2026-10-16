package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNotOpen is returned by a read or the retention of a Deferred whose
	// database is not open (yet); it also wraps the last open error.
	ErrNotOpen = errors.New("the state database is not open")
	// ErrClosed is returned by every call on a Deferred after Close.
	ErrClosed = errors.New("the state database is closed")
)

// Deferred is a Store that the runner's Load and Checkpoint open when first
// used and, after a failed open, again on each later use, so the daemon
// reaches its state database once the state directory becomes available
// instead of running from memory until a restart. Until it is open those
// calls fail with the open error, Degraded reports true, and the reads and
// the retention fail fast with ErrNotOpen: they never open the database nor
// wait for a pending open (a command must not hang on it). After Close every
// call fails with ErrClosed and the database is never opened again, so
// nothing reaches it once the instance lock is released.
type Deferred struct {
	open func(ctx context.Context) (*Store, error)

	// openMu serialises the opens and Close; st is set once an open has
	// succeeded, closed by Close.
	openMu  sync.Mutex
	st      atomic.Pointer[Store]
	closed  atomic.Bool
	lastErr atomic.Pointer[error] // the last open error, while not open
}

// NewDeferred returns a Deferred over st, which may be nil when the first open
// failed; open opens the database on a later call.
func NewDeferred(st *Store, open func(ctx context.Context) (*Store, error)) *Deferred {
	d := &Deferred{open: open}
	if st != nil {
		d.st.Store(st)
	}

	return d
}

// Load implements the runner's store, opening the database first when needed.
func (d *Deferred) Load(ctx context.Context) (State, error) {
	st, err := d.get(ctx)
	if err != nil {
		return State{}, err
	}

	return st.Load(ctx)
}

// Checkpoint implements the runner's store, opening the database first when needed.
func (d *Deferred) Checkpoint(ctx context.Context, dirty State, newAlerts []Alert, newSamples []SampleRow) error {
	st, err := d.get(ctx)
	if err != nil {
		return err
	}

	return st.Checkpoint(ctx, dirty, newAlerts, newSamples)
}

// Degraded reports whether the database is not open yet or its last
// checkpoint could not be written. It never opens the database.
func (d *Deferred) Degraded() bool {
	st := d.st.Load()

	return st == nil || st.Degraded()
}

// Alerts is Store.Alerts; it fails fast while the database is not open.
func (d *Deferred) Alerts(ctx context.Context, filter string, since time.Time, limit int) ([]Alert, error) {
	st, err := d.opened()
	if err != nil {
		return nil, err
	}

	return st.Alerts(ctx, filter, since, limit)
}

// Metrics is Store.Metrics; it fails fast while the database is not open.
func (d *Deferred) Metrics(ctx context.Context, prefix string, now time.Time) ([]MetricStats, error) {
	st, err := d.opened()
	if err != nil {
		return nil, err
	}

	return st.Metrics(ctx, prefix, now)
}

// Retention is Store.Retention; it fails fast while the database is not open.
func (d *Deferred) Retention(ctx context.Context, now time.Time) (RetentionResult, error) {
	st, err := d.opened()
	if err != nil {
		return RetentionResult{}, err
	}

	return st.Retention(ctx, now)
}

// Close closes the database if it was opened; later calls fail with
// ErrClosed. Closing again does nothing.
func (d *Deferred) Close() error {
	d.openMu.Lock()
	defer d.openMu.Unlock()

	if d.closed.Swap(true) {
		return nil
	}
	if st := d.st.Load(); st != nil {
		return st.Close()
	}

	return nil
}

// opened returns the store when it is open, without opening it or waiting
// for a pending open.
func (d *Deferred) opened() (*Store, error) {
	if d.closed.Load() {
		return nil, ErrClosed
	}
	if st := d.st.Load(); st != nil {
		return st, nil
	}
	if last := d.lastErr.Load(); last != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotOpen, *last)
	}

	return nil, ErrNotOpen
}

// get returns the open store, opening it first when needed.
func (d *Deferred) get(ctx context.Context) (*Store, error) {
	if st, err := d.opened(); !errors.Is(err, ErrNotOpen) {
		return st, err
	}

	d.openMu.Lock()
	defer d.openMu.Unlock()

	if st, err := d.opened(); !errors.Is(err, ErrNotOpen) {
		return st, err
	}
	st, err := d.open(ctx)
	if err != nil {
		d.lastErr.Store(&err)

		return nil, fmt.Errorf("open the state database: %w", err)
	}
	d.st.Store(st)
	d.lastErr.Store(nil)

	return st, nil
}
