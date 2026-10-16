package runner

import (
	"testing"
	"time"
)

func TestDue(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	iv := 5 * time.Minute
	tests := []struct {
		name string
		last time.Time
		now  time.Time
		want bool
	}{
		{"never run", time.Time{}, t0, true},
		{"just ran", t0, t0, false},
		{"half way", t0, t0.Add(iv / 2), false},
		{"one tick early", t0, t0.Add(iv - Tick), false},
		{"interval elapsed", t0, t0.Add(iv), true},
		{"tick jitter tolerated", t0, t0.Add(iv - time.Second), true},
		{"long overdue", t0, t0.Add(10 * iv), true},
		{"clock went backwards", t0, t0.Add(-time.Hour), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := due(tt.last, tt.now, iv); got != tt.want {
				t.Fatalf("due(%v, %v) = %v, want %v", tt.last, tt.now, got, tt.want)
			}
		})
	}
}
