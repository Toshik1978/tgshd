package runner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

type fakeBook struct {
	mu      sync.Mutex
	bases   []store.Baseline
	retired []string
}

func (b *fakeBook) Baselines() []store.Baseline {
	b.mu.Lock()
	defer b.mu.Unlock()

	return slices.Clone(b.bases)
}

func (b *fakeBook) Retire(serial string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i := range b.bases {
		if b.bases[i].Serial == serial && !b.bases[i].Retired {
			b.bases[i].Retired = true
			b.retired = append(b.retired, serial)

			return true
		}
	}

	return false
}

func newBook() *fakeBook {
	return &fakeBook{bases: []store.Baseline{
		{Serial: "TEST0000000001", Model: "Example NVMe SSD", Bus: "nvme"},
		{Serial: "TEST0000000002", Model: "Example SATA HDD", Bus: "ata"},
		{Serial: "OTHER000000002", Model: "Example SATA SSD", Bus: "ata"},
		{Serial: "GONE0000000001", Model: "Old Drive", Bus: "ata", Retired: true},
	}}
}

func retireFixture(t *testing.T, st *fakeStore, book *fakeBook) (*fixture, *fakeCheck) {
	t.Helper()

	c := newFakeCheck(nil, "disks", health.CheckResult{Readings: []health.Reading{
		{Key: "disks/TEST0000000001/health", Verdict: health.Bad, Text: "drive failed"},
		{Key: "disks/TEST0000000001/temp", Verdict: health.Bad, Text: "hot"},
		{Key: "disks/TEST0000000002/health", Verdict: health.Bad, Text: "other drive failed"},
	}})
	f := newFixture(t, st, checkOf(c))
	f.r.opts.Drives = book
	f.tick(t)

	return f, c
}

func TestRetire(t *testing.T) {
	st := &fakeStore{}
	book := newBook()
	f, c := retireFixture(t, st, book)
	sent := len(f.tlg.texts())

	res, err := f.r.Retire(context.Background(), "0001")
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if res.Drive.Serial != "TEST0000000001" || res.Cleared != 2 {
		t.Fatalf("result = %+v", res)
	}
	if got := res.Drive.Label(); got != "nvme Example NVMe SSD …0001" {
		t.Fatalf("label = %q", got)
	}

	texts := f.tlg.texts()[sent:]
	if len(texts) != 1 || !strings.Contains(texts[0], host) || !strings.Contains(texts[0], "Drive retired") ||
		strings.Contains(texts[0], "TEST0000000001") {
		t.Fatalf("pushed = %q", texts)
	}
	if _, ok := f.r.engine.Condition("disks/TEST0000000001/health"); ok {
		t.Fatal("condition still tracked")
	}
	if _, ok := f.r.engine.Condition("disks/TEST0000000002/health"); !ok {
		t.Fatal("other drive's condition was cleared")
	}

	c.set(health.CheckResult{})
	f.next(t)
	for _, s := range f.tlg.texts()[sent+1:] {
		if strings.Contains(s, "RESOLVED") {
			t.Fatalf("RESOLVED sent: %q", s)
		}
	}
	if !slices.Contains(st.alertKinds(), store.KindRetired) {
		t.Fatalf("alert log = %v", st.alertKinds())
	}
	if !slices.Equal(book.retired, []string{"TEST0000000001"}) {
		t.Fatalf("retired = %v", book.retired)
	}
}

func TestRetireMatching(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		wantErr error
		want    int // candidates
	}{
		{"full serial", "TEST0000000002", nil, 0},
		{"suffix case-insensitive", "r000000002", nil, 0},
		{"ambiguous suffix", "0002", ErrAmbiguousDrive, 2},
		{"too short", "001", ErrNoDrive, 3},
		{"unknown", "ZZZZ", ErrNoDrive, 3},
		{"empty", "", ErrNoDrive, 3},
		{"already retired", "GONE0000000001", ErrNoDrive, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			book := newBook()
			f, _ := retireFixture(t, &fakeStore{}, book)
			sent := len(f.tlg.texts())

			res, err := f.r.Retire(context.Background(), tc.arg)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(res.Candidates) != tc.want {
				t.Fatalf("candidates = %+v, want %d", res.Candidates, tc.want)
			}
			if tc.wantErr != nil && (len(book.retired) != 0 || len(f.tlg.texts()) != sent) {
				t.Fatalf("failed retire had effects: %v %q", book.retired, f.tlg.texts()[sent:])
			}
		})
	}
}

func TestRetireWithoutDrives(t *testing.T) {
	f := newFixture(t, &fakeStore{})
	if _, err := f.r.Retire(context.Background(), "ABCD"); !errors.Is(err, ErrNoDrive) {
		t.Fatalf("err = %v", err)
	}
}

func TestRetireBetweenRunAndApply(t *testing.T) {
	book := newBook()
	entered, release := make(chan struct{}), make(chan struct{})
	chk := health.Check{
		Name: "disks", Interval: Tick, Mode: health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			close(entered)
			<-release

			return health.CheckResult{Readings: []health.Reading{
				{Key: "disks/TEST0000000001/health", Verdict: health.Bad, Text: "stale"},
				{Key: "disks/TEST0000000002/health", Verdict: health.Bad, Text: "other"},
			}}
		},
	}
	f := newFixture(t, &fakeStore{}, chk)
	f.r.opts.Drives = book

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.tick(t)
	}()
	<-entered
	if _, err := f.r.Retire(context.Background(), "TEST0000000001"); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	close(release)
	<-done

	if _, ok := f.r.engine.Condition("disks/TEST0000000001/health"); ok {
		t.Fatal("stale result recreated a retired drive's condition")
	}
	if _, ok := f.r.engine.Condition("disks/TEST0000000002/health"); !ok {
		t.Fatal("other drive's reading was dropped")
	}
	if countContaining(f.tlg.texts(), "stale") != 0 {
		t.Fatalf("telegram = %q", f.tlg.texts())
	}
}
