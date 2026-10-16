package network_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

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

func verdicts(res health.CheckResult) map[string]health.Verdict {
	m := map[string]health.Verdict{}
	for _, r := range res.Readings {
		m[r.Key] = r.Verdict
	}
	return m
}

func TestRun(t *testing.T) {
	gw := netip.MustParseAddr("192.0.2.1")
	targets := []string{"203.0.113.10", "198.51.100.20"}

	tests := []struct {
		name    string
		gateway fakeGateway
		pinger  *fakePinger
		want    map[string]health.Verdict
		wantErr bool
	}{
		{
			name:    "all up",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"192.0.2.1": true, "203.0.113.10": true}},
			want:    map[string]health.Verdict{"network/router": health.Good, "network/internet": health.Good},
		},
		{
			name:    "any single target suffices",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"192.0.2.1": true, "198.51.100.20": true}},
			want:    map[string]health.Verdict{"network/router": health.Good, "network/internet": health.Good},
		},
		{
			name:    "internet down",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"192.0.2.1": true}},
			want:    map[string]health.Verdict{"network/router": health.Good, "network/internet": health.Bad},
		},
		{
			name:    "router down hides internet",
			gateway: fakeGateway{addr: gw},
			pinger:  &fakePinger{up: map[string]bool{"203.0.113.10": true}},
			want:    map[string]health.Verdict{"network/router": health.Bad},
		},
		{
			name:    "gateway lookup fails",
			gateway: fakeGateway{err: errors.New("no route")},
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
			got := verdicts(res)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

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
	got := verdicts(res)
	if res.Err != nil || got["network/router"] != health.Good || got["network/internet"] != health.Bad {
		t.Fatalf("got %+v", res)
	}
}
