package ping

import (
	"context"
	"fmt"
	"time"

	pingtool "github.com/prometheus-community/pro-bing"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type ping struct {
	logger *zap.Logger
}

// New instantiate new object to do ping test.
func New(logger *zap.Logger) *ping {
	logger.Info("Ping object created")
	return &ping{
		logger: logger,
	}
}

// Ping do actual ping test and return per-host status.
func (p *ping) Ping(ctx context.Context, hosts []string) (map[string]bool, error) {
	// Do ping in parallel
	grp, _ := errgroup.WithContext(ctx)
	ch := make(chan string, len(hosts))
	for _, host := range hosts {
		grp.Go(func() error {
			pinger, err := pingtool.NewPinger(host)
			if err != nil {
				return fmt.Errorf("failed to ping: %w", err)
			}
			pinger.SetPrivileged(true)
			pinger.Count = 3
			pinger.Timeout = 5 * time.Second
			if err := pinger.Run(); err != nil {
				return fmt.Errorf("failed to ping: %w", err)
			}

			stat := pinger.Statistics()
			if stat.PacketsSent == stat.PacketsRecv {
				ch <- host
			}

			return nil
		})
	}
	if err := grp.Wait(); err != nil {
		return nil, fmt.Errorf("failed to ping: %w", err)
	}
	close(ch)

	// Collect all successful results
	resp := make(map[string]bool)
	for host := range ch {
		resp[host] = true
	}

	return resp, nil
}
