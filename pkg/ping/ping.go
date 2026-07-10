package ping

import (
	"context"
	"sync"
	"time"

	pingtool "github.com/prometheus-community/pro-bing"
	"go.uber.org/zap"
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

// Ping pings every host in parallel and returns per-host reachability. A setup
// or send error for a single host is recorded as unreachable (and logged)
// rather than failing the whole batch, so one bad host never masks the rest.
func (p *ping) Ping(ctx context.Context, hosts []string) (map[string]bool, error) {
	resp := make(map[string]bool, len(hosts))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for _, host := range hosts {
		wg.Go(func() {
			reachable := p.pingHost(ctx, host)

			mu.Lock()
			resp[host] = reachable
			mu.Unlock()
		})
	}
	wg.Wait()

	return resp, nil
}

// pingHost reports whether host answered every probe.
func (p *ping) pingHost(ctx context.Context, host string) bool {
	logger := p.logger.With(zap.String("host", host))

	pinger, err := pingtool.NewPinger(host)
	if err != nil {
		logger.With(zap.Error(err)).Warn("Failed to create pinger")
		return false
	}
	pinger.SetPrivileged(true)
	pinger.Count = 3
	pinger.Timeout = 5 * time.Second
	if err := pinger.RunWithContext(ctx); err != nil {
		logger.With(zap.Error(err)).Warn("Failed to ping host")
		return false
	}

	stat := pinger.Statistics()

	return stat.PacketsSent == stat.PacketsRecv
}
