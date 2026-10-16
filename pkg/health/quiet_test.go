package health_test

import (
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

func TestQuiet(t *testing.T) {
	const grace = 10 * time.Minute

	tests := []struct {
		name      string
		maintHeld bool
		uptime    time.Duration
		grace     time.Duration
		wantQuiet bool
		want      string
	}{
		{"normal", false, time.Hour, grace, false, ""},
		{"maintenance", true, time.Hour, grace, true, health.ReasonMaintenance},
		{"boot grace", false, 5 * time.Minute, grace, true, health.ReasonBootGrace},
		{"grace boundary is over", false, grace, grace, false, ""},
		{"just under grace", false, grace - time.Nanosecond, grace, true, health.ReasonBootGrace},
		{"maintenance wins over grace", true, time.Minute, grace, true, health.ReasonMaintenance},
		{"zero grace disables", false, 0, 0, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quiet, reason := health.Quiet(tt.maintHeld, tt.uptime, tt.grace)
			if quiet != tt.wantQuiet || reason != tt.want {
				t.Fatalf("Quiet() = %v, %q; want %v, %q", quiet, reason, tt.wantQuiet, tt.want)
			}
		})
	}
}
