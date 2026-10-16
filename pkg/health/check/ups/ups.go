// Package ups judges whether the UPS monitoring service (NUT) returns a fresh
// reading. Every failure to obtain one, including a hang, counts as broken.
package ups

import (
	"context"
	"errors"
	"fmt"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// Status is one fresh UPS reading. Charge and Voltage are nil when the UPS
// does not report them. Its fields match pkg/power.Status so an adapter can
// convert directly.
type Status struct {
	State   string
	Charge  *float64
	Voltage *float64
}

// Source reads the UPS through NUT. Any error means no fresh reading.
type Source interface {
	Status(ctx context.Context) (Status, error)
}

// Check is the UPS freshness check.
type Check struct {
	source Source
	key    string
}

// New creates the check for the named UPS; an empty name is keyed "nut".
func New(source Source, name string) *Check {
	if name == "" {
		name = "nut"
	}

	return &Check{source: source, key: "ups/" + name}
}

// Run reads the UPS once. A read error is a Bad reading, not a check error,
// because a broken NUT service is exactly what this check reports (FR-032).
func (c *Check) Run(ctx context.Context) health.CheckResult {
	status, err := c.source.Status(ctx)
	if err != nil {
		// Shutdown is not a broken UPS: report it as a check error.
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(ctxErr, context.DeadlineExceeded) {
			return health.CheckResult{Err: ctxErr}
		}

		return health.CheckResult{Readings: []health.Reading{{
			Key:     c.key,
			Verdict: health.Bad,
			Text:    fmt.Sprintf("UPS monitoring service is not returning fresh data: %v", err),
		}}}
	}

	var samples []health.Sample
	if status.Charge != nil {
		samples = append(samples, health.Sample{Metric: "ups.charge", Value: *status.Charge})
	}
	if status.Voltage != nil {
		samples = append(samples, health.Sample{Metric: "ups.voltage", Value: *status.Voltage})
	}

	return health.CheckResult{
		Readings: []health.Reading{{
			Key:     c.key,
			Verdict: health.Good,
			Text:    "UPS monitoring service returns fresh data",
		}},
		Samples: samples,
	}
}
