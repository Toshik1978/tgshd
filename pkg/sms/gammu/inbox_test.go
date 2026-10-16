package gammu

import (
	"errors"
	"testing"
	"time"
)

type fakeRow struct {
	vals []any
	err  error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != len(f.vals) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		if err := assign(d, f.vals[i]); err != nil {
			return err
		}
	}

	return nil
}

func assign(dest, val any) error {
	var ok bool
	switch p := dest.(type) {
	case *int64:
		*p, ok = val.(int64)
	case *string:
		*p, ok = val.(string)
	case *time.Time:
		*p, ok = val.(time.Time)
	}
	if !ok {
		return errors.New("unsupported destination or value type")
	}

	return nil
}

func TestScanInboxMessage(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	row := fakeRow{vals: []any{int64(7), "+10000000000", "hello", "050003CC0201", at}}

	got, err := scanInboxMessage(row)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := InboxMessage{
		ID: 7, Sender: "+10000000000", Text: "hello", UDH: "050003CC0201",
		ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local),
	}
	if !got.ReceivedAt.Equal(want.ReceivedAt) || got.ReceivedAt.Location() != time.Local {
		t.Errorf("ReceivedAt = %v, want the stored wall clock in local time %v", got.ReceivedAt, want.ReceivedAt)
	}
	got.ReceivedAt, want.ReceivedAt = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWallClockIn(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*60*60)
	// pgx decodes a timestamp without time zone as that wall clock in UTC.
	stored := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

	got := wallClockIn(stored, zone)
	want := time.Date(2026, 1, 2, 3, 4, 5, 6, zone)
	if !got.Equal(want) || got.Location() != zone {
		t.Errorf("wallClockIn = %v, want %v", got, want)
	}
	if d := stored.Sub(got); d != 3*time.Hour {
		t.Errorf("instant moved by %v, want 3h (the zone offset)", d)
	}
}

func TestScanInboxMessageError(t *testing.T) {
	boom := errors.New("boom")
	if _, err := scanInboxMessage(fakeRow{err: boom}); !errors.Is(err, boom) {
		t.Errorf("expected wrapped boom, got %v", err)
	}
}
