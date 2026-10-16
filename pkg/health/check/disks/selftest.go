package disks

import (
	"slices"
	"time"
)

// SelfTestHeadSize is how many of the newest log entries the baseline keeps
// to recognise what is new on the next read.
const SelfTestHeadSize = 5

// SelfTestState is what the baseline remembers about a drive's self-test log.
type SelfTestState struct {
	// Head is the identities of the newest up to SelfTestHeadSize entries seen
	// last time, newest first. Empty on a first sighting and for a baseline
	// stored before the head existed.
	Head []SelfTestID
	// Seen is true once the log has been read, even when it was empty: the
	// entries that appear in a log seen empty are new. False on a first
	// sighting and for a baseline stored before it existed, whose head (when
	// not empty) tells the same.
	Seen bool
	// OKAt is when a passed self-test was last newly observed (Unix seconds),
	// zero when never.
	OKAt int64
}

// SelfTestNewest is the newest finished self-test, for status while the drive
// is not read.
type SelfTestNewest struct {
	Result      string `json:"result"` // passed or failed
	Description string `json:"description"`
	// Hours is the drive's power-on hours when the test ran; zero when the
	// log gave none (and for data stored before it was kept).
	Hours int64 `json:"hours,omitempty"`
}

// SelfTestUpdate is the outcome of reading a self-test log against the stored
// state.
type SelfTestUpdate struct {
	// Failures are the new failed entries that are not the newest finished
	// entry, newest first. Each one is reported once; the newest finished
	// entry is reported by the self-test condition instead.
	Failures []SelfTestEntry
	// Head and Seen are the state to store.
	Head []SelfTestID
	Seen bool
	// OKAt is the ok time to store, never later than now.
	OKAt int64
	// Newest is the newest finished entry, nil when the log has none.
	Newest *SelfTestNewest
}

// AlignSelfTests compares a drive's self-test log (newest first) with the
// stored state. Entries above the first position where the stored head
// matches are new. When the head cannot be aligned (log cleared or wrapped),
// only entries newer than the stored newest power-on hours are new.
//
// A newly observed passed entry sets OKAt to now. A log never seen before is
// adopted whole as the head: nothing is new, OKAt is left alone (a stale
// adopted log must not make an overdue drive look freshly tested), and the
// age then rests on the first-sighting grace (see SelfTestAge). Every entry
// of a log last seen empty is new.
func AlignSelfTests(prev SelfTestState, log []SelfTestEntry, now time.Time) SelfTestUpdate {
	out := SelfTestUpdate{Head: prev.Head, Seen: true, OKAt: min(prev.OKAt, now.Unix())}

	newestFinished, newest := newestOf(log)
	out.Newest = newest

	if len(log) == 0 {
		return out
	}
	out.Head = logHead(log)
	n := len(log)
	switch {
	case len(prev.Head) > 0:
		n = newCount(prev.Head, log)
	case !prev.Seen:
		return out
	}

	for i, e := range log[:n] {
		if e.Result == SelfTestPassed {
			out.OKAt = now.Unix()
		}
		if e.Result == SelfTestFailed && i != newestFinished {
			out.Failures = append(out.Failures, e)
		}
	}

	return out
}

// newestOf finds the newest finished entry of a log (newest first): its
// position, -1 when none, and its summary.
func newestOf(log []SelfTestEntry) (int, *SelfTestNewest) {
	for i, e := range log {
		if e.Result != SelfTestPassed && e.Result != SelfTestFailed {
			continue
		}
		n := &SelfTestNewest{Result: resultName(e.Result), Description: e.Description}
		if e.hasHours {
			n.Hours = e.ID.Hours
		}

		return i, n
	}

	return -1, nil
}

// SelfTestAge is the calendar time since the last observed pass, or since the
// drive was first seen when none was observed. A future okAt (clock step) is
// clamped to now.
func SelfTestAge(okAt int64, firstSeen, now time.Time) time.Duration {
	start := firstSeen
	if okAt > 0 {
		if ok := time.Unix(okAt, 0); ok.After(start) {
			start = ok
		}
	}
	if start.After(now) {
		return 0
	}

	return now.Sub(start)
}

// newCount is how many of the newest log entries are new relative to head,
// which must not be empty.
func newCount(head []SelfTestID, log []SelfTestEntry) int {
	if at := alignedAt(head, log); at >= 0 {
		return at
	}
	// Not aligned: only entries newer than the stored newest hours are new.
	n := 0
	for n < len(log) && log[n].hasHours && newerThan(log[n], head[0].Hours) {
		n++
	}

	return n
}

// newerThan reports whether e ran after power-on hours h. ATA hours wrap at
// 65536, so they are compared modulo 65536: e is newer when it is less than
// half the range ahead of h.
func newerThan(e SelfTestEntry, h int64) bool {
	if !e.wraps {
		return e.ID.Hours > h
	}
	d := (e.ID.Hours - h) % ataSelfTestHoursWrap
	if d < 0 {
		d += ataSelfTestHoursWrap
	}

	return d != 0 && d < ataSelfTestHoursWrap/2
}

// alignedAt returns the first log position where the head matches, or -1.
// A head longer than the rest of the log must match the whole rest.
func alignedAt(head []SelfTestID, log []SelfTestEntry) int {
	for i := range log {
		n := min(len(head), len(log)-i)
		match := true
		for k := range n {
			if log[i+k].ID != head[k] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}

	return -1
}

func logHead(log []SelfTestEntry) []SelfTestID {
	head := make([]SelfTestID, 0, SelfTestHeadSize)
	for _, e := range log[:min(len(log), SelfTestHeadSize)] {
		head = append(head, e.ID)
	}

	return slices.Clip(head)
}

func resultName(r SelfTestResult) string {
	if r == SelfTestFailed {
		return "failed"
	}

	return "passed"
}
