package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
)

// Figures are the memory figures /status and the weekly summary show: the
// host now and its largest services.
type Figures struct {
	Host Host
	// Top are the services holding the most anonymous memory, largest first.
	Top []Service
}

// Figures reads the host and its services now and keeps the n largest
// services. A failed host read returns no figures; a failed services read
// returns the host without services, with the error.
func (c *Check) Figures(ctx context.Context, n int) (Figures, error) {
	host, err := c.source.Host(ctx)
	if err != nil {
		return Figures{}, fmt.Errorf("read host memory: %w", err)
	}
	services, err := c.source.Services(ctx)
	if err != nil {
		return Figures{Host: host}, fmt.Errorf("read services: %w", err)
	}
	top := slices.SortedStableFunc(
		slices.Values(services),
		func(a, b Service) int { return cmp.Compare(b.Anon, a.Anon) },
	)

	return Figures{Host: host, Top: top[:min(n, len(top))]}, nil
}
