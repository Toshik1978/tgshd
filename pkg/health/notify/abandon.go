package notify

import (
	"errors"
)

// ErrNotSent is wrapped by the error of a send that was refused without being
// attempted (e.g. the tick's delivery deadline had passed): nothing went out,
// so a rate mark spent on it is given back.
var ErrNotSent = errors.New("send not attempted")

// ErrPastDeadline is wrapped, with ErrNotSent, by the error of a send refused
// because the tick's delivery deadline had passed. Every other channel of the
// tick is past the same deadline, so no fallback is tried; a refusal without
// it (an abandoned send on the channel still hangs) may fall back.
var ErrPastDeadline = errors.New("delivery deadline passed")

// AbandonedError is returned by a Sender (or Mailer) that stopped waiting for
// a send still in progress. Result yields the send's own outcome once it
// returns. The notifier queues the message but does not re-send it while the
// send runs: a late success drops it from the queue, a late failure leaves it
// for the next flush.
type AbandonedError struct {
	Cause  error
	Result <-chan error
}

// Error implements error.
func (e *AbandonedError) Error() string {
	return "send abandoned while in progress: " + e.Cause.Error()
}

// Unwrap returns the cause.
func (e *AbandonedError) Unwrap() error { return e.Cause }

// flight is a queued message whose abandoned send is still in progress. Its
// id follows the message through a Merge renumbering.
type flight struct {
	id int64
}

// track marks the queued message id in flight when err is an AbandonedError,
// and settles it when the send returns.
func (n *Notifier) track(id int64, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.trackLocked(id, err)
}

// trackLocked is track for a caller that holds the lock.
func (n *Notifier) trackLocked(id int64, err error) {
	var ab *AbandonedError
	if !errors.As(err, &ab) || ab.Result == nil {
		return
	}

	f := &flight{id: id}
	n.inflight[id] = f

	go n.settle(f, ab.Result)
}

// settle waits for an abandoned send: a success drops the message from the
// queue, a failure leaves it for the next flush.
func (n *Notifier) settle(f *flight, result <-chan error) {
	err := <-result

	n.mu.Lock()
	defer n.mu.Unlock()

	if n.inflight[f.id] != f {
		return // Dropped from the queue meanwhile (Restore, Expire, trim).
	}

	delete(n.inflight, f.id)

	if err == nil {
		n.removeLocked(f.id)
	} else {
		n.bumpLocked(f.id)
	}
}

// inFlight reports whether the queued message id has a send in progress.
func (n *Notifier) inFlight(id int64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	_, ok := n.inflight[id]

	return ok
}

// renumberLocked moves the in-flight marks to the messages' new IDs (old ID
// to new ID). The caller holds the lock.
func (n *Notifier) renumberLocked(ids map[int64]int64) {
	moved := make(map[int64]*flight, len(n.inflight))
	for from, f := range n.inflight {
		if to, ok := ids[from]; ok {
			f.id = to
			moved[to] = f
		}
	}

	n.inflight = moved
}
