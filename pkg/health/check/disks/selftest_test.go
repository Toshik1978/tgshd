package disks

import (
	"slices"
	"testing"
	"time"
)

func stEntry(hours int64, res SelfTestResult) SelfTestEntry {
	return SelfTestEntry{
		ID:          SelfTestID{Hours: hours, Type: 1, Code: int(res)},
		Description: "Short offline",
		Result:      res,
		hasHours:    true,
	}
}

func stHead(log []SelfTestEntry) []SelfTestID {
	ids := make([]SelfTestID, 0, len(log))
	for _, e := range log {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestAlignSelfTests(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	nowU := now.Unix()

	// Newest first.
	log := []SelfTestEntry{
		stEntry(900, SelfTestPassed),
		stEntry(800, SelfTestFailed),
		stEntry(700, SelfTestPassed),
		stEntry(600, SelfTestPassed),
		stEntry(500, SelfTestPassed),
		stEntry(400, SelfTestPassed),
		stEntry(300, SelfTestPassed),
	}
	head5 := stHead(log[:5])

	tests := []struct {
		name         string
		prev         SelfTestState
		log          []SelfTestEntry
		wantFailures []int64 // hours
		wantHead     []SelfTestID
		wantOKAt     int64
		wantNewest   *SelfTestNewest
	}{
		{
			name:       "first sighting adopts the whole log, no event, no ok time",
			prev:       SelfTestState{},
			log:        log,
			wantHead:   head5,
			wantOKAt:   0,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name:       "first sighting keeps a stored 001 ok time",
			prev:       SelfTestState{OKAt: 500},
			log:        log,
			wantHead:   head5,
			wantOKAt:   500,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name:       "nothing new",
			prev:       SelfTestState{Head: head5, OKAt: 500},
			log:        log,
			wantHead:   head5,
			wantOKAt:   500,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name: "new pass sets ok time",
			prev: SelfTestState{Head: head5, OKAt: 500},
			log: append([]SelfTestEntry{stEntry(1000, SelfTestPassed)},
				log...),
			wantHead:   stHead(append([]SelfTestEntry{stEntry(1000, SelfTestPassed)}, log[:4]...)),
			wantOKAt:   nowU,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name: "new failure that is the newest finished entry is left to the state condition",
			prev: SelfTestState{Head: head5, OKAt: 500},
			log: append([]SelfTestEntry{stEntry(1000, SelfTestFailed)},
				log...),
			wantHead:   stHead(append([]SelfTestEntry{stEntry(1000, SelfTestFailed)}, log[:4]...)),
			wantOKAt:   500,
			wantNewest: &SelfTestNewest{Result: "failed", Description: "Short offline"},
		},
		{
			name: "failure followed by a pass is one event and the pass sets ok time",
			prev: SelfTestState{Head: head5, OKAt: 500},
			log: append([]SelfTestEntry{stEntry(1100, SelfTestPassed), stEntry(1000, SelfTestFailed)},
				log...),
			wantFailures: []int64{1000},
			wantHead: stHead(
				append([]SelfTestEntry{stEntry(1100, SelfTestPassed), stEntry(1000, SelfTestFailed)}, log[:3]...),
			),
			wantOKAt:   nowU,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name: "aborted and in progress do not count as pass or failure",
			prev: SelfTestState{Head: head5, OKAt: 500},
			log: append([]SelfTestEntry{stEntry(1100, SelfTestInProgress), stEntry(1000, SelfTestAborted)},
				log...),
			wantHead: stHead(
				append([]SelfTestEntry{stEntry(1100, SelfTestInProgress), stEntry(1000, SelfTestAborted)}, log[:3]...),
			),
			wantOKAt:   500,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name:     "old failure in the log is not re-reported when it is below the head",
			prev:     SelfTestState{Head: head5, OKAt: 500},
			log:      log, // the failure at 800 is inside the head
			wantHead: head5, wantOKAt: 500,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name: "unaligned head: only entries newer than the stored newest hours are new",
			prev: SelfTestState{Head: []SelfTestID{{Hours: 5000, Type: 1, Code: 0}}, OKAt: 500},
			log: []SelfTestEntry{
				stEntry(6200, SelfTestPassed),
				stEntry(6100, SelfTestFailed),
				stEntry(4000, SelfTestFailed), // older than stored newest: not new
			},
			wantFailures: []int64{6100},
			wantHead: []SelfTestID{
				{Hours: 6200, Type: 1, Code: int(SelfTestPassed)},
				{Hours: 6100, Type: 1, Code: int(SelfTestFailed)},
				{Hours: 4000, Type: 1, Code: int(SelfTestFailed)},
			},
			wantOKAt:   nowU,
			wantNewest: &SelfTestNewest{Result: "passed", Description: "Short offline"},
		},
		{
			name:       "unaligned head with nothing newer changes nothing but the head",
			prev:       SelfTestState{Head: []SelfTestID{{Hours: 5000, Type: 1, Code: 0}}, OKAt: 500},
			log:        []SelfTestEntry{stEntry(100, SelfTestFailed)},
			wantHead:   []SelfTestID{{Hours: 100, Type: 1, Code: int(SelfTestFailed)}},
			wantOKAt:   500,
			wantNewest: &SelfTestNewest{Result: "failed", Description: "Short offline"},
		},
		{
			name:     "empty log keeps the stored head and ok time",
			prev:     SelfTestState{Head: head5, OKAt: 500},
			log:      nil,
			wantHead: head5,
			wantOKAt: 500,
		},
		{
			name:     "future ok time is clamped to now",
			prev:     SelfTestState{Head: head5, OKAt: nowU + 9999},
			log:      log,
			wantHead: head5,
			wantOKAt: nowU,
			wantNewest: &SelfTestNewest{
				Result:      "passed",
				Description: "Short offline",
			},
		},
		{
			name: "first sighting of a failed newest entry raises no event",
			prev: SelfTestState{},
			log:  []SelfTestEntry{stEntry(900, SelfTestFailed), stEntry(800, SelfTestPassed)},
			wantHead: []SelfTestID{
				{Hours: 900, Type: 1, Code: int(SelfTestFailed)},
				{Hours: 800, Type: 1, Code: int(SelfTestPassed)},
			},
			wantNewest: &SelfTestNewest{Result: "failed", Description: "Short offline"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AlignSelfTests(tt.prev, tt.log, now)

			var hours []int64
			for _, f := range got.Failures {
				hours = append(hours, f.ID.Hours)
			}
			if !slices.Equal(hours, tt.wantFailures) {
				t.Errorf("failures = %v, want %v", hours, tt.wantFailures)
			}
			if !slices.Equal(got.Head, tt.wantHead) {
				t.Errorf("head = %v, want %v", got.Head, tt.wantHead)
			}
			if got.OKAt != tt.wantOKAt {
				t.Errorf("OKAt = %d, want %d", got.OKAt, tt.wantOKAt)
			}
			switch {
			case (got.Newest == nil) != (tt.wantNewest == nil):
				t.Errorf("newest = %v, want %v", got.Newest, tt.wantNewest)
			case got.Newest != nil &&
				(got.Newest.Result != tt.wantNewest.Result || got.Newest.Description != tt.wantNewest.Description):
				t.Errorf("newest = %v, want %v", *got.Newest, *tt.wantNewest)
			}
		})
	}
}

func TestAlignSelfTestsHeadCap(t *testing.T) {
	var log []SelfTestEntry
	for h := int64(1000); h > 0; h -= 100 {
		log = append(log, stEntry(h, SelfTestPassed))
	}
	got := AlignSelfTests(SelfTestState{}, log, time.Unix(10, 0))
	if len(got.Head) != SelfTestHeadSize {
		t.Fatalf("head length = %d, want %d", len(got.Head), SelfTestHeadSize)
	}
}

func TestSelfTestAge(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	first := now.Add(-100 * time.Hour)

	tests := []struct {
		name string
		okAt int64
		want time.Duration
	}{
		{"no pass observed uses first sighting", 0, 100 * time.Hour},
		{"ok time newer than first sighting", now.Add(-10 * time.Hour).Unix(), 10 * time.Hour},
		{"ok time older than first sighting", now.Add(-500 * time.Hour).Unix(), 100 * time.Hour},
		{"future ok time clamps to now", now.Add(time.Hour).Unix(), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelfTestAge(tt.okAt, first, now); got != tt.want {
				t.Errorf("age = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAlignSelfTestsNewestHours: the newest finished entry's power-on hours
// are kept, skipping an unfinished entry above it.
func TestAlignSelfTestsNewestHours(t *testing.T) {
	log := []SelfTestEntry{stEntry(950, SelfTestInProgress), stEntry(900, SelfTestFailed), stEntry(800, SelfTestPassed)}
	got := AlignSelfTests(SelfTestState{}, log, time.Unix(1_000_000, 0))
	if got.Newest == nil || got.Newest.Result != "failed" || got.Newest.Hours != 900 {
		t.Errorf("Newest = %+v, want failed at 900 h", got.Newest)
	}
}
