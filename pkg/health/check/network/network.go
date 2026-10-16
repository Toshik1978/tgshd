// Package network checks reachability of the default gateway and the internet.
package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// Name is the check name used for routing and state.
const Name = "network"

// Condition keys produced by the check.
const (
	KeyRouter   = Name + "/router"
	KeyInternet = Name + "/internet"
)

// GatewaySource reports the host's default gateway.
type GatewaySource interface {
	DefaultGateway() (netip.Addr, error)
}

// Pinger probes every host and reports per-host reachability (any reply means up).
type Pinger interface {
	Reachable(ctx context.Context, hosts []string) (map[string]bool, error)
}

// Check evaluates gateway and internet reachability.
type Check struct {
	gateway GatewaySource
	pinger  Pinger
	targets []string
}

// New creates the check; targets are the internet hosts, any one answering means up.
func New(gateway GatewaySource, pinger Pinger, targets []string) *Check {
	return &Check{gateway: gateway, pinger: pinger, targets: targets}
}

// Run pings the gateway, then (only if it answers) the internet targets.
// While the gateway is down the internet is not judged and its reading is omitted.
// Failing to find the gateway or to ping makes the whole check Unknown.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	if c.pinger == nil {
		return health.CheckResult{Err: errors.New("ping is not available")}
	}
	gw, err := c.gateway.DefaultGateway()
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("find default gateway: %w", err)}
	}
	gwHost := gw.String()

	up, err := c.pinger.Reachable(ctx, []string{gwHost})
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("ping gateway: %w", err)}
	}
	if !up[gwHost] {
		return health.CheckResult{Readings: []health.Reading{
			{Key: KeyRouter, Verdict: health.Bad, Text: "Router " + gwHost + " is not answering"},
		}}
	}
	router := health.Reading{Key: KeyRouter, Verdict: health.Good, Text: "Router " + gwHost + " is reachable"}

	up, err = c.pinger.Reachable(ctx, c.targets)
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("ping internet targets: %w", err)}
	}
	internet := health.Reading{Key: KeyInternet, Verdict: health.Bad, Text: "Internet is unreachable"}
	for _, t := range c.targets {
		if up[t] {
			internet.Verdict = health.Good
			internet.Text = "Internet is reachable"
			break
		}
	}

	return health.CheckResult{Readings: []health.Reading{router, internet}}
}
