package health

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// RateMark records when a rate-limited message last went out on a channel.
type RateMark struct {
	Key        string
	Channel    string
	LastSentAt time.Time
}

type rateKey struct {
	key     string
	channel string
}

// RateMarks limits how often a message identified by key and channel may be
// sent, e.g. checker errors (24 h) or kernel "still happening" (6 h). It is
// safe for concurrent use.
type RateMarks struct {
	mu    sync.Mutex
	marks map[rateKey]time.Time
}

// NewRateMarks returns an empty set of rate marks.
func NewRateMarks() *RateMarks {
	return &RateMarks{marks: make(map[rateKey]time.Time)}
}

// Allow reports whether a message for key on channel may be sent at now, given
// the minimum window between sends. When it returns true the mark is advanced
// to now; a denial leaves the mark untouched. A mark in the future (the clock
// went backwards) does not suppress anything.
func (r *RateMarks) Allow(key, channel string, now time.Time, window time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := rateKey{key: key, channel: channel}
	if last, ok := r.marks[k]; ok {
		if elapsed := now.Sub(last); elapsed >= 0 && elapsed < window {
			return false
		}
	}
	r.marks[k] = now

	return true
}

// Revert gives back the mark that Allow set at at, for a send that was
// refused without being attempted; a mark set since is kept.
func (r *RateMarks) Revert(key, channel string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := rateKey{key: key, channel: channel}
	if last, ok := r.marks[k]; ok && last.Equal(at) {
		delete(r.marks, k)
	}
}

// Export returns the marks ordered by key then channel, for persistence.
func (r *RateMarks) Export() []RateMark {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]RateMark, 0, len(r.marks))
	for k, at := range r.marks {
		out = append(out, RateMark{Key: k.key, Channel: k.channel, LastSentAt: at})
	}
	slices.SortFunc(out, func(a, b RateMark) int {
		return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.Channel, b.Channel))
	})

	return out
}

// Merge folds the given marks in, keeping the later one per key and channel.
func (r *RateMarks) Merge(marks []RateMark) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, m := range marks {
		k := rateKey{key: m.Key, channel: m.Channel}
		if cur, ok := r.marks[k]; !ok || m.LastSentAt.After(cur) {
			r.marks[k] = m.LastSentAt
		}
	}
}

// Restore replaces the marks with the given ones, e.g. loaded from the store.
func (r *RateMarks) Restore(marks []RateMark) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.marks = make(map[rateKey]time.Time, len(marks))
	for _, m := range marks {
		r.marks[rateKey{key: m.Key, channel: m.Channel}] = m.LastSentAt
	}
}
