// Package network checks reachability of the default gateway and the internet
// as one connectivity condition.
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

// KeyConnectivity is the one condition the check produces: bad while the
// host cannot reach the internet, whatever the cause.
const KeyConnectivity = Name + "/connectivity"

// The 001 condition keys, cleared silently by every run (R21).
const (
	formerKeyRouter   = Name + "/router"
	formerKeyInternet = Name + "/internet"
)

// ErrNoRoute is what a GatewaySource returns (possibly wrapped) when the
// host has no usable default route; the check judges it as the router down.
var ErrNoRoute = errors.New("no usable default route")

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

// Run judges connectivity as one condition: Bad when there is no default
// route, when the gateway does not answer, or (gateway up) when no internet
// target answers. A changing cause stays one Bad streak. Failing to read the
// route table or to ping makes the run unreadable (Err). Every run lists the
// 001 keys in Forget.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	res := c.run(ctx)
	res.Forget = []string{formerKeyRouter, formerKeyInternet}

	return res
}

func (c *Check) run(ctx context.Context) health.CheckResult {
	if c.pinger == nil {
		return health.CheckResult{Err: errors.New("ping is not available")}
	}
	gw, err := c.gateway.DefaultGateway()
	if errors.Is(err, ErrNoRoute) {
		return bad("No usable default route")
	}
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("find default gateway: %w", err)}
	}
	gwHost := gw.String()

	up, err := c.pinger.Reachable(ctx, []string{gwHost})
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("ping gateway: %w", err)}
	}
	if !up[gwHost] {
		return bad("Router " + gwHost + " is not answering")
	}

	up, err = c.pinger.Reachable(ctx, c.targets)
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("ping internet targets: %w", err)}
	}
	for _, t := range c.targets {
		if up[t] {
			return health.CheckResult{Readings: []health.Reading{
				{Key: KeyConnectivity, Verdict: health.Good, Text: "Network is reachable"},
			}}
		}
	}

	return bad("Internet is unreachable (router " + gwHost + " answers)")
}

func bad(text string) health.CheckResult {
	return health.CheckResult{Readings: []health.Reading{{Key: KeyConnectivity, Verdict: health.Bad, Text: text}}}
}
