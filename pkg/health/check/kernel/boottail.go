package kernel

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
)

var (
	// ErrNoJournal is returned by a JournalSource when there is no persistent journal to read.
	ErrNoJournal = errors.New("no persistent journal")
	// ErrJournalCapped is returned by a JournalSource, together with the records read, when the
	// boot holds more records than the source hands over.
	ErrJournalCapped = errors.New("journal record cap reached")
)

// JournalSource reads the kernel records of a past boot from the persistent journal, oldest
// first. Each record's Time is the journal's _SOURCE_MONOTONIC_TIMESTAMP and its Seq is
// unused. The read honours ctx; when ctx ends or the cap is reached, the records read so far
// are returned with the error.
type JournalSource interface {
	Read(ctx context.Context, bootID string) ([]Record, error)
}

// bootTail reads the stored boot's records after the cursor from the journal, bounded by
// health.MaxCheckTimeout, and reports each class found as one Event. A read cut by that limit
// or by the source's record cap still reports what was read and raises a checker error. A read
// that fails otherwise reports what was read and logs a warning. No journal source or no
// persistent journal skip the tail. The caller holds c.mu and moves the cursor afterwards
// unless ctx itself ended.
func (c *Check) bootTail(ctx context.Context) ([]health.Reading, error) {
	if c.journal == nil {
		return nil, nil
	}

	cur := c.st.Cursor
	bounded, cancel, _ := health.Bound(ctx, health.MaxCheckTimeout)
	defer cancel()
	recs, err := c.journal.Read(bounded, cur.BootID)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("read previous boot's kernel log tail: %w", ctx.Err())
	}

	var reason string
	switch {
	case errors.Is(err, ErrJournalCapped):
		reason = "record cap"
	case errors.Is(err, ErrNoJournal):
		c.log.Info("previous boot's kernel log tail could not be checked: no persistent journal")

		return nil, nil
	case err != nil && bounded.Err() != nil:
		reason = "time limit"
	case err != nil:
		// The records read before the failure are still reported.
		c.log.Warn("previous boot's kernel log tail could not be checked", zap.Error(err))

		return c.tailReadings(tailAfter(recs, cur)), nil
	}

	readings := c.tailReadings(tailAfter(recs, cur))
	if reason == "" {
		return readings, nil
	}

	return readings, health.CheckError{
		Subject: "boottail",
		Kind:    "incomplete",
		Err:     fmt.Errorf("previous boot's kernel log tail was not read completely: %s", reason),
	}
}

// tailAfter returns the records logged after the cursor's record. Records at the cursor's time
// are compared by message: those up to and including the first one carrying the cursor's
// message were processed; when none carries it, every one of them is kept.
func tailAfter(recs []Record, cur Cursor) []Record {
	if !cur.Seen {
		return recs
	}

	at := -1
	for i, r := range recs {
		if usec(r.Time) == cur.Usec && r.Message == cur.Message {
			at = i

			break
		}
	}

	var out []Record
	for i, r := range recs {
		t := usec(r.Time)
		if t > cur.Usec || (t == cur.Usec && i > at) {
			out = append(out, r)
		}
	}

	return out
}

// tailReadings reports every class the kernel records match, one Event per class.
func (c *Check) tailReadings(recs []Record) []health.Reading {
	var (
		found []entry
		kills []oomKill
		pair  oomPairer
	)
	for _, r := range recs {
		if r.Facility != 0 {
			continue
		}
		if kill, paired := pair.feed(r.Message); paired {
			kills = append(kills, kill)

			continue
		}
		if key, ok := classify(c.classes, r); ok {
			found = append(found, entry{key: key, msg: r.Message})
		}
	}

	agg := aggregate(found)
	var out []health.Reading
	for _, cl := range c.classes {
		if p := agg[cl.key]; p != nil {
			out = append(out, event(cl.key, fmt.Sprintf("Kernel (previous boot): %s found (%s). Last: %s",
				cl.label, records(p.count), clip(p.sample))))
		}
	}

	return append(out, oomReadings(c.services, kills, "Kernel (previous boot)")...)
}
