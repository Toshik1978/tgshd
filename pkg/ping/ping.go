package ping

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	pingtool "github.com/prometheus-community/pro-bing"
	"go.uber.org/zap"
)

// probeFunc sends the probes to host. It returns an error for any failure to
// run the probes, including network-level ones (unreachable network, DNS
// failure); a host that simply does not answer yields statistics with no
// received packets. Callers classify the error with isOutage.
type probeFunc func(ctx context.Context, host string) (*pingtool.Statistics, error)

type ping struct {
	logger *zap.Logger
	probe  probeFunc
}

// New instantiate new object to do ping test.
func New(logger *zap.Logger) *ping {
	logger.Info("Ping object created")

	return &ping{
		logger: logger,
		probe:  probeHost,
	}
}

// Ping pings every host in parallel and returns per-host reachability. A host
// counts as reachable only if every one of its probes was answered. A setup or
// send error for a single host is recorded as unreachable (and logged) rather
// than failing the whole batch, so one bad host never masks the rest.
func (p *ping) Ping(ctx context.Context, hosts []string) (map[string]bool, error) {
	resp, _ := p.probeAll(ctx, hosts, func(s *pingtool.Statistics) bool { return s.PacketsSent == s.PacketsRecv })

	return resp, nil
}

// Reachable is like Ping but tolerates packet loss: a host is up when at
// least one of its probes was answered. Unlike Ping it reports probing
// failures: if any host could not be probed because of a setup error (for
// example missing CAP_NET_RAW) it returns an error, so callers can tell "could
// not check" from "no answer".
func (p *ping) Reachable(ctx context.Context, hosts []string) (map[string]bool, error) {
	resp, err := p.probeAll(ctx, hosts, func(s *pingtool.Statistics) bool { return s.PacketsRecv > 0 })
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// probeAll probes every host in parallel and judges each with up. The error is
// the joined probing failures, if any; failed hosts are recorded as down.
func (p *ping) probeAll(
	ctx context.Context, hosts []string, up func(*pingtool.Statistics) bool,
) (map[string]bool, error) {
	resp := make(map[string]bool, len(hosts))
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)

	for _, host := range hosts {
		wg.Go(func() {
			reachable, err := p.pingHost(ctx, host, up)

			mu.Lock()
			resp[host] = reachable
			if err != nil {
				errs = append(errs, err)
			}
			mu.Unlock()
		})
	}
	wg.Wait()

	return resp, errors.Join(errs...)
}

// pingHost probes host and reports whether up accepts the outcome. A network
// outage error (see isOutage) is logged and counts as unreachable with a nil
// error; any other probing failure is logged, returned, and counts as unreachable.
func (p *ping) pingHost(ctx context.Context, host string, up func(*pingtool.Statistics) bool) (bool, error) {
	stat, err := p.probe(ctx, host)
	if err != nil {
		p.logger.With(zap.String("host", host), zap.Error(err)).Warn("Failed to ping host")

		if isOutage(err) {
			return false, nil
		}

		return false, fmt.Errorf("ping %s: %w", host, err)
	}

	return up(stat), nil
}

// isOutage reports whether err means the network cannot reach the host (no
// route, interface down, unresolvable name), which is a genuine "down" result.
// Everything else (missing privileges, socket creation failure, cancelled
// context, unknown errors) means the probe could not be carried out.
func isOutage(err error) bool {
	for _, errno := range []syscall.Errno{
		syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.EADDRNOTAVAIL,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}

	var dnsErr *net.DNSError

	return errors.As(err, &dnsErr)
}

// probeHost sends three privileged ICMP probes to host.
func probeHost(ctx context.Context, host string) (*pingtool.Statistics, error) {
	pinger, err := pingtool.NewPinger(host)
	if err != nil {
		return nil, fmt.Errorf("create pinger: %w", err)
	}
	pinger.SetPrivileged(true)
	pinger.Count = 3
	pinger.Timeout = 5 * time.Second
	if err := pinger.RunWithContext(ctx); err != nil {
		return nil, fmt.Errorf("run pinger: %w", err)
	}

	return pinger.Statistics(), nil
}
