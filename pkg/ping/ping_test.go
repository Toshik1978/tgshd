package ping

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	pingtool "github.com/prometheus-community/pro-bing"
	"go.uber.org/zap"
)

func fakeProbe(stats map[string]*pingtool.Statistics, errHosts map[string]error) probeFunc {
	return func(_ context.Context, host string) (*pingtool.Statistics, error) {
		if err := errHosts[host]; err != nil {
			return nil, err
		}

		return stats[host], nil
	}
}

func stat(sent, recv int) *pingtool.Statistics {
	return &pingtool.Statistics{PacketsSent: sent, PacketsRecv: recv}
}

func TestPingAndReachable(t *testing.T) {
	stats := map[string]*pingtool.Statistics{
		"all":  stat(3, 3),
		"some": stat(3, 1),
		"none": stat(3, 0),
	}
	hosts := []string{"all", "some", "none"}
	p := &ping{logger: zap.NewNop(), probe: fakeProbe(stats, nil)}

	got, err := p.Ping(t.Context(), hosts)
	if err != nil {
		t.Fatal(err)
	}
	if !got["all"] || got["some"] || got["none"] {
		t.Errorf("Ping = %v", got)
	}

	got, err = p.Reachable(t.Context(), hosts)
	if err != nil {
		t.Fatal(err)
	}
	if !got["all"] || !got["some"] || got["none"] {
		t.Errorf("Reachable = %v", got)
	}
}

func TestSetupFailure(t *testing.T) {
	boom := errors.New("operation not permitted")
	p := &ping{
		logger: zap.NewNop(),
		probe:  fakeProbe(map[string]*pingtool.Statistics{"ok": stat(3, 3)}, map[string]error{"bad": boom}),
	}

	if _, err := p.Reachable(t.Context(), []string{"ok", "bad"}); !errors.Is(err, boom) {
		t.Errorf("Reachable err = %v, want wrapped %v", err, boom)
	}

	got, err := p.Ping(t.Context(), []string{"ok", "bad"})
	if err != nil || !got["ok"] || got["bad"] {
		t.Errorf("Ping = %v, %v; want ok up, bad down, no error", got, err)
	}
}

func TestIsOutage(t *testing.T) {
	sendErr := &net.OpError{Op: "write", Err: os.NewSyscallError("sendto", syscall.ENETUNREACH)}
	permErr := &net.OpError{Op: "listen", Err: os.NewSyscallError("socket", syscall.EPERM)}
	dnsErr := &net.DNSError{Err: "no such host", Name: "example.org"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"network unreachable", fmt.Errorf("run pinger: %w", sendErr), true},
		{"host unreachable", fmt.Errorf("run: %w", syscall.EHOSTUNREACH), true},
		{"network down", fmt.Errorf("run: %w", syscall.ENETDOWN), true},
		{"address not available", fmt.Errorf("run: %w", syscall.EADDRNOTAVAIL), true},
		{"dns failure", fmt.Errorf("create pinger: %w", dnsErr), true},
		{"permission", fmt.Errorf("run: %w", permErr), false},
		{"access", fmt.Errorf("run: %w", syscall.EACCES), false},
		{"af unsupported", fmt.Errorf("run: %w", syscall.EAFNOSUPPORT), false},
		{"proto unsupported", fmt.Errorf("run: %w", syscall.EPROTONOSUPPORT), false},
		{"listen op", &net.OpError{Op: "listen", Err: errors.New("boom")}, false},
		{"cancelled", context.Canceled, false},
		{"unknown", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOutage(tt.err); got != tt.want {
				t.Errorf("isOutage = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutageIsDownNotError(t *testing.T) {
	p := &ping{
		logger: zap.NewNop(),
		probe:  fakeProbe(nil, map[string]error{"gw": fmt.Errorf("run: %w", syscall.ENETUNREACH)}),
	}

	got, err := p.Reachable(t.Context(), []string{"gw"})
	if err != nil || got["gw"] {
		t.Errorf("Reachable = %v, %v; want down with nil error", got, err)
	}
}
