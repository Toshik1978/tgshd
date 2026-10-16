package network_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/network"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
)

type fakeGateway struct {
	addr netip.Addr
	err  error
}

func (f fakeGateway) DefaultGateway() (netip.Addr, error) { return f.addr, f.err }

type fakePinger struct {
	up  map[string]bool
	err error
}

func (f *fakePinger) Reachable(_ context.Context, hosts []string) (map[string]bool, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		out[h] = f.up[h]
	}

	return out, nil
}

func TestRun(t *testing.T) {
	gw := netip.MustParseAddr("192.0.2.1")
	targets := []string{"203.0.113.10", "198.51.100.20"}

	tests := []struct {
		name    string
		gateway fakeGateway
		pinger  *fakePinger
		verdict health.Verdict
		text    string
		wantErr bool
	}{
		{
			name:    "all up",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"192.0.2.1": true, "203.0.113.10": true}},
			verdict: health.Good, text: "Network is reachable",
		},
		{
			name:    "any single target suffices",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"192.0.2.1": true, "198.51.100.20": true}},
			verdict: health.Good, text: "Network is reachable",
		},
		{
			name:    "internet down",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"192.0.2.1": true}},
			verdict: health.Bad, text: "Internet is unreachable (router 192.0.2.1 answers)",
		},
		{
			name:    "router down",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"203.0.113.10": true}},
			verdict: health.Bad, text: "Router 192.0.2.1 is not answering",
		},
		{
			name:    "route table unreadable",
			gateway: fakeGateway{err: errors.New("open /proc/net/route: permission denied")},
			pinger:  &fakePinger{},
			wantErr: true,
		},
		{
			name:    "ping unavailable",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{err: errors.New("no capability")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := network.New(tt.gateway, tt.pinger, targets).Run(t.Context())
			if tt.wantErr {
				if res.Err == nil || len(res.Readings) != 0 {
					t.Fatalf("want error and no readings, got %+v", res)
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("unexpected error: %v", res.Err)
			}
			if len(res.Readings) != 1 {
				t.Fatalf("readings = %+v, want one", res.Readings)
			}
			r := res.Readings[0]
			if r.Key != network.KeyConnectivity || r.Kind != health.State || r.Verdict != tt.verdict ||
				r.Text != tt.text {
				t.Fatalf("reading = %+v, want %s %v %q", r, network.KeyConnectivity, tt.verdict, tt.text)
			}
		})
	}
}

// TestNoRouteIsRouterDown: without a usable default route the network is
// judged down (not unreadable), so the PROBLEM is raised (US5 acc. 1, FR-033).
func TestNoRouteIsRouterDown(t *testing.T) {
	gw := fakeGateway{err: fmt.Errorf("read route: %w", network.ErrNoRoute)}
	pinger := &fakePinger{up: map[string]bool{"203.0.113.10": true}}
	res := network.New(gw, pinger, []string{"203.0.113.10"}).Run(t.Context())
	if res.Err != nil {
		t.Fatalf("err = %v, want a Bad reading", res.Err)
	}
	if len(res.Readings) != 1 || res.Readings[0].Key != network.KeyConnectivity ||
		res.Readings[0].Verdict != health.Bad || res.Readings[0].Text != "No usable default route" {
		t.Fatalf("readings = %+v, want one Bad %q", res.Readings, "No usable default route")
	}

	e := health.NewEngine()
	alerts := e.Apply(t0(), network.Name, 5*time.Minute, 0, res.Readings)
	if len(alerts) != 1 || alerts[0].Kind != health.AlertProblem || alerts[0].Text != "No usable default route" {
		t.Fatalf("alerts = %+v, want one PROBLEM", alerts)
	}
}

// TestCauseShiftOneOutage: an outage whose cause moves between no route, the
// router and the internet is one condition with exactly one PROBLEM and one
// RESOLVED (US5 acc. 2, FR-034, SC-008).
func TestCauseShiftOneOutage(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.1")
	targets := []string{"203.0.113.10"}
	steps := []struct {
		gateway fakeGateway
		up      map[string]bool
	}{
		{fakeGateway{addr: addr}, map[string]bool{"192.0.2.1": true, "203.0.113.10": true}},
		{fakeGateway{addr: addr}, map[string]bool{}},                  // router down
		{fakeGateway{addr: addr}, map[string]bool{"192.0.2.1": true}}, // internet down
		{fakeGateway{err: network.ErrNoRoute}, nil},                   // route lost
		{fakeGateway{addr: addr}, map[string]bool{}},                  // router down again
		{fakeGateway{addr: addr}, map[string]bool{"192.0.2.1": true}}, // internet down again
		{fakeGateway{addr: addr}, map[string]bool{"192.0.2.1": true, "203.0.113.10": true}},
	}

	e := health.NewEngine()
	now := t0()
	var problems, resolved int
	for i, s := range steps {
		res := network.New(s.gateway, &fakePinger{up: s.up}, targets).Run(t.Context())
		if res.Err != nil {
			t.Fatalf("step %d: err = %v", i, res.Err)
		}
		for _, a := range e.Apply(now, network.Name, 5*time.Minute, 5*time.Minute, res.Readings) {
			switch a.Kind {
			case health.AlertProblem:
				problems++
			case health.AlertResolved:
				resolved++
			case health.AlertEvent:
				t.Fatalf("step %d: unexpected event %+v", i, a)
			}
		}
		now = now.Add(5 * time.Minute)
	}
	if problems != 1 || resolved != 1 {
		t.Fatalf("problems = %d, resolved = %d, want 1 and 1", problems, resolved)
	}
}

// TestFormerKeysForgotten: every run, readable or not, lists the 001 keys in
// Forget so a condition stored before the upgrade is cleared silently.
func TestFormerKeysForgotten(t *testing.T) {
	addr := netip.MustParseAddr("192.0.2.1")
	want := []string{"network/router", "network/internet"}
	cases := map[string]health.CheckResult{
		"good": network.New(fakeGateway{addr: addr}, &fakePinger{up: map[string]bool{"192.0.2.1": true, "203.0.113.10": true}}, []string{"203.0.113.10"}).
			Run(t.Context()),
		"bad":      network.New(fakeGateway{addr: addr}, &fakePinger{}, nil).Run(t.Context()),
		"no route": network.New(fakeGateway{err: network.ErrNoRoute}, &fakePinger{}, nil).Run(t.Context()),
		"unreadable": network.New(fakeGateway{err: errors.New("permission denied")}, &fakePinger{}, nil).
			Run(t.Context()),
		"no pinger": network.New(fakeGateway{addr: addr}, nil, nil).Run(t.Context()),
	}
	for name, res := range cases {
		if got := slices.Sorted(slices.Values(res.Forget)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Errorf("%s: Forget = %q, want %q", name, res.Forget, want)
		}
	}
}

func t0() time.Time { return time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC) }

func TestNilPinger(t *testing.T) {
	res := network.New(fakeGateway{addr: netip.MustParseAddr("192.0.2.1")}, nil, nil).Run(t.Context())
	if res.Err == nil {
		t.Fatal("want error for missing pinger")
	}
}

func TestName(t *testing.T) {
	if network.Name != notify.CheckNetwork {
		t.Fatalf("Name = %q, want %q", network.Name, notify.CheckNetwork)
	}
}

type secondCallFails struct {
	calls int
}

func (f *secondCallFails) Reachable(_ context.Context, hosts []string) (map[string]bool, error) {
	f.calls++
	if f.calls > 1 {
		return nil, errors.New("probe failed")
	}

	out := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		out[h] = true
	}

	return out, nil
}

func TestInternetPingError(t *testing.T) {
	gw := fakeGateway{addr: netip.MustParseAddr("192.0.2.1")}
	res := network.New(gw, &secondCallFails{}, []string{"203.0.113.10"}).Run(t.Context())
	if res.Err == nil || len(res.Readings) != 0 {
		t.Fatalf("want error and no readings, got %+v", res)
	}
}

func TestNoInternetTargets(t *testing.T) {
	gw := fakeGateway{addr: netip.MustParseAddr("192.0.2.1")}
	res := network.New(gw, &fakePinger{up: map[string]bool{"192.0.2.1": true}}, nil).Run(t.Context())
	if res.Err != nil || len(res.Readings) != 1 || res.Readings[0].Verdict != health.Bad ||
		res.Readings[0].Text != "Internet is unreachable (router 192.0.2.1 answers)" {
		t.Fatalf("got %+v", res)
	}
}
